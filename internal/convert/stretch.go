package convert

import (
	"fmt"
	"sort"
	"strings"

	"cub2tif/internal/geotiff"
	"cub2tif/internal/isis"
)

// computeStretch finds per-band stretch limits: fixed LO,HI, or percentiles
// (0.5-99.5 for auto, 0-100 for minmax) of a sample of about 2M pixels.
func computeStretch(c *isis.Cube, bands []int, raw bool, mode string) ([]geotiff.Stretch, error) {
	out := make([]geotiff.Stretch, len(bands))
	lo, hi := 0.5, 99.5
	switch strings.ToLower(mode) {
	case "auto":
	case "minmax":
		lo, hi = 0, 100
	default:
		v, err := parseFloats(mode, 2)
		if err != nil {
			return nil, fmt.Errorf("--stretch: use auto, minmax or LO,HI")
		}
		for i := range out {
			out[i] = geotiff.Stretch{Lo: v[0], Hi: v[1]}
		}
		return out, nil
	}
	nrows := min(c.H, 1024)
	step := max(1, c.W*nrows/2_000_000)
	row := make([]float64, c.W)
	for i, b := range bands {
		var vals []float64
		for k := 0; k < nrows; k++ {
			y := int((float64(k) + 0.5) * float64(c.H) / float64(nrows))
			if err := c.ReadRegion(b, 0, y, c.W, 1, row, c.W, raw); err != nil {
				return nil, err
			}
			for x := (k * 7) % step; x < c.W; x += step {
				if v := row[x]; v == v {
					vals = append(vals, v)
				}
			}
		}
		if len(vals) == 0 {
			out[i] = geotiff.Stretch{Lo: 0, Hi: 1}
			continue
		}
		sort.Float64s(vals)
		at := func(p float64) float64 {
			idx := int(p / 100 * float64(len(vals)-1))
			return vals[idx]
		}
		out[i] = geotiff.Stretch{Lo: at(lo), Hi: at(hi)}
	}
	return out, nil
}
