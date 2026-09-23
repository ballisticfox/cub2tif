// Package proj implements the map projections ISIS uses, forward and inverse,
// on spheres and ellipsoids, plus what is needed to describe them in GeoTIFF
// keys and PROJ strings.
//
// Every projection maps to and from one canonical ground coordinate: east
// longitude and planetocentric latitude, in radians. Each formula works in
// the latitude convention ISIS uses for it, and Projection converts at the
// boundary, so any two projections of the same body chain correctly.
package proj

import "math"

// Degree/radian conversion factors.
const (
	D2R = math.Pi / 180
	R2D = 180 / math.Pi
)

// Body is the target body's reference shape (meters).
type Body struct {
	Name string
	A, B float64
}

// Sphere reports whether the body is a sphere (all formulas simplify).
func (b Body) Sphere() bool { return b.A == b.B }

// Ecc is the first eccentricity.
func (b Body) Ecc() float64 {
	if b.A <= b.B {
		return 0
	}
	return math.Sqrt(1 - (b.B*b.B)/(b.A*b.A))
}

// LatKind is the latitude convention a set of numbers is expressed in.
type LatKind int

const (
	Ocentric LatKind = iota
	Ographic
)

func (k LatKind) String() string {
	if k == Ographic {
		return "planetographic"
	}
	return "planetocentric"
}

// ToGraphic converts a planetocentric latitude (radians) to planetographic.
func (b Body) ToGraphic(lat float64) float64 {
	if b.Sphere() || math.Abs(lat) >= math.Pi/2 {
		return lat
	}
	return math.Atan2(math.Sin(lat)*b.A*b.A, math.Cos(lat)*b.B*b.B)
}

// ToCentric converts a planetographic latitude (radians) to planetocentric.
func (b Body) ToCentric(lat float64) float64 {
	if b.Sphere() || math.Abs(lat) >= math.Pi/2 {
		return lat
	}
	return math.Atan2(math.Sin(lat)*b.B*b.B, math.Cos(lat)*b.A*b.A)
}

// ConvertLat converts a latitude (radians) between conventions.
func (b Body) ConvertLat(lat float64, from, to LatKind) float64 {
	if from == to || b.Sphere() {
		return lat
	}
	if to == Ographic {
		return b.ToGraphic(lat)
	}
	return b.ToCentric(lat)
}

// localRadius mirrors ISIS TProjection::LocalRadius.
func (b Body) localRadius(latDeg float64) float64 {
	if b.Sphere() {
		return b.A
	}
	l := latDeg * D2R
	return b.A * b.B / math.Hypot(b.B*math.Cos(l), b.A*math.Sin(l))
}

// NormLon wraps an angle (radians) into [-pi, pi).
func NormLon(l float64) float64 {
	if l >= -math.Pi && l < math.Pi {
		return l
	}
	l = math.Mod(l+math.Pi, 2*math.Pi)
	if l < 0 {
		l += 2 * math.Pi
	}
	return l - math.Pi
}
