package engine

import (
	"fmt"
	"sort"

	"github.com/faisalg1t/EleoneSQL/internal/catalog"
	"github.com/faisalg1t/EleoneSQL/internal/sqlparser"
	"github.com/faisalg1t/EleoneSQL/internal/txn"
)

func (s *Session) execSelect(t *txn.Txn, st *sqlparser.SelectStmt) (*Result, error) {
	td, ok := s.Store.Catalog.Table(st.Table)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", st.Table)
	}

	// Only push WHERE down into an index seek when there's no JOIN (once
	// joined, a single-column equality no longer determines the base
	// table's contribution to the result on its own).
	var baseWhere sqlparser.Expr
	if len(st.Joins) == 0 {
		baseWhere = st.Where
	}
	_, rows, err := loadRows(t, td, baseWhere, st.Alias)
	if err != nil {
		return nil, err
	}

	tuples := make([]Tuple, len(rows))
	for i, vals := range rows {
		tuples[i] = Tuple{{Alias: st.Alias, Table: td, Values: vals}}
	}

	for _, j := range st.Joins {
		jtd, ok := s.Store.Catalog.Table(j.Table)
		if !ok {
			return nil, fmt.Errorf("table %q does not exist", j.Table)
		}
		_, jrows, err := fetchAllRows(t, jtd)
		if err != nil {
			return nil, err
		}
		var next []Tuple
		for _, left := range tuples {
			for _, jvals := range jrows {
				candidate := append(append(Tuple{}, left...), Binding{Alias: j.Alias, Table: jtd, Values: jvals})
				v, err := eval(j.On, candidate)
				if err != nil {
					return nil, err
				}
				if v.Truthy() {
					next = append(next, candidate)
				}
			}
		}
		tuples = next
	}

	// Always re-apply WHERE against the assembled tuples: it's a no-op
	// (every row already matches) when loadRows took the index-seek fast
	// path, and it's the only filtering step when that path wasn't used
	// or a JOIN is involved.
	if st.Where != nil {
		tuples = filterTuples(tuples, st.Where)
	}

	// Aggregate: COUNT(*) as the sole select item.
	if len(st.Items) == 1 {
		if _, isCount := st.Items[0].Expr.(*sqlparser.CountStarExpr); isCount {
			return &Result{
				Columns: []string{st.Items[0].Alias},
				Rows:    [][]catalog.Value{{catalog.IntValue(int64(len(tuples)))}},
			}, nil
		}
	}

	if len(st.OrderBy) > 0 {
		sortTuples(tuples, st.OrderBy)
	}
	if st.HasLim && st.Limit < len(tuples) {
		tuples = tuples[:st.Limit]
	}

	cols, err := s.selectColumns(st, td)
	if err != nil {
		return nil, err
	}
	outRows := make([][]catalog.Value, len(tuples))
	for i, tp := range tuples {
		row, err := projectTuple(st, tp)
		if err != nil {
			return nil, err
		}
		outRows[i] = row
	}
	return &Result{Columns: cols, Rows: outRows}, nil
}

func filterTuples(tuples []Tuple, where sqlparser.Expr) []Tuple {
	var out []Tuple
	for _, tp := range tuples {
		v, err := eval(where, tp)
		if err == nil && v.Truthy() {
			out = append(out, tp)
		}
	}
	return out
}

func sortTuples(tuples []Tuple, terms []sqlparser.OrderTerm) {
	sort.SliceStable(tuples, func(i, j int) bool {
		for _, term := range terms {
			vi, erri := eval(term.Expr, tuples[i])
			vj, errj := eval(term.Expr, tuples[j])
			if erri != nil || errj != nil {
				continue
			}
			c := catalog.Compare(vi, vj)
			if c == 0 {
				continue
			}
			if term.Desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
}

func (s *Session) selectColumns(st *sqlparser.SelectStmt, td *catalog.TableDef) ([]string, error) {
	if st.Star {
		multi := len(st.Joins) > 0
		cols := make([]string, 0, len(td.Columns))
		for _, c := range td.Columns {
			if multi {
				cols = append(cols, st.Alias+"."+c.Name)
			} else {
				cols = append(cols, c.Name)
			}
		}
		for _, j := range st.Joins {
			jtd, ok := s.Store.Catalog.Table(j.Table)
			if !ok {
				return nil, fmt.Errorf("table %q does not exist", j.Table)
			}
			for _, c := range jtd.Columns {
				cols = append(cols, j.Alias+"."+c.Name)
			}
		}
		return cols, nil
	}
	cols := make([]string, len(st.Items))
	for i, item := range st.Items {
		if item.Alias != "" {
			cols[i] = item.Alias
		} else {
			cols[i] = fmt.Sprintf("col%d", i+1)
		}
	}
	return cols, nil
}

func projectTuple(st *sqlparser.SelectStmt, tp Tuple) ([]catalog.Value, error) {
	if st.Star {
		var out []catalog.Value
		for _, b := range tp {
			out = append(out, b.Values...)
		}
		return out, nil
	}
	out := make([]catalog.Value, len(st.Items))
	for i, item := range st.Items {
		v, err := eval(item.Expr, tp)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
