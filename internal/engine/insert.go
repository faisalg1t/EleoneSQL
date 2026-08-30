package engine

import (
	"fmt"

	"github.com/faisaljs/EleoneSQL/internal/catalog"
	"github.com/faisaljs/EleoneSQL/internal/sqlparser"
	"github.com/faisaljs/EleoneSQL/internal/txn"
	"github.com/faisaljs/EleoneSQL/internal/util"
)

func (s *Session) execInsert(t *txn.Txn, st *sqlparser.InsertStmt) (*Result, error) {
	td, ok := s.Store.Catalog.Table(st.Table)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", st.Table)
	}

	// Map declared (or implicit, all-columns) insert columns to schema
	// column positions.
	targetCols := st.Columns
	if len(targetCols) == 0 {
		targetCols = make([]string, len(td.Columns))
		for i, c := range td.Columns {
			targetCols[i] = c.Name
		}
	}
	positions := make([]int, len(targetCols))
	for i, name := range targetCols {
		idx := td.ColumnIndex(name)
		if idx < 0 {
			return nil, fmt.Errorf("column %q does not exist on table %q", name, st.Table)
		}
		positions[i] = idx
	}

	count := 0
	for _, rowExprs := range st.Rows {
		if len(rowExprs) != len(targetCols) {
			return nil, fmt.Errorf("engine: expected %d values, got %d", len(targetCols), len(rowExprs))
		}
		values := make([]catalog.Value, len(td.Columns))
		for i := range values {
			values[i] = catalog.NullValue(td.Columns[i].Type)
		}
		for i, e := range rowExprs {
			v, err := eval(e, nil)
			if err != nil {
				return nil, err
			}
			col := td.Columns[positions[i]]
			cv, err := coerce(v, col.Type)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", col.Name, err)
			}
			values[positions[i]] = cv
		}
		for i, col := range td.Columns {
			if col.NotNull && values[i].Null {
				return nil, fmt.Errorf("column %q may not be NULL", col.Name)
			}
		}

		rowID := td.NextRow
		td.NextRow++
		if err := s.Store.Catalog.SaveTable(td); err != nil {
			return nil, err
		}
		rowKey := util.EncodeUint64(rowID)

		for _, idx := range td.Indexes {
			colIdx := td.ColumnIndex(idx.Column)
			v := values[colIdx]
			if idx.Unique && !v.Null {
				if err := s.checkUniqueViaTxn(t, td, idx, v); err != nil {
					return nil, err
				}
			}
			ikey := indexKey(v, rowKey)
			if err := t.Put(txn.IndexTarget(td.Name, idx.Name), ikey, rowKey); err != nil {
				return nil, err
			}
		}

		rowBytes := catalog.EncodeRow(values)
		if err := t.Put(txn.TableTarget(td.Name), rowKey, rowBytes); err != nil {
			return nil, err
		}
		count++
	}
	return &Result{Message: "INSERT", RowsAffected: count}, nil
}

func (s *Session) checkUniqueViaTxn(t *txn.Txn, td *catalog.TableDef, idx catalog.IndexDef, v catalog.Value) error {
	prefix := v.SortKey()
	cur, err := t.Cursor(txn.IndexTarget(td.Name, idx.Name), prefix)
	if err != nil {
		return err
	}
	k, _, ok, err := cur.Next()
	if err != nil {
		return err
	}
	if ok && hasPrefix(k, prefix) {
		return fmt.Errorf("engine: duplicate value violates unique constraint on %q", idx.Column)
	}
	return nil
}

func hasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}
