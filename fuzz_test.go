package main

import (
	"encoding/csv"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// unquoteE reads back an E'…' literal as PostgreSQL does (for the escapes
// quoteLiteral writes: \\ and ”).
func unquoteE(t *testing.T, lit string) string {
	if !strings.HasPrefix(lit, "E'") || !strings.HasSuffix(lit, "'") || len(lit) < 3 {
		t.Fatalf("not an E'' literal: %q", lit)
	}
	body := lit[2 : len(lit)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch c := body[i]; c {
		case '\\':
			if i+1 >= len(body) || body[i+1] != '\\' {
				t.Fatalf("lone backslash in %q", lit)
			}
			b.WriteByte('\\')
			i++
		case '\'':
			if i+1 >= len(body) || body[i+1] != '\'' {
				t.Fatalf("unescaped quote ends the literal early: %q", lit)
			}
			b.WriteByte('\'')
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func FuzzQuoteLiteral(f *testing.F) {
	for _, s := range []string{"", "a'b", `a\b`, `\'`, "'; DROP TABLE x; --", `\\''`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got := unquoteE(t, quoteLiteral(s)); got != s {
			t.Fatalf("round trip: %q -> %q", s, got)
		}
	})
}

// A value quoted for a keyword/value connection string must come back as
// exactly that value and never add another keyword (such as passfile=).
func FuzzQuoteKV(f *testing.F) {
	for _, s := range []string{"bob", "a b", "x' passfile='/etc/shadow", `\`, `'`, "=", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if s == "" || strings.ContainsRune(s, 0) || !utf8.ValidString(s) {
			return
		}
		cfg, err := pgconn.ParseConfig("host=127.0.0.1 port=5432 dbname=d sslmode=disable passfile='' user=" + quoteKV(s))
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if cfg.User != s || cfg.Host != "127.0.0.1" || cfg.Database != "d" {
			t.Fatalf("user %q parsed as %q (host %q, db %q)", s, cfg.User, cfg.Host, cfg.Database)
		}
	})
}

func FuzzCSVField(f *testing.F) {
	for _, s := range []string{"", "a", "a,b", `say "hi"`, "line\nbreak", "=1+1", "-5", "-x", " lead", `\.`} {
		f.Add(s, false)
	}
	f.Fuzz(func(t *testing.T, s string, escape bool) {
		if !utf8.ValidString(s) || strings.ContainsRune(s, '\r') {
			return // encoding/csv rewrites \r\n; COPY output is not affected
		}
		line := csvField(&s, escape) + ",x\n"
		rec, err := csv.NewReader(strings.NewReader(line)).Read()
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		want := s
		if escape {
			want = escapeFormula(s)
		}
		if len(rec) != 2 || rec[0] != want || rec[1] != "x" {
			t.Fatalf("%q read back as %q", s, rec)
		}
		if escape && rec[0] != "" && strings.ContainsRune("=+-@\t", rune(rec[0][0])) && !plainNumber.MatchString(rec[0]) {
			t.Fatalf("formula survived: %q", rec[0])
		}
	})
}

func FuzzKeyWhereCtid(f *testing.F) {
	for _, s := range []string{"(0,1):123", "", ":", "(0,1)", "x:y:z", "'; --:1"} {
		f.Add(s)
	}
	m := &TableMeta{Schema: "s", Name: "t", KeyMode: "ctid"}
	f.Fuzz(func(t *testing.T, s string) {
		args := &sqlArgs{}
		w, err := keyWhere(m, rowKey{"__ctid": &s}, args)
		if err != nil {
			return
		}
		if w != "ctid = $1::tid AND xmin = $2::xid" || len(args.vals) != 2 {
			t.Fatalf("%q gave %q with %d args", s, w, len(args.vals))
		}
	})
}

// Filter values always travel as parameters, never inside the SQL text.
func FuzzBuildWhere(f *testing.F) {
	ops := []string{"eq", "neq", "lt", "gt", "lte", "gte", "contains", "ncontains", "starts", "null", "notnull", "true", "false"}
	for i := range ops {
		f.Add(uint8(i), "x' OR 1=1 --", "search'")
	}
	m := &TableMeta{Schema: "s", Name: "t", KeyMode: "pk", PK: []string{"id"}, Columns: []Column{{Name: "id", PK: true}, {Name: "v"}}}
	f.Fuzz(func(t *testing.T, op uint8, val, search string) {
		req := &viewReq{Filters: []filterRule{{Col: "v", Op: ops[int(op)%len(ops)], Val: &val}}, Search: search}
		args := &sqlArgs{}
		w, err := buildWhere(m, req, args)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(w, `'\;`) || strings.Contains(w, "--") {
			t.Fatalf("value leaked into SQL: %q", w)
		}
	})
}

func FuzzClientIP(f *testing.F) {
	for _, s := range []string{"1.2.3.4", "1.2.3.4, 10.0.0.2", "garbage, 10.0.0.2", "", ",,,", "::1, 10.0.0.9"} {
		f.Add(s)
	}
	a := newApp(config{addr: "127.0.0.1:0", trustProxy: "10.0.0.0/24"})
	f.Fuzz(func(t *testing.T, xff string) {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		r.Header.Set("X-Forwarded-For", xff)
		ip := a.clientIP(r)
		if ip == "10.0.0.1" {
			return
		}
		addr, err := netip.ParseAddr(ip)
		if err != nil || a.trust.has(addr.String()) {
			t.Fatalf("XFF %q gave client %q", xff, ip)
		}
	})
}

func FuzzLoginURL(f *testing.F) {
	for _, s := range []string{"postgres://u:p@h:5432/db?sslmode=require", "postgres://h/db?passfile=/x", "postgresql://[::1]/d", "%", "postgres://a,b/c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		l, err := loginReq{URL: s}.fromURL()
		if err != nil {
			return
		}
		if l.URL != "" {
			t.Fatalf("URL kept: %q", l.URL)
		}
	})
}
