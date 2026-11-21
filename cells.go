package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Long values are cut in grid rows, so a page stays small however big the
// documents in it are. A cut cell comes with the md5 of its full text; the
// browser loads full values through /api/cells when a cell is opened,
// edited or copied, and checks the hash before writing over a value it
// never saw in full.
const (
	rowCellBudget = 65536 // characters per row, shared by its long columns
	minCut        = 256
	maxCut        = 8192
	maxCellsBytes = 64 << 20 // per /api/cells response
)

// cuttable reports columns whose text can be long: strings, arrays and
// user-defined types (json, jsonb, xml, tsvector, geometry...). Key and
// link columns are never cut, since the browser uses their values.
func cuttable(c *Column) bool {
	if c.PK || c.FK != nil || c.Udt == "bytea" {
		return false
	}
	switch c.Cat {
	case "S", "U", "A":
		return true
	}
	return false
}

// cutLen is the length, in characters, that long values of this table are
// cut to in the grid (0: nothing is cut). Relations without a row key are
// never cut: there would be no way to load a record's full values.
func cutLen(m *TableMeta) int {
	if m.KeyMode == "" {
		return 0
	}
	n := 0
	for i := range m.Columns {
		if cuttable(&m.Columns[i]) {
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return min(maxCut, max(minCut, rowCellBudget/n))
}

// cutCols lists, for each cut column, its index in a grid row. Their hashes
// follow the visible columns in the same order (see selectList).
func cutCols(m *TableMeta) []int {
	if cutLen(m) == 0 {
		return nil
	}
	off := 0
	if m.KeyMode == "ctid" {
		off = 1
	}
	var out []int
	for i := range m.Columns {
		if cuttable(&m.Columns[i]) {
			out = append(out, i+off)
		}
	}
	return out
}

// finishRow turns a row read with selectList(m, false) into what the
// browser gets: the visible values, followed by {"cut": {index: md5}} when
// any value was cut. Values are strings or null, so the object is
// unambiguous.
func finishRow(m *TableMeta, row []any) []any {
	cols := cutCols(m)
	if len(cols) == 0 || len(row) < len(cols) {
		return row
	}
	vis := len(row) - len(cols)
	var cut map[string]string
	for j, ri := range cols {
		if h, ok := row[vis+j].(*string); ok && h != nil {
			if cut == nil {
				cut = map[string]string{}
			}
			cut[strconv.Itoa(ri)] = *h
		}
	}
	row = row[:vis]
	if cut != nil {
		row = append(row, map[string]any{"cut": cut})
	}
	return row
}

func finishRows(m *TableMeta, rows [][]any) [][]any {
	for i := range rows {
		rows[i] = finishRow(m, rows[i])
	}
	return rows
}

// textOf renders a value with its type's output function, exactly the text
// the browser gets for it, keeping NULL as NULL.
func textOf(col string) string {
	return "CASE WHEN " + col + " IS NULL THEN NULL ELSE format('%s', " + col + ") END"
}

func hashOf(col string) string {
	return "CASE WHEN " + col + " IS NULL THEN NULL ELSE md5(format('%s', " + col + ")) END"
}

type cellsReq struct {
	tableRef
	Keys []rowKey `json:"keys"`
	Cols []string `json:"cols"`
}

type fullCells struct {
	Values []any `json:"values"`
	Hashes []any `json:"hashes"`
}

// handleCells returns the full values of some columns of some records, with
// the md5 of each. A record that no longer has its key (deleted, or a
// ctid/xmin key after an update) comes back as null.
func (a *App) handleCells(s *Session, db *DB, w http.ResponseWriter, r *http.Request) (any, error) {
	var req cellsReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := db.tableMeta(ctx, req.Schema, req.Name, false)
	if err != nil {
		return nil, err
	}
	if len(req.Keys) == 0 || len(req.Cols) == 0 {
		return map[string]any{"rows": []any{}}, nil
	}
	if len(req.Keys) > maxBatch {
		return nil, fmt.Errorf("load up to %d records at a time", maxBatch)
	}
	var sel []string
	for _, name := range req.Cols {
		c := m.column(name)
		if c == nil {
			return nil, fmt.Errorf("unknown column %q", name)
		}
		if c.Udt == "bytea" {
			return nil, errors.New("binary columns are not loaded into the browser")
		}
		sel = append(sel, ident(c.Name))
	}
	for _, name := range req.Cols {
		sel = append(sel, hashOf(ident(name)))
	}
	conn, err := db.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release(conn, false)
	pc := conn.Conn().PgConn()
	out := make([]*fullCells, len(req.Keys))
	size := 0
	for i, k := range req.Keys {
		args := &sqlArgs{}
		where, err := keyWhere(m, k, args)
		if err != nil {
			return nil, err
		}
		q := "SELECT " + strings.Join(sel, ", ") + " FROM " + ident(m.Schema, m.Name) + " WHERE " + where
		res := pc.ExecParams(ctx, q, args.vals, nil, nil, nil).Read()
		if res.Err != nil {
			return nil, res.Err
		}
		if len(res.Rows) != 1 {
			continue
		}
		row := textRow(res.Rows[0])
		for _, v := range res.Rows[0] {
			size += len(v)
		}
		if size > maxCellsBytes {
			return nil, fmt.Errorf("these values are too large to load in the browser (over %d MB); use the SQL editor or a CSV export", maxCellsBytes>>20)
		}
		n := len(req.Cols)
		out[i] = &fullCells{Values: row[:n], Hashes: row[n:]}
	}
	return map[string]any{"rows": out}, nil
}

// ---------- connection limits ----------

// acquireWait bounds how long a request waits for a free connection
// (a variable so tests can shorten it).
var acquireWait = 20 * time.Second

var errBusy = errors.New("all database connections of this session are busy; try again in a moment")

// acquire takes a pooled connection, failing with errBusy instead of
// queueing forever when every connection stays in use.
func (db *DB) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	actx, cancel := context.WithTimeout(ctx, acquireWait)
	defer cancel()
	conn, err := db.pool.Acquire(actx)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, errBusy
	}
	return conn, err
}

var errLongBusy = errors.New("other SQL queries or exports of this session are still running; wait for them to finish or cancel them")

// startLong takes a slot for a SQL-editor query or an export.
func (db *DB) startLong(ctx context.Context) (func(), error) {
	t := time.NewTimer(acquireWait)
	defer t.Stop()
	select {
	case db.long <- struct{}{}:
		return func() { <-db.long }, nil
	case <-t.C:
		return nil, errLongBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
