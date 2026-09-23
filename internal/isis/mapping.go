package isis

import (
	"fmt"
	"strings"

	"cub2tif/internal/proj"
	"cub2tif/internal/pvl"
)

// projectionFromMapping builds the projection an ISIS Mapping group
// describes. Longitudes are converted to east; latitudes stay in the
// label's convention, which proj.Params carries.
func projectionFromMapping(m *pvl.Node) (*proj.Projection, error) {
	name := m.Str("ProjectionName")
	cn, ok := proj.CanonicalName(name)
	if !ok || cn == "geographic" {
		return nil, fmt.Errorf("ISIS projection %q is not supported", name)
	}
	a, err := m.Float("EquatorialRadius")
	if err != nil {
		return nil, err
	}
	pp := proj.Params{
		Name:    cn,
		Body:    proj.Body{Name: m.Str("TargetName"), A: a, B: m.FloatOr("PolarRadius", a)},
		LatType: latType(m),
		CLon:    m.FloatOr("CenterLongitude", 0),
		CLat:    m.FloatOr("CenterLatitude", 0),
		K0:      m.FloatOr("ScaleFactor", 0),
		Par1:    m.FloatOr("FirstStandardParallel", 0),
	}
	pp.Par2 = m.FloatOr("SecondStandardParallel", pp.Par1)
	if strings.EqualFold(m.Str("LongitudeDirection"), "PositiveWest") {
		pp.CLon = -pp.CLon
		if pp.CLon <= -180 {
			pp.CLon += 360
		}
	}
	if cn == "equirectangular" {
		pp.Radius = m.FloatOr("CenterLatitudeRadius", 0)
	}
	if cn == "polarstereographic" && pp.CLat == 0 {
		// ISIS requires it; infer the pole from the latitude range
		pp.CLat = 90
		if m.FloatOr("MinimumLatitude", 0)+m.FloatOr("MaximumLatitude", 0) < 0 {
			pp.CLat = -90
		}
	}
	return proj.New(pp)
}

func latType(m *pvl.Node) proj.LatKind {
	if strings.EqualFold(m.Str("LatitudeType"), "Planetographic") {
		return proj.Ographic
	}
	return proj.Ocentric
}

// LatType is the latitude convention of the cube's Mapping group.
func (c *Cube) LatType() proj.LatKind {
	if c.Mapping == nil {
		return proj.Ocentric
	}
	return latType(c.Mapping)
}

// Center returns the middle of the map as east longitude and planetocentric
// latitude in degrees. The label's lat/lon range gives clean numbers (0,0 for
// a global map) where the pixel grid's center can be off by half a pixel.
// Only valid when Proj is set.
func (c *Cube) Center() (lon, lat float64) {
	m := c.Mapping
	if m.Has("MinimumLatitude") && m.Has("MaximumLatitude") && m.Has("MinimumLongitude") && m.Has("MaximumLongitude") {
		lat = (m.FloatOr("MinimumLatitude", 0) + m.FloatOr("MaximumLatitude", 0)) / 2
		lon = (m.FloatOr("MinimumLongitude", 0) + m.FloatOr("MaximumLongitude", 0)) / 2
		if strings.EqualFold(m.Str("LongitudeDirection"), "PositiveWest") {
			lon = -lon
		}
		lat = c.Proj.Body.ConvertLat(lat*proj.D2R, c.LatType(), proj.Ocentric) * proj.R2D
		return proj.NormLon(lon*proj.D2R)*proj.R2D + 0, lat + 0 // +0 turns -0 into 0
	}
	x := c.Grid.X0 + float64(c.W)/2*c.Grid.ResX
	y := c.Grid.Y0 - float64(c.H)/2*c.Grid.ResY
	lo, la, ok := c.Proj.Inverse(x, y)
	if !ok {
		return c.Proj.Lon0 * proj.R2D, 0
	}
	return proj.NormLon(lo) * proj.R2D, la * proj.R2D
}
