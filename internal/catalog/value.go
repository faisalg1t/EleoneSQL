package catalog

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/faisalg1t/EleoneSQL/internal/util"
)

// Type is a column's SQL data type.
type Type byte

const (
	TypeInteger Type = iota + 1
	TypeFloat
	TypeText
	TypeBoolean
)

func (t Type) String() string {
	switch t {
	case TypeInteger:
		return "INTEGER"
	case TypeFloat:
		return "FLOAT"
	case TypeText:
		return "TEXT"
	case TypeBoolean:
		return "BOOLEAN"
	default:
		return "UNKNOWN"
	}
}

// ParseType maps a SQL type keyword to a Type.
func ParseType(s string) (Type, error) {
	switch strings.ToUpper(s) {
	case "INT", "INTEGER", "BIGINT", "SMALLINT":
		return TypeInteger, nil
	case "FLOAT", "DOUBLE", "REAL", "DECIMAL", "NUMERIC":
		return TypeFloat, nil
	case "TEXT", "VARCHAR", "CHAR", "STRING":
		return TypeText, nil
	case "BOOL", "BOOLEAN":
		return TypeBoolean, nil
	default:
		return 0, fmt.Errorf("catalog: unknown type %q", s)
	}
}

// Value is a runtime SQL value of any supported type, or SQL NULL.
type Value struct {
	Type Type
	Null bool
	I    int64
	F    float64
	S    string
	B    bool
}

func NullValue(t Type) Value     { return Value{Type: t, Null: true} }
func IntValue(v int64) Value     { return Value{Type: TypeInteger, I: v} }
func FloatValue(v float64) Value { return Value{Type: TypeFloat, F: v} }
func TextValue(v string) Value   { return Value{Type: TypeText, S: v} }
func BoolValue(v bool) Value     { return Value{Type: TypeBoolean, B: v} }

// String renders the value the way it should print in query results.
func (v Value) String() string {
	if v.Null {
		return "NULL"
	}
	switch v.Type {
	case TypeInteger:
		return strconv.FormatInt(v.I, 10)
	case TypeFloat:
		return strconv.FormatFloat(v.F, 'g', -1, 64)
	case TypeText:
		return v.S
	case TypeBoolean:
		if v.B {
			return "true"
		}
		return "false"
	}
	return "?"
}

// SortKey returns an order-preserving byte encoding of v, used both for
// index keys and for ORDER BY comparisons.
func (v Value) SortKey() []byte {
	if v.Null {
		return []byte{0x00}
	}
	var body []byte
	switch v.Type {
	case TypeInteger:
		body = util.EncodeInt64(v.I)
	case TypeFloat:
		body = util.EncodeFloat64(v.F)
	case TypeText:
		body = []byte(v.S)
	case TypeBoolean:
		if v.B {
			body = []byte{1}
		} else {
			body = []byte{0}
		}
	}
	out := make([]byte, 0, len(body)+1)
	out = append(out, 0x01) // non-null marker; sorts after NULL (0x00)
	out = append(out, body...)
	return out
}

// Compare orders a against b (NULL sorts first). Only meaningful for
// same-typed values; callers coerce beforehand.
func Compare(a, b Value) int {
	if a.Null && b.Null {
		return 0
	}
	if a.Null {
		return -1
	}
	if b.Null {
		return 1
	}
	switch a.Type {
	case TypeInteger:
		bv := b.I
		if b.Type == TypeFloat {
			return compareFloat(float64(a.I), b.F)
		}
		if a.I < bv {
			return -1
		} else if a.I > bv {
			return 1
		}
		return 0
	case TypeFloat:
		bf := b.F
		if b.Type == TypeInteger {
			bf = float64(b.I)
		}
		return compareFloat(a.F, bf)
	case TypeText:
		return strings.Compare(a.S, b.S)
	case TypeBoolean:
		if a.B == b.B {
			return 0
		}
		if !a.B {
			return -1
		}
		return 1
	}
	return 0
}

func compareFloat(a, b float64) int {
	if a < b {
		return -1
	} else if a > b {
		return 1
	}
	return 0
}

// Truthy reports whether v counts as true in a boolean context (WHERE
// clause). NULL and zero-ish values are false.
func (v Value) Truthy() bool {
	if v.Null {
		return false
	}
	switch v.Type {
	case TypeBoolean:
		return v.B
	case TypeInteger:
		return v.I != 0
	case TypeFloat:
		return v.F != 0
	case TypeText:
		return v.S != ""
	}
	return false
}

// EncodeRow serializes a row (one Value per column, in schema order) into
// bytes suitable for storage as a b-tree leaf value.
func EncodeRow(values []Value) []byte {
	// Null bitmap first, one byte per column for simplicity/robustness.
	buf := make([]byte, 0, 32)
	nulls := make([]byte, len(values))
	for i, v := range values {
		if v.Null {
			nulls[i] = 1
		}
	}
	buf = append(buf, nulls...)
	for _, v := range values {
		if v.Null {
			continue
		}
		switch v.Type {
		case TypeInteger:
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], uint64(v.I))
			buf = append(buf, b[:]...)
		case TypeFloat:
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], math.Float64bits(v.F))
			buf = append(buf, b[:]...)
		case TypeText:
			var lb [4]byte
			binary.BigEndian.PutUint32(lb[:], uint32(len(v.S)))
			buf = append(buf, lb[:]...)
			buf = append(buf, v.S...)
		case TypeBoolean:
			if v.B {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
		}
	}
	return buf
}

// DecodeRow deserializes bytes produced by EncodeRow given the column types.
func DecodeRow(data []byte, types []Type) ([]Value, error) {
	n := len(types)
	if len(data) < n {
		return nil, fmt.Errorf("catalog: row data too short for %d columns", n)
	}
	nulls := data[:n]
	off := n
	out := make([]Value, n)
	for i, t := range types {
		if nulls[i] == 1 {
			out[i] = NullValue(t)
			continue
		}
		switch t {
		case TypeInteger:
			if off+8 > len(data) {
				return nil, fmt.Errorf("catalog: truncated int column %d", i)
			}
			out[i] = IntValue(int64(binary.BigEndian.Uint64(data[off:])))
			off += 8
		case TypeFloat:
			if off+8 > len(data) {
				return nil, fmt.Errorf("catalog: truncated float column %d", i)
			}
			out[i] = FloatValue(math.Float64frombits(binary.BigEndian.Uint64(data[off:])))
			off += 8
		case TypeText:
			if off+4 > len(data) {
				return nil, fmt.Errorf("catalog: truncated text length column %d", i)
			}
			l := int(binary.BigEndian.Uint32(data[off:]))
			off += 4
			if off+l > len(data) {
				return nil, fmt.Errorf("catalog: truncated text column %d", i)
			}
			out[i] = TextValue(string(data[off : off+l]))
			off += l
		case TypeBoolean:
			if off+1 > len(data) {
				return nil, fmt.Errorf("catalog: truncated bool column %d", i)
			}
			out[i] = BoolValue(data[off] == 1)
			off++
		}
	}
	return out, nil
}
