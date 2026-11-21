package main

// Integration tests run against a real PostgreSQL. They are skipped unless
// AIRADMINER_TEST_DSN points at a superuser connection, for example:
//
//	make demo
//	AIRADMINER_TEST_DSN=postgres://postgres:secret@127.0.0.1:55432/postgres go test ./...

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func testDSN(t *testing.T) *url.URL {
	t.Helper()
	dsn := os.Getenv("AIRADMINER_TEST_DSN")
	if dsn == "" {
		t.Skip("AIRADMINER_TEST_DSN not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// testDB creates a scratch database and returns its name.
func testDB(t *testing.T, setup string) string {
	t.Helper()
	u := testDSN(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("aa_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), u.String())
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	if setup != "" {
		du := *u
		du.Path = "/" + name
		c, err := pgx.Connect(ctx, du.String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, setup); err != nil {
			t.Fatal(err)
		}
	}
	return name
}

type client struct {
	t   *testing.T
	h   http.Handler
	sid string
	db  string // sent as X-Airadminer-Db when set
}

func (c *client) call(path string, body any) (int, map[string]any) {
	c.t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "http://127.0.0.1/api/"+path, bytes.NewReader(b))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Airadminer", "1")
	if c.db != "" {
		req.Header.Set(dbHeader, url.PathEscape(c.db))
	}
	if c.sid != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: c.sid})
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == cookieName {
			c.sid = ck.Value
		}
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func loginBody(u *url.URL, db, password string) map[string]any {
	return map[string]any{"host": u.Hostname(), "port": u.Port(), "user": u.User.Username(), "password": password, "database": db}
}

func signIn(t *testing.T, a *App, db string) *client {
	t.Helper()
	u := testDSN(t)
	pw, _ := u.User.Password()
	c := &client{t: t, h: a.routes()}
	if code, out := c.call("login", loginBody(u, db, pw)); code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	return c
}

// C1: an empty password must not be filled in from the server's own
// PGPASSWORD or ~/.pgpass.
func TestIntegrationLoginDoesNotUseServerCredentials(t *testing.T) {
	u := testDSN(t)
	pw, _ := u.User.Password()
	if pw == "" {
		t.Skip("test DSN has no password")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PGPASSWORD", pw)
	os.WriteFile(filepath.Join(home, ".pgpass"), []byte("*:*:*:*:"+pw+"\n"), 0o600)
	a := newApp(config{addr: "127.0.0.1:0"})
	c := &client{t: t, h: a.routes()}
	code, out := c.call("login", loginBody(u, "postgres", ""))
	if code == 200 {
		t.Fatalf("signed in with an empty password using the server's credentials: %v", out)
	}
}

func TestIntegrationSessionCap(t *testing.T) {
	db := testDB(t, "")
	a := newApp(config{addr: "127.0.0.1:0", maxSessions: 1})
	signIn(t, a, db)
	u := testDSN(t)
	pw, _ := u.User.Password()
	c := &client{t: t, h: a.routes()}
	if code, _ := c.call("login", loginBody(u, db, pw)); code != http.StatusServiceUnavailable {
		t.Fatalf("second session: got %d, want 503", code)
	}
	a.closeAll()
}

// C3: on a table without a primary key, an edit must not land on another
// row that reused the slot of the row the browser saw.
func TestIntegrationCtidReuseIsRefused(t *testing.T) {
	db := testDB(t, `CREATE TABLE nopk (v text) WITH (autovacuum_enabled = off);
		INSERT INTO nopk VALUES ('first');`)
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	c := signIn(t, a, db)
	ref := map[string]any{"schema": "public", "name": "nopk"}
	_, out := c.call("rows", ref)
	key := out["rows"].([]any)[0].([]any)[0].(string)

	// Someone else deletes the row, VACUUM frees the slot, a new row takes it.
	u := testDSN(t)
	du := *u
	du.Path = "/" + db
	other, err := pgx.Connect(context.Background(), du.String())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	var before, after string
	other.QueryRow(context.Background(), "SELECT ctid::text FROM nopk").Scan(&before)
	other.Exec(context.Background(), "DELETE FROM nopk")
	other.Exec(context.Background(), "VACUUM nopk")
	other.Exec(context.Background(), "INSERT INTO nopk VALUES ('second')")
	other.QueryRow(context.Background(), "SELECT ctid::text FROM nopk").Scan(&after)
	if before != after {
		t.Skipf("slot was not reused (%s then %s); cannot reproduce", before, after)
	}

	code, res := c.call("update", map[string]any{"schema": "public", "name": "nopk", "key": map[string]any{"__ctid": key}, "values": map[string]any{"v": "overwritten"}})
	if code == 200 {
		t.Fatalf("update hit the unrelated row: %v", res)
	}
	var v string
	other.QueryRow(context.Background(), "SELECT v FROM nopk").Scan(&v)
	if v != "second" {
		t.Fatalf("unrelated row changed to %q", v)
	}

	// The key the update returns must work for the next edit.
	_, out = c.call("rows", ref)
	key = out["rows"].([]any)[0].([]any)[0].(string)
	code, res = c.call("update", map[string]any{"schema": "public", "name": "nopk", "key": map[string]any{"__ctid": key}, "values": map[string]any{"v": "a"}})
	if code != 200 {
		t.Fatalf("fresh key refused: %v", res)
	}
	key = res["row"].([]any)[0].(string)
	if code, res = c.call("update", map[string]any{"schema": "public", "name": "nopk", "key": map[string]any{"__ctid": key}, "values": map[string]any{"v": "b"}}); code != 200 {
		t.Fatalf("returned key refused: %v", res)
	}
}

// C4: session state set in the SQL editor must not survive into the next
// request that reuses the pooled connection.
func TestIntegrationSQLEditorStateIsReset(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (id int PRIMARY KEY, v text);
		INSERT INTO t VALUES (1, 'a');
		CREATE TABLE audit (n int);
		CREATE FUNCTION trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO audit VALUES (1); RETURN NEW; END $$;
		CREATE TRIGGER t_audit AFTER UPDATE ON t FOR EACH ROW EXECUTE FUNCTION trg();`)
	a := newApp(config{addr: "127.0.0.1:0", poolSize: 1})
	defer a.closeAll()
	c := signIn(t, a, db)
	if code, out := c.call("query", map[string]any{"sql": "SET session_replication_role = replica; SET statement_timeout = '1h'; CREATE TEMP TABLE leak (x int); SELECT pg_advisory_lock(42)"}); code != 200 || out["error"] != nil {
		t.Fatalf("query: %d %v", code, out)
	}
	// With one connection in the pool, this edit runs on the same connection.
	if code, out := c.call("update", map[string]any{"schema": "public", "name": "t", "key": map[string]any{"id": "1"}, "values": map[string]any{"v": "b"}}); code != 200 {
		t.Fatalf("update: %d %v", code, out)
	}
	_, out := c.call("query", map[string]any{"sql": "SELECT current_setting('session_replication_role'), current_setting('statement_timeout'), to_regclass('pg_temp.leak') IS NULL, (SELECT count(*) FROM audit), (SELECT count(*) FROM pg_locks WHERE locktype = 'advisory')"})
	row := out["results"].([]any)[0].(map[string]any)["rows"].([]any)[0].([]any)
	got := fmt.Sprint(row...)
	if want := "origin0t10"; strings.ReplaceAll(got, " ", "") != want {
		t.Fatalf("session state leaked: %v (want role=origin, timeout=0, no temp table, trigger fired once, no advisory lock)", row)
	}
}

// C5: a request built for one database must not run on another after the
// session switched (for example from another tab).
func TestIntegrationDatabaseSwitchIsDetected(t *testing.T) {
	setup := `CREATE TABLE t (id int PRIMARY KEY, v text); INSERT INTO t VALUES (1, 'orig');`
	dbA := testDB(t, setup)
	dbB := testDB(t, setup)
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	tabA := signIn(t, a, dbA)
	tabA.db = dbA
	tabB := &client{t: t, h: tabA.h, sid: tabA.sid}
	if code, out := tabB.call("use", map[string]any{"database": dbB}); code != 200 {
		t.Fatalf("use: %v", out)
	}
	code, out := tabA.call("update", map[string]any{"schema": "public", "name": "t", "key": map[string]any{"id": "1"}, "values": map[string]any{"v": "wrong db"}})
	if code != http.StatusConflict || out["code"] != "dbchanged" {
		t.Fatalf("stale tab edit: got %d %v, want 409 dbchanged", code, out)
	}
	tabB.db = dbB
	_, out = tabB.call("rows", map[string]any{"schema": "public", "name": "t"})
	if v := out["rows"].([]any)[0].([]any)[1]; v != "orig" {
		t.Fatalf("database B changed: %v", v)
	}
}

// A failed write inside writeMany must leave the connection usable.
func TestIntegrationFailedBatchRollsBack(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (id int PRIMARY KEY, v text NOT NULL); INSERT INTO t VALUES (1, 'a'), (2, 'b');`)
	a := newApp(config{addr: "127.0.0.1:0", poolSize: 1})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	code, out := c.call("batch", map[string]any{"schema": "public", "name": "t", "updates": []any{
		map[string]any{"key": map[string]any{"id": "1"}, "values": map[string]any{"v": "changed"}},
		map[string]any{"key": map[string]any{"id": "2"}, "values": map[string]any{"v": nil}},
	}})
	if code == 200 || out["index"] != float64(1) {
		t.Fatalf("batch: %d %v", code, out)
	}
	_, out = c.call("rows", map[string]any{"schema": "public", "name": "t"})
	if v := out["rows"].([]any)[0].([]any)[1]; v != "a" {
		t.Fatalf("partial batch committed: %v", v)
	}
}

// H4: the expected values the browser sends are the text it received. That
// text must compare equal for every common type, or edits would be refused
// spuriously. And a cell changed by someone else must make the edit fail.
func TestIntegrationExpectRoundTripsAndDetectsConflicts(t *testing.T) {
	db := testDB(t, `CREATE TYPE mood AS ENUM ('ok', 'sad');
		CREATE TABLE t (id int PRIMARY KEY, j json, jb jsonb, f float8, r real, n numeric, ts timestamptz, d date,
		  b bool, a int[], m mood, u uuid, iv interval, ip inet, big bigint, x text, bin bytea, host inet, ch char(5), xm xml, pt point, tsr tstzrange, bits bit varying(8), money money);
		INSERT INTO t VALUES (1, '{"b": 1,  "a": 12345678901234567890}', '{"z": [1, 2.50]}', 0.1, 1.1, 1.10,
		  '2025-01-02 03:04:05.123456+00', '2025-01-02', true, '{1,NULL,3}', 'sad', gen_random_uuid(), '1 day 02:03:04',
		  '10.0.0.1/8', 9223372036854775807, E'line\nnext', '\xdeadbeef', '10.0.0.1', 'ab', '<a>1</a>', '(1.5,2)', '[2025-01-01,2025-02-01)', B'101', 12.34);`)
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	ref := map[string]any{"schema": "public", "name": "t"}
	_, out := c.call("table", ref)
	var names []string
	for _, col := range out["columns"].([]any) {
		names = append(names, col.(map[string]any)["name"].(string))
	}
	_, out = c.call("rows", ref)
	row := out["rows"].([]any)[0].([]any)
	expect := map[string]any{}
	for i, n := range names {
		if n != "id" {
			expect[n] = row[i]
		}
	}
	for n, v := range expect {
		if code, res := c.call("update", map[string]any{"schema": "public", "name": "t", "key": map[string]any{"id": "1"}, "values": map[string]any{"id": "1"}, "expect": map[string]any{n: v}}); code != 200 {
			t.Errorf("column %s: text %v does not compare equal: %v", n, v, res)
		}
	}
	code, res := c.call("update", map[string]any{"schema": "public", "name": "t", "key": map[string]any{"id": "1"}, "values": map[string]any{"x": "changed"}, "expect": expect})
	if code != 200 {
		t.Fatalf("expect with unchanged values refused: %v", res)
	}

	// Someone else changes x; an edit based on the old value must fail.
	u := testDSN(t)
	du := *u
	du.Path = "/" + db
	other, err := pgx.Connect(context.Background(), du.String())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if _, err := other.Exec(context.Background(), "UPDATE t SET x = 'theirs'"); err != nil {
		t.Fatal(err)
	}
	code, res = c.call("update", map[string]any{"schema": "public", "name": "t", "key": map[string]any{"id": "1"}, "values": map[string]any{"x": "mine"}, "expect": map[string]any{"x": "changed"}})
	if code == 200 {
		t.Fatalf("overwrote a concurrent change: %v", res)
	}
	var x string
	other.QueryRow(context.Background(), "SELECT x FROM t").Scan(&x)
	if x != "theirs" {
		t.Fatalf("x = %q", x)
	}
	// Expecting NULL works too.
	other.Exec(context.Background(), "UPDATE t SET x = NULL")
	if code, res = c.call("update", map[string]any{"schema": "public", "name": "t", "key": map[string]any{"id": "1"}, "values": map[string]any{"x": "set"}, "expect": map[string]any{"x": nil}}); code != 200 {
		t.Fatalf("expect null: %v", res)
	}
}

// Long values reach the grid cut, with the md5 of the full text; /api/cells
// returns the full values, and writes return cut rows the same way.
func TestIntegrationLongValuesAreCut(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (id int PRIMARY KEY, doc text, j jsonb, arr int[], short text, pad char(5), nothing text);
		INSERT INTO t VALUES (1, repeat('ab', 50000), (SELECT jsonb_agg(g) FROM generate_series(1, 20000) g),
		  (SELECT array_agg(g) FROM generate_series(1, 20000) g), 'hi', 'x', NULL);`)
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	ref := map[string]any{"schema": "public", "name": "t"}
	_, out := c.call("rows", ref)
	row := out["rows"].([]any)[0].([]any)
	if len(row) != 8 {
		t.Fatalf("want 7 values and a cut marker, got %d: %.200v", len(row), row)
	}
	cut, _ := row[7].(map[string]any)["cut"].(map[string]any)
	if len(cut) != 3 || cut["1"] == nil || cut["2"] == nil || cut["3"] == nil {
		t.Fatalf("cut marker: %v", row[7])
	}
	for i := 1; i <= 3; i++ {
		if n := len([]rune(row[i].(string))); n > maxCut {
			t.Errorf("column %d: %d characters reached the grid", i, n)
		}
	}
	if row[4] != "hi" || row[5] != "x    " || row[6] != nil {
		t.Fatalf("short values changed: %q %q %v", row[4], row[5], row[6])
	}

	u := testDSN(t)
	du := *u
	du.Path = "/" + db
	other, err := pgx.Connect(context.Background(), du.String())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	var doc, j, arr, hDoc string
	other.QueryRow(context.Background(), "SELECT doc, j::text, arr::text, md5(doc) FROM t").Scan(&doc, &j, &arr, &hDoc)
	if cut["1"] != hDoc {
		t.Fatalf("hash %v, want md5 of the full text %v", cut["1"], hDoc)
	}

	_, out = c.call("cells", map[string]any{"schema": "public", "name": "t", "keys": []any{map[string]any{"id": "1"}, map[string]any{"id": "2"}}, "cols": []string{"doc", "j", "arr", "nothing"}})
	rows := out["rows"].([]any)
	if rows[1] != nil {
		t.Fatalf("missing record: %v", rows[1])
	}
	full := rows[0].(map[string]any)
	vals, hashes := full["values"].([]any), full["hashes"].([]any)
	if vals[0] != doc || vals[1] != j || vals[2] != arr || vals[3] != nil {
		t.Fatalf("full values differ")
	}
	if hashes[0] != hDoc || hashes[1] != cut["2"] || hashes[3] != nil {
		t.Fatalf("hashes: %v", hashes)
	}

	// The full value round-trips as an expected value, and the row a write
	// returns is cut too.
	code, res := c.call("update", map[string]any{"schema": "public", "name": "t", "key": map[string]any{"id": "1"}, "values": map[string]any{"short": "yo"}, "expect": map[string]any{"doc": doc, "j": j}})
	if code != 200 {
		t.Fatalf("update with full expect: %v", res)
	}
	if r := res["row"].([]any); len(r) != 8 || r[4] != "yo" {
		t.Fatalf("returned row: %.200v", r)
	}
	if code, out := c.call("cells", map[string]any{"schema": "public", "name": "t", "keys": []any{map[string]any{"id": "1"}}, "cols": []string{"nope"}}); code == 200 {
		t.Fatalf("unknown column accepted: %v", out)
	}
}

// The SQL editor keeps a bounded result, and dropping rows past the cap
// does not roll back earlier statements of the same script.
func TestIntegrationQueryResultIsCapped(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (n int); INSERT INTO t VALUES (0);`)
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	code, out := c.call("query", map[string]any{"sql": "UPDATE t SET n = 1; SELECT repeat('x', 1000000) FROM generate_series(1, 100)"})
	if code != 200 || out["error"] != nil {
		t.Fatalf("query: %d %v", code, out)
	}
	res := out["results"].([]any)
	last := res[len(res)-1].(map[string]any)
	if last["truncated"] != true || last["total"] != float64(100) {
		t.Fatalf("truncated=%v total=%v", last["truncated"], last["total"])
	}
	if n := len(last["rows"].([]any)); n == 0 || n*1000000 > maxQueryBytes {
		t.Fatalf("kept %d rows of 1 MB", n)
	}
	_, out = c.call("query", map[string]any{"sql": "SELECT n FROM t"})
	if v := out["results"].([]any)[0].(map[string]any)["rows"].([]any)[0].([]any)[0]; v != "1" {
		t.Fatalf("earlier statement rolled back: n = %v", v)
	}
}

// Long queries cannot take every connection: browsing still works, and a
// query that finds no free slot fails fast with 503 instead of queueing.
func TestIntegrationLongQueriesLeaveRoomForBrowsing(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (id int PRIMARY KEY); INSERT INTO t VALUES (1);`)
	old := acquireWait
	acquireWait = 300 * time.Millisecond
	defer func() { acquireWait = old }()
	a := newApp(config{addr: "127.0.0.1:0", poolSize: 2})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	started := make(chan struct{})
	done := make(chan int)
	go func() {
		close(started)
		code, _ := (&client{t: t, h: c.h, sid: c.sid, db: db}).call("query", map[string]any{"sql": "SELECT pg_sleep(1.5)"})
		done <- code
	}()
	<-started
	time.Sleep(200 * time.Millisecond)
	if code, out := c.call("query", map[string]any{"sql": "SELECT 1"}); code != http.StatusServiceUnavailable {
		t.Fatalf("second query while one runs: %d %v, want 503", code, out)
	}
	if code, out := c.call("rows", map[string]any{"schema": "public", "name": "t"}); code != 200 {
		t.Fatalf("browsing while a query runs: %d %v", code, out)
	}
	if code := <-done; code != 200 {
		t.Fatalf("long query: %d", code)
	}
}

// Many concurrent requests on one session: every one succeeds and no
// connection or goroutine is left behind.
func TestIntegrationConcurrentLoad(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (id int PRIMARY KEY, v text, doc text);
		INSERT INTO t SELECT g, 'v' || g, repeat('d', 20000) FROM generate_series(1, 500) g;`)
	a := newApp(config{addr: "127.0.0.1:0", poolSize: 3})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	before := runtime.NumGoroutine()
	const workers, each = 32, 15
	errs := make(chan string, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			cl := &client{t: t, h: c.h, sid: c.sid, db: db}
			id := strconv.Itoa(w + 1)
			for i := 0; i < each; i++ {
				var code int
				var out map[string]any
				switch i % 5 {
				case 0:
					code, out = cl.call("rows", map[string]any{"schema": "public", "name": "t", "offset": w * 10, "limit": 200})
				case 1:
					code, out = cl.call("count", map[string]any{"schema": "public", "name": "t", "search": "v1"})
				case 2:
					code, out = cl.call("update", map[string]any{"schema": "public", "name": "t", "key": map[string]any{"id": id}, "values": map[string]any{"v": fmt.Sprint("w", i)}})
				case 3:
					code, out = cl.call("cells", map[string]any{"schema": "public", "name": "t", "keys": []any{map[string]any{"id": id}}, "cols": []string{"doc"}})
				case 4:
					code, out = cl.call("query", map[string]any{"sql": "SELECT pg_sleep(0.005), count(*) FROM t"})
				}
				if code != 200 {
					errs <- fmt.Sprintf("worker %d step %d: %d %v", w, i, code, out)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	s := a.sessionFor(func() *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: c.sid})
		return r
	}())
	st := s.current().pool.Stat()
	if st.AcquiredConns() != 0 || st.TotalConns() > 3 {
		t.Fatalf("pool after load: acquired %d, total %d", st.AcquiredConns(), st.TotalConns())
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+5 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+5 {
		t.Fatalf("goroutines: %d before, %d after", before, n)
	}
}
