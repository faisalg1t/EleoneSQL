package engine

import (
	"bytes"
	"fmt"

	"github.com/faisaljs/EleoneSQL/internal/catalog"
	"github.com/faisaljs/EleoneSQL/internal/sqlparser"
	"github.com/faisaljs/EleoneSQL/internal/storage"
)

// DDL statements are not WAL-logged individually (see internal/txn docs);
// instead each one fsyncs the data file directly before returning, so a
// successful CREATE/DROP TABLE is durable immediately.

func (s *Session) execCreateTable(st *sqlparser.CreateTableStmt) (*Result, error) {
	if _, exists := s.Store.Catalog.Table(st.Table); exists {
		return nil, fmt.Errorf("table %q already exists", st.Table)
	}
	cols := make([]catalog.ColumnDef, len(st.Columns))
	pkCount := 0
	for i, c := range st.Columns {
		t, err := catalog.ParseType(c.Type)
		if err != nil {
			return nil, err
		}
		cols[i] = catalog.ColumnDef{
			Name:       c.Name,
			Type:       t,
			PrimaryKey: c.PrimaryKey,
			Unique:     c.Unique,
			NotNull:    c.NotNull,
		}
		if c.PrimaryKey {
			pkCount++
		}
	}
	if pkCount > 1 {
		return nil, fmt.Errorf("engine: only a single-column PRIMARY KEY is supported")
	}
	td, err := s.Store.Catalog.CreateTable(st.Table, cols)
	if err != nil {
		return nil, err
	}
	// A declared PRIMARY KEY or UNIQUE column gets a backing index so
	// uniqueness can be enforced and equality lookups are fast.
	for _, c := range cols {
		if c.PrimaryKey || c.Unique {
			if _, err := s.createIndexFor(td, c.Name, "idx_"+td.Name+"_"+c.Name, true); err != nil {
				return nil, err
			}
		}
	}
	if err := s.Store.Pager.Sync(); err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("CREATE TABLE %s", st.Table)}, nil
}

func (s *Session) execDropTable(st *sqlparser.DropTableStmt) (*Result, error) {
	if err := s.Store.Catalog.DropTable(st.Table); err != nil {
		return nil, err
	}
	if err := s.Store.Pager.Sync(); err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("DROP TABLE %s", st.Table)}, nil
}

func (s *Session) execCreateIndex(st *sqlparser.CreateIndexStmt) (*Result, error) {
	td, ok := s.Store.Catalog.Table(st.Table)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", st.Table)
	}
	if td.ColumnIndex(st.Column) < 0 {
		return nil, fmt.Errorf("column %q does not exist on table %q", st.Column, st.Table)
	}
	for _, idx := range td.Indexes {
		if idx.Name == st.Name {
			return nil, fmt.Errorf("index %q already exists", st.Name)
		}
	}
	if _, err := s.createIndexFor(td, st.Column, st.Name, st.Unique); err != nil {
		return nil, err
	}
	if err := s.Store.Pager.Sync(); err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("CREATE INDEX %s", st.Name)}, nil
}

// createIndexFor builds a secondary index over an existing (possibly
// non-empty) table and registers it in the catalog.
func (s *Session) createIndexFor(td *catalog.TableDef, column, indexName string, unique bool) (*catalog.IndexDef, error) {
	colIdx := td.ColumnIndex(column)
	if colIdx < 0 {
		return nil, fmt.Errorf("column %q does not exist", column)
	}
	ibt, err := storage.OpenBTree(s.Store.Pager, 0)
	if err != nil {
		return nil, err
	}

	// Backfill from existing rows.
	rowbt, err := storage.OpenBTree(s.Store.Pager, td.Root)
	if err != nil {
		return nil, err
	}
	cur, err := rowbt.NewCursor(nil)
	if err != nil {
		return nil, err
	}
	types := td.ColumnTypes()
	for {
		k, v, ok, err := cur.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		vals, err := catalog.DecodeRow(v, types)
		if err != nil {
			return nil, err
		}
		ikey := indexKey(vals[colIdx], k)
		if unique {
			if dup, err := hasIndexEntryForValue(ibt, vals[colIdx]); err != nil {
				return nil, err
			} else if dup {
				return nil, fmt.Errorf("engine: duplicate value violates unique constraint on %q", column)
			}
		}
		if err := ibt.Put(ikey, k); err != nil {
			return nil, err
		}
	}

	def := catalog.IndexDef{Name: indexName, Column: column, Unique: unique, Root: ibt.Root()}
	td.Indexes = append(td.Indexes, def)
	if err := s.Store.Catalog.SaveTable(td); err != nil {
		return nil, err
	}
	return &def, nil
}

// indexKey builds a secondary-index key: the value's order-preserving
// sort key followed by the row id, so entries with equal values remain
// individually addressable and sort together for range/equality scans.
func indexKey(v catalog.Value, rowKey []byte) []byte {
	sk := v.SortKey()
	out := make([]byte, len(sk)+len(rowKey))
	copy(out, sk)
	copy(out[len(sk):], rowKey)
	return out
}

func hasIndexEntryForValue(ibt *storage.BTree, v catalog.Value) (bool, error) {
	prefix := v.SortKey()
	cur, err := ibt.NewCursor(prefix)
	if err != nil {
		return false, err
	}
	k, _, ok, err := cur.Next()
	if err != nil || !ok {
		return false, err
	}
	return bytes.HasPrefix(k, prefix), nil
}

func (s *Session) execShowTables() (*Result, error) {
	names := s.Store.Catalog.TableNames()
	rows := make([][]catalog.Value, len(names))
	for i, n := range names {
		rows[i] = []catalog.Value{catalog.TextValue(n)}
	}
	return &Result{Columns: []string{"table"}, Rows: rows}, nil
}
