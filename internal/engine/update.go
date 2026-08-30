package engine

import (
	"fmt"

	"github.com/faisaljs/EleoneSQL/internal/catalog"
	"github.com/faisaljs/EleoneSQL/internal/sqlparser"
	"github.com/faisaljs/EleoneSQL/internal/txn"
)

func (s *Session) execUpdate(t *txn.Txn, st *sqlparser.UpdateStmt) (*Result, error) {
	td, ok := s.Store.Catalog.Table(st.Table)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", st.Table)
	}
	keys, rows, err := loadRows(t, td, st.Where, st.Table)
	if err != nil {
		return nil, err
	}

	assignPos := make([]int, len(st.Set))
	for i, a := range st.Set {
		idx := td.ColumnIndex(a.Column)
		if idx < 0 {
			return nil, fmt.Errorf("column %q does not exist on table %q", a.Column, st.Table)
		}
		assignPos[i] = idx
	}

	count := 0
	for i, key := range keys {
		tp := Tuple{{Alias: st.Table, Table: td, RowKey: key, Values: rows[i]}}
		if st.Where != nil {
			v, err := eval(st.Where, tp)
			if err != nil {
				return nil, err
			}
			if !v.Truthy() {
				continue
			}
		}

		newVals := append([]catalog.Value(nil), rows[i]...)
		for j, a := range st.Set {
			v, err := eval(a.Value, tp)
			if err != nil {
				return nil, err
			}
			col := td.Columns[assignPos[j]]
			cv, err := coerce(v, col.Type)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", col.Name, err)
			}
			newVals[assignPos[j]] = cv
		}
		for ci, col := range td.Columns {
			if col.NotNull && newVals[ci].Null {
				return nil, fmt.Errorf("column %q may not be NULL", col.Name)
			}
		}

		for _, idx := range td.Indexes {
			colIdx := td.ColumnIndex(idx.Column)
			oldV, newV := rows[i][colIdx], newVals[colIdx]
			if catalog.Compare(oldV, newV) == 0 {
				continue
			}
			if idx.Unique && !newV.Null {
				if err := s.checkUniqueViaTxn(t, td, idx, newV); err != nil {
					return nil, err
				}
			}
			oldKey := indexKey(oldV, key)
			if err := t.Delete(txn.IndexTarget(td.Name, idx.Name), oldKey); err != nil {
				return nil, err
			}
			newKey := indexKey(newV, key)
			if err := t.Put(txn.IndexTarget(td.Name, idx.Name), newKey, key); err != nil {
				return nil, err
			}
		}

		if err := t.Put(txn.TableTarget(td.Name), key, catalog.EncodeRow(newVals)); err != nil {
			return nil, err
		}
		count++
	}
	return &Result{Message: "UPDATE", RowsAffected: count}, nil
}
