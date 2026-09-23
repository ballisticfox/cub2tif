package proj

import (
	"math"
	"testing"
)

// Every projection must invert its own forward transform.
func TestProjectionRoundTrip(t *testing.T) {
	bodies := []Body{{Name: "Sphere", A: 1737400, B: 1737400}, {Name: "Mars", A: 3396190, B: 3376200}}
	specs := []Params{
		{Name: "equirectangular", CLat: 30, CLon: 10},
		{Name: "simplecylindrical", CLon: 180},
		{Name: "geographic", CLon: 0},
		{Name: "sinusoidal", CLon: -40},
		{Name: "orthographic", CLat: 35, CLon: 120},
		{Name: "polarstereographic", CLat: 90, CLon: 0},
		{Name: "polarstereographic", CLat: -71, CLon: 45},
		{Name: "mercator", CLat: 15, CLon: 0},
		{Name: "transversemercator", CLat: 10, CLon: 30, K0: 0.9996},
		{Name: "lambertconformal", Par1: 20, Par2: 60, CLat: 40, CLon: -100},
		{Name: "lambertazimuthal", CLat: -30, CLon: 60},
		{Name: "lambertazimuthal", CLat: 90, CLon: 0},
	}
	for _, b := range bodies {
		for _, lt := range []LatKind{Ocentric, Ographic} {
			for _, sp := range specs {
				sp.Body, sp.LatType = b, lt
				p, err := New(sp)
				if err != nil {
					t.Fatalf("%s: %v", sp.Name, err)
				}
				n := 0
				for lat := -85.0; lat <= 85; lat += 5 {
					for lon := -175.0; lon <= 175; lon += 5 {
						lo, la := lon*D2R, lat*D2R
						if !p.domain.Contains(lo, la, p.Lon0) {
							continue
						}
						x, y, ok := p.Forward(lo, la)
						if !ok {
							continue
						}
						lo2, la2, ok := p.Inverse(x, y)
						if !ok {
							t.Errorf("%s %s %s: inverse failed at %v,%v", b.Name, lt, sp.Name, lon, lat)
							continue
						}
						if d := math.Abs(NormLon(lo2-lo)) * math.Cos(la); d > 1e-9 || math.Abs(la2-la) > 1e-9 {
							t.Errorf("%s %s %s: round trip %v,%v -> %v,%v", b.Name, lt, sp.Name, lon, lat, lo2*R2D, la2*R2D)
						}
						n++
					}
				}
				if n == 0 {
					t.Errorf("%s: no points tested", sp.Name)
				}
			}
		}
	}
}

// Latitude conversions must invert each other and be identities on a sphere.
func TestLatitudeConversion(t *testing.T) {
	mars := Body{A: 3396190, B: 3376200}
	for lat := -90.0; lat <= 90; lat += 7.5 {
		l := lat * D2R
		if d := math.Abs(mars.ToCentric(mars.ToGraphic(l)) - l); d > 1e-14 {
			t.Errorf("round trip at %v: %g", lat, d)
		}
		if lat != 0 && math.Abs(lat) != 90 && math.Abs(mars.ToGraphic(l)) <= math.Abs(l) {
			t.Errorf("planetographic latitude should be larger in magnitude at %v", lat)
		}
	}
	moon := Body{A: 1737400, B: 1737400}
	if moon.ToGraphic(0.5) != 0.5 {
		t.Error("sphere conversion must be the identity")
	}
}

// Global cylindrical maps unwrap across the antimeridian.
func TestNormLon(t *testing.T) {
	for _, c := range [][2]float64{{0, 0}, {math.Pi, -math.Pi}, {-math.Pi, -math.Pi}, {3 * math.Pi / 2, -math.Pi / 2}, {-5 * math.Pi, -math.Pi}} {
		if got := NormLon(c[0]); math.Abs(got-c[1]) > 1e-12 {
			t.Errorf("NormLon(%v) = %v, want %v", c[0], got, c[1])
		}
	}
}
