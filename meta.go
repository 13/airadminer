package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type FKRef struct {
	Name   string `json:"name"`
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Column string `json:"column"`
}

type Incoming struct {
	Name      string `json:"name"`
	Schema    string `json:"schema"`
	Table     string `json:"table"`
	Column    string `json:"column"`    // column on the referencing table
	RefColumn string `json:"refColumn"` // column on this table
}

type Column struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"` // formatted, e.g. "character varying(80)"
	Udt       string   `json:"udt"`  // base type name, e.g. "varchar", "_int4"
	Cat       string   `json:"cat"`  // pg_type.typcategory of the base type
	Nullable  bool     `json:"nullable"`
	Default   *string  `json:"default"`
	Identity  string   `json:"identity,omitempty"`  // "a" always, "d" by default
	Generated string   `json:"generated,omitempty"` // "s" stored
	Comment   *string  `json:"comment"`
	Enum      []string `json:"enum,omitempty"`
	FK        *FKRef   `json:"fk,omitempty"`
	PK        bool     `json:"pk"`
}

type Index struct {
	Name    string `json:"name"`
	Def     string `json:"def"`
	Primary bool   `json:"primary"`
	Unique  bool   `json:"unique"`
	Size    int64  `json:"size"`
}

type Constraint struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Def  string `json:"def"`
}

type TableMeta struct {
	Schema      string       `json:"schema"`
	Name        string       `json:"name"`
	Kind        string       `json:"kind"`
	OID         uint32       `json:"oid"`
	Estimate    int64        `json:"estimate"`
	Size        *int64       `json:"size"`
	Comment     *string      `json:"comment"`
	Columns     []Column     `json:"columns"`
	PK          []string     `json:"pk"`
	KeyMode     string       `json:"keyMode"` // "pk", "ctid" or "" (read-only)
	Indexes     []Index      `json:"indexes"`
	Constraints []Constraint `json:"constraints"`
	Incoming    []Incoming   `json:"incoming"`
	ViewDef     *string      `json:"viewDef,omitempty"`
}

func (m *TableMeta) column(name string) *Column {
	for i := range m.Columns {
		if m.Columns[i].Name == name {
			return &m.Columns[i]
		}
	}
	return nil
}

type TableInfo struct {
	Schema   string  `json:"schema"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Estimate int64   `json:"estimate"`
	Size     *int64  `json:"size"`
	Comment  *string `json:"comment"`
}

const userSchemas = `n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp\_%'`

func listTables(ctx context.Context, q querier) ([]TableInfo, []string, error) {
	rows, err := q.Query(ctx, `
		SELECT n.nspname, c.relname, c.relkind::text, c.reltuples::bigint,
		       CASE WHEN c.relkind IN ('r','m','p') THEN pg_total_relation_size(c.oid) END,
		       obj_description(c.oid, 'pg_class')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r','v','m','f','p') AND NOT c.relispartition AND `+userSchemas+`
		ORDER BY 1, 2`)
	if err != nil {
		return nil, nil, err
	}
	tables, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TableInfo, error) {
		var t TableInfo
		err := r.Scan(&t.Schema, &t.Name, &t.Kind, &t.Estimate, &t.Size, &t.Comment)
		return t, err
	})
	if err != nil {
		return nil, nil, err
	}
	rows, err = q.Query(ctx, `SELECT n.nspname FROM pg_namespace n WHERE `+userSchemas+` ORDER BY n.nspname = 'public' DESC, 1`)
	if err != nil {
		return nil, nil, err
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return tables, schemas, err
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (db *DB) tableMeta(ctx context.Context, schema, name string, fresh bool) (*TableMeta, error) {
	key := schema + "\x00" + name
	if !fresh {
		db.metaMu.Lock()
		m := db.metaCache[key]
		db.metaMu.Unlock()
		if m != nil {
			return m, nil
		}
	}
	m, err := loadMeta(ctx, db.pool, schema, name)
	if err != nil {
		return nil, err
	}
	db.metaMu.Lock()
	db.metaCache[key] = m
	db.metaMu.Unlock()
	return m, nil
}

func loadMeta(ctx context.Context, q querier, schema, name string) (*TableMeta, error) {
	m := &TableMeta{Schema: schema, Name: name}
	err := q.QueryRow(ctx, `
		SELECT c.oid, c.relkind::text, c.reltuples::bigint,
		       CASE WHEN c.relkind IN ('r','m','p') THEN pg_total_relation_size(c.oid) END,
		       obj_description(c.oid, 'pg_class'),
		       CASE WHEN c.relkind IN ('v','m') THEN pg_get_viewdef(c.oid, true) END
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, name).
		Scan(&m.OID, &m.Kind, &m.Estimate, &m.Size, &m.Comment, &m.ViewDef)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("table %s.%s does not exist", schema, name)
	}
	if err != nil {
		return nil, err
	}

	rows, err := q.Query(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod),
		       COALESCE(bt.typname, t.typname)::text, COALESCE(bt.typcategory, t.typcategory)::text,
		       NOT a.attnotnull, pg_get_expr(d.adbin, d.adrelid),
		       a.attidentity::text, a.attgenerated::text, col_description(a.attrelid, a.attnum),
		       CASE WHEN COALESCE(bt.typtype, t.typtype) = 'e' THEN
		         (SELECT array_agg(e.enumlabel::text ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid = COALESCE(bt.oid, t.oid))
		       END
		FROM pg_attribute a
		JOIN pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_type bt ON t.typtype = 'd' AND bt.oid = t.typbasetype
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, m.OID)
	if err != nil {
		return nil, err
	}
	m.Columns, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Column, error) {
		var c Column
		err := r.Scan(&c.Name, &c.Type, &c.Udt, &c.Cat, &c.Nullable, &c.Default, &c.Identity, &c.Generated, &c.Comment, &c.Enum)
		return c, err
	})
	if err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `
		SELECT a.attname FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = $1 AND i.indisprimary
		ORDER BY array_position(i.indkey::int2[], a.attnum)`, m.OID)
	if err != nil {
		return nil, err
	}
	if m.PK, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, err
	}
	for _, p := range m.PK {
		if c := m.column(p); c != nil {
			c.PK = true
		}
	}
	switch {
	case len(m.PK) > 0 && (m.Kind == "r" || m.Kind == "p"):
		m.KeyMode = "pk"
	case m.Kind == "r":
		m.KeyMode = "ctid"
	}

	// Single-column foreign keys become per-column links.
	rows, err = q.Query(ctx, `
		SELECT con.conname, cardinality(con.conkey),
		       (SELECT attname FROM pg_attribute WHERE attrelid = con.conrelid AND attnum = con.conkey[1]),
		       fn.nspname, fc.relname,
		       (SELECT attname FROM pg_attribute WHERE attrelid = con.confrelid AND attnum = con.confkey[1])
		FROM pg_constraint con
		JOIN pg_class fc ON fc.oid = con.confrelid
		JOIN pg_namespace fn ON fn.oid = fc.relnamespace
		WHERE con.conrelid = $1 AND con.contype = 'f'`, m.OID)
	if err != nil {
		return nil, err
	}
	type fkRow struct {
		fk   FKRef
		n    int
		attr string
	}
	fks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (fkRow, error) {
		var f fkRow
		err := r.Scan(&f.fk.Name, &f.n, &f.attr, &f.fk.Schema, &f.fk.Table, &f.fk.Column)
		return f, err
	})
	if err != nil {
		return nil, err
	}
	for _, f := range fks {
		if f.n != 1 {
			continue
		}
		if c := m.column(f.attr); c != nil && c.FK == nil {
			fk := f.fk
			c.FK = &fk
		}
	}

	rows, err = q.Query(ctx, `
		SELECT con.conname, n.nspname, c.relname,
		       (SELECT attname FROM pg_attribute WHERE attrelid = con.conrelid AND attnum = con.conkey[1]),
		       (SELECT attname FROM pg_attribute WHERE attrelid = con.confrelid AND attnum = con.confkey[1])
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE con.confrelid = $1 AND con.contype = 'f' AND cardinality(con.conkey) = 1 AND NOT c.relispartition
		ORDER BY 2, 3, 4`, m.OID)
	if err != nil {
		return nil, err
	}
	m.Incoming, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Incoming, error) {
		var in Incoming
		err := r.Scan(&in.Name, &in.Schema, &in.Table, &in.Column, &in.RefColumn)
		return in, err
	})
	if err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `
		SELECT ic.relname, pg_get_indexdef(i.indexrelid), i.indisprimary, i.indisunique, pg_relation_size(i.indexrelid)
		FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
		WHERE i.indrelid = $1 ORDER BY i.indisprimary DESC, ic.relname`, m.OID)
	if err != nil {
		return nil, err
	}
	m.Indexes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Index, error) {
		var ix Index
		err := r.Scan(&ix.Name, &ix.Def, &ix.Primary, &ix.Unique, &ix.Size)
		return ix, err
	})
	if err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `
		SELECT conname, CASE contype WHEN 'p' THEN 'primary key' WHEN 'f' THEN 'foreign key' WHEN 'u' THEN 'unique'
		                             WHEN 'c' THEN 'check' WHEN 'x' THEN 'exclusion' WHEN 'n' THEN 'not null' ELSE contype::text END,
		       pg_get_constraintdef(oid, true)
		FROM pg_constraint WHERE conrelid = $1 AND contype <> 'n'
		ORDER BY array_position(ARRAY['p','u','f','c','x']::"char"[], contype), conname`, m.OID)
	if err != nil {
		return nil, err
	}
	m.Constraints, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Constraint, error) {
		var c Constraint
		err := r.Scan(&c.Name, &c.Type, &c.Def)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	if m.Columns == nil {
		m.Columns = []Column{}
	}
	return m, nil
}
