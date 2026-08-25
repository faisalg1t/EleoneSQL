package engine

import (
	"github.com/faisalg1t/EleoneSQL/internal/catalog"
	"github.com/faisalg1t/EleoneSQL/internal/sqlparser"
	"github.com/faisalg1t/EleoneSQL/internal/txn"
)

// fetchAllRows materializes every row of a table as parallel slices of row
// keys and decoded values. EleoneSQL's query engine works over fully
// materialized row sets rather than streaming cursors through the whole
// pipeline — simpler to get right, at the cost of memory scaling with
// table size; see the README roadmap for streaming execution.
func fetchAllRows(t *txn.Txn, td *catalog.TableDef) ([][]byte, [][]catalog.Value, error) {
	cur, err := t.Cursor(txn.TableTarget(td.Name), nil)
	if err != nil {
		return nil, nil, err
	}
	types := td.ColumnTypes()
	var keys [][]byte
	var rows [][]catalog.Value
	for {
		k, v, ok, err := cur.Next()
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			break
		}
		vals, err := catalog.DecodeRow(v, types)
		if err != nil {
			return nil, nil, err
		}
		keys = append(keys, k)
		rows = append(rows, vals)
	}
	return keys, rows, nil
}

// findEqualityIndex looks for a top-level `column = literal` WHERE clause
// that matches one of td's indexes, so callers can do an index seek instead
// of a full scan. Returns ok=false if no such fast path applies.
func findEqualityIndex(td *catalog.TableDef, where sqlparser.Expr, alias string) (idx catalog.IndexDef, lit catalog.Value, ok bool) {
	be, isBin := where.(*sqlparser.BinaryExpr)
	if !isBin || be.Op != "=" {
		return
	}
	col, litExpr, matched := matchColumnLiteral(be.Left, be.Right, alias)
	if !matched {
		col, litExpr, matched = matchColumnLiteral(be.Right, be.Left, alias)
	}
	if !matched {
		return
	}
	for _, ix := range td.Indexes {
		if ix.Column == col {
			v, err := literalValue(litExpr)
			if err != nil {
				return catalog.IndexDef{}, catalog.Value{}, false
			}
			colType := td.Columns[td.ColumnIndex(col)].Type
			cv, err := coerce(v, colType)
			if err != nil {
				return catalog.IndexDef{}, catalog.Value{}, false
			}
			return ix, cv, true
		}
	}
	return
}

func matchColumnLiteral(a, b sqlparser.Expr, alias string) (col string, lit sqlparser.Expr, ok bool) {
	cr, isCol := a.(*sqlparser.ColumnRef)
	if !isCol {
		return "", nil, false
	}
	if cr.Table != "" && cr.Table != alias {
		return "", nil, false
	}
	if _, isLit := b.(*sqlparser.LiteralExpr); !isLit {
		return "", nil, false
	}
	return cr.Name, b, true
}

func literalValue(e sqlparser.Expr) (catalog.Value, error) {
	return eval(e, nil)
}

// indexEqualityLookup returns the row keys whose indexed column equals v.
func indexEqualityLookup(t *txn.Txn, td *catalog.TableDef, idx catalog.IndexDef, v catalog.Value) ([][]byte, error) {
	prefix := v.SortKey()
	cur, err := t.Cursor(txn.IndexTarget(td.Name, idx.Name), prefix)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for {
		k, rowKey, ok, err := cur.Next()
		if err != nil {
			return nil, err
		}
		if !ok || !hasPrefix(k, prefix) {
			break
		}
		out = append(out, append([]byte(nil), rowKey...))
	}
	return out, nil
}

// loadRows returns keys/values for td, using an index seek when the WHERE
// clause allows it and falling back to a full scan otherwise.
func loadRows(t *txn.Txn, td *catalog.TableDef, where sqlparser.Expr, alias string) ([][]byte, [][]catalog.Value, error) {
	if where != nil {
		if idx, lit, ok := findEqualityIndex(td, where, alias); ok {
			rowKeys, err := indexEqualityLookup(t, td, idx, lit)
			if err != nil {
				return nil, nil, err
			}
			types := td.ColumnTypes()
			rows := make([][]catalog.Value, 0, len(rowKeys))
			keys := make([][]byte, 0, len(rowKeys))
			for _, rk := range rowKeys {
				raw, err := t.Get(txn.TableTarget(td.Name), rk)
				if err != nil {
					continue // stale index entry from a since-rolled-back txn path; skip
				}
				vals, err := catalog.DecodeRow(raw, types)
				if err != nil {
					return nil, nil, err
				}
				keys = append(keys, rk)
				rows = append(rows, vals)
			}
			return keys, rows, nil
		}
	}
	return fetchAllRows(t, td)
}
