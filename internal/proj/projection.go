package proj

import (
	"fmt"
	"math"
	"strings"

	"cub2tif/internal/raster"
)

// Projection maps between canonical ground coordinates (east longitude,
// planetocentric latitude, radians) and projected x/y in meters, or in
// degrees for the geographic "projection".
type Projection struct {
	Name   string  // canonical short name, e.g. "equirectangular"
	Spec   Params  // the parameters it was built from
	Body   Body    // body shape, for latitude conversions
	Kind   LatKind // latitude convention the formula works in
	Lon0   float64 // central longitude, radians east
	Period float64 // x distance of one full turn for cylindrical maps, else 0
	Geo    bool    // a lon/lat degree grid rather than a projection

	f       formula
	domain  Domain
	geotiff GeoTIFF
	projStr string
	desc    string
	sameKey string
}

// Forward projects canonical lon/lat (radians) to x/y.
func (p *Projection) Forward(lon, lat float64) (x, y float64, ok bool) {
	phi := lat
	if p.Kind == Ographic {
		phi = p.Body.ToGraphic(lat)
	}
	x, y, ok = p.f.fwd(NormLon(lon-p.Lon0), phi)
	if !ok || math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		return 0, 0, false
	}
	return x, y, true
}

// Inverse maps x/y to canonical lon/lat (radians). The longitude is not
// wrapped, so a grid can run continuously past +/-180.
func (p *Projection) Inverse(x, y float64) (lon, lat float64, ok bool) {
	dl, phi, ok := p.f.inv(x, y)
	if !ok || math.IsNaN(dl) || math.IsNaN(phi) {
		return 0, 0, false
	}
	if p.Kind == Ographic {
		phi = p.Body.ToCentric(phi)
	}
	return p.Lon0 + dl, phi, true
}

// String describes the projection and its parameters for people.
func (p *Projection) String() string { return p.desc }

// PROJ is the equivalent PROJ.4 definition.
func (p *Projection) PROJ() string { return p.projStr }

// Same reports whether two projections are mathematically identical.
func (p *Projection) Same(q *Projection) bool { return p.sameKey == q.sameKey }

// Domain is the region an automatically sized output is limited to.
func (p *Projection) Domain() Domain { return p.domain }

// GeoTIFF describes how the projection is encoded in GeoTIFF keys.
func (p *Projection) GeoTIFF() GeoTIFF { return p.geotiff }

var displayNames = map[string]string{
	"equirectangular": "Equirectangular", "simplecylindrical": "SimpleCylindrical", "geographic": "Geographic",
	"sinusoidal": "Sinusoidal", "orthographic": "Orthographic", "polarstereographic": "PolarStereographic",
	"mercator": "Mercator", "transversemercator": "TransverseMercator", "lambertconformal": "LambertConformal",
	"lambertazimuthal": "LambertAzimuthalEqualArea",
}

// DisplayName is the ISIS-style name, e.g. "PolarStereographic".
func (p *Projection) DisplayName() string {
	if n, ok := displayNames[p.Name]; ok {
		return n
	}
	return p.Name
}

// Domain limits an automatically sized output to where the projection is
// useful: a polar map stops at the equator, Mercator at +/-85 degrees, an
// orthographic map at the visible hemisphere.
type Domain struct {
	LatMin, LatMax float64 // canonical degrees
	DLonMax        float64 // max |lon - lon0| in degrees; 0 = unlimited
	CosDistMin     float64 // min cosine of the angle from (CLat, lon0); -2 = unlimited
	CLat           float64 // canonical radians, for CosDistMin
}

func fullDomain() Domain { return Domain{LatMin: -90, LatMax: 90, CosDistMin: -2} }

// Contains reports whether canonical lon/lat (radians) lies in the domain of
// a projection centered on lon0.
func (d Domain) Contains(lon, lat, lon0 float64) bool {
	ld := lat * R2D
	if ld < d.LatMin-1e-9 || ld > d.LatMax+1e-9 {
		return false
	}
	dl := NormLon(lon - lon0)
	if d.DLonMax > 0 && math.Abs(dl)*R2D > d.DLonMax+1e-9 {
		return false
	}
	if d.CosDistMin > -2 {
		c := math.Sin(d.CLat)*math.Sin(lat) + math.Cos(d.CLat)*math.Cos(lat)*math.Cos(dl)
		if c < d.CosDistMin {
			return false
		}
	}
	return true
}

// GeoTIFF key IDs for projection parameters.
const (
	KeyStdParallel1      = 3078
	KeyStdParallel2      = 3079
	KeyNatOriginLong     = 3080
	KeyNatOriginLat      = 3081
	KeyFalseEasting      = 3082
	KeyFalseNorthing     = 3083
	KeyFalseOriginLong   = 3084
	KeyFalseOriginLat    = 3085
	KeyFalseOriginE      = 3086
	KeyFalseOriginN      = 3087
	KeyCenterLong        = 3088
	KeyCenterLat         = 3089
	KeyScaleAtNatOrigin  = 3092
	KeyStraightVertPoleL = 3095
)

// GeoTIFF coordinate transformation codes.
const (
	CTTransverseMercator = 1
	CTMercator           = 7
	CTLambertConfConic2  = 8
	CTLambertAzimEqArea  = 10
	CTPolarStereographic = 15
	CTEquirectangular    = 17
	CTOrthographic       = 21
	CTSinusoidal         = 24
)

// GeoTIFF is a projection's GeoTIFF encoding: the ellipsoid of its CRS, the
// coordinate transformation code and the parameter keys (degrees/meters).
// Sphere-only formulas get a spherical CRS, as GDAL's ISIS driver does.
type GeoTIFF struct {
	Geographic  bool
	Ellipsoid   Body
	LocalRadius bool // the sphere is the body's radius at a latitude
	CT          int
	Params      []GeoParam
}

type GeoParam struct {
	Key   int
	Value float64
}

// Params are the user and label facing parameters, in degrees, with
// latitudes in LatType's convention.
type Params struct {
	Name    string // canonical name, see CanonicalName
	Body    Body
	LatType LatKind
	CLon    float64 // central longitude, east
	CLat    float64 // center latitude, or latitude of true scale
	HasCLat bool
	HasCLon bool
	Par1    float64 // standard parallels (Lambert conformal)
	Par2    float64
	K0      float64 // scale factor; 0 = the projection's default
	Radius  float64 // equirectangular sphere radius; 0 = local radius at CLat
	Lat0    float64 // equirectangular latitude of origin (PROJ lat_0)
}

var aliases = map[string]string{
	"equirectangular": "equirectangular", "eqc": "equirectangular", "equi": "equirectangular",
	"simplecylindrical": "simplecylindrical", "simplecyl": "simplecylindrical", "simple": "simplecylindrical",
	"geographic": "geographic", "geo": "geographic", "longlat": "geographic", "latlong": "geographic", "lonlat": "geographic",
	"sinusoidal": "sinusoidal", "sinu": "sinusoidal", "sin": "sinusoidal",
	"orthographic": "orthographic", "ortho": "orthographic",
	"polarstereographic": "polarstereographic", "ps": "polarstereographic", "polar": "polarstereographic", "stere": "polarstereographic", "polarstereo": "polarstereographic",
	"mercator": "mercator", "merc": "mercator",
	"transversemercator": "transversemercator", "tmerc": "transversemercator", "tm": "transversemercator",
	"lambertconformal": "lambertconformal", "lcc": "lambertconformal", "lambertconformalconic": "lambertconformal",
	"lambertazimuthalequalarea": "lambertazimuthal", "lambertazimuthal": "lambertazimuthal", "laea": "lambertazimuthal",
}

// CanonicalName resolves a projection name or alias (case and underscores
// ignored), e.g. "PolarStereographic", "ps" -> "polarstereographic".
func CanonicalName(s string) (string, bool) {
	n, ok := aliases[strings.ToLower(strings.ReplaceAll(s, "_", ""))]
	return n, ok
}

// New builds a projection from parameters.
func New(pp Params) (*Projection, error) {
	b := pp.Body
	e := b.Ecc()
	lt := pp.LatType
	p := &Projection{Name: pp.Name, Spec: pp, Body: b, Lon0: pp.CLon * D2R, domain: fullDomain()}
	num := raster.FormatNum
	// a latitude parameter in the planetographic convention; in degrees it
	// is passed through untouched when no conversion applies, so a requested
	// 30 is written as 30 rather than 29.999999999999996
	graphicDeg := func(latDeg float64) float64 {
		if lt == Ographic || b.Sphere() {
			return latDeg
		}
		return b.ConvertLat(latDeg*D2R, lt, Ographic) * R2D
	}
	graphic := func(latDeg float64) float64 { return graphicDeg(latDeg) * D2R }
	name := b.Name
	if name == "" {
		name = "Body"
	}
	ellps := func(bb Body) string {
		if bb.Sphere() {
			return "+R=" + num(bb.A)
		}
		return "+a=" + num(bb.A) + " +b=" + num(bb.B)
	}
	sphere := func(r float64) Body { return Body{Name: name, A: r, B: r} }
	g := &p.geotiff

	switch pp.Name {
	case "equirectangular", "simplecylindrical":
		p.Kind = lt
		clat := pp.CLat
		if pp.Name == "simplecylindrical" {
			clat = 0
		}
		R := pp.Radius
		if R == 0 {
			R = b.localRadius(clat)
		}
		cts := math.Cos(clat * D2R)
		if cts < 1e-10 {
			return nil, fmt.Errorf("equirectangular: center latitude too close to the pole")
		}
		p.f = eqcF{R: R, cts: cts, phi0: pp.Lat0 * D2R}
		p.Period = 2 * math.Pi * R * cts
		*g = GeoTIFF{Ellipsoid: sphere(R), LocalRadius: R != b.A, CT: CTEquirectangular, Params: []GeoParam{
			{KeyStdParallel1, clat}, {KeyCenterLong, pp.CLon}, {KeyCenterLat, pp.Lat0}, {KeyFalseEasting, 0}, {KeyFalseNorthing, 0}}}
		p.projStr = fmt.Sprintf("+proj=eqc +lat_ts=%s +lat_0=%s +lon_0=%s +x_0=0 +y_0=0 %s +units=m +no_defs", num(clat), num(pp.Lat0), num(pp.CLon), ellps(g.Ellipsoid))
		title := "Equirectangular"
		if pp.Name == "simplecylindrical" {
			title = "Simple Cylindrical"
		}
		p.desc = fmt.Sprintf("%s (clon=%s, clat=%s, %s)", title, num(pp.CLon), num(clat), lt)
		p.sameKey = fmt.Sprintf("eqc|%v|%v|%v|%v|%v", R, cts, pp.Lat0, pp.CLon, lt)
	case "geographic":
		p.Kind = lt
		p.Geo = true
		p.f = geoF{lon0deg: pp.CLon}
		p.Period = 360
		// Planetocentric degrees on an ellipsoid are not geodetic latitudes, so
		// that grid is described on the equatorial sphere.
		ell := b
		if lt == Ocentric {
			ell = sphere(b.A)
		}
		ell.Name = name
		*g = GeoTIFF{Geographic: true, Ellipsoid: ell}
		p.projStr = fmt.Sprintf("+proj=longlat %s +no_defs", ellps(ell))
		p.desc = fmt.Sprintf("Geographic lon/lat degrees (%s)", lt)
		p.sameKey = fmt.Sprintf("geo|%v|%v", pp.CLon, lt)
	case "sinusoidal":
		p.Kind = lt
		p.f = sinuF{R: b.A}
		*g = GeoTIFF{Ellipsoid: sphere(b.A), CT: CTSinusoidal, Params: []GeoParam{
			{KeyCenterLong, pp.CLon}, {KeyFalseEasting, 0}, {KeyFalseNorthing, 0}}}
		p.projStr = fmt.Sprintf("+proj=sinu +lon_0=%s +x_0=0 +y_0=0 %s +units=m +no_defs", num(pp.CLon), ellps(g.Ellipsoid))
		p.desc = fmt.Sprintf("Sinusoidal (clon=%s, %s)", num(pp.CLon), lt)
		p.sameKey = fmt.Sprintf("sinu|%v|%v", pp.CLon, lt)
	case "orthographic":
		p.Kind = Ographic
		pg := graphic(pp.CLat)
		sp0, cp0 := math.Sincos(pg)
		p.f = orthoF{R: b.A, sp0: sp0, cp0: cp0}
		*g = GeoTIFF{Ellipsoid: sphere(b.A), CT: CTOrthographic, Params: []GeoParam{
			{KeyCenterLat, graphicDeg(pp.CLat)}, {KeyCenterLong, pp.CLon}, {KeyFalseEasting, 0}, {KeyFalseNorthing, 0}}}
		p.projStr = fmt.Sprintf("+proj=ortho +lat_0=%s +lon_0=%s +x_0=0 +y_0=0 %s +units=m +no_defs", num(graphicDeg(pp.CLat)), num(pp.CLon), ellps(g.Ellipsoid))
		p.desc = fmt.Sprintf("Orthographic (clon=%s, clat=%s)", num(pp.CLon), num(pp.CLat))
		p.domain.CosDistMin = 0 // the visible hemisphere
		p.domain.CLat = b.ToCentric(pg)
		p.sameKey = fmt.Sprintf("ortho|%v|%v", pg, pp.CLon)
	case "polarstereographic":
		p.Kind = Ographic
		if pp.CLat == 0 {
			return nil, fmt.Errorf("polarstereographic: center latitude must be non-zero (use 90 or -90)")
		}
		pg := graphic(pp.CLat)
		k0 := pp.K0
		if k0 == 0 {
			k0 = 1
		}
		p.f = newPS(b.A, e, pg, k0)
		// GDAL reads NatOriginLat as the pole (with k0) when it is +/-90 and as
		// the latitude of true scale otherwise, which matches ISIS's CenterLatitude.
		latTS := graphicDeg(pp.CLat)
		*g = GeoTIFF{Ellipsoid: b, CT: CTPolarStereographic, Params: []GeoParam{
			{KeyNatOriginLat, latTS}, {KeyStraightVertPoleL, pp.CLon}, {KeyScaleAtNatOrigin, k0}, {KeyFalseEasting, 0}, {KeyFalseNorthing, 0}}}
		g.Ellipsoid.Name = name
		pole := 90.0
		if pg < 0 {
			pole = -90
			p.domain.LatMax = 0
		} else {
			p.domain.LatMin = 0
		}
		if math.Abs(math.Abs(latTS)-90) < 1e-9 {
			p.projStr = fmt.Sprintf("+proj=stere +lat_0=%s +lon_0=%s +k=%s +x_0=0 +y_0=0 %s +units=m +no_defs", num(pole), num(pp.CLon), num(k0), ellps(b))
		} else {
			p.projStr = fmt.Sprintf("+proj=stere +lat_0=%s +lat_ts=%s +lon_0=%s +x_0=0 +y_0=0 %s +units=m +no_defs", num(pole), num(latTS), num(pp.CLon), ellps(b))
		}
		p.desc = fmt.Sprintf("Polar Stereographic (clon=%s, clat=%s)", num(pp.CLon), num(pp.CLat))
		p.sameKey = fmt.Sprintf("ps|%v|%v|%v", pg, pp.CLon, k0)
	case "mercator":
		p.Kind = Ographic
		pg := graphic(pp.CLat)
		k0 := msfn(pg, e) // true scale at CLat
		if pp.K0 != 0 {
			k0 *= pp.K0
		}
		p.f = mercF{a: b.A, e: e, k0: k0}
		p.Period = 2 * math.Pi * b.A * k0
		*g = GeoTIFF{Ellipsoid: b, CT: CTMercator, Params: []GeoParam{
			{KeyNatOriginLong, pp.CLon}, {KeyNatOriginLat, 0}, {KeyScaleAtNatOrigin, k0}, {KeyFalseEasting, 0}, {KeyFalseNorthing, 0}}}
		g.Ellipsoid.Name = name
		p.projStr = fmt.Sprintf("+proj=merc +lon_0=%s +k=%s +x_0=0 +y_0=0 %s +units=m +no_defs", num(pp.CLon), num(k0), ellps(b))
		p.desc = fmt.Sprintf("Mercator (clon=%s, true scale lat=%s)", num(pp.CLon), num(pp.CLat))
		p.domain.LatMin, p.domain.LatMax = -85, 85
		p.sameKey = fmt.Sprintf("merc|%v|%v", k0, pp.CLon)
	case "transversemercator":
		p.Kind = Ographic
		pg := graphic(pp.CLat)
		k0 := pp.K0
		if k0 == 0 {
			k0 = 1
		}
		p.f = newTM(b.A, b.B, pg, k0)
		*g = GeoTIFF{Ellipsoid: b, CT: CTTransverseMercator, Params: []GeoParam{
			{KeyNatOriginLong, pp.CLon}, {KeyNatOriginLat, graphicDeg(pp.CLat)}, {KeyScaleAtNatOrigin, k0}, {KeyFalseEasting, 0}, {KeyFalseNorthing, 0}}}
		g.Ellipsoid.Name = name
		p.projStr = fmt.Sprintf("+proj=tmerc +lat_0=%s +lon_0=%s +k=%s +x_0=0 +y_0=0 %s +units=m +no_defs", num(graphicDeg(pp.CLat)), num(pp.CLon), num(k0), ellps(b))
		p.desc = fmt.Sprintf("Transverse Mercator (clon=%s, clat=%s, k=%s)", num(pp.CLon), num(pp.CLat), num(k0))
		p.domain.DLonMax = 30
		p.sameKey = fmt.Sprintf("tm|%v|%v|%v", pg, pp.CLon, k0)
	case "lambertconformal":
		p.Kind = Ographic
		p1, p2, p0 := graphic(pp.Par1), graphic(pp.Par2), graphic(pp.CLat)
		f, err := newLCC(b.A, e, p1, p2, p0)
		if err != nil {
			return nil, err
		}
		p.f = f
		*g = GeoTIFF{Ellipsoid: b, CT: CTLambertConfConic2, Params: []GeoParam{
			{KeyStdParallel1, graphicDeg(pp.Par1)}, {KeyStdParallel2, graphicDeg(pp.Par2)}, {KeyFalseOriginLat, graphicDeg(pp.CLat)}, {KeyFalseOriginLong, pp.CLon}, {KeyFalseOriginE, 0}, {KeyFalseOriginN, 0}}}
		g.Ellipsoid.Name = name
		p.projStr = fmt.Sprintf("+proj=lcc +lat_1=%s +lat_2=%s +lat_0=%s +lon_0=%s +x_0=0 +y_0=0 %s +units=m +no_defs", num(graphicDeg(pp.Par1)), num(graphicDeg(pp.Par2)), num(graphicDeg(pp.CLat)), num(pp.CLon), ellps(b))
		p.desc = fmt.Sprintf("Lambert Conformal (par1=%s, par2=%s, clat=%s, clon=%s)", num(pp.Par1), num(pp.Par2), num(pp.CLat), num(pp.CLon))
		// the pole on the far side of the cone is at infinity
		if f.n > 0 {
			p.domain.LatMin = -30
		} else {
			p.domain.LatMax = 30
		}
		p.domain.DLonMax = 90
		p.sameKey = fmt.Sprintf("lcc|%v|%v|%v|%v", p1, p2, p0, pp.CLon)
	case "lambertazimuthal":
		p.Kind = Ographic
		pg := graphic(pp.CLat)
		p.f = newLAEA(b.A, e, pg)
		*g = GeoTIFF{Ellipsoid: b, CT: CTLambertAzimEqArea, Params: []GeoParam{
			{KeyCenterLat, graphicDeg(pp.CLat)}, {KeyCenterLong, pp.CLon}, {KeyFalseEasting, 0}, {KeyFalseNorthing, 0}}}
		g.Ellipsoid.Name = name
		p.projStr = fmt.Sprintf("+proj=laea +lat_0=%s +lon_0=%s +x_0=0 +y_0=0 %s +units=m +no_defs", num(graphicDeg(pp.CLat)), num(pp.CLon), ellps(b))
		p.desc = fmt.Sprintf("Lambert Azimuthal Equal Area (clon=%s, clat=%s)", num(pp.CLon), num(pp.CLat))
		p.domain.CosDistMin = 0 // the near hemisphere; the antipode is a circle
		p.domain.CLat = b.ToCentric(pg)
		p.sameKey = fmt.Sprintf("laea|%v|%v", pg, pp.CLon)
	default:
		return nil, fmt.Errorf("unsupported projection %q", pp.Name)
	}
	p.sameKey += fmt.Sprintf("|%v|%v", b.A, b.B)
	return p, nil
}
