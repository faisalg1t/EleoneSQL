// Package util holds small, dependency-free helpers shared across
// EleoneSQL's internal packages, chiefly the byte-order-preserving value
// codec used both for row storage and for index keys (so that a plain
// bytes.Compare on encoded values matches SQL ordering semantics).
package util

import (
	"encoding/binary"
	"math"
)

// EncodeInt64 returns a big-endian, sign-flipped encoding of v such that
// byte-wise comparison of the result matches numeric comparison of v.
func EncodeInt64(v int64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(v)^(1<<63))
	return buf
}

// DecodeInt64 reverses EncodeInt64.
func DecodeInt64(b []byte) int64 {
	u := binary.BigEndian.Uint64(b)
	return int64(u ^ (1 << 63))
}

// EncodeFloat64 returns an order-preserving encoding of an IEEE-754 float.
func EncodeFloat64(v float64) []byte {
	bits := math.Float64bits(v)
	if v >= 0 {
		bits ^= 1 << 63
	} else {
		bits = ^bits
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, bits)
	return buf
}

// DecodeFloat64 reverses EncodeFloat64.
func DecodeFloat64(b []byte) float64 {
	bits := binary.BigEndian.Uint64(b)
	if bits&(1<<63) != 0 {
		bits ^= 1 << 63
	} else {
		bits = ^bits
	}
	return math.Float64frombits(bits)
}

// EncodeUint64 is a plain big-endian encoding, used for row IDs.
func EncodeUint64(v uint64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, v)
	return buf
}

// DecodeUint64 reverses EncodeUint64.
func DecodeUint64(b []byte) uint64 {
	return binary.BigEndian.Uint64(b)
}
