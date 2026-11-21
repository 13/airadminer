package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"plain":      "E'plain'",
		"it's":       "E'it''s'",
		`back\slash`: `E'back\\slash'`,
		`\'; drop`:   `E'\\''; drop'`,
	}
	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLikeEscape(t *testing.T) {
	if got := likeEscape(`50%_off\`); got != `50\%\_off\\` {
		t.Fatalf("likeEscape = %q", got)
	}
}

func testMeta() *TableMeta {
	return &TableMeta{
		Schema: "s", Name: "t", KeyMode: "pk", PK: []string{"id"},
		Columns: []Column{{Name: "id", PK: true}, {Name: "name"}, {Name: `we"ird`}},
	}
}

func TestBuildWhereParams(t *testing.T) {
	m := testMeta()
	v := "x"
	req := &viewReq{
		Filters: []filterRule{{Col: "name", Op: "contains", Val: &v}, {Col: `we"ird`, Op: "null"}},
		Search:  "abc",
	}
	args := &sqlArgs{}
	where, err := buildWhere(m, req, args)
	if err != nil {
		t.Fatal(err)
	}
	want := ` WHERE "name"::text ILIKE $1 AND "we""ird" IS NULL AND "__airadminer_row"::text ILIKE $2`
	if where != want {
		t.Fatalf("where = %s\nwant    %s", where, want)
	}
	if len(args.vals) != 2 || string(args.vals[0]) != "%x%" || string(args.vals[1]) != "%abc%" {
		t.Fatalf("args = %q", args.vals)
	}
}

func TestBuildWhereRejectsUnknown(t *testing.T) {
	if _, err := buildWhere(testMeta(), &viewReq{Filters: []filterRule{{Col: "nope", Op: "eq"}}}, &sqlArgs{}); err == nil {
		t.Fatal("expected error for unknown column")
	}
	if _, err := buildWhere(testMeta(), &viewReq{Filters: []filterRule{{Col: "id", Op: "; drop"}}}, &sqlArgs{}); err == nil {
		t.Fatal("expected error for unknown operator")
	}
}

func TestBuildOrderTiebreak(t *testing.T) {
	got, err := buildOrder(testMeta(), &viewReq{Sort: []sortRule{{Col: "name", Desc: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if got != ` ORDER BY "name" DESC, "id"` {
		t.Fatalf("order = %s", got)
	}
}

func TestKeyWhere(t *testing.T) {
	id := "7"
	args := &sqlArgs{}
	w, err := keyWhere(testMeta(), rowKey{"id": &id}, args)
	if err != nil || w != `"id" = $1` {
		t.Fatalf("keyWhere = %q, %v", w, err)
	}
	if _, err := keyWhere(testMeta(), rowKey{}, &sqlArgs{}); err == nil {
		t.Fatal("expected error for missing key")
	}
	ro := testMeta()
	ro.KeyMode = ""
	if _, err := keyWhere(ro, rowKey{"id": &id}, &sqlArgs{}); err == nil {
		t.Fatal("expected read-only error")
	}
}

func TestLoginConfigIgnoresServerEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PGPASSWORD", "from-env")
	t.Setenv("PGHOST", "envhost")
	os.WriteFile(filepath.Join(home, ".pgpass"), []byte("*:*:*:*:from-pgpass\n"), 0o600)
	a := newApp(config{})
	cfg, err := a.loginConfig(loginReq{Host: "db", Port: "6543", User: "u", Database: "app", SSLMode: "require"})
	if err != nil {
		t.Fatal(err)
	}
	cc := cfg.ConnConfig
	if cc.Password != "" || cc.Host != "db" || cc.Port != 6543 || cc.User != "u" || cc.Database != "app" {
		t.Fatalf("got host=%s port=%d user=%s db=%s password=%q", cc.Host, cc.Port, cc.User, cc.Database, cc.Password)
	}
	cfg, err = a.loginConfig(loginReq{Host: "db", User: "u", Password: "p@ss/w'o\\rd"})
	if err != nil || cfg.ConnConfig.Password != "p@ss/w'o\\rd" {
		t.Fatalf("password not kept: %v", err)
	}
	t.Setenv("PGSERVICE", "prod")
	if _, err := a.loginConfig(loginReq{Host: "db", User: "u"}); err == nil {
		t.Fatal("PGSERVICE must make the login screen fail closed")
	}
}

func TestLoginConfigRejects(t *testing.T) {
	a := newApp(config{})
	bad := []loginReq{
		{Host: "db", Port: "abc", User: "u"},
		{Host: "db", Port: "70000", User: "u"},
		{Host: "db"}, // no user
		{Host: "db", User: "u", SSLMode: "bogus"},
		{Host: "/var/run/postgresql", User: "postgres"}, // socket without -allow-socket
		{Host: "a,b", User: "u"},
		{URL: "postgres://u@db/app?passfile=/etc/shadow"},
		{URL: "postgres://u@db/app?sslkey=/home/x/.ssh/id_rsa"},
		{URL: "postgres://u@db/app?service=prod"},
		{URL: "host=db user=u"},
		{URL: "postgres://u@a,b/app"},
	}
	for _, r := range bad {
		if _, err := a.loginConfig(r); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	cfg, err := a.loginConfig(loginReq{URL: "postgresql://u:p%40ss@db:6000/app?sslmode=require"})
	if err != nil || cfg.ConnConfig.Password != "p@ss" || cfg.ConnConfig.Port != 6000 || cfg.ConnConfig.Database != "app" {
		t.Fatalf("url login: %v", err)
	}
	sock := newApp(config{allowSocket: true})
	if _, err := sock.loginConfig(loginReq{Host: "/tmp", User: "u"}); err != nil {
		t.Fatalf("socket with -allow-socket: %v", err)
	}
	tls := newApp(config{requireTLS: true})
	cfg, _ = tls.loginConfig(loginReq{Host: "db", User: "u", SSLMode: "disable"})
	if cfg.ConnConfig.TLSConfig == nil || len(cfg.ConnConfig.Fallbacks) != 0 {
		t.Fatal("-require-tls must force TLS without plaintext fallback")
	}
}

func TestHostPolicy(t *testing.T) {
	p, _ := parseHostPolicy("db.internal, pg:6432, 10.0.0.0/8")
	if p.checkName("db.internal", "5432") != nil || p.checkName("pg", "6432") != nil {
		t.Fatal("listed names must pass")
	}
	if p.checkName("pg", "5432") == nil {
		// nets are configured, so unlisted names are decided by address
		ips, err := p.lookup("pg", "5432")(t.Context(), "127.0.0.1")
		if err == nil {
			t.Fatalf("127.0.0.1 is not in 10.0.0.0/8: %v", ips)
		}
	}
	if ips, err := p.lookup("x", "5432")(t.Context(), "10.1.2.3"); err != nil || len(ips) != 1 {
		t.Fatalf("10.1.2.3 must pass: %v", err)
	}
	open, _ := parseHostPolicy("")
	if _, err := open.lookup("m", "80")(t.Context(), "169.254.169.254"); err == nil {
		t.Fatal("link-local must be refused by default")
	}
	if _, err := open.lookup("l", "5432")(t.Context(), "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	only, _ := parseHostPolicy("db.internal")
	if only.checkName("evil", "5432") == nil {
		t.Fatal("unlisted name must be refused")
	}
}

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter(3, time.Second)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !l.allow("ip", now) {
			t.Fatal("burst refused")
		}
	}
	if l.allow("ip", now) {
		t.Fatal("4th attempt allowed")
	}
	if !l.allow("other", now) {
		t.Fatal("limit must be per client")
	}
	if !l.allow("ip", now.Add(time.Second)) {
		t.Fatal("refill missing")
	}
}

func TestKeyWhereCtid(t *testing.T) {
	m := &TableMeta{Schema: "s", Name: "t", KeyMode: "ctid"}
	k := "(0,1):1234"
	args := &sqlArgs{}
	w, err := keyWhere(m, rowKey{"__ctid": &k}, args)
	if err != nil || w != "ctid = $1::tid AND xmin = $2::xid" || string(args.vals[0]) != "(0,1)" || string(args.vals[1]) != "1234" {
		t.Fatalf("keyWhere = %q %q %v", w, args.vals, err)
	}
	old := "(0,1)"
	if _, err := keyWhere(m, rowKey{"__ctid": &old}, &sqlArgs{}); err == nil {
		t.Fatal("a bare ctid must be refused")
	}
}

func TestGuardAndHost(t *testing.T) {
	a := newApp(config{addr: "127.0.0.1:0"})
	h := a.routes()

	req := httptest.NewRequest("POST", "http://localhost/api/meta", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing header: got %d", rec.Code)
	}

	req = httptest.NewRequest("POST", "http://localhost/api/meta", strings.NewReader("{}"))
	req.Header.Set("X-Airadminer", "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "http://evil.example/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding host: got %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "http://127.0.0.1:8080/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app.js") {
		t.Fatalf("index: got %d", rec.Code)
	}
}

func TestBasicAuth(t *testing.T) {
	a := newApp(config{addr: "0.0.0.0:0", auth: "admin:secret"})
	h := a.routes()
	req := httptest.NewRequest("GET", "http://host/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: got %d", rec.Code)
	}
	req.SetBasicAuth("admin", "secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with auth: got %d", rec.Code)
	}
}

func TestDuplicateSQLSkipsSelfFilledColumns(t *testing.T) {
	def := "nextval('t_id_seq'::regclass)"
	m := &TableMeta{
		Schema: "s", Name: "t", KeyMode: "pk", PK: []string{"id"},
		Columns: []Column{
			{Name: "id", PK: true, Default: &def},
			{Name: "name"},
			{Name: "seq", Identity: "a"},
			{Name: "total", Generated: "s"},
			{Name: "blob", Udt: "bytea"},
		},
	}
	got, err := duplicateSQL(m, `"id" = $1`, nil, &sqlArgs{})
	if err != nil {
		t.Fatal(err)
	}
	want := `INSERT INTO "s"."t" ("name", "blob") SELECT "name", "blob" FROM "s"."t" WHERE "id" = $1 RETURNING "id", "name", "seq", "total", octet_length("blob")::text AS "blob"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestDuplicateSQLKeepsPKWithoutDefault(t *testing.T) {
	m := &TableMeta{Schema: "s", Name: "t", KeyMode: "pk", PK: []string{"code"}, Columns: []Column{{Name: "code", PK: true}}}
	if got, _ := duplicateSQL(m, `"code" = $1`, nil, &sqlArgs{}); !strings.Contains(got, `("code") SELECT "code"`) {
		t.Fatalf("pk without default must be copied: %s", got)
	}
}

func TestDuplicateSQLOverrides(t *testing.T) {
	m := &TableMeta{Schema: "s", Name: "t", KeyMode: "pk", PK: []string{"code"},
		Columns: []Column{{Name: "code", PK: true}, {Name: "title"}, {Name: "gen", Generated: "s"}}}
	code := "B-2"
	args := &sqlArgs{}
	args.add(&code) // the key's own parameter comes first
	got, err := duplicateSQL(m, `"code" = $1`, map[string]*string{"code": &code}, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `("code", "title") SELECT $2, "title" FROM`) {
		t.Fatalf("override not applied: %s", got)
	}
	if _, err := duplicateSQL(m, "true", map[string]*string{"gen": &code}, &sqlArgs{}); err == nil {
		t.Fatal("expected error overriding a generated column")
	}
	if _, err := duplicateSQL(m, "true", map[string]*string{"nope": &code}, &sqlArgs{}); err == nil {
		t.Fatal("expected error for unknown column")
	}
}

func TestUpdateAndInsertSQL(t *testing.T) {
	m := testMeta()
	id, name := "7", "Ann"
	st, err := updateSQL(m, rowKey{"id": &id}, map[string]*string{"name": &name, `we"ird`: nil})
	if err != nil {
		t.Fatal(err)
	}
	if want := `UPDATE "s"."t" SET "name" = $1, "we""ird" = $2 WHERE "id" = $3 RETURNING "id", "name", "we""ird"`; st.sql != want {
		t.Fatalf("update = %s", st.sql)
	}
	if len(st.args) != 3 || st.args[1] != nil || string(st.args[2]) != "7" {
		t.Fatalf("update args = %q", st.args)
	}
	if _, err := updateSQL(m, rowKey{"id": &id}, map[string]*string{"nope": &name}); err == nil {
		t.Fatal("expected error for unknown column")
	}
	if _, err := updateSQL(m, rowKey{}, map[string]*string{"name": &name}); err == nil {
		t.Fatal("expected error for missing key")
	}
	st, err = insertSQL(m, nil)
	if err != nil || !strings.HasPrefix(st.sql, `INSERT INTO "s"."t" DEFAULT VALUES RETURNING`) {
		t.Fatalf("insert = %s, %v", st.sql, err)
	}
	st, _ = insertSQL(m, map[string]*string{"name": &name})
	if !strings.HasPrefix(st.sql, `INSERT INTO "s"."t" ("name") VALUES ($1) RETURNING`) {
		t.Fatalf("insert = %s", st.sql)
	}
}
