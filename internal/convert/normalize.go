package convert

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"cub2tif/internal/geotiff"
	"cub2tif/internal/isis"
	"cub2tif/internal/raster"
)

// Normalizing maps a cube's exact value range onto 0-1 (or the full range of
// an unsigned integer type) so image editors like GIMP can work on it. The
// mapping is written to a .lbl next to the .tif, in ISIS terms:
//
//	physical = DN * Multiplier + Base

type normInfo struct {
	lo, hi         float64 // value range as read (raw DNs with --raw)
	dnMax          float64 // 1 for float output, 255 / 65535 for integers
	mult, base     float64 // DN -> physical units
	invalid        int64   // no-data pixels in the source
	alpha          bool    // an alpha channel marks no-data
	physLo, physHi float64 // physical range
}

func (n *normInfo) mapping() *geotiff.Normalize {
	k := 0.0
	if n.hi > n.lo {
		k = n.dnMax / (n.hi - n.lo)
	}
	return &geotiff.Normalize{Lo: n.lo, K: k, DNMax: n.dnMax}
}

// normalizeType picks the output type for --normalize.
func normalizeType(o *Options) (raster.PixType, error) {
	if o.Type == "" {
		return raster.Float32, nil
	}
	t, err := raster.ParsePixType(o.Type)
	if err != nil || (t != raster.Float32 && t != raster.Float64 && t != raster.Uint8 && t != raster.Uint16) {
		return 0, fmt.Errorf("--normalize writes float32, float64, uint8 or uint16")
	}
	return t, nil
}

// planNormalize scans the cube for its exact range and derives the mapping.
func planNormalize(c *isis.Cube, bands []int, o *Options, pt raster.PixType, warp bool) (*normInfo, error) {
	lo, hi, invalid, err := scanRange(c, bands, o.Raw, o.Threads)
	if err != nil {
		return nil, err
	}
	if math.IsInf(lo, 1) {
		return nil, fmt.Errorf("no valid pixels to normalize")
	}
	n := &normInfo{lo: lo, hi: hi, invalid: invalid, dnMax: 1}
	if !pt.IsFloat() {
		_, n.dnMax = pt.Range()
	}
	n.alpha = warp || invalid > 0
	// compose with ISIS Base/Multiplier when --raw kept the stored DNs
	m, b := 1.0, 0.0
	if o.Raw {
		m, b = c.Mult, c.Base
	}
	n.mult = m
	if hi > lo {
		n.mult = (hi - lo) / n.dnMax * m
	}
	n.base = lo*m + b
	n.physLo, n.physHi = lo*m+b, hi*m+b
	if n.physLo > n.physHi {
		n.physLo, n.physHi = n.physHi, n.physLo
	}
	return n, nil
}

// scanRange reads every selected band once, in parallel, for the exact
// minimum and maximum of the valid pixels.
func scanRange(c *isis.Cube, bands []int, raw bool, threads int) (lo, hi float64, invalid int64, err error) {
	bh := 64
	if c.Tiled {
		bh = c.TL
	}
	type job struct{ band, y int }
	jobs := make(chan job, threads*2)
	var mu sync.Mutex
	lo, hi = math.Inf(1), math.Inf(-1)
	var wg sync.WaitGroup
	for i := 0; i < max(threads, 1); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]float64, c.W*bh)
			l, h, bad := math.Inf(1), math.Inf(-1), int64(0)
			var e error
			for j := range jobs {
				if e != nil {
					continue
				}
				n := min(bh, c.H-j.y)
				if e = c.ReadRegion(j.band, 0, j.y, c.W, n, buf, c.W, raw); e != nil {
					continue
				}
				for _, v := range buf[:c.W*n] {
					if v != v {
						bad++
						continue
					}
					if v < l {
						l = v
					}
					if v > h {
						h = v
					}
				}
			}
			mu.Lock()
			lo, hi, invalid = math.Min(lo, l), math.Max(hi, h), invalid+bad
			if e != nil && err == nil {
				err = e
			}
			mu.Unlock()
		}()
	}
	for _, b := range bands {
		for y := 0; y < c.H; y += bh {
			jobs <- job{b, y}
		}
	}
	close(jobs)
	wg.Wait()
	return
}

// SampledRange is the quick look the wizard uses to decide whether to offer
// normalizing: a spread of rows, never more than a few million pixels.
func SampledRange(c *isis.Cube, bands []int, raw bool) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	nrows := min(c.H, 64)
	step := max(1, c.W*nrows*len(bands)/2_000_000)
	row := make([]float64, c.W)
	for _, b := range bands {
		for k := 0; k < nrows; k++ {
			y := int((float64(k) + 0.5) * float64(c.H) / float64(nrows))
			if c.ReadRegion(b, 0, y, c.W, 1, row, c.W, raw) != nil {
				return
			}
			for x := 0; x < c.W; x += step {
				if v := row[x]; v == v {
					lo, hi = math.Min(lo, v), math.Max(hi, v)
				}
			}
		}
	}
	return
}

// labelPath is where the .lbl for an output goes: beside it, same name. It
// never lands on the input (an ISIS detached label can itself be a .lbl).
func labelPath(outPath, in string) string {
	p := strings.TrimSuffix(outPath, filepath.Ext(outPath)) + ".lbl"
	a1, _ := filepath.Abs(p)
	a2, _ := filepath.Abs(in)
	if strings.EqualFold(a1, a2) {
		p = outPath + ".lbl"
	}
	return p
}

var isisTypeName = map[raster.PixType]string{
	raster.Uint8: "UnsignedByte", raster.Uint16: "UnsignedWord", raster.Float32: "Real", raster.Float64: "Double",
}

// pvl value formatting: full precision so the mapping round-trips exactly
func pvlNum(v float64) string {
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if a := math.Abs(v); a == 0 || (a >= 1e-6 && a < 1e15) {
		s = strconv.FormatFloat(v, 'f', -1, 64)
	}
	if !strings.ContainsAny(s, ".eEn") {
		s += ".0"
	}
	return s
}

func pvlStr(s string) string { return `"` + strings.ReplaceAll(s, `"`, `'`) + `"` }

// writeNormLabel writes the PVL sidecar that makes a normalized TIFF
// recoverable: the DN mapping, what the alpha channel means, and the
// georeferencing (which image editors drop when they save).
func writeNormLabel(path string, j *job) error {
	in, outPath, c, bands, pt, n, p, grid := j.in, j.out, j.c, j.bands, j.typ, j.norm, j.proj, j.grid
	var b strings.Builder
	w := func(indent int, format string, a ...any) {
		b.WriteString(strings.Repeat("  ", indent))
		fmt.Fprintf(&b, format, a...)
		b.WriteString("\r\n")
	}
	dnMax := pvlNum(n.dnMax)
	w(0, "/* Written by cub2tif %s. The pixel values in %s are normalized.", Version, filepath.Base(outPath))
	w(0, "   Physical value = DN * Multiplier + Base  (as in an ISIS Pixels group). */")
	w(0, "")
	w(0, "Object = NormalizedImage")
	w(1, "^Image       = %s", pvlStr(filepath.Base(outPath)))
	if abs, err := filepath.Abs(in); err == nil {
		in = abs
	}
	w(1, "SourceCube   = %s", pvlStr(in))
	w(1, "CreatedBy    = %s", pvlStr("cub2tif "+Version))
	w(1, "CreationTime = %s", time.Now().Format("2006-01-02T15:04:05"))
	w(0, "")
	w(1, "Group = Dimensions")
	w(2, "Samples = %d", grid.W)
	w(2, "Lines   = %d", grid.H)
	w(2, "Bands   = %d", len(bands))
	w(1, "End_Group")
	w(0, "")
	w(1, "Group = Pixels")
	w(2, "Type          = %s", isisTypeName[pt])
	w(2, "ByteOrder     = Lsb")
	w(2, "Base          = %s", pvlNum(n.base))
	w(2, "Multiplier    = %s", pvlNum(n.mult))
	w(2, "DnRange       = (0.0, %s)", dnMax)
	w(2, "ValidMinimum  = %s", pvlNum(n.physLo))
	w(2, "ValidMaximum  = %s", pvlNum(n.physHi))
	if n.alpha {
		w(2, "AlphaChannel  = Yes")
		w(2, "AlphaMeaning  = %s", pvlStr(fmt.Sprintf("last channel: %s = data, 0 = no data (ISIS Null and other special pixels)", dnMax)))
	} else {
		w(2, "AlphaChannel  = No")
	}
	w(1, "End_Group")
	if c.BandInfo != nil {
		names := make([]string, len(bands))
		for i, bd := range bands {
			names[i] = pvlStr(c.BandInfo[bd])
		}
		w(0, "")
		w(1, "Group = BandBin")
		w(2, "Name = (%s)", strings.Join(names, ", "))
		w(1, "End_Group")
	}
	if p != nil {
		// The ISIS Mapping group still describes the pixels when the projection
		// is unchanged (a crop only moves the corner).
		if c.Mapping != nil && p.Same(c.Proj) {
			w(0, "")
			w(1, "Group = Mapping")
			for _, k := range c.Mapping.Keywords {
				val := strings.Join(k.Values, ", ")
				switch k.Name {
				case "UpperLeftCornerX":
					val = pvlNum(grid.X0)
				case "UpperLeftCornerY":
					val = pvlNum(grid.Y0)
				case "PixelResolution":
					val = pvlNum(grid.ResX)
				case "Scale":
					if sc, err := strconv.ParseFloat(k.Values[0], 64); err == nil {
						val = pvlNum(sc * c.Grid.ResX / grid.ResX)
					}
				}
				if len(k.Values) > 1 {
					val = "(" + val + ")"
				} else if strings.ContainsAny(val, " \t") {
					val = pvlStr(val)
				}
				if u := mappingUnit(k.Name); u != "" {
					val += " " + u
				}
				w(2, "%-20s = %s", k.Name, val)
			}
			w(1, "End_Group")
		}
		units := "meters"
		if p.Geo {
			units = "degrees"
		}
		w(0, "")
		w(1, "Group = Georeference")
		w(2, "ProjString       = %s", pvlStr(p.PROJ()))
		w(2, "Projection       = %s", pvlStr(p.String()))
		w(2, "TargetName       = %s", pvlStr(p.GeoTIFF().Ellipsoid.Name))
		w(2, "EquatorialRadius = %s <meters>", pvlNum(p.GeoTIFF().Ellipsoid.A))
		w(2, "PolarRadius      = %s <meters>", pvlNum(p.GeoTIFF().Ellipsoid.B))
		w(2, "UpperLeftCornerX = %s <%s>", pvlNum(grid.X0), units)
		w(2, "UpperLeftCornerY = %s <%s>", pvlNum(grid.Y0), units)
		w(2, "PixelResolutionX = %s <%s/pixel>", pvlNum(grid.ResX), units)
		w(2, "PixelResolutionY = %s <%s/pixel>", pvlNum(grid.ResY), units)
		w(2, "Reprojected      = %s", map[bool]string{true: "Yes", false: "No"}[j.warp])
		w(1, "End_Group")
	}
	w(0, "End_Object")
	w(0, "End")
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	os.Remove(path)
	return os.Rename(tmp, path)
}

func mappingUnit(key string) string {
	switch key {
	case "EquatorialRadius", "PolarRadius", "UpperLeftCornerX", "UpperLeftCornerY", "CenterLatitudeRadius":
		return "<meters>"
	case "PixelResolution":
		return "<meters/pixel>"
	case "Scale":
		return "<pixels/degree>"
	}
	return ""
}
