package engine

import (
	"fmt"

	"github.com/faisaljs/EleoneSQL/internal/sqlparser"
	"github.com/faisaljs/EleoneSQL/internal/txn"
)

func (s *Session) execDelete(t *txn.Txn, st *sqlparser.DeleteStmt) (*Result, error) {
	td, ok := s.Store.Catalog.Table(st.Table)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", st.Table)
	}
	keys, rows, err := loadRows(t, td, st.Where, st.Table)
	if err != nil {
		return nil, err
	}

	count := 0
	for i, key := range keys {
		if st.Where != nil {
			tp := Tuple{{Alias: st.Table, Table: td, RowKey: key, Values: rows[i]}}
			v, err := eval(st.Where, tp)
			if err != nil {
				return nil, err
			}
			if !v.Truthy() {
				continue
			}
		}
		for _, idx := range td.Indexes {
			colIdx := td.ColumnIndex(idx.Column)
			ikey := indexKey(rows[i][colIdx], key)
			if err := t.Delete(txn.IndexTarget(td.Name, idx.Name), ikey); err != nil {
				return nil, err
			}
		}
		if err := t.Delete(txn.TableTarget(td.Name), key); err != nil {
			return nil, err
		}
		count++
	}
	return &Result{Message: "DELETE", RowsAffected: count}, nil
}
