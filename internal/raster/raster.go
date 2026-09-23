// Package raster holds the small types shared by the reader, the writer and
// the conversion engine: sample types, grids and number formatting.
package raster

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// PixType is a pixel sample type.
type PixType int

const (
	Uint8 PixType = iota
	Int8
	Int16
	Uint16
	Int32
	Uint32
	Float32
	Float64
)

// Size is the sample size in bytes.
func (t PixType) Size() int {
	switch t {
	case Uint8, Int8:
		return 1
	case Int16, Uint16:
		return 2
	case Int32, Uint32, Float32:
		return 4
	}
	return 8
}

func (t PixType) String() string {
	return [...]string{"uint8", "int8", "int16", "uint16", "int32", "uint32", "float32", "float64"}[t]
}

func (t PixType) IsFloat() bool { return t == Float32 || t == Float64 }

func (t PixType) IsSigned() bool { return t == Int8 || t == Int16 || t == Int32 || t.IsFloat() }

// Range is the representable range of the type.
func (t PixType) Range() (lo, hi float64) {
	switch t {
	case Uint8:
		return 0, math.MaxUint8
	case Int8:
		return math.MinInt8, math.MaxInt8
	case Int16:
		return math.MinInt16, math.MaxInt16
	case Uint16:
		return 0, math.MaxUint16
	case Int32:
		return math.MinInt32, math.MaxInt32
	case Uint32:
		return 0, math.MaxUint32
	case Float32:
		return -math.MaxFloat32, math.MaxFloat32
	}
	return -math.MaxFloat64, math.MaxFloat64
}

// ParsePixType accepts the usual spellings (uint8, byte, float32, double, ...).
func ParsePixType(s string) (PixType, error) {
	switch strings.ToLower(s) {
	case "uint8", "byte", "u8":
		return Uint8, nil
	case "int8", "i8":
		return Int8, nil
	case "int16", "i16":
		return Int16, nil
	case "uint16", "u16":
		return Uint16, nil
	case "int32", "i32":
		return Int32, nil
	case "uint32", "u32":
		return Uint32, nil
	case "float32", "f32", "float", "real":
		return Float32, nil
	case "float64", "f64", "double":
		return Float64, nil
	}
	return 0, fmt.Errorf("unknown pixel type %q", s)
}

// Grid is a north-up raster grid. X0,Y0 is the upper-left corner of the
// upper-left pixel; pixel (col,row) covers
// [X0+col*ResX, X0+(col+1)*ResX] x [Y0-(row+1)*ResY, Y0-row*ResY].
type Grid struct {
	W, H       int
	X0, Y0     float64
	ResX, ResY float64
}

// FormatNum prints a number without trailing zeros or exponent noise.
func FormatNum(v float64) string {
	if v == 0 {
		return "0" // not "-0"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// Round6 rounds to six significant digits, for display.
func Round6(v float64) float64 {
	if v == 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return v
	}
	m := math.Pow(10, 6-math.Ceil(math.Log10(math.Abs(v))))
	return math.Round(v*m) / m
}
