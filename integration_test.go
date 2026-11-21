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
	"log/slog"
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
	t    *testing.T
	h    http.Handler
	sid  string
	db   string            // sent as X-Airadminer-Db when set
	peer string            // RemoteAddr host (default 127.0.0.1)
	hdr  map[string]string // extra request headers
	last *httptest.ResponseRecorder
}

func (c *client) call(path string, body any) (int, map[string]any) {
	c.t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "http://127.0.0.1/api/"+path, bytes.NewReader(b))
	req.RemoteAddr = "127.0.0.1:1234"
	if c.peer != "" {
		req.RemoteAddr = c.peer + ":1234"
	}
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Airadminer", "1")
	if c.db != "" {
		req.Header.Set(dbHeader, url.PathEscape(c.db))
	}
	if c.sid != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: c.sid})
	}
	if c.sid != "" {
		req.AddCookie(&http.Cookie{Name: hostCookieName, Value: c.sid})
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	c.last = rec
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == cookieName || ck.Name == hostCookieName {
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

// auditTo sends the app's audit events to a buffer and returns a reader.
func auditTo(a *App) func() []map[string]any {
	var buf safeBuffer
	a.auditLog = slog.New(slog.NewJSONHandler(&buf, nil))
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil {
				out = append(out, m)
			}
		}
		return out
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestIntegrationAuditLog(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (id int PRIMARY KEY, secret text); INSERT INTO t VALUES (1, 'old');`)
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	events := auditTo(a)
	u := testDSN(t)
	bad := &client{t: t, h: a.routes()}
	bad.call("login", loginBody(u, db, "wrong-password"))
	c := signIn(t, a, db)
	c.db = db
	c.hdr = map[string]string{"X-Request-Id": "spoofed"} // not from a trusted proxy: ignored
	c.call("update", map[string]any{"schema": "public", "name": "t", "key": map[string]any{"id": "1"}, "values": map[string]any{"secret": "s3cr3t-value"}})
	c.call("update", map[string]any{"schema": "public", "name": "t", "key": map[string]any{"id": "9"}, "values": map[string]any{"secret": "x"}})
	c.call("query", map[string]any{"sql": "SELECT 'hunter2' AS pw; SELECT nope"})
	c.call("rows", map[string]any{"schema": "public", "name": "t"}) // reads are not audited

	ev := events()
	var got []string
	for _, e := range ev {
		got = append(got, fmt.Sprint(e["event"], ":", e["ok"]))
	}
	want := "login:false login:true update:true update:false sql:false"
	if strings.Join(got, " ") != want {
		t.Fatalf("events: %v, want %s", got, want)
	}
	upd := ev[2]
	if upd["table"] != "public.t" || upd["database"] != db || upd["db_user"] != u.User.Username() || fmt.Sprint(upd["columns"]) != "[secret]" {
		t.Fatalf("update event: %v", upd)
	}
	if upd["request_id"] == "spoofed" || upd["request_id"] == "" {
		t.Fatalf("request id: %v", upd["request_id"])
	}
	all := fmt.Sprint(ev)
	for _, leak := range []string{"s3cr3t-value", "hunter2", "wrong-password", `"keys"`} {
		if strings.Contains(all, leak) {
			t.Fatalf("audit log contains %q: %s", leak, all)
		}
	}
	if ev[4]["sql_sha256"] == nil {
		t.Fatalf("sql event: %v", ev[4])
	}

	// With -audit-detail, SQL text and keys are recorded (values still not).
	a.cfg.auditDetail = true
	c.call("query", map[string]any{"sql": "SELECT 1"})
	c.call("delete", map[string]any{"schema": "public", "name": "t", "keys": []any{map[string]any{"id": "1"}}})
	ev = events()
	if ev[5]["sql"] != "SELECT 1" || fmt.Sprint(ev[6]["keys"]) != "[map[id:1]]" {
		t.Fatalf("detail events: %v %v", ev[5], ev[6])
	}
}

func TestIntegrationMetrics(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (id int PRIMARY KEY);`)
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	c.call("rows", map[string]any{"schema": "public", "name": "t"})
	rec := httptest.NewRecorder()
	a.opsRoutes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`airadminer_http_requests_total{route="POST /api/rows",code="200"} 1`,
		`airadminer_http_request_duration_seconds_count{route="POST /api/rows"} 1`,
		`airadminer_logins_total{result="ok"} 1`,
		"airadminer_sessions 1",
		`airadminer_db_connections{state="max"} 4`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if t.Failed() {
		t.Log(body)
	}
}

func TestIntegrationSessionLifetime(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (id int PRIMARY KEY);`)
	a := newApp(config{addr: "127.0.0.1:0", sessionLifetime: time.Hour})
	defer a.closeAll()
	c := signIn(t, a, db)
	if code, _ := c.call("tables", nil); code != 200 {
		t.Fatalf("fresh session: %d", code)
	}
	a.mu.Lock()
	s := a.sessions[c.sid]
	a.mu.Unlock()
	s.mu.Lock()
	s.created = time.Now().Add(-2 * time.Hour) // in use all along, but signed in too long ago
	s.mu.Unlock()
	if code, _ := c.call("tables", nil); code != http.StatusUnauthorized {
		t.Fatalf("expired session: %d, want 401", code)
	}
	if a.sessionCount() != 0 {
		t.Fatal("expired session not removed")
	}
}

// Behind a trusted HTTPS proxy, the cookie is Secure with the __Host-
// prefix, and sign-in attempts are limited per browser, not per proxy.
func TestIntegrationBehindProxy(t *testing.T) {
	db := testDB(t, "")
	a := newApp(config{addr: "127.0.0.1:0", trustProxy: "10.9.9.9"})
	defer a.closeAll()
	u := testDSN(t)
	pw, _ := u.User.Password()
	c := &client{t: t, h: a.routes(), peer: "10.9.9.9", hdr: map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.7"}}
	if code, out := c.call("login", loginBody(u, db, pw)); code != 200 {
		t.Fatalf("login: %v", out)
	}
	var ck *http.Cookie
	for _, x := range c.last.Result().Cookies() {
		if x.Name == hostCookieName {
			ck = x
		}
	}
	if ck == nil || !ck.Secure || ck.Path != "/" {
		t.Fatalf("cookie: %+v", c.last.Result().Cookies())
	}
	if c.last.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("no HSTS over HTTPS")
	}
	if code, _ := c.call("tables", nil); code != 200 {
		t.Fatalf("session with __Host- cookie: %d", code)
	}
	attacker := &client{t: t, h: c.h, peer: "10.9.9.9", hdr: map[string]string{"X-Forwarded-For": "198.51.100.1"}}
	limited := false
	for i := 0; i < 15; i++ {
		if code, _ := attacker.call("login", loginBody(u, db, "nope")); code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("attacker was never rate-limited")
	}
	other := &client{t: t, h: c.h, peer: "10.9.9.9", hdr: map[string]string{"X-Forwarded-For": "203.0.113.8"}}
	if code, _ := other.call("login", loginBody(u, db, pw)); code == http.StatusTooManyRequests {
		t.Fatal("another browser behind the same proxy was rate-limited")
	}
}

// With -dsn, one browser switching database does not move anyone else.
func TestIntegrationDSNDatabaseSwitchIsPerBrowser(t *testing.T) {
	dbA := testDB(t, `CREATE TABLE only_a (id int PRIMARY KEY);`)
	dbB := testDB(t, `CREATE TABLE only_b (id int PRIMARY KEY);`)
	u := testDSN(t)
	du := *u
	du.Path = "/" + dbA
	a := newApp(config{addr: "127.0.0.1:0", dsn: du.String()})
	if err := a.initFixed(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.closeAll()
	h := a.routes()
	one := &client{t: t, h: h, db: dbA}
	two := &client{t: t, h: h, db: dbA}
	if code, out := one.call("use", map[string]any{"database": dbB}); code != 200 {
		t.Fatalf("use: %v", out)
	}
	one.db = dbB
	if _, out := one.call("meta", nil); out["database"] != dbB {
		t.Fatalf("switching browser is on %v", out["database"])
	}
	if _, out := two.call("meta", nil); out["database"] != dbA {
		t.Fatalf("other browser moved to %v", out["database"])
	}
	if code, out := two.call("rows", map[string]any{"schema": "public", "name": "only_a"}); code != 200 {
		t.Fatalf("other browser: %d %v", code, out)
	}
	if code, out := one.call("use", map[string]any{"database": dbA}); code != 200 {
		t.Fatalf("switch back: %v", out)
	}
	if a.sessionCount() != 1 {
		t.Fatalf("switching back made another session: %d", a.sessionCount())
	}
}

func TestIntegrationNoSQL(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (id int PRIMARY KEY, v text); INSERT INTO t VALUES (1, 'a');`)
	a := newApp(config{addr: "127.0.0.1:0", noSQL: true})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	if code, _ := c.call("query", map[string]any{"sql": "SELECT 1"}); code != http.StatusForbidden {
		t.Fatalf("query: %d", code)
	}
	raw := map[string]any{"schema": "public", "name": "t", "filters": []any{map[string]any{"op": "sql", "val": "true"}}}
	for _, path := range []string{"rows", "count", "export"} {
		if code, _ := c.call(path, raw); code != http.StatusForbidden {
			t.Errorf("%s with a SQL filter: %d", path, code)
		}
	}
	if code, _ := c.call("rows", map[string]any{"schema": "public", "name": "t"}); code != 200 {
		t.Fatalf("plain rows: %d", code)
	}
	if _, out := c.call("meta", nil); out["sql"] != false {
		t.Fatalf("meta sql = %v", out["sql"])
	}
}

// The escaping exporter writes what COPY writes, except for formulas.
func TestIntegrationCSVEscaping(t *testing.T) {
	db := testDB(t, `CREATE TABLE t (id int PRIMARY KEY, v text, n numeric, j jsonb);
		INSERT INTO t VALUES (1, 'plain', 1.5, '{"a": [1, "x,y"]}'), (2, '', -3, NULL), (3, NULL, NULL, '"q\"uote"'),
		  (4, E'multi\nline, "quoted"', 0, '[]'), (5, ' spaced ', 2e10, '{}');`)
	export := func(escape bool, filter string) string {
		a := newApp(config{addr: "127.0.0.1:0", csvEscape: escape})
		defer a.closeAll()
		c := signIn(t, a, db)
		c.db = db
		body := map[string]any{"schema": "public", "name": "t"}
		if filter != "" {
			body["filters"] = []any{map[string]any{"op": "sql", "val": filter}}
		}
		c.call("export", body)
		if c.last.Code != 200 {
			t.Fatalf("export: %d %s", c.last.Code, c.last.Body.String())
		}
		return c.last.Body.String()
	}
	if copyOut, escOut := export(false, ""), export(true, ""); copyOut != escOut {
		t.Fatalf("outputs differ:\nCOPY:\n%s\nescaping:\n%s", copyOut, escOut)
	}
	other, _ := pgx.Connect(context.Background(), func() string { du := *testDSN(t); du.Path = "/" + db; return du.String() }())
	defer other.Close(context.Background())
	other.Exec(context.Background(), `INSERT INTO t VALUES (6, '=HYPERLINK("http://x")', -1, NULL), (7, '@SUM(1)', NULL, NULL), (8, '-x', NULL, NULL)`)
	out := export(true, "id >= 6")
	for _, want := range []string{`6,"'=HYPERLINK(""http://x"")",-1,`, "7,'@SUM(1),,", "8,'-x,,"} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// A failing query is still answered as a JSON error, not a broken file.
	a := newApp(config{addr: "127.0.0.1:0", csvEscape: true})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	code, res := c.call("export", map[string]any{"schema": "public", "name": "t", "filters": []any{map[string]any{"op": "sql", "val": "nope"}}})
	if code != http.StatusBadRequest || res["error"] == nil || c.last.Header().Get("Content-Disposition") != "" {
		t.Fatalf("bad filter: %d %v", code, res)
	}
}

// Keyset pages are exactly the OFFSET pages, forwards and backwards, for
// every kind of row key, with and without filters.
func TestIntegrationKeysetMatchesOffset(t *testing.T) {
	db := testDB(t, `CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy');
		CREATE TABLE ints (id int PRIMARY KEY, v text);
		INSERT INTO ints SELECT g, 'v' || (g % 7) FROM generate_series(1, 53) g;
		CREATE TABLE comp (a text, b int, v text, PRIMARY KEY (a, b));
		INSERT INTO comp SELECT chr(65 + g % 5) || CASE WHEN g % 3 = 0 THEN ' x' ELSE 'é' END, g, 'v' FROM generate_series(1, 41) g;
		CREATE TABLE uu (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), v int);
		INSERT INTO uu (v) SELECT g FROM generate_series(1, 30) g;
		CREATE TABLE en (m mood, n int, PRIMARY KEY (m, n));
		INSERT INTO en SELECT (ARRAY['sad','ok','happy'])[1 + g % 3]::mood, g FROM generate_series(1, 25) g;
		CREATE TABLE nopk (v int);
		INSERT INTO nopk SELECT g FROM generate_series(1, 44) g;
		DELETE FROM nopk WHERE v % 5 = 0;`)
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	page := func(body map[string]any) []any {
		t.Helper()
		code, out := c.call("rows", body)
		if code != 200 {
			t.Fatalf("rows %v: %d %v", body, code, out)
		}
		return out["rows"].([]any)
	}
	keyOf := func(m map[string]any, row []any) map[string]any {
		if m["keyMode"] == "ctid" {
			return map[string]any{"__ctid": row[0]}
		}
		k := map[string]any{}
		for _, p := range m["pk"].([]any) {
			for i, col := range m["columns"].([]any) {
				if col.(map[string]any)["name"] == p {
					k[p.(string)] = row[i]
				}
			}
		}
		return k
	}
	const n = 7
	for _, tc := range []struct {
		table   string
		filters []any
	}{
		{"ints", nil},
		{"ints", []any{map[string]any{"col": "v", "op": "eq", "val": "v3"}}},
		{"ints", []any{map[string]any{"op": "sql", "val": "id % 2 = 0"}}},
		{"comp", nil},
		{"uu", nil},
		{"en", nil},
		{"nopk", nil},
	} {
		_, m := c.call("table", map[string]any{"schema": "public", "name": tc.table})
		base := func() map[string]any {
			return map[string]any{"schema": "public", "name": tc.table, "limit": n, "filters": tc.filters}
		}
		var byOffset [][]any
		for off := 0; ; off += n {
			b := base()
			b["offset"] = off
			p := page(b)
			if len(p) == 0 {
				break
			}
			byOffset = append(byOffset, p)
		}
		// forwards
		for i := 1; i < len(byOffset); i++ {
			prev := byOffset[i-1]
			b := base()
			b["after"] = keyOf(m, prev[len(prev)-1].([]any))
			if got := page(b); fmt.Sprint(got) != fmt.Sprint(byOffset[i]) {
				t.Fatalf("%s %v: page %d after key differs:\n%v\n%v", tc.table, tc.filters, i, got, byOffset[i])
			}
		}
		// backwards
		for i := len(byOffset) - 2; i >= 0; i-- {
			b := base()
			b["before"] = keyOf(m, byOffset[i+1][0].([]any))
			if got := page(b); fmt.Sprint(got) != fmt.Sprint(byOffset[i]) {
				t.Fatalf("%s %v: page %d before key differs:\n%v\n%v", tc.table, tc.filters, i, got, byOffset[i])
			}
		}
	}
	// A user sort falls back to OFFSET.
	_, out := c.call("rows", map[string]any{"schema": "public", "name": "ints", "sort": []any{map[string]any{"col": "v"}}, "after": map[string]any{"id": "3"}, "offset": 0, "limit": 5})
	if out["keyset"] != false || out["rows"].([]any)[0].([]any)[1] != "v0" {
		t.Fatalf("sorted view: %v", out)
	}
}

func TestIntegrationKeysetIsFastDeepDown(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 2-million-row table")
	}
	db := testDB(t, `CREATE TABLE big (id int PRIMARY KEY, v text);
		INSERT INTO big SELECT g, md5(g::text) FROM generate_series(1, 2000000) g; ANALYZE big;`)
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	c := signIn(t, a, db)
	c.db = db
	timeIt := func(body map[string]any) (time.Duration, []any) {
		start := time.Now()
		_, out := c.call("rows", body)
		return time.Since(start), out["rows"].([]any)
	}
	off, rowsOff := timeIt(map[string]any{"schema": "public", "name": "big", "offset": 1900000, "limit": 200})
	ks, rowsKs := timeIt(map[string]any{"schema": "public", "name": "big", "after": map[string]any{"id": "1900000"}, "limit": 200})
	if fmt.Sprint(rowsOff) != fmt.Sprint(rowsKs) {
		t.Fatal("pages differ")
	}
	t.Logf("row 1,900,000: OFFSET %v, keyset %v", off, ks)
	if ks*5 > off {
		t.Fatalf("keyset (%v) not much faster than OFFSET (%v)", ks, off)
	}
}

// In a table with children (inheritance, or TimescaleDB chunks) the same
// ctid and xmin turn up in each child: rows inserted by one transaction
// into empty children all sit at (0,1). Keys must still name one row.
func TestIntegrationInheritedCtidKeys(t *testing.T) {
	db := testDB(t, `CREATE TABLE p (v text);
		CREATE TABLE c1 () INHERITS (p);
		CREATE TABLE c2 () INHERITS (p);
		INSERT INTO c1 VALUES ('a');
		INSERT INTO c2 VALUES ('b');`)
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	c := signIn(t, a, db)
	ref := map[string]any{"schema": "public", "name": "p"}
	inheritedKeysWork(t, c, ref, 2)
}

// inheritedKeysWork checks a ctid-keyed table with children of n rows:
// keyset paging sees each row once, and an update changes one row only.
func inheritedKeysWork(t *testing.T, c *client, ref map[string]any, n int) {
	t.Helper()
	if _, m := c.call("table", ref); m["keyMode"] != "ctid" {
		t.Fatalf("keyMode = %v", m["keyMode"])
	}
	seen := map[string]bool{}
	var first []any
	var after any
	for {
		req := map[string]any{"schema": ref["schema"], "name": ref["name"], "limit": 7}
		if after != nil {
			req["after"] = map[string]any{"__ctid": after}
		}
		code, out := c.call("rows", req)
		if code != 200 {
			t.Fatalf("rows: %d %v", code, out)
		}
		rows := out["rows"].([]any)
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			k := r.([]any)[0].(string)
			if seen[k] {
				t.Fatalf("row %s seen twice", k)
			}
			seen[k] = true
			if first == nil {
				first = r.([]any)
			}
			after = k
		}
	}
	if len(seen) != n {
		t.Fatalf("paged through %d rows, want %d", len(seen), n)
	}
	code, res := c.call("update", map[string]any{"schema": ref["schema"], "name": ref["name"], "key": map[string]any{"__ctid": first[0]}, "values": map[string]any{"v": "changed"}})
	if code != 200 {
		t.Fatalf("update: %d %v", code, res)
	}
	_, out := c.call("rows", map[string]any{"schema": ref["schema"], "name": ref["name"], "limit": 1000})
	changed := 0
	for _, r := range out["rows"].([]any) {
		for _, v := range r.([]any) {
			if v == "changed" {
				changed++
			}
		}
	}
	if changed != 1 {
		t.Fatalf("update changed %d rows, want 1", changed)
	}
}

// Runs only when the server has TimescaleDB, e.g. timescale/timescaledb:latest-pg17.
func TestIntegrationTimescaleHypertable(t *testing.T) {
	db := testDB(t, "")
	u := testDSN(t)
	du := *u
	du.Path = "/" + db
	conn, err := pgx.Connect(context.Background(), du.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
		t.Skipf("no TimescaleDB: %v", err)
	}
	if _, err := conn.Exec(context.Background(), `
		CREATE TABLE m (t timestamptz NOT NULL, v text);
		SELECT create_hypertable('m', 't', chunk_time_interval => interval '1 day');
		INSERT INTO m SELECT '2026-01-01'::timestamptz + i * interval '1 hour', i::text FROM generate_series(0, 71) i;
		CREATE TABLE z (t timestamptz NOT NULL, v text);
		SELECT create_hypertable('z', 't', chunk_time_interval => interval '1 day');
		INSERT INTO z SELECT '2026-01-01'::timestamptz + i * interval '1 hour', i::text FROM generate_series(0, 71) i;
		ALTER TABLE z SET (timescaledb.compress);
		SELECT compress_chunk(c) FROM show_chunks('z') c;
		ANALYZE;`); err != nil {
		t.Fatal(err)
	}
	a := newApp(config{addr: "127.0.0.1:0"})
	defer a.closeAll()
	c := signIn(t, a, db)

	_, out := c.call("tables", nil)
	for _, s := range out["schemas"].([]any) {
		if strings.Contains(s.(string), "timescaledb") {
			t.Errorf("schema %s listed", s)
		}
	}
	found := 0
	for _, tb := range out["tables"].([]any) {
		tb := tb.(map[string]any)
		if strings.Contains(tb["schema"].(string), "timescaledb") {
			t.Fatalf("internal table %s.%s listed", tb["schema"], tb["name"])
		}
		if tb["name"] == "m" || tb["name"] == "z" {
			found++
			if tb["hypertable"] != true || tb["size"].(float64) < 16384 || tb["estimate"].(float64) <= 0 {
				t.Errorf("hypertable %s: %v", tb["name"], tb)
			}
		}
	}
	if found != 2 {
		t.Fatalf("found %d hypertables", found)
	}

	inheritedKeysWork(t, c, map[string]any{"schema": "public", "name": "m"}, 72)

	// ctid and xmin cannot be read from compressed chunks: without a
	// primary key the hypertable is shown read-only, but it is shown.
	ref := map[string]any{"schema": "public", "name": "z"}
	if _, m := c.call("table", ref); m["keyMode"] != "" || m["hypertable"] != true {
		t.Fatalf("compressed hypertable meta: keyMode %v hypertable %v", m["keyMode"], m["hypertable"])
	}
	if code, out := c.call("rows", map[string]any{"schema": "public", "name": "z", "limit": 100}); code != 200 || len(out["rows"].([]any)) != 72 {
		t.Fatalf("rows of compressed hypertable: %d %v", code, out)
	}
}
