package geotiff

import (
	"sort"

	"cub2tif/internal/proj"
)

// GeoKey IDs used here.
const (
	gtModelType         = 1024
	gtRasterType        = 1025
	gtCitation          = 1026
	geographicType      = 2048
	geogCitation        = 2049
	geogGeodeticDatum   = 2050
	geogPrimeMeridian   = 2051
	geogLinearUnits     = 2052
	geogAngularUnits    = 2054
	geogEllipsoid       = 2056
	geogSemiMajorAxis   = 2057
	geogSemiMinorAxis   = 2058
	projectedCSType     = 3072
	pcsCitation         = 3073
	projection          = 3074
	projCoordTrans      = 3075
	projLinearUnits     = 3076
	userDefined         = 32767
	greenwich           = 8901
	meter               = 9001
	degree              = 9102
	pixelIsArea         = 1
	modelTypeProjected  = 1
	modelTypeGeographic = 2
)

// geoKeys encodes the GeoKeyDirectory plus its double and ASCII parameter
// tables. Planetary bodies have no EPSG codes, so everything is
// user-defined; the GDAL-style citation strings carry the body's name.
func geoKeys(p *proj.Projection) (keys []int, dbl []float64, asc string) {
	type key struct{ id, loc, count, val int }
	var ks []key
	short := func(id, v int) { ks = append(ks, key{id, 0, 1, v}) }
	double := func(id int, v float64) {
		ks = append(ks, key{id, 34736, 1, len(dbl)})
		dbl = append(dbl, v)
	}
	text := func(id int, s string) {
		ks = append(ks, key{id, 34737, len(s) + 1, len(asc)})
		asc += s + "|"
	}

	g := p.GeoTIFF()
	body := g.Ellipsoid.Name
	if body == "" {
		body = "Unknown"
	}
	ellipsoid := body
	if g.LocalRadius {
		ellipsoid += "_localRadius"
	}
	pcs := p.DisplayName() + " " + body

	if g.Geographic {
		short(gtModelType, modelTypeGeographic)
		text(gtCitation, "GCS_"+body)
	} else {
		short(gtModelType, modelTypeProjected)
		text(gtCitation, pcs)
	}
	short(gtRasterType, pixelIsArea)
	short(geographicType, userDefined)
	text(geogCitation, "GCS Name = GCS_"+body+"|Datum = D_"+body+"|Ellipsoid = "+ellipsoid+"|Primem = Reference_Meridian|AUnits = degree|")
	short(geogGeodeticDatum, userDefined)
	short(geogPrimeMeridian, greenwich)
	short(geogLinearUnits, meter)
	short(geogAngularUnits, degree)
	short(geogEllipsoid, userDefined)
	double(geogSemiMajorAxis, g.Ellipsoid.A)
	double(geogSemiMinorAxis, g.Ellipsoid.B)
	if !g.Geographic {
		short(projectedCSType, userDefined)
		text(pcsCitation, pcs)
		short(projection, userDefined)
		short(projCoordTrans, g.CT)
		short(projLinearUnits, meter)
		for _, pr := range g.Params {
			double(pr.Key, pr.Value)
		}
	}
	sort.SliceStable(ks, func(i, j int) bool { return ks[i].id < ks[j].id })
	keys = []int{1, 1, 0, len(ks)} // directory version 1.1.0
	for _, k := range ks {
		keys = append(keys, k.id, k.loc, k.count, k.val)
	}
	return keys, dbl, asc
}
