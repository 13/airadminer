package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestLoginDSN(t *testing.T) {
	d, err := loginReq{Host: "db", Port: "6543", User: "u", Password: "p@ss/word", Database: "app", SSLMode: "require"}.dsn()
	if err != nil {
		t.Fatal(err)
	}
	if d != "postgres://u:p%40ss%2Fword@db:6543/app?sslmode=require" {
		t.Fatalf("dsn = %s", d)
	}
	if _, err := (loginReq{Port: "abc"}).dsn(); err == nil {
		t.Fatal("expected port error")
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
