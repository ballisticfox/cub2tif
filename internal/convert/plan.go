package convert

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"cub2tif/internal/isis"
	"cub2tif/internal/proj"
	"cub2tif/internal/raster"
)

type gridOpts struct {
	res       float64
	resY      float64
	bounds    []float64 // minlon,minlat,maxlon,maxlat (degrees, dst lat type)
	extent    []float64 // xmin,ymin,xmax,ymax in dst units
	size      []int     // W,H
	boundsLat proj.LatKind
}

// inSource reports whether canonical lon/lat falls inside the cube footprint.
func inSource(c *isis.Cube, lon, lat float64) bool {
	sx, sy, ok := c.Proj.Forward(lon, lat)
	if !ok {
		return false
	}
	u := (sx - c.Grid.X0) / c.Grid.ResX
	v := (c.Grid.Y0 - sy) / c.Grid.ResY
	if c.Proj.Period > 0 {
		P := c.Proj.Period / c.Grid.ResX
		u -= math.Floor((u-float64(c.W)/2)/P+0.5) * P
	}
	const eps = 1e-6
	return u >= -eps && u <= float64(c.W)+eps && v >= -eps && v <= float64(c.H)+eps
}

// planGrid chooses the output grid for reprojection.
func planGrid(c *isis.Cube, dp *proj.Projection, o gridOpts) (raster.Grid, []string, error) {
	var warns []string
	sp := c.Proj
	same := sp.Same(dp)
	res, resY := o.res, o.resY
	if res == 0 && len(o.size) == 0 {
		switch {
		case same:
			res = c.Grid.ResX
		case dp.Geo:
			if sc := c.Mapping.FloatOr("Scale", 0); sc > 0 {
				res = 1 / sc
			} else {
				res = c.Grid.ResX / (sp.Body.A * proj.D2R)
			}
		default:
			res = c.Grid.ResX
		}
	}
	if resY == 0 {
		resY = res
	}

	var xmin, ymin, xmax, ymax float64
	if len(o.extent) == 4 {
		xmin, ymin, xmax, ymax = o.extent[0], o.extent[1], o.extent[2], o.extent[3]
	} else {
		xmin, ymin = math.Inf(1), math.Inf(1)
		xmax, ymax = math.Inf(-1), math.Inf(-1)
		add := func(lon, lat float64) {
			x, y, ok := dp.Forward(lon, lat)
			if !ok {
				return
			}
			xmin, xmax = math.Min(xmin, x), math.Max(xmax, x)
			ymin, ymax = math.Min(ymin, y), math.Max(ymax, y)
		}
		if len(o.bounds) == 4 {
			lo0, la0, lo1, la1 := o.bounds[0], o.bounds[1], o.bounds[2], o.bounds[3]
			conv := func(la float64) float64 { return dp.Body.ConvertLat(la*proj.D2R, o.boundsLat, proj.Ocentric) }
			const n = 256
			for i := 0; i <= n; i++ {
				for j := 0; j <= n; j++ {
					if i != 0 && i != n && j != 0 && j != n && (i%8 != 0 || j%8 != 0) {
						continue
					}
					lon := lo0 + (lo1-lo0)*float64(i)/n
					lat := la0 + (la1-la0)*float64(j)/n
					add(lon*proj.D2R, conv(lat))
				}
			}
		} else {
			d := dp.Domain()
			try := func(lon, lat float64) {
				if d.Contains(lon, lat, dp.Lon0) {
					add(lon, lat)
				}
			}
			// source footprint: dense edges plus an interior lattice
			const eps = 1e-6
			W, H := float64(c.W), float64(c.H)
			pix := func(u, v float64) {
				u = math.Min(math.Max(u, eps), W-eps)
				v = math.Min(math.Max(v, eps), H-eps)
				x := c.Grid.X0 + u*c.Grid.ResX
				y := c.Grid.Y0 - v*c.Grid.ResY
				if lon, lat, ok := sp.Inverse(x, y); ok {
					try(lon, lat)
				}
			}
			const ne = 2048
			for i := 0; i <= ne; i++ {
				t := float64(i) / ne
				pix(t*W, 0)
				pix(t*W, H)
				pix(0, t*H)
				pix(W, t*H)
			}
			const ni = 256
			for i := 0; i <= ni; i++ {
				for j := 0; j <= ni; j++ {
					pix(float64(i)/ni*W, float64(j)/ni*H)
				}
			}
			// domain boundary lines that cut through the footprint
			inSrc := func(lon, lat float64) {
				if inSource(c, lon, lat) {
					try(lon, lat)
				}
			}
			const nb = 2880
			for i := 0; i <= nb; i++ {
				lon := -math.Pi + 2*math.Pi*float64(i)/nb
				lat := -math.Pi/2 + math.Pi*float64(i)/nb
				if d.LatMin > -90 {
					inSrc(lon, d.LatMin*proj.D2R)
				}
				if d.LatMax < 90 {
					inSrc(lon, d.LatMax*proj.D2R)
				}
				// the target's antimeridian bounds cylindrical/pseudo-cylindrical maps
				inSrc(dp.Lon0+math.Pi-1e-9, lat)
				inSrc(dp.Lon0-math.Pi+1e-9, lat)
				if d.DLonMax > 0 {
					inSrc(dp.Lon0+d.DLonMax*proj.D2R, lat)
					inSrc(dp.Lon0-d.DLonMax*proj.D2R, lat)
				}
				if d.CosDistMin > -2 {
					// points at angular distance acos(cosDistMin) from the center
					ad := math.Acos(d.CosDistMin) - 1e-9
					az := 2 * math.Pi * float64(i) / nb
					sl := math.Sin(d.CLat)*math.Cos(ad) + math.Cos(d.CLat)*math.Sin(ad)*math.Cos(az)
					plat := math.Asin(clamp1(sl))
					plon := dp.Lon0 + math.Atan2(math.Sin(az)*math.Sin(ad)*math.Cos(d.CLat), math.Cos(ad)-math.Sin(d.CLat)*sl)
					inSrc(plon, plat)
				}
			}
			// the projection center / poles if they are inside the footprint
			for _, pt := range [][2]float64{{dp.Lon0, d.CLat}, {0, math.Pi / 2}, {0, -math.Pi / 2}} {
				inSrc(pt[0], pt[1])
			}
		}
		if math.IsInf(xmin, 0) || !(xmax > xmin) || !(ymax > ymin) {
			return raster.Grid{}, warns, fmt.Errorf("the requested area does not project into %s (try --bounds or --extent)", dp)
		}
	}
	if len(o.size) == 2 {
		g := raster.Grid{W: o.size[0], H: o.size[1], X0: xmin, Y0: ymax}
		g.ResX = (xmax - xmin) / float64(g.W)
		g.ResY = (ymax - ymin) / float64(g.H)
		return g, warns, nil
	}
	// snap to the resolution grid (aligned to the source grid if the
	// projection is unchanged, else to the origin)
	ox, oy := 0.0, 0.0
	if same {
		ox, oy = c.Grid.X0, c.Grid.Y0
	}
	const tol = 1e-6
	x0 := ox + math.Floor((xmin-ox)/res+tol)*res
	x1 := ox + math.Ceil((xmax-ox)/res-tol)*res
	y1 := oy + math.Ceil((ymax-oy)/resY-tol)*resY
	y0 := oy + math.Floor((ymin-oy)/resY+tol)*resY
	g := raster.Grid{W: int(math.Round((x1 - x0) / res)), H: int(math.Round((y1 - y0) / resY)), X0: x0, Y0: y1, ResX: res, ResY: resY}
	if g.W <= 0 || g.H <= 0 {
		return g, warns, fmt.Errorf("output grid is empty")
	}
	px := float64(g.W) * float64(g.H)
	if px > 4e10 || g.W > 1<<30 || g.H > 1<<30 {
		return g, warns, fmt.Errorf("output grid %dx%d is unreasonably large; limit it with --bounds, --extent or --res", g.W, g.H)
	}
	if px > 16*float64(c.W)*float64(c.H) {
		warns = append(warns, fmt.Sprintf("output grid %dx%d is much larger than the input; consider --bounds or --res", g.W, g.H))
	}
	return g, warns, nil
}

// PlanWarp resolves the target projection and output grid for a mapped cube.
func PlanWarp(c *isis.Cube, o *Options) (*proj.Projection, raster.Grid, []string, error) {
	var none raster.Grid
	p, err := ParseProjSpec(o.Proj, c, o.LatType)
	if err != nil {
		return nil, none, nil, err
	}
	// --bounds latitudes are in the output's convention unless that is a
	// formula-internal one the user never chose; then the cube's
	g := gridOpts{boundsLat: p.Kind}
	if o.LatType != "" {
		g.boundsLat, _ = ParseLatType(o.LatType)
	} else if p.Kind != c.Proj.Kind && !p.Geo {
		g.boundsLat = c.LatType()
	}
	if o.Res != "" {
		v, err := parseFloats(o.Res, 1, 2)
		if err != nil {
			return nil, none, nil, fmt.Errorf("--res: %v", err)
		}
		g.res = v[0]
		if len(v) == 2 {
			g.resY = v[1]
		}
		if g.res <= 0 || g.resY < 0 {
			return nil, none, nil, fmt.Errorf("--res must be positive")
		}
	}
	if o.Bounds != "" {
		if g.bounds, err = parseFloats(o.Bounds, 4); err != nil {
			return nil, none, nil, fmt.Errorf("--bounds: %v", err)
		}
		if g.bounds[2] <= g.bounds[0] || g.bounds[3] <= g.bounds[1] {
			return nil, none, nil, fmt.Errorf("--bounds must be W,S,E,N with W<E and S<N")
		}
	}
	if o.Extent != "" {
		if g.extent, err = parseFloats(o.Extent, 4); err != nil {
			return nil, none, nil, fmt.Errorf("--extent: %v", err)
		}
		if g.extent[2] <= g.extent[0] || g.extent[3] <= g.extent[1] {
			return nil, none, nil, fmt.Errorf("--extent must be XMIN,YMIN,XMAX,YMAX")
		}
	}
	if o.Size != "" {
		v, err := parseFloats(o.Size, 2)
		if err != nil || v[0] < 1 || v[1] < 1 {
			return nil, none, nil, fmt.Errorf("--size must be W,H")
		}
		if o.Res != "" {
			return nil, none, nil, fmt.Errorf("use either --size or --res, not both")
		}
		g.size = []int{int(v[0]), int(v[1])}
	}
	grid, warns, err := planGrid(c, p, g)
	return p, grid, warns, err
}

// ParseLatType accepts ocentric/ographic and their long forms.
func ParseLatType(s string) (proj.LatKind, error) {
	switch strings.ToLower(s) {
	case "ocentric", "planetocentric", "centric":
		return proj.Ocentric, nil
	case "ographic", "planetographic", "graphic":
		return proj.Ographic, nil
	}
	return 0, fmt.Errorf("unknown latitude type %q (use ocentric or ographic)", s)
}

// parseFloats reads a comma-separated list with one of the given lengths.
func parseFloats(s string, lengths ...int) ([]float64, error) {
	parts := strings.Split(s, ",")
	ok := false
	for _, n := range lengths {
		ok = ok || len(parts) == n
	}
	if !ok {
		return nil, fmt.Errorf("expected %v comma-separated numbers, got %q", lengths, s)
	}
	v := make([]float64, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, fmt.Errorf("bad number %q", p)
		}
		v[i] = f
	}
	return v, nil
}

// ParseFloats is parseFloats for the wizard's input checks.
func ParseFloats(s string, lengths ...int) ([]float64, error) { return parseFloats(s, lengths...) }

func clamp1(v float64) float64 { return math.Max(-1, math.Min(1, v)) }
