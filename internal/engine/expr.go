package engine

import (
	"fmt"

	"github.com/faisaljs/EleoneSQL/internal/catalog"
	"github.com/faisaljs/EleoneSQL/internal/sqlparser"
)

// Binding is one table's contribution to the current row during execution
// (a plain scan has one binding; a JOIN has one per joined table).
type Binding struct {
	Alias  string
	Table  *catalog.TableDef
	RowKey []byte
	Values []catalog.Value
}

// Tuple is the current row being evaluated, possibly assembled from
// several joined tables.
type Tuple []Binding

func (tp Tuple) resolve(ref *sqlparser.ColumnRef) (catalog.Value, error) {
	var found *catalog.Value
	var foundIn string
	for _, b := range tp {
		if ref.Table != "" && ref.Table != b.Alias {
			continue
		}
		idx := b.Table.ColumnIndex(ref.Name)
		if idx < 0 {
			continue
		}
		if found != nil {
			return catalog.Value{}, fmt.Errorf("engine: column %q is ambiguous", ref.Name)
		}
		v := b.Values[idx]
		found = &v
		foundIn = b.Alias
	}
	if found == nil {
		return catalog.Value{}, fmt.Errorf("engine: unknown column %q", ref.Name)
	}
	_ = foundIn
	return *found, nil
}

// eval evaluates expr against the current tuple.
func eval(expr sqlparser.Expr, tp Tuple) (catalog.Value, error) {
	switch e := expr.(type) {
	case *sqlparser.LiteralExpr:
		switch e.Kind {
		case "int":
			return catalog.IntValue(e.I), nil
		case "float":
			return catalog.FloatValue(e.F), nil
		case "string":
			return catalog.TextValue(e.S), nil
		case "bool":
			return catalog.BoolValue(e.B), nil
		case "null":
			return catalog.NullValue(catalog.TypeText), nil
		}
		return catalog.Value{}, fmt.Errorf("engine: bad literal kind %q", e.Kind)

	case *sqlparser.ColumnRef:
		return tp.resolve(e)

	case *sqlparser.UnaryExpr:
		v, err := eval(e.Expr, tp)
		if err != nil {
			return catalog.Value{}, err
		}
		switch e.Op {
		case "NOT":
			return catalog.BoolValue(!v.Truthy()), nil
		case "-":
			if v.Type == catalog.TypeFloat {
				return catalog.FloatValue(-v.F), nil
			}
			return catalog.IntValue(-v.I), nil
		}
		return catalog.Value{}, fmt.Errorf("engine: unknown unary op %q", e.Op)

	case *sqlparser.BinaryExpr:
		return evalBinary(e, tp)

	case *sqlparser.CountStarExpr:
		return catalog.Value{}, fmt.Errorf("engine: COUNT(*) may only appear in the select list")

	default:
		return catalog.Value{}, fmt.Errorf("engine: unsupported expression %T", expr)
	}
}

func evalBinary(e *sqlparser.BinaryExpr, tp Tuple) (catalog.Value, error) {
	if e.Op == "AND" || e.Op == "OR" {
		l, err := eval(e.Left, tp)
		if err != nil {
			return catalog.Value{}, err
		}
		if e.Op == "AND" && !l.Truthy() {
			return catalog.BoolValue(false), nil
		}
		if e.Op == "OR" && l.Truthy() {
			return catalog.BoolValue(true), nil
		}
		r, err := eval(e.Right, tp)
		if err != nil {
			return catalog.Value{}, err
		}
		return catalog.BoolValue(r.Truthy()), nil
	}

	l, err := eval(e.Left, tp)
	if err != nil {
		return catalog.Value{}, err
	}
	r, err := eval(e.Right, tp)
	if err != nil {
		return catalog.Value{}, err
	}

	switch e.Op {
	case "=":
		if l.Null || r.Null {
			return catalog.NullValue(catalog.TypeBoolean), nil
		}
		return catalog.BoolValue(catalog.Compare(l, r) == 0), nil
	case "<>":
		if l.Null || r.Null {
			return catalog.NullValue(catalog.TypeBoolean), nil
		}
		return catalog.BoolValue(catalog.Compare(l, r) != 0), nil
	case "<":
		if l.Null || r.Null {
			return catalog.NullValue(catalog.TypeBoolean), nil
		}
		return catalog.BoolValue(catalog.Compare(l, r) < 0), nil
	case "<=":
		if l.Null || r.Null {
			return catalog.NullValue(catalog.TypeBoolean), nil
		}
		return catalog.BoolValue(catalog.Compare(l, r) <= 0), nil
	case ">":
		if l.Null || r.Null {
			return catalog.NullValue(catalog.TypeBoolean), nil
		}
		return catalog.BoolValue(catalog.Compare(l, r) > 0), nil
	case ">=":
		if l.Null || r.Null {
			return catalog.NullValue(catalog.TypeBoolean), nil
		}
		return catalog.BoolValue(catalog.Compare(l, r) >= 0), nil
	case "+", "-", "*", "/":
		return arith(e.Op, l, r)
	}
	return catalog.Value{}, fmt.Errorf("engine: unknown operator %q", e.Op)
}

func arith(op string, l, r catalog.Value) (catalog.Value, error) {
	if l.Null || r.Null {
		return catalog.NullValue(catalog.TypeFloat), nil
	}
	if l.Type == catalog.TypeFloat || r.Type == catalog.TypeFloat {
		lf, rf := toFloat(l), toFloat(r)
		switch op {
		case "+":
			return catalog.FloatValue(lf + rf), nil
		case "-":
			return catalog.FloatValue(lf - rf), nil
		case "*":
			return catalog.FloatValue(lf * rf), nil
		case "/":
			if rf == 0 {
				return catalog.Value{}, fmt.Errorf("engine: division by zero")
			}
			return catalog.FloatValue(lf / rf), nil
		}
	}
	li, ri := l.I, r.I
	switch op {
	case "+":
		return catalog.IntValue(li + ri), nil
	case "-":
		return catalog.IntValue(li - ri), nil
	case "*":
		return catalog.IntValue(li * ri), nil
	case "/":
		if ri == 0 {
			return catalog.Value{}, fmt.Errorf("engine: division by zero")
		}
		return catalog.IntValue(li / ri), nil
	}
	return catalog.Value{}, fmt.Errorf("engine: unknown arithmetic op %q", op)
}

func toFloat(v catalog.Value) float64 {
	if v.Type == catalog.TypeFloat {
		return v.F
	}
	return float64(v.I)
}

// coerce converts a literal-derived Value to match a column's declared
// type where that's an unambiguous, lossless conversion (int literal into
// a FLOAT column, for example).
func coerce(v catalog.Value, want catalog.Type) (catalog.Value, error) {
	if v.Null {
		return catalog.NullValue(want), nil
	}
	if v.Type == want {
		return v, nil
	}
	if v.Type == catalog.TypeInteger && want == catalog.TypeFloat {
		return catalog.FloatValue(float64(v.I)), nil
	}
	return catalog.Value{}, fmt.Errorf("engine: cannot use a %s value where %s is expected", v.Type, want)
}
