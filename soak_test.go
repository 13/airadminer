package main

// Soak test: a mixed workload over real HTTP for AIRADMINER_SOAK (e.g. 10m),
// with every database connection of the server killed now and then.
//
//	AIRADMINER_SOAK=10m make test-integration   # or: make soak
//
// It fails if memory or goroutines keep growing, a connection is left
// checked out, a request fails unexpectedly, or the server does not
// recover after its connections are killed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestSoak(t *testing.T) {
	dur, err := time.ParseDuration(os.Getenv("AIRADMINER_SOAK"))
	if err != nil || dur <= 0 {
		t.Skip("set AIRADMINER_SOAK to a duration, e.g. 10m")
	}
	db := testDB(t, `CREATE TABLE items (id int PRIMARY KEY, v text, n bigint, doc jsonb);
		INSERT INTO items SELECT g, 'v' || g, g, jsonb_build_object('k', repeat('x', 300 * (g % 50))) FROM generate_series(1, 100000) g;
		CREATE TABLE nopk (v text); INSERT INTO nopk SELECT 'r' || g FROM generate_series(1, 5000) g;
		ANALYZE;`)
	// Each worker is its own browser behind a proxy, with its own address,
	// as the sign-in rate limit counts per address.
	a := newApp(config{addr: "127.0.0.1:0", poolSize: 4, maxSessions: 16, trustProxy: "127.0.0.1"})
	defer a.closeAll()
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	u := testDSN(t)
	pw, _ := u.User.Password()

	type stat struct {
		mu        sync.Mutex
		lat       []time.Duration
		codes     map[int]int
		unexpect  []string
		inChaos   atomic.Bool
		chaosErrs int
	}
	st := &stat{codes: map[int]int{}}

	type browser struct {
		*http.Client
		ip string
	}
	var nextIP atomic.Int32
	newClient := func() browser {
		jar, _ := cookiejar.New(nil)
		n := nextIP.Add(1)
		return browser{&http.Client{Jar: jar, Timeout: time.Minute}, fmt.Sprintf("10.1.%d.%d", n/250, n%250+1)}
	}
	call := func(c browser, path string, body any, ok ...int) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+"/api/"+path, bytes.NewReader(b))
		req.Header.Set("X-Airadminer", "1")
		req.Header.Set("X-Forwarded-For", c.ip)
		start := time.Now()
		res, err := c.Do(req)
		if err != nil {
			st.mu.Lock()
			st.unexpect = append(st.unexpect, path+": "+err.Error())
			st.mu.Unlock()
			return 0
		}
		msg, _ := io.ReadAll(res.Body)
		res.Body.Close()
		d := time.Since(start)
		st.mu.Lock()
		defer st.mu.Unlock()
		st.lat = append(st.lat, d)
		st.codes[res.StatusCode]++
		good := res.StatusCode == 200
		for _, o := range ok {
			good = good || res.StatusCode == o
		}
		if !good {
			// While connections are being killed, a request may fail with
			// 503 (unavailable, try again); nothing else may.
			if st.inChaos.Load() && res.StatusCode == 503 {
				st.chaosErrs++
			} else if len(st.unexpect) < 50 {
				st.unexpect = append(st.unexpect, fmt.Sprintf("%s: %d %s", path, res.StatusCode, msg))
			}
		}
		return res.StatusCode
	}
	login := func(c browser) {
		if code := call(c, "login", loginBody(u, db, pw)); code != 200 {
			t.Fatalf("login: %d", code)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	var wg sync.WaitGroup
	var ops atomic.Int64
	ref := func(name string) map[string]any { return map[string]any{"schema": "public", "name": name} }
	with := func(m map[string]any, kv ...any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := newClient()
			login(c)
			rng := rand.New(rand.NewPCG(uint64(w), 7))
			for i := 0; ctx.Err() == nil; i++ {
				id := fmt.Sprint(w*1000 + rng.IntN(1000) + 1)
				switch rng.IntN(12) {
				case 0, 1, 2:
					call(c, "rows", with(ref("items"), "offset", rng.IntN(99000), "limit", 200))
				case 3:
					call(c, "rows", with(ref("items"), "after", map[string]any{"id": id}, "limit", 200))
				case 4:
					call(c, "count", with(ref("items"), "search", "v1"))
				case 5:
					call(c, "update", with(ref("items"), "key", map[string]any{"id": id}, "values", map[string]any{"v": fmt.Sprint("w", w, "-", i)}))
				case 6:
					var ups []any
					for k := 0; k < 20; k++ {
						ups = append(ups, map[string]any{"key": map[string]any{"id": fmt.Sprint(w*1000 + k + 1)}, "values": map[string]any{"n": fmt.Sprint(i)}})
					}
					call(c, "batch", with(ref("items"), "updates", ups))
				case 7:
					call(c, "cells", with(ref("items"), "keys", []any{map[string]any{"id": id}}, "cols", []string{"doc"}))
				case 8:
					call(c, "query", map[string]any{"sql": "SET work_mem = '8MB'; SELECT count(*), sum(n) FROM items WHERE id % 7 = " + fmt.Sprint(w%7)})
				case 9:
					call(c, "export", with(ref("nopk")))
				case 10:
					call(c, "rows", with(ref("nopk"), "offset", rng.IntN(4800), "limit", 200))
				case 11:
					if rng.IntN(10) == 0 { // a session ends and a new one starts
						call(c, "logout", nil)
						c = newClient()
						login(c)
					} else {
						call(c, "meta", nil)
					}
				}
				ops.Add(1)
			}
		}(w)
	}

	// Chaos: every 20 s kill all of the server's connections, as a database
	// restart or failover would.
	adminURL := *u
	adminURL.Path = "/" + db
	admin, err := pgx.Connect(context.Background(), adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	type sample struct {
		at    time.Duration
		heap  uint64
		gor   int
		conns int64
	}
	var samples []sample
	begin := time.Now()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for n := 1; ctx.Err() == nil; n++ {
		select {
		case <-ctx.Done():
		case <-tick.C:
			if n%2 == 0 {
				st.inChaos.Store(true)
				_, _ = admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'airadminer' AND datname = $1`, db)
				time.AfterFunc(3*time.Second, func() { st.inChaos.Store(false) })
			}
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			_, conns := a.poolStats()
			samples = append(samples, sample{time.Since(begin).Round(time.Second), ms.HeapAlloc, runtime.NumGoroutine(), conns[0]})
		}
	}
	wg.Wait()
	srv.CloseClientConnections()

	// Report.
	st.mu.Lock()
	sort.Slice(st.lat, func(i, j int) bool { return st.lat[i] < st.lat[j] })
	q := func(p float64) time.Duration { return st.lat[int(float64(len(st.lat)-1)*p)] }
	t.Logf("%d requests in %v (%.0f/s), p50 %v, p99 %v, max %v; status codes %v; %d failures while connections were being killed",
		ops.Load(), dur, float64(ops.Load())/dur.Seconds(), q(0.5), q(0.99), q(1), st.codes, st.chaosErrs)
	for _, s := range samples {
		t.Logf("  %6v heap %6.1f MB  goroutines %4d  connections in use %d", s.at, float64(s.heap)/1e6, s.gor, s.conns)
	}
	for _, e := range st.unexpect {
		t.Error("unexpected: " + e)
	}
	st.mu.Unlock()

	// Recovered: a fresh request works and nothing is left checked out.
	c := newClient()
	login(c)
	if code := call(c, "rows", with(ref("items"), "limit", 10)); code != 200 {
		t.Fatalf("after the soak: %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, conns := a.poolStats()
		if conns[0] == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections still checked out", conns[0])
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Flat memory and goroutines: the second half may not grow beyond the
	// first half's peak by more than a margin.
	if len(samples) >= 6 {
		half := len(samples) / 2
		var peakHeap uint64
		peakGor := 0
		for _, s := range samples[1:half] {
			peakHeap = max(peakHeap, s.heap)
			peakGor = max(peakGor, s.gor)
		}
		last := samples[len(samples)-1]
		if last.heap > peakHeap*3/2+32<<20 {
			t.Errorf("heap grew: peak %d MB in the first half, %d MB at the end", peakHeap>>20, last.heap>>20)
		}
		if last.gor > peakGor+50 {
			t.Errorf("goroutines grew: peak %d in the first half, %d at the end", peakGor, last.gor)
		}
	}
}
