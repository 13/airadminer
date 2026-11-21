package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxBody       = 16 << 20
	maxPageRows   = 1000
	maxQueryRows  = 10000
	maxQueryBytes = 32 << 20 // SQL editor result kept per request
	countTimeout  = 3 * time.Second
	exactCountMax = 2_000_000 // above this estimate, unfiltered counts use the planner estimate
)

type apiErr struct {
	Error    string `json:"error"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
	Code     string `json:"code,omitempty"`
	Position int32  `json:"position,omitempty"`
	Index    *int   `json:"index,omitempty"` // which change of a batch failed
}

// dbHeader carries the database the page was built for (URL-escaped).
const dbHeader = "X-Airadminer-Db"

type handler func(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error)

// auditedRoutes: every request to these is written to the audit log,
// whether it succeeds or not. Reads other than exports are not audited.
var auditedRoutes = map[string]string{
	"/api/update": "update", "/api/insert": "insert", "/api/delete": "delete",
	"/api/duplicate": "duplicate", "/api/batch": "batch", "/api/query": "sql",
	"/api/export": "export", "/api/use": "use_database", "/api/logout": "logout",
}

var errNoSQL = errors.New("SQL is turned off on this server (-no-sql)")

func (a *App) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", a.guard(a.handleLogin))
	mux.HandleFunc("POST /api/logout", a.guard(a.authed(a.handleLogout)))
	mux.HandleFunc("POST /api/meta", a.guard(a.authed(a.handleMeta)))
	mux.HandleFunc("POST /api/use", a.guard(a.authed(a.handleUse)))
	mux.HandleFunc("POST /api/tables", a.guard(a.authed(a.handleTables)))
	mux.HandleFunc("POST /api/table", a.guard(a.authed(a.handleTable)))
	mux.HandleFunc("POST /api/rows", a.guard(a.authed(a.handleRows)))
	mux.HandleFunc("POST /api/count", a.guard(a.authed(a.handleCount)))
	mux.HandleFunc("POST /api/cells", a.guard(a.authed(a.handleCells)))
	mux.HandleFunc("POST /api/update", a.guard(a.authed(a.handleUpdate)))
	mux.HandleFunc("POST /api/insert", a.guard(a.authed(a.handleInsert)))
	mux.HandleFunc("POST /api/delete", a.guard(a.authed(a.handleDelete)))
	mux.HandleFunc("POST /api/duplicate", a.guard(a.authed(a.handleDuplicate)))
	mux.HandleFunc("POST /api/batch", a.guard(a.authed(a.handleBatch)))
	mux.HandleFunc("POST /api/query", a.guard(a.authed(a.handleQuery)))
	mux.HandleFunc("POST /api/export", a.guard(a.authed(a.handleExport)))
}

// guard rejects cross-site requests: browsers cannot add a custom header
// cross-origin without a CORS preflight, which this server never approves.
func (a *App) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Airadminer") != "1" {
			writeJSON(w, http.StatusForbidden, apiErr{Error: "missing X-Airadminer header"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next(w, r)
	}
}

func (a *App) authed(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := a.sessionFor(r)
		var db *DB
		if s != nil {
			db = s.current()
		}
		if db == nil {
			writeJSON(w, http.StatusUnauthorized, apiErr{Error: "not signed in", Code: "auth"})
			return
		}
		// The browser names the database its page was built for. If this
		// session moved to another database since (another tab switched),
		// refuse instead of applying an edit to a same-named table there.
		if want, ok := r.Header[dbHeader]; ok {
			name, err := url.PathUnescape(strings.Join(want, ""))
			if err != nil || name != db.name {
				writeJSON(w, http.StatusConflict, apiErr{Error: "this session switched to database " + db.name + "; the page was reloaded", Code: "dbchanged"})
				return
			}
		}
		out, err := h(s, db, w, r)
		ri := infoOf(r)
		if ev, ok := auditedRoutes[r.URL.Path]; ok {
			aerr := err
			if aerr == nil {
				aerr = ri.auditErr
			}
			a.audit(r, ev, aerr, "database", db.name, "db_user", db.user)
		}
		if err != nil {
			ri.err = err.Error()
			writeErr(w, err)
			return
		}
		if out != nil {
			writeJSON(w, http.StatusOK, out)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "error", err)
	}
}

func writeErr(w http.ResponseWriter, err error) {
	var index *int
	var be *batchErr
	if errors.As(err, &be) {
		index = &be.index
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && !connLost(err) {
		writeJSON(w, http.StatusBadRequest, apiErr{Error: pgErr.Message, Detail: pgErr.Detail, Hint: pgErr.Hint, Code: pgErr.Code, Position: pgErr.Position, Index: index})
		return
	}
	if errors.Is(err, errNoSQL) {
		writeJSON(w, http.StatusForbidden, apiErr{Error: err.Error(), Code: "nosql"})
		return
	}
	if errors.Is(err, context.Canceled) {
		writeJSON(w, 499, apiErr{Error: "query canceled"})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeJSON(w, http.StatusGatewayTimeout, apiErr{Error: "query timed out"})
		return
	}
	if isUnavailable(err) {
		writeJSON(w, http.StatusServiceUnavailable, apiErr{Error: err.Error(), Code: "unavailable"})
		return
	}
	writeJSON(w, http.StatusBadRequest, apiErr{Error: err.Error()})
}

// isUnavailable reports errors worth retrying: the database could not be
// reached or the connection broke, as opposed to a rejected statement.
func isUnavailable(err error) bool {
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) || pgconn.SafeToRetry(err) || errors.Is(err, errBusy) || errors.Is(err, errLongBusy) || connLost(err) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// connLost reports a connection the server dropped or that broke, such as
// during a database restart or failover, as opposed to a failed statement.
func connLost(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "57P01", "57P02", "57P03": // admin_shutdown, crash_shutdown, cannot_connect_now
			return true
		}
		return strings.HasPrefix(pgErr.Code, "08") // connection exceptions
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "conn closed")
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return fmt.Errorf("bad request: %w", err)
	}
	return nil
}

// ---------- session ----------

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.fixed != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if !a.logins.allow(a.clientIP(r), time.Now()) {
		a.metrics.count("logins", "limited")
		w.Header().Set("Retry-After", "10")
		writeJSON(w, http.StatusTooManyRequests, apiErr{Error: "too many sign-in attempts; wait a few seconds and try again"})
		return
	}
	var req loginReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	cfg, err := a.loginConfig(req)
	fail := func(err error) {
		a.metrics.count("logins", "failed")
		a.audit(r, "login", err, "host", req.Host, "port", req.Port, "db_user", req.User, "database", req.Database)
		writeErr(w, err)
	}
	if err != nil {
		fail(err)
		return
	}
	old := a.sessionFor(r)
	if old == nil && a.sessionCount() >= a.cfg.maxSessions {
		a.metrics.count("logins", "full")
		writeJSON(w, http.StatusServiceUnavailable, apiErr{Error: errTooManySessions.Error()})
		return
	}
	s, err := a.openSession(r.Context(), cfg)
	if err != nil {
		fail(err)
		return
	}
	if old != nil && !old.fixed {
		a.dropSession(w, old)
	}
	if err := a.addSession(w, r, s); err != nil {
		go s.close()
		a.metrics.count("logins", "full")
		writeJSON(w, http.StatusServiceUnavailable, apiErr{Error: err.Error()})
		return
	}
	a.metrics.count("logins", "ok")
	a.audit(r, "login", nil, "host", cfg.ConnConfig.Host, "port", cfg.ConnConfig.Port, "db_user", cfg.ConnConfig.User, "database", s.db.name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleLogout(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	if s.fixed {
		return nil, errors.New("this server is started with a fixed -dsn; there is nothing to sign out of")
	}
	a.dropSession(w, s)
	return map[string]any{"ok": true}, nil
}

func (a *App) handleMeta(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	pool := db.pool
	out := map[string]any{"fixed": s.fixed, "readonly": a.cfg.readonly, "version": version, "sql": !a.cfg.noSQL, "csvEscape": a.cfg.csvEscape}
	var user, dbName, server, host string
	var ro string
	err := pool.QueryRow(ctx, `SELECT current_user, current_database(), current_setting('server_version'),
		COALESCE(inet_server_addr()::text, ''), current_setting('default_transaction_read_only')`).Scan(&user, &dbName, &server, &host, &ro)
	if err != nil {
		return nil, err
	}
	cc := pool.Config().ConnConfig
	if cc.Host != "" {
		host = cc.Host
	}
	out["user"], out["database"], out["server"], out["host"], out["port"] = user, dbName, server, host, cc.Port
	out["readonly"] = a.cfg.readonly || ro == "on"
	rows, err := pool.Query(ctx, `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate AND has_database_privilege(datname, 'CONNECT') ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	dbs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out["databases"] = dbs
	return out, nil
}

func (a *App) handleUse(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		Database string `json:"database"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.Database == "" {
		return nil, errors.New("database is required")
	}
	note(r, "to", req.Database)
	if s == a.fixed {
		// With -dsn everyone shares one session. Switching database gives
		// this browser a session of its own, so nobody else is moved.
		if a.sessionCount() >= a.cfg.maxSessions {
			return nil, errTooManySessions
		}
		own := &Session{baseCfg: s.baseCfg, fixed: true, created: time.Now(), lastUsed: time.Now()}
		if err := own.connect(r.Context(), req.Database, a.cfg); err != nil {
			return nil, err
		}
		if err := a.addSession(w, r, own); err != nil {
			go own.close()
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	if err := s.connect(r.Context(), req.Database, a.cfg); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (a *App) handleTables(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	db.forgetMeta()
	tables, schemas, err := listTables(r.Context(), db.pool)
	if err != nil {
		return nil, err
	}
	if tables == nil {
		tables = []TableInfo{}
	}
	return map[string]any{"tables": tables, "schemas": schemas}, nil
}

type tableRef struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

func (a *App) handleTable(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req tableRef
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return db.tableMeta(r.Context(), req.Schema, req.Name, true)
}

// ---------- browsing ----------

type sortRule struct {
	Col  string `json:"col"`
	Desc bool   `json:"desc"`
}

type filterRule struct {
	Col string  `json:"col"`
	Op  string  `json:"op"`
	Val *string `json:"val"`
}

type viewReq struct {
	tableRef
	Sort    []sortRule   `json:"sort"`
	Filters []filterRule `json:"filters"`
	Search  string       `json:"search"`
	Offset  int64        `json:"offset"`
	Limit   int64        `json:"limit"`
	// Keyset paging: the rows right after (or before) the record with this
	// key, in the default order. Used instead of Offset when given.
	After  rowKey `json:"after"`
	Before rowKey `json:"before"`
}

// keyset returns the condition and order for paging from a key, or ok=false
// when the view cannot page that way: with a sort of the user's own, NULLs
// and mixed directions would need a far more complex condition, so those
// views use OFFSET.
func keyset(m *TableMeta, req *viewReq, args *sqlArgs) (cond, order string, reverse, ok bool, err error) {
	key, op, dir := req.After, " > ", ""
	if key == nil {
		key, op, dir, reverse = req.Before, " < ", " DESC", true
	}
	if key == nil || len(req.Sort) > 0 {
		return "", "", false, false, nil
	}
	switch m.KeyMode {
	case "pk":
		var cols, vals, ord []string
		for _, p := range m.PK {
			v, has := key[p]
			if !has || v == nil {
				return "", "", false, false, fmt.Errorf("missing primary key value for %q", p)
			}
			cols = append(cols, ident(p))
			vals = append(vals, args.add(v))
			ord = append(ord, ident(p)+dir)
		}
		return "(" + strings.Join(cols, ", ") + ")" + op + "(" + strings.Join(vals, ", ") + ")", " ORDER BY " + strings.Join(ord, ", "), reverse, true, nil
	case "ctid":
		v := key["__ctid"]
		if v == nil {
			return "", "", false, false, errors.New("missing row id")
		}
		tid, _, _ := strings.Cut(*v, ":")
		return "ctid" + op + args.add(&tid) + "::tid", " ORDER BY ctid" + dir, reverse, true, nil
	}
	return "", "", false, false, nil
}

const rowAlias = `"__airadminer_row"`

// sqlArgs collects bind parameters, or inlines escaped literals when
// the statement cannot take parameters (COPY).
type sqlArgs struct {
	inline bool
	vals   [][]byte
}

func (a *sqlArgs) add(v *string) string {
	if a.inline {
		if v == nil {
			return "NULL"
		}
		return quoteLiteral(*v)
	}
	if v == nil {
		a.vals = append(a.vals, nil)
	} else {
		a.vals = append(a.vals, []byte(*v))
	}
	return "$" + strconv.Itoa(len(a.vals))
}

func quoteLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return "E'" + s + "'"
}

func ident(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func strp(s string) *string { return &s }

// hasRawSQL reports whether the view carries user-written SQL, which may
// change session state (set_config, advisory locks) on its connection.
// checkView refuses raw SQL filters when SQL is turned off.
func (a *App) checkView(req *viewReq) error {
	if a.cfg.noSQL && req.hasRawSQL() {
		return errNoSQL
	}
	return nil
}

func (req *viewReq) hasRawSQL() bool {
	for _, f := range req.Filters {
		if f.Op == "sql" {
			return true
		}
	}
	return false
}

func buildWhere(m *TableMeta, req *viewReq, args *sqlArgs) (string, error) {
	var conds []string
	for _, f := range req.Filters {
		if f.Op == "sql" {
			if f.Val != nil && strings.TrimSpace(*f.Val) != "" {
				conds = append(conds, "("+*f.Val+")")
			}
			continue
		}
		c := m.column(f.Col)
		if c == nil {
			return "", fmt.Errorf("unknown column %q", f.Col)
		}
		col := ident(c.Name)
		val := ""
		if f.Val != nil {
			val = *f.Val
		}
		switch f.Op {
		case "eq":
			conds = append(conds, col+" = "+args.add(&val))
		case "neq":
			conds = append(conds, col+" IS DISTINCT FROM "+args.add(&val))
		case "lt", "gt", "lte", "gte":
			op := map[string]string{"lt": "<", "gt": ">", "lte": "<=", "gte": ">="}[f.Op]
			conds = append(conds, col+" "+op+" "+args.add(&val))
		case "contains":
			conds = append(conds, col+"::text ILIKE "+args.add(strp("%"+likeEscape(val)+"%")))
		case "ncontains":
			conds = append(conds, "("+col+" IS NULL OR "+col+"::text NOT ILIKE "+args.add(strp("%"+likeEscape(val)+"%"))+")")
		case "starts":
			conds = append(conds, col+"::text ILIKE "+args.add(strp(likeEscape(val)+"%")))
		case "null":
			conds = append(conds, col+" IS NULL")
		case "notnull":
			conds = append(conds, col+" IS NOT NULL")
		case "true":
			conds = append(conds, col+" IS TRUE")
		case "false":
			conds = append(conds, col+" IS FALSE")
		default:
			return "", fmt.Errorf("unknown filter operator %q", f.Op)
		}
	}
	if s := strings.TrimSpace(req.Search); s != "" {
		conds = append(conds, rowAlias+"::text ILIKE "+args.add(strp("%"+likeEscape(s)+"%")))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), nil
}

// outerOrder rewrites an ORDER BY for the page subquery, where the system
// column ctid is available as "__aa_ctid". Quoted identifiers are skipped.
func outerOrder(order string) string {
	var b strings.Builder
	quoted := false
	for i := 0; i < len(order); i++ {
		c := order[i]
		if c == '"' {
			quoted = !quoted
		}
		if !quoted && strings.HasPrefix(order[i:], "ctid") && (i == 0 || order[i-1] == ' ') && (i+4 == len(order) || order[i+4] == ' ' || order[i+4] == ',') {
			b.WriteString(`"__aa_ctid"`)
			i += 3
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func buildOrder(m *TableMeta, req *viewReq) (string, error) {
	var parts []string
	seen := map[string]bool{}
	for _, s := range req.Sort {
		c := m.column(s.Col)
		if c == nil {
			return "", fmt.Errorf("unknown column %q", s.Col)
		}
		dir := " ASC"
		if s.Desc {
			dir = " DESC"
		}
		parts = append(parts, ident(c.Name)+dir)
		seen[c.Name] = true
	}
	// Stable tiebreak so offset paging never shows a row twice.
	switch m.KeyMode {
	case "pk":
		for _, p := range m.PK {
			if !seen[p] {
				parts = append(parts, ident(p))
			}
		}
	case "ctid":
		parts = append(parts, "ctid")
	}
	if len(parts) == 0 {
		return "", nil
	}
	return " ORDER BY " + strings.Join(parts, ", "), nil
}

// selectList returns the grid's column expressions. Binary columns are
// summarised by their size instead of shipping the bytes to the browser,
// and long values are cut (see cells.go): the md5 of each cut column's
// full text follows the visible columns, for finishRow to fold in.
func selectList(m *TableMeta, forExport bool) string {
	return selectListWith(m, forExport, "ctid", "xmin")
}

// selectListWith reads the row id from the given ctid and xmin columns:
// the system columns, or their copies when selecting from a subquery.
func selectListWith(m *TableMeta, forExport bool, ctid, xmin string) string {
	var parts, hashes []string
	if m.KeyMode == "ctid" && !forExport {
		// ctid alone is a slot that VACUUM can hand to another row; with
		// xmin the key names exactly this version of this row.
		parts = append(parts, ctid+`::text || ':' || `+xmin+`::text AS "__ctid"`)
	}
	cut := 0
	if !forExport {
		cut = cutLen(m)
	}
	for i := range m.Columns {
		c := &m.Columns[i]
		col := ident(c.Name)
		switch {
		case forExport:
			parts = append(parts, col)
		case c.Udt == "bytea":
			parts = append(parts, "octet_length("+col+")::text AS "+col)
		case cut > 0 && cuttable(c):
			n := strconv.Itoa(cut)
			parts = append(parts, "left("+textOf(col)+", "+n+") AS "+col)
			hashes = append(hashes, "CASE WHEN char_length("+textOf(col)+") > "+n+" THEN md5(format('%s', "+col+")) END")
		default:
			parts = append(parts, col)
		}
	}
	parts = append(parts, hashes...)
	if len(parts) == 0 {
		return "NULL"
	}
	return strings.Join(parts, ", ")
}

func (a *App) handleRows(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req viewReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := a.checkView(&req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := db.tableMeta(ctx, req.Schema, req.Name, false)
	if err != nil {
		return nil, err
	}
	args := &sqlArgs{}
	where, err := buildWhere(m, &req, args)
	if err != nil {
		return nil, err
	}
	order, err := buildOrder(m, &req)
	if err != nil {
		return nil, err
	}
	if req.Limit <= 0 || req.Limit > maxPageRows {
		req.Limit = 100
	}
	if req.Offset < 0 {
		req.Offset = 0
	}
	page := fmt.Sprintf(" LIMIT %d OFFSET %d", req.Limit, req.Offset)
	// Deep in a big table OFFSET reads and throws away every earlier row;
	// paging from the neighbouring page's key goes straight to the spot.
	cond, korder, reverse, ok, err := keyset(m, &req, args)
	if err != nil {
		return nil, err
	}
	if ok {
		if where == "" {
			where = " WHERE " + cond
		} else {
			where += " AND " + cond
		}
		order, page = korder, fmt.Sprintf(" LIMIT %d", req.Limit)
	}
	// The page is cut out in a subquery of plain columns, and the grid's
	// expressions (cut text, md5) run only for the rows returned: with a
	// large OFFSET they would otherwise run for every row skipped.
	sys := ""
	if m.KeyMode == "ctid" {
		sys = `, ctid AS "__aa_ctid", xmin AS "__aa_xmin"`
	}
	inner := "SELECT " + rowAlias + ".*" + sys + " FROM " + ident(m.Schema, m.Name) + " AS " + rowAlias + where + order + page
	sql := "SELECT " + selectListWith(m, false, `"__aa_ctid"`, `"__aa_xmin"`) + " FROM (" + inner + ") AS " + rowAlias + outerOrder(order)
	start := time.Now()
	res, err := db.execText(ctx, sql, args.vals, req.hasRawSQL())
	if err != nil {
		return nil, err
	}
	if reverse {
		for i, j := 0, len(res.rows)-1; i < j; i, j = i+1, j-1 {
			res.rows[i], res.rows[j] = res.rows[j], res.rows[i]
		}
	}
	return map[string]any{"rows": finishRows(m, res.rows), "ms": time.Since(start).Milliseconds(), "keyset": ok}, nil
}

func (a *App) handleCount(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req viewReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := a.checkView(&req); err != nil {
		return nil, err
	}
	m, err := db.tableMeta(r.Context(), req.Schema, req.Name, false)
	if err != nil {
		return nil, err
	}
	args := &sqlArgs{}
	where, err := buildWhere(m, &req, args)
	if err != nil {
		return nil, err
	}
	if where == "" && m.Estimate > exactCountMax {
		return map[string]any{"count": m.Estimate, "exact": false}, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), countTimeout)
	defer cancel()
	res, err := db.execText(ctx, "SELECT count(*) FROM "+ident(m.Schema, m.Name)+" AS "+rowAlias+where, args.vals, req.hasRawSQL())
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return map[string]any{"count": max(m.Estimate, 0), "exact": false}, nil
		}
		return nil, err
	}
	n, _ := strconv.ParseInt(*res.rows[0][0].(*string), 10, 64)
	return map[string]any{"count": n, "exact": true}, nil
}

// ---------- editing ----------

type rowKey map[string]*string

func keyWhere(m *TableMeta, key rowKey, args *sqlArgs) (string, error) {
	switch m.KeyMode {
	case "pk":
		var conds []string
		for _, p := range m.PK {
			v, ok := key[p]
			if !ok || v == nil {
				return "", fmt.Errorf("missing primary key value for %q", p)
			}
			conds = append(conds, ident(p)+" = "+args.add(v))
		}
		return strings.Join(conds, " AND "), nil
	case "ctid":
		v := key["__ctid"]
		if v == nil {
			return "", errors.New("missing row id")
		}
		tid, xmin, ok := strings.Cut(*v, ":")
		if !ok || tid == "" || xmin == "" {
			return "", errors.New("this row id is out of date; refresh and try again")
		}
		return "ctid = " + args.add(&tid) + "::tid AND xmin = " + args.add(&xmin) + "::xid", nil
	}
	return "", errors.New("this relation is read-only here: it has no primary key")
}

type editReq struct {
	tableRef
	Key    rowKey             `json:"key"`
	Keys   []rowKey           `json:"keys"`
	Values map[string]*string `json:"values"`
	Expect map[string]*string `json:"expect"`
}

// noteWrite records what a write touches for the audit log: the table, the
// columns and the number of records. Keys only with -audit-detail; values never.
func (a *App) noteWrite(r *http.Request, m *TableMeta, values map[string]*string, keys []rowKey) {
	cols := make([]string, 0, len(values))
	for _, c := range m.Columns {
		if _, ok := values[c.Name]; ok {
			cols = append(cols, c.Name)
		}
	}
	note(r, "table", m.Schema+"."+m.Name, "records", max(len(keys), 1))
	if len(cols) > 0 {
		note(r, "columns", cols)
	}
	if a.cfg.auditDetail && len(keys) > 0 {
		note(r, "keys", keys)
	}
}

func (a *App) editable(db *DB, ctx context.Context, ref tableRef) (*TableMeta, error) {
	if a.cfg.readonly {
		return nil, errors.New("airadminer is running in read-only mode")
	}
	return db.tableMeta(ctx, ref.Schema, ref.Name, false)
}

// stmt is one parameterised write that must touch exactly one row.
type stmt struct {
	sql  string
	args [][]byte
}

// updateSQL builds an UPDATE of one row. expect holds, per column, the value
// the user saw (PostgreSQL text format). Each is compared with the column's
// current text, so a cell someone else changed meanwhile makes the update
// match no row and fail instead of silently overwriting their change.
func updateSQL(m *TableMeta, key rowKey, values, expect map[string]*string) (stmt, error) {
	if len(values) == 0 {
		return stmt{}, errors.New("nothing to update")
	}
	args := &sqlArgs{}
	var sets []string
	for _, c := range m.Columns { // column order keeps the SQL deterministic
		if v, ok := values[c.Name]; ok {
			sets = append(sets, ident(c.Name)+" = "+args.add(v))
		}
	}
	if len(sets) != len(values) {
		return stmt{}, errors.New("unknown column in update")
	}
	where, err := keyWhere(m, key, args)
	if err != nil {
		return stmt{}, err
	}
	for _, c := range m.Columns {
		v, ok := expect[c.Name]
		if !ok || c.Udt == "bytea" { // the grid only sees a byte count for bytea
			continue
		}
		// format('%s') renders with the type's output function, exactly the
		// text the browser got. A ::text cast differs for bool, inet, char(n).
		if v == nil {
			where += " AND " + ident(c.Name) + " IS NULL"
		} else {
			where += " AND " + ident(c.Name) + " IS NOT NULL AND format('%s', " + ident(c.Name) + ") = " + args.add(v)
		}
	}
	for name := range expect {
		if m.column(name) == nil {
			return stmt{}, errors.New("unknown column in update")
		}
	}
	return stmt{"UPDATE " + ident(m.Schema, m.Name) + " SET " + strings.Join(sets, ", ") + " WHERE " + where + " RETURNING " + selectList(m, false), args.vals}, nil
}

func insertSQL(m *TableMeta, values map[string]*string) (stmt, error) {
	args := &sqlArgs{}
	var cols, vals []string
	for _, c := range m.Columns {
		if v, ok := values[c.Name]; ok {
			cols = append(cols, ident(c.Name))
			vals = append(vals, args.add(v))
		}
	}
	if len(cols) != len(values) {
		return stmt{}, errors.New("unknown column in insert")
	}
	sql := "INSERT INTO " + ident(m.Schema, m.Name)
	if len(cols) == 0 {
		sql += " DEFAULT VALUES"
	} else {
		sql += " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ")"
	}
	return stmt{sql + " RETURNING " + selectList(m, false), args.vals}, nil
}

func (a *App) handleUpdate(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req editReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := a.editable(db, ctx, req.tableRef)
	if err != nil {
		return nil, err
	}
	a.noteWrite(r, m, req.Values, []rowKey{req.Key})
	st, err := updateSQL(m, req.Key, req.Values, req.Expect)
	if err != nil {
		return nil, err
	}
	return db.writeOne(ctx, m, st)
}

func (a *App) handleInsert(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req editReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := a.editable(db, ctx, req.tableRef)
	if err != nil {
		return nil, err
	}
	a.noteWrite(r, m, req.Values, nil)
	st, err := insertSQL(m, req.Values)
	if err != nil {
		return nil, err
	}
	return db.writeOne(ctx, m, st)
}

type batchReq struct {
	tableRef
	Updates []struct {
		Key    rowKey             `json:"key"`
		Values map[string]*string `json:"values"`
		Expect map[string]*string `json:"expect"`
	} `json:"updates"`
	Inserts []map[string]*string `json:"inserts"`
	Deletes []rowKey             `json:"deletes"`
}

const maxBatch = 2000

// handleBatch applies many row updates, inserts and deletes in one
// transaction, so a paste, a multi-cell clear or an undo either lands
// completely or not at all.
func (a *App) handleBatch(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req batchReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := a.editable(db, ctx, req.tableRef)
	if err != nil {
		return nil, err
	}
	n := len(req.Updates) + len(req.Inserts) + len(req.Deletes)
	note(r, "table", m.Schema+"."+m.Name, "updates", len(req.Updates), "inserts", len(req.Inserts), "deletes", len(req.Deletes))
	if a.cfg.auditDetail {
		keys := make([]rowKey, 0, len(req.Updates))
		for _, u := range req.Updates {
			keys = append(keys, u.Key)
		}
		note(r, "keys", keys, "deleted_keys", req.Deletes)
	}
	if n == 0 {
		return nil, errors.New("nothing to change")
	}
	if n > maxBatch {
		return nil, fmt.Errorf("change up to %d rows at a time", maxBatch)
	}
	stmts := make([]stmt, 0, n)
	for _, u := range req.Updates {
		st, err := updateSQL(m, u.Key, u.Values, u.Expect)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, st)
	}
	for _, v := range req.Inserts {
		st, err := insertSQL(m, v)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, st)
	}
	for _, k := range req.Deletes {
		args := &sqlArgs{}
		where, err := keyWhere(m, k, args)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, stmt{"DELETE FROM " + ident(m.Schema, m.Name) + " WHERE " + where + " RETURNING " + selectList(m, false), args.vals})
	}
	rows, err := db.writeMany(ctx, stmts)
	if err != nil {
		return nil, err
	}
	u, i := len(req.Updates), len(req.Updates)+len(req.Inserts)
	return map[string]any{"updated": finishRows(m, rows[:u]), "inserted": finishRows(m, rows[u:i]), "deleted": len(rows) - i}, nil
}

// writeMany runs each statement in one transaction. Each must touch exactly
// one row; otherwise everything is rolled back.
func (db *DB) writeMany(ctx context.Context, stmts []stmt) ([][]any, error) {
	conn, err := db.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release(conn, false)
	pc := conn.Conn().PgConn()
	if _, err := pc.Exec(ctx, "BEGIN").ReadAll(); err != nil {
		return nil, err
	}
	rollback := func(err error) ([][]any, error) {
		rollbackOrClose(conn)
		return nil, err
	}
	rows := make([][]any, 0, len(stmts))
	for i, st := range stmts {
		res := pc.ExecParams(ctx, st.sql, st.args, nil, nil, nil).Read()
		if res.Err != nil {
			if len(stmts) > 1 {
				return rollback(&batchErr{i, res.Err})
			}
			return rollback(res.Err)
		}
		if n := res.CommandTag.RowsAffected(); n != 1 {
			if n == 0 {
				return rollback(errors.New("the record was changed or deleted by someone else since you loaded it; nothing was saved. Refresh to see the current values"))
			}
			return rollback(fmt.Errorf("refusing to change %d rows at once", n))
		}
		rows = append(rows, textRow(res.Rows[0]))
	}
	if _, err := pc.Exec(ctx, "COMMIT").ReadAll(); err != nil {
		return nil, err
	}
	return rows, nil
}

// batchErr tells the client which statement of a batch failed, keeping the
// underlying PostgreSQL error (and its detail and hint) reachable.
type batchErr struct {
	index int
	err   error
}

func (e *batchErr) Error() string { return e.err.Error() }
func (e *batchErr) Unwrap() error { return e.err }

func (db *DB) writeOne(ctx context.Context, m *TableMeta, st stmt) (any, error) {
	rows, err := db.writeMany(ctx, []stmt{st})
	if err != nil {
		return nil, err
	}
	return map[string]any{"row": finishRow(m, rows[0])}, nil
}

func (a *App) handleDelete(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req editReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := a.editable(db, ctx, req.tableRef)
	if err != nil {
		return nil, err
	}
	a.noteWrite(r, m, nil, req.Keys)
	if len(req.Keys) == 0 {
		return nil, errors.New("no rows selected")
	}
	if len(req.Keys) > maxBatch {
		return nil, fmt.Errorf("delete up to %d records at a time", maxBatch)
	}
	conn, err := db.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release(conn, false)
	pc := conn.Conn().PgConn()
	if _, err := pc.Exec(ctx, "BEGIN").ReadAll(); err != nil {
		return nil, err
	}
	var total int64
	for _, k := range req.Keys {
		args := &sqlArgs{}
		where, err := keyWhere(m, k, args)
		if err == nil {
			res := pc.ExecParams(ctx, "DELETE FROM "+ident(m.Schema, m.Name)+" WHERE "+where, args.vals, nil, nil, nil).Read()
			err = res.Err
			total += res.CommandTag.RowsAffected()
		}
		if err != nil {
			rollbackOrClose(conn)
			return nil, err
		}
	}
	if _, err := pc.Exec(ctx, "COMMIT").ReadAll(); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": total}, nil
}

// duplicateSQL copies one row into a new one with INSERT … SELECT, so values
// never leave the server (binary columns copy too). Columns the database
// fills on its own are left out: identity and generated columns, and
// primary-key columns with a default, such as serial ids and uuid defaults.
// overrides replaces copied values (or sets a left-out column) with
// parameters, so a copy can get a new unique key in the same statement.
func duplicateSQL(m *TableMeta, where string, overrides map[string]*string, args *sqlArgs) (string, error) {
	for name := range overrides {
		c := m.column(name)
		if c == nil {
			return "", errors.New("unknown column in duplicate")
		}
		if c.Generated != "" || c.Identity == "a" {
			return "", fmt.Errorf("column %q is set by the database", name)
		}
	}
	var cols, exprs []string
	for _, c := range m.Columns {
		if v, ok := overrides[c.Name]; ok {
			cols = append(cols, ident(c.Name))
			exprs = append(exprs, args.add(v))
			continue
		}
		if c.Identity != "" || c.Generated != "" || (c.PK && c.Default != nil) {
			continue
		}
		cols = append(cols, ident(c.Name))
		exprs = append(exprs, ident(c.Name))
	}
	t := ident(m.Schema, m.Name)
	ret := " RETURNING " + selectList(m, false)
	if len(cols) == 0 {
		// Every column fills itself: make one default row per existing source row.
		return "INSERT INTO " + t + " SELECT FROM " + t + " WHERE " + where + ret, nil
	}
	return "INSERT INTO " + t + " (" + strings.Join(cols, ", ") + ") SELECT " + strings.Join(exprs, ", ") + " FROM " + t + " WHERE " + where + ret, nil
}

const maxDuplicate = 1000

func (a *App) handleDuplicate(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req editReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := a.editable(db, ctx, req.tableRef)
	if err != nil {
		return nil, err
	}
	a.noteWrite(r, m, req.Values, req.Keys)
	if len(req.Keys) == 0 {
		return nil, errors.New("no rows selected")
	}
	if len(req.Values) > 0 && len(req.Keys) != 1 {
		return nil, errors.New("changed values can only be given when copying one record")
	}
	if len(req.Keys) > maxDuplicate {
		return nil, fmt.Errorf("duplicate up to %d records at a time", maxDuplicate)
	}
	stmts := make([]stmt, 0, len(req.Keys))
	for _, k := range req.Keys {
		args := &sqlArgs{}
		where, err := keyWhere(m, k, args)
		if err != nil {
			return nil, err
		}
		sql, err := duplicateSQL(m, where, req.Values, args)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, stmt{sql, args.vals})
	}
	rows, err := db.writeMany(ctx, stmts)
	if err != nil {
		return nil, err
	}
	return map[string]any{"rows": finishRows(m, rows)}, nil
}

// ---------- SQL editor ----------

type queryCol struct {
	Name string `json:"name"`
	Type string `json:"type"`
	OID  uint32 `json:"oid"`
}

type queryResult struct {
	Columns   []queryCol `json:"columns"`
	Rows      [][]any    `json:"rows"`
	Tag       string     `json:"tag"`
	Truncated bool       `json:"truncated"`
	Total     int64      `json:"total"` // rows the statement returned, kept or not
}

func (a *App) handleQuery(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		SQL string `json:"sql"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if a.cfg.noSQL {
		return nil, errNoSQL
	}
	if strings.TrimSpace(req.SQL) == "" {
		return nil, errors.New("write a query first")
	}
	note(r, a.sqlNote(req.SQL)...)
	ctx := r.Context()
	done, err := db.startLong(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	conn, err := db.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release(conn, true) // SET, SET ROLE, temp tables... must not leak into later requests
	pc := conn.Conn().PgConn()
	start := time.Now()
	results := []queryResult{}
	kept := 0 // bytes of every result so far
	mrr := pc.Exec(ctx, req.SQL)
	for mrr.NextResult() {
		rr := mrr.ResultReader()
		qr := queryResult{Rows: [][]any{}}
		for _, f := range rr.FieldDescriptions() {
			qr.Columns = append(qr.Columns, queryCol{Name: f.Name, OID: f.DataTypeOID})
		}
		// Rows past the caps are read and dropped rather than canceling the
		// statement: canceling would roll back earlier statements of the
		// same script, which run in one implicit transaction.
		for rr.NextRow() {
			qr.Total++
			if qr.Truncated {
				continue
			}
			vals := rr.Values()
			n := 16
			for _, v := range vals {
				n += len(v) + 8
			}
			if len(qr.Rows) >= maxQueryRows || kept+n > maxQueryBytes {
				qr.Truncated = true
				continue
			}
			kept += n
			qr.Rows = append(qr.Rows, textRow(vals))
		}
		tag, err := rr.Close()
		qr.Tag = tag.String()
		if err != nil {
			break // surfaced by mrr.Close
		}
		results = append(results, qr)
	}
	execErr := mrr.Close()
	ms := time.Since(start).Milliseconds()
	if len(results) > 0 {
		db.fillTypeNames(ctx, conn, results)
	}
	out := map[string]any{"results": results, "ms": ms}
	if execErr != nil {
		if errors.Is(execErr, context.Canceled) {
			return nil, execErr
		}
		e := apiErr{Error: execErr.Error()}
		var pgErr *pgconn.PgError
		if errors.As(execErr, &pgErr) {
			e = apiErr{Error: pgErr.Message, Detail: pgErr.Detail, Hint: pgErr.Hint, Code: pgErr.Code, Position: pgErr.Position}
		}
		out["error"] = e
		infoOf(r).auditErr = errors.New(e.Error)
	}
	if mayChangeSchema(req.SQL) {
		db.forgetMeta()
		out["schemaChanged"] = true
	}
	return out, nil
}

func mayChangeSchema(sql string) bool {
	u := strings.ToUpper(sql)
	for _, kw := range []string{"CREATE", "ALTER", "DROP", "RENAME", "COMMENT", "TRUNCATE"} {
		if strings.Contains(u, kw) {
			return true
		}
	}
	return false
}

func (db *DB) fillTypeNames(ctx context.Context, conn *pgxpool.Conn, results []queryResult) {
	db.metaMu.Lock()
	var missing []int64
	for _, r := range results {
		for _, c := range r.Columns {
			if _, ok := db.typeNames[c.OID]; !ok {
				missing = append(missing, int64(c.OID))
			}
		}
	}
	db.metaMu.Unlock()
	if len(missing) > 0 {
		rows, err := conn.Query(ctx, `SELECT oid::int8, format_type(oid, NULL) FROM pg_type WHERE oid::int8 = ANY($1)`, missing)
		if err == nil {
			for rows.Next() {
				var oid int64
				var name string
				if rows.Scan(&oid, &name) == nil {
					db.metaMu.Lock()
					db.typeNames[uint32(oid)] = name
					db.metaMu.Unlock()
				}
			}
			rows.Close()
		}
	}
	db.metaMu.Lock()
	defer db.metaMu.Unlock()
	for i := range results {
		for j := range results[i].Columns {
			results[i].Columns[j].Type = db.typeNames[results[i].Columns[j].OID]
		}
	}
}

// ---------- export ----------

func (a *App) handleExport(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req viewReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := a.checkView(&req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := db.tableMeta(ctx, req.Schema, req.Name, false)
	if err != nil {
		return nil, err
	}
	note(r, "table", m.Schema+"."+m.Name, "filters", len(req.Filters), "search", req.Search != "")
	for _, f := range req.Filters {
		if f.Op == "sql" && f.Val != nil {
			note(r, a.sqlNote(*f.Val)...)
		}
	}
	if a.cfg.csvEscape {
		return a.exportEscaped(ctx, w, db, m, &req)
	}
	args := &sqlArgs{inline: true}
	where, err := buildWhere(m, &req, args)
	if err != nil {
		return nil, err
	}
	order, err := buildOrder(m, &req)
	if err != nil {
		return nil, err
	}
	sql := "COPY (SELECT " + selectList(m, true) + " FROM " + ident(m.Schema, m.Name) + " AS " + rowAlias + where + order + ") TO STDOUT WITH (FORMAT csv, HEADER)"
	done, err := db.startLong(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	conn, err := db.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release(conn, req.hasRawSQL())
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", m.Name+".csv"))
	bw := &lazyWriter{w: w}
	if _, err := conn.Conn().PgConn().CopyTo(ctx, bw, sql); err != nil {
		if !bw.started {
			w.Header().Del("Content-Disposition")
			return nil, err
		}
		a.log.Warn("export failed after it started", "table", m.Schema+"."+m.Name, "error", err)
	}
	if !bw.started {
		w.WriteHeader(http.StatusOK)
	}
	return nil, nil
}

// lazyWriter delays the 200 header until the first byte so an early
// SQL error can still be reported as JSON.
type lazyWriter struct {
	w       http.ResponseWriter
	started bool
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		l.w.WriteHeader(http.StatusOK)
	}
	return l.w.Write(p)
}

// ---------- low level ----------

const cleanupTimeout = 5 * time.Second

// rollbackOrClose ends a failed transaction. If even that fails (broken or
// stuck connection), the connection is closed instead of going back to the
// pool in an unknown state.
func rollbackOrClose(conn *pgxpool.Conn) {
	pc := conn.Conn().PgConn()
	if pc.IsClosed() || pc.TxStatus() == 'I' {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if _, err := pc.Exec(ctx, "ROLLBACK").ReadAll(); err != nil {
		_ = pc.Close(ctx)
	}
}

// release returns a connection to the pool. A connection that ran SQL the
// user wrote is reset first: DISCARD ALL undoes SET, SET ROLE, session
// authorization, temp tables, advisory locks, LISTEN and prepared
// statements, back to the settings the connection started with. If the
// reset fails the connection is closed rather than reused.
func release(conn *pgxpool.Conn, userSQL bool) {
	defer conn.Release()
	pc := conn.Conn().PgConn()
	if pc.IsClosed() {
		return
	}
	if pc.TxStatus() != 'I' {
		rollbackOrClose(conn)
	}
	if !userSQL || pc.IsClosed() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if _, err := pc.Exec(ctx, "DISCARD ALL").ReadAll(); err != nil {
		_ = pc.Close(ctx)
		return
	}
	if err := conn.Conn().DeallocateAll(ctx); err != nil {
		_ = pc.Close(ctx)
	}
}

type textResult struct {
	rows [][]any
}

func textRow(vals [][]byte) []any {
	row := make([]any, len(vals))
	for i, v := range vals {
		if v == nil {
			row[i] = nil
		} else {
			s := string(v)
			row[i] = &s
		}
	}
	return row
}

// execText runs one parameterised statement and returns every value in
// PostgreSQL's own text format, so the browser sees exactly what psql would.
func (db *DB) execText(ctx context.Context, sql string, params [][]byte, userSQL bool) (*textResult, error) {
	res, err := db.execTextOnce(ctx, sql, params, userSQL)
	// A pooled connection the server dropped (restart, failover, killed
	// backend) fails the first statement sent on it. These statements only
	// read, so try once more on a fresh connection; not with SQL the user
	// wrote, which might do more than read.
	if err != nil && !userSQL && ctx.Err() == nil && connLost(err) {
		res, err = db.execTextOnce(ctx, sql, params, userSQL)
	}
	return res, err
}

func (db *DB) execTextOnce(ctx context.Context, sql string, params [][]byte, userSQL bool) (*textResult, error) {
	conn, err := db.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release(conn, userSQL)
	res := conn.Conn().PgConn().ExecParams(ctx, sql, params, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	out := &textResult{rows: make([][]any, len(res.Rows))}
	for i, r := range res.Rows {
		out.rows[i] = textRow(r)
	}
	return out, nil
}
