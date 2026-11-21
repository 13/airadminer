package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// CSV export with formula escaping (-csv-escape-formulas). A cell starting
// with = + - @ or a tab/CR can run as a formula when the file is opened in a
// spreadsheet ("CSV injection"); such a cell gets a leading apostrophe,
// unless it is a plain number. The output is otherwise the same as
// COPY … (FORMAT csv, HEADER): NULL is an empty field and empty text is "".

var plainNumber = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

func escapeFormula(v string) string {
	if v == "" || !strings.ContainsRune("=+-@\t\r", rune(v[0])) || plainNumber.MatchString(v) {
		return v
	}
	return "'" + v
}

// csvField renders one field as COPY's CSV format does.
func csvField(v *string, escape bool) string {
	if v == nil {
		return ""
	}
	s := *v
	if escape {
		s = escapeFormula(s)
	}
	if s == "" || strings.ContainsAny(s, ",\"\n\r") || s == `\.` {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func writeCSVLine(w io.Writer, fields []*string, escape bool) error {
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(csvField(f, escape))
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

func (a *App) exportEscaped(ctx context.Context, w http.ResponseWriter, db *DB, m *TableMeta, req *viewReq) (any, error) {
	args := &sqlArgs{}
	where, err := buildWhere(m, req, args)
	if err != nil {
		return nil, err
	}
	order, err := buildOrder(m, req)
	if err != nil {
		return nil, err
	}
	sql := "SELECT " + selectList(m, true) + " FROM " + ident(m.Schema, m.Name) + " AS " + rowAlias + where + order
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
	lw := &lazyWriter{w: w}
	// Buffered: an error in the first 64 KB can still be answered as JSON.
	bw := bufio.NewWriterSize(lw, 64<<10)
	rr := conn.Conn().PgConn().ExecParams(ctx, sql, args.vals, nil, nil, nil)
	header := make([]*string, 0, len(m.Columns))
	for _, f := range rr.FieldDescriptions() {
		header = append(header, strp(f.Name))
	}
	werr := writeCSVLine(bw, header, true)
	fields := make([]*string, len(header))
	for werr == nil && rr.NextRow() {
		for i, v := range rr.Values() {
			if v == nil {
				fields[i] = nil
			} else {
				fields[i] = strp(string(v))
			}
		}
		werr = writeCSVLine(bw, fields, true)
	}
	if _, err := rr.Close(); err != nil {
		if !lw.started {
			w.Header().Del("Content-Disposition")
			return nil, err
		}
		a.log.Warn("export failed after it started", "table", m.Schema+"."+m.Name, "error", err)
		return nil, nil
	}
	if werr == nil {
		werr = bw.Flush()
	}
	if werr != nil {
		a.log.Warn("export: writing the response failed", "table", m.Schema+"."+m.Name, "error", werr)
	}
	if !lw.started {
		w.WriteHeader(http.StatusOK)
	}
	return nil, nil
}
