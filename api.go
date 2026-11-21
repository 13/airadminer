package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
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

type handler func(s *Session, w http.ResponseWriter, r *http.Request) (any, error)

func (a *App) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", a.guard(a.handleLogin))
	mux.HandleFunc("POST /api/logout", a.guard(a.authed(a.handleLogout)))
	mux.HandleFunc("POST /api/meta", a.guard(a.authed(a.handleMeta)))
	mux.HandleFunc("POST /api/use", a.guard(a.authed(a.handleUse)))
	mux.HandleFunc("POST /api/tables", a.guard(a.authed(a.handleTables)))
	mux.HandleFunc("POST /api/table", a.guard(a.authed(a.handleTable)))
	mux.HandleFunc("POST /api/rows", a.guard(a.authed(a.handleRows)))
	mux.HandleFunc("POST /api/count", a.guard(a.authed(a.handleCount)))
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
		if s == nil || s.Pool() == nil {
			writeJSON(w, http.StatusUnauthorized, apiErr{Error: "not signed in", Code: "auth"})
			return
		}
		out, err := h(s, w, r)
		if err != nil {
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
		log.Printf("write response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, err error) {
	var index *int
	var be *batchErr
	if errors.As(err, &be) {
		index = &be.index
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		writeJSON(w, http.StatusBadRequest, apiErr{Error: pgErr.Message, Detail: pgErr.Detail, Hint: pgErr.Hint, Code: pgErr.Code, Position: pgErr.Position, Index: index})
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
	writeJSON(w, http.StatusBadRequest, apiErr{Error: err.Error()})
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
	var req loginReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	dsn, err := req.dsn()
	if err != nil {
		writeErr(w, err)
		return
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		writeErr(w, fmt.Errorf("invalid connection settings: %w", err))
		return
	}
	s, err := a.openSession(r.Context(), cfg)
	if err != nil {
		writeErr(w, err)
		return
	}
	if old := a.sessionFor(r); old != nil && !old.fixed {
		a.dropSession(w, old)
	}
	a.addSession(w, r, s)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleLogout(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	if s.fixed {
		return nil, errors.New("this server is started with a fixed -dsn; there is nothing to sign out of")
	}
	a.dropSession(w, s)
	return map[string]any{"ok": true}, nil
}

func (a *App) handleMeta(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	pool := s.Pool()
	out := map[string]any{"fixed": s.fixed, "readonly": a.cfg.readonly, "version": version}
	var user, db, server, host string
	var ro string
	err := pool.QueryRow(ctx, `SELECT current_user, current_database(), current_setting('server_version'),
		COALESCE(inet_server_addr()::text, ''), current_setting('default_transaction_read_only')`).Scan(&user, &db, &server, &host, &ro)
	if err != nil {
		return nil, err
	}
	cc := pool.Config().ConnConfig
	if cc.Host != "" {
		host = cc.Host
	}
	out["user"], out["database"], out["server"], out["host"], out["port"] = user, db, server, host, cc.Port
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

func (a *App) handleUse(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		Database string `json:"database"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.Database == "" {
		return nil, errors.New("database is required")
	}
	if err := s.connect(r.Context(), req.Database, a.cfg.readonly); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (a *App) handleTables(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	s.forgetMeta()
	tables, schemas, err := listTables(r.Context(), s.Pool())
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

func (a *App) handleTable(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req tableRef
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return s.tableMeta(r.Context(), req.Schema, req.Name, true)
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
// summarised by their size instead of shipping the bytes to the browser.
func selectList(m *TableMeta, forExport bool) string {
	var parts []string
	if m.KeyMode == "ctid" && !forExport {
		parts = append(parts, `ctid::text AS "__ctid"`)
	}
	for _, c := range m.Columns {
		col := ident(c.Name)
		if c.Udt == "bytea" && !forExport {
			parts = append(parts, "octet_length("+col+")::text AS "+col)
		} else {
			parts = append(parts, col)
		}
	}
	if len(parts) == 0 {
		return "NULL"
	}
	return strings.Join(parts, ", ")
}

func (a *App) handleRows(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req viewReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := s.tableMeta(ctx, req.Schema, req.Name, false)
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
	sql := "SELECT " + selectList(m, false) + " FROM " + ident(m.Schema, m.Name) + " AS " + rowAlias +
		where + order + fmt.Sprintf(" LIMIT %d OFFSET %d", req.Limit, req.Offset)
	start := time.Now()
	res, err := s.execText(ctx, sql, args.vals)
	if err != nil {
		return nil, err
	}
	return map[string]any{"rows": res.rows, "ms": time.Since(start).Milliseconds()}, nil
}

func (a *App) handleCount(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req viewReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	m, err := s.tableMeta(r.Context(), req.Schema, req.Name, false)
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
	res, err := s.execText(ctx, "SELECT count(*) FROM "+ident(m.Schema, m.Name)+" AS "+rowAlias+where, args.vals)
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
		return "ctid = " + args.add(v) + "::tid", nil
	}
	return "", errors.New("this relation is read-only here: it has no primary key")
}

type editReq struct {
	tableRef
	Key    rowKey             `json:"key"`
	Keys   []rowKey           `json:"keys"`
	Values map[string]*string `json:"values"`
}

func (a *App) editable(s *Session, ctx context.Context, ref tableRef) (*TableMeta, error) {
	if a.cfg.readonly {
		return nil, errors.New("airadminer is running in read-only mode")
	}
	return s.tableMeta(ctx, ref.Schema, ref.Name, false)
}

// stmt is one parameterised write that must touch exactly one row.
type stmt struct {
	sql  string
	args [][]byte
}

func updateSQL(m *TableMeta, key rowKey, values map[string]*string) (stmt, error) {
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

func (a *App) handleUpdate(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req editReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := a.editable(s, ctx, req.tableRef)
	if err != nil {
		return nil, err
	}
	st, err := updateSQL(m, req.Key, req.Values)
	if err != nil {
		return nil, err
	}
	return s.writeOne(ctx, st.sql, st.args)
}

func (a *App) handleInsert(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req editReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := a.editable(s, ctx, req.tableRef)
	if err != nil {
		return nil, err
	}
	st, err := insertSQL(m, req.Values)
	if err != nil {
		return nil, err
	}
	return s.writeOne(ctx, st.sql, st.args)
}

type batchReq struct {
	tableRef
	Updates []struct {
		Key    rowKey             `json:"key"`
		Values map[string]*string `json:"values"`
	} `json:"updates"`
	Inserts []map[string]*string `json:"inserts"`
	Deletes []rowKey             `json:"deletes"`
}

const maxBatch = 2000

// handleBatch applies many row updates, inserts and deletes in one
// transaction, so a paste, a multi-cell clear or an undo either lands
// completely or not at all.
func (a *App) handleBatch(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req batchReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := a.editable(s, ctx, req.tableRef)
	if err != nil {
		return nil, err
	}
	n := len(req.Updates) + len(req.Inserts) + len(req.Deletes)
	if n == 0 {
		return nil, errors.New("nothing to change")
	}
	if n > maxBatch {
		return nil, fmt.Errorf("change up to %d rows at a time", maxBatch)
	}
	stmts := make([]stmt, 0, n)
	for _, u := range req.Updates {
		st, err := updateSQL(m, u.Key, u.Values)
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
	rows, err := s.writeMany(ctx, stmts)
	if err != nil {
		return nil, err
	}
	u, i := len(req.Updates), len(req.Updates)+len(req.Inserts)
	return map[string]any{"updated": rows[:u], "inserted": rows[u:i], "deleted": len(rows) - i}, nil
}

// writeMany runs each statement in one transaction. Each must touch exactly
// one row; otherwise everything is rolled back.
func (s *Session) writeMany(ctx context.Context, stmts []stmt) ([][]any, error) {
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	pc := conn.Conn().PgConn()
	if _, err := pc.Exec(ctx, "BEGIN").ReadAll(); err != nil {
		return nil, err
	}
	rollback := func(err error) ([][]any, error) {
		_, _ = pc.Exec(context.Background(), "ROLLBACK").ReadAll()
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
				return rollback(errors.New("the row no longer exists or was changed by someone else; refresh and try again"))
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

func (s *Session) writeOne(ctx context.Context, sql string, params [][]byte) (any, error) {
	rows, err := s.writeMany(ctx, []stmt{{sql, params}})
	if err != nil {
		return nil, err
	}
	return map[string]any{"row": rows[0]}, nil
}

func (a *App) handleDelete(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req editReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := a.editable(s, ctx, req.tableRef)
	if err != nil {
		return nil, err
	}
	if len(req.Keys) == 0 {
		return nil, errors.New("no rows selected")
	}
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
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
			_, _ = pc.Exec(context.Background(), "ROLLBACK").ReadAll()
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

func (a *App) handleDuplicate(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req editReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := a.editable(s, ctx, req.tableRef)
	if err != nil {
		return nil, err
	}
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
	rows, err := s.writeMany(ctx, stmts)
	if err != nil {
		return nil, err
	}
	return map[string]any{"rows": rows}, nil
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
}

func (a *App) handleQuery(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		SQL string `json:"sql"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.SQL) == "" {
		return nil, errors.New("write a query first")
	}
	ctx := r.Context()
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	pc := conn.Conn().PgConn()
	start := time.Now()
	results := []queryResult{}
	mrr := pc.Exec(ctx, req.SQL)
	for mrr.NextResult() {
		rr := mrr.ResultReader()
		qr := queryResult{Rows: [][]any{}}
		for _, f := range rr.FieldDescriptions() {
			qr.Columns = append(qr.Columns, queryCol{Name: f.Name, OID: f.DataTypeOID})
		}
		for rr.NextRow() {
			if len(qr.Rows) >= maxQueryRows {
				qr.Truncated = true
				continue
			}
			qr.Rows = append(qr.Rows, textRow(rr.Values()))
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
	// A failed statement inside an explicit transaction leaves the pooled
	// connection aborted; reset it so the next query starts clean.
	if pc.TxStatus() != 'I' {
		_, _ = pc.Exec(context.Background(), "ROLLBACK").ReadAll()
	}
	if len(results) > 0 {
		s.fillTypeNames(ctx, conn, results)
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
	}
	if s.mayChangeSchema(req.SQL) {
		s.forgetMeta()
		out["schemaChanged"] = true
	}
	return out, nil
}

func (s *Session) mayChangeSchema(sql string) bool {
	u := strings.ToUpper(sql)
	for _, kw := range []string{"CREATE", "ALTER", "DROP", "RENAME", "COMMENT", "TRUNCATE"} {
		if strings.Contains(u, kw) {
			return true
		}
	}
	return false
}

func (s *Session) fillTypeNames(ctx context.Context, conn *pgxpool.Conn, results []queryResult) {
	s.metaMu.Lock()
	var missing []int64
	for _, r := range results {
		for _, c := range r.Columns {
			if _, ok := s.typeNames[c.OID]; !ok {
				missing = append(missing, int64(c.OID))
			}
		}
	}
	s.metaMu.Unlock()
	if len(missing) > 0 {
		rows, err := conn.Query(ctx, `SELECT oid::int8, format_type(oid, NULL) FROM pg_type WHERE oid::int8 = ANY($1)`, missing)
		if err == nil {
			for rows.Next() {
				var oid int64
				var name string
				if rows.Scan(&oid, &name) == nil {
					s.metaMu.Lock()
					s.typeNames[uint32(oid)] = name
					s.metaMu.Unlock()
				}
			}
			rows.Close()
		}
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	for i := range results {
		for j := range results[i].Columns {
			results[i].Columns[j].Type = s.typeNames[results[i].Columns[j].OID]
		}
	}
}

// ---------- export ----------

func (a *App) handleExport(s *Session, w http.ResponseWriter, r *http.Request) (any, error) {
	var req viewReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := s.tableMeta(ctx, req.Schema, req.Name, false)
	if err != nil {
		return nil, err
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
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", m.Name+".csv"))
	bw := &lazyWriter{w: w}
	if _, err := conn.Conn().PgConn().CopyTo(ctx, bw, sql); err != nil {
		if !bw.started {
			w.Header().Del("Content-Disposition")
			return nil, err
		}
		log.Printf("export %s.%s: %v", m.Schema, m.Name, err)
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
func (s *Session) execText(ctx context.Context, sql string, params [][]byte) (*textResult, error) {
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
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
