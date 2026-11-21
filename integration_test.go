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
	"strings"
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
