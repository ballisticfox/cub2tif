package convert

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cub2tif/internal/geotiff"
	"cub2tif/internal/isis"
	"cub2tif/internal/proj"
	"cub2tif/internal/raster"
)

// Batch converts the inputs one after another, reporting each failure, and
// returns how many failed.
func Batch(inputs []string, o *Options) (failed int) {
	start := time.Now()
	multi := len(inputs) > 1
	for i, in := range inputs {
		if multi && !o.Quiet {
			fmt.Fprintf(os.Stderr, "[%d/%d] %s\n", i+1, len(inputs), in)
		}
		if err := File(in, o, multi); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", in, err)
			failed++
		}
	}
	if multi && !o.Quiet {
		fmt.Fprintf(os.Stderr, "done: %d converted, %d failed in %.2fs\n", len(inputs)-failed, failed, time.Since(start).Seconds())
	}
	return failed
}

// job is one resolved conversion: every option turned into a decision.
type job struct {
	o         *Options
	in, out   string
	c         *isis.Cube
	bands     []int // 0-based
	typ       raster.PixType
	stretch   []geotiff.Stretch
	norm      *normInfo
	nodata    float64
	hasNoData bool
	comp      int
	level     int
	pred      int
	warp      bool
	src       TileSource
	proj      *proj.Projection // nil: not georeferenced
	grid      raster.Grid
}

// File converts one cube; multi says there are several inputs (so Out is a
// folder).
func File(in string, o *Options, multi bool) error {
	start := time.Now()
	c, err := isis.Open(in)
	if err != nil {
		return err
	}
	defer c.Close()

	j := &job{o: o, in: in, c: c, warp: o.WantsWarp()}
	if j.out, err = OutputPath(in, o, multi); err != nil {
		return err
	}
	if samePath(j.out, in) {
		return fmt.Errorf("output would overwrite the input")
	}
	for _, step := range []func() error{j.chooseBands, j.chooseGrid, j.chooseValues, j.chooseCompression} {
		if err := step(); err != nil {
			return err
		}
	}
	if err := j.write(); err != nil {
		return err
	}
	j.report(time.Since(start))
	return nil
}

func samePath(a, b string) bool {
	a1, err1 := filepath.Abs(a)
	b1, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && strings.EqualFold(a1, b1)
}

func (j *job) warn(format string, a ...any) {
	if !j.o.Quiet {
		fmt.Fprintf(os.Stderr, "warning: "+format+"\n", a...)
	}
}

func (j *job) chooseBands() error {
	if j.o.Bands == "" {
		for b := 0; b < j.c.B; b++ {
			j.bands = append(j.bands, b)
		}
		return nil
	}
	for _, s := range strings.Split(j.o.Bands, ",") {
		b, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || b < 1 || b > j.c.B {
			return fmt.Errorf("invalid band %q (cube has %d bands)", s, j.c.B)
		}
		j.bands = append(j.bands, b-1)
	}
	return nil
}

// OutputType is the sample type a cube is written as, before stretching or
// normalizing: the stored type, or float when Base/Multiplier are applied.
func OutputType(c *isis.Cube, o *Options) (raster.PixType, error) {
	if o.Stretch != "" {
		return raster.Uint8, nil
	}
	if o.Normalize {
		return normalizeType(o)
	}
	t := c.Type
	if c.HasScaling() && !o.Raw {
		t = raster.Float32
		if c.Type == raster.Float64 {
			t = raster.Float64
		}
	}
	if o.Type != "" {
		return raster.ParsePixType(o.Type)
	}
	return t, nil
}

// chooseValues settles the sample type and how values and no-data map to it.
func (j *job) chooseValues() error {
	o := j.o
	if o.Stretch != "" && o.Normalize {
		return fmt.Errorf("use either --normalize or --stretch, not both")
	}
	var err error
	if j.typ, err = OutputType(j.c, o); err != nil {
		return err
	}
	switch {
	case o.Stretch != "":
		if j.stretch, err = computeStretch(j.c, j.bands, o.Raw, o.Stretch); err != nil {
			return err
		}
		j.nodata, j.hasNoData = 0, true
		return nil
	case o.Normalize:
		if o.NoData != "" {
			j.warn("--nodata is ignored with --normalize (no-data goes in the alpha channel)")
		}
		j.norm, err = planNormalize(j.c, j.bands, o, j.typ, j.warp)
		return err // no GDAL no-data value: alpha marks missing pixels
	}
	j.nodata, j.hasNoData = defaultNoData(j.typ), true
	switch strings.ToLower(o.NoData) {
	case "":
	case "none":
		j.hasNoData = false
	case "nan":
		j.nodata = math.NaN()
	default:
		if j.nodata, err = strconv.ParseFloat(o.NoData, 64); err != nil {
			return fmt.Errorf("bad --nodata %q", o.NoData)
		}
	}
	return nil
}

func (j *job) chooseCompression() error {
	o := j.o
	switch strings.ToLower(o.Compress) {
	case "deflate", "zip", "zlib":
		j.comp, j.level = geotiff.CompDeflate, 6
	case "zstd":
		j.comp, j.level = geotiff.CompZstd, 3
	case "none", "raw", "uncompressed":
		j.comp = geotiff.CompNone
	default:
		return fmt.Errorf("unknown --compress %q", o.Compress)
	}
	if o.Level != 0 {
		j.level = o.Level
	}
	if j.comp == geotiff.CompDeflate && (j.level < 1 || j.level > 9) {
		return fmt.Errorf("--level for deflate must be 1-9")
	}
	j.pred = 1
	switch o.Predictor {
	case "auto":
		if j.typ.IsFloat() {
			j.pred = 3
		} else {
			j.pred = 2
		}
	case "none", "1":
	case "2":
		j.pred = 2
	case "3":
		if !j.typ.IsFloat() {
			return fmt.Errorf("predictor 3 needs a floating point output type")
		}
		j.pred = 3
	default:
		return fmt.Errorf("unknown --predictor %q", o.Predictor)
	}
	if j.comp == geotiff.CompNone {
		j.pred = 1
	}
	if o.Tile < 16 || o.Tile%16 != 0 || o.Tile > 4096 {
		return fmt.Errorf("--tile must be a multiple of 16 between 16 and 4096")
	}
	return nil
}

// chooseGrid sets up the pixel source and the output grid.
func (j *job) chooseGrid() error {
	c, o := j.c, j.o
	if !j.warp {
		j.src = &CubeSource{cube: c, bands: j.bands, raw: o.Raw}
		j.grid = raster.Grid{W: c.W, H: c.H}
		if c.Proj == nil {
			reason := "no Mapping group"
			if c.ProjErr != nil {
				reason = c.ProjErr.Error()
			}
			j.warn("output is not georeferenced: %s", reason)
			return nil
		}
		j.proj, j.grid = c.Proj, c.Grid
		return nil
	}
	if c.Proj == nil {
		if c.ProjErr != nil {
			return fmt.Errorf("cannot reproject: %v", c.ProjErr)
		}
		return fmt.Errorf("cannot reproject: the cube has no map projection (Mapping group)")
	}
	var warns []string
	var err error
	if j.proj, j.grid, warns, err = PlanWarp(c, o); err != nil {
		return err
	}
	for _, w := range warns {
		j.warn("%s", w)
	}
	var rs resampling
	switch strings.ToLower(o.Resample) {
	case "nearest", "near":
		rs = nearest
	case "bilinear":
		rs = bilinear
	case "cubic", "bicubic":
		rs = cubic
	default:
		return fmt.Errorf("unknown --resample %q", o.Resample)
	}
	approx := 0.125 // pixels
	if o.Exact {
		approx = 0
	}
	j.src = newWarpSource(c, j.bands, o.Raw, j.proj, j.grid, rs, approx, int64(o.CacheMB)<<20, o.Threads)
	return nil
}

// write runs the pipeline into a temporary file and moves it into place.
func (j *job) write() error {
	o := j.o
	overviews := o.Overviews
	if overviews && j.norm != nil {
		// image editors open every TIFF directory as another layer
		overviews = false
		if !o.Quiet {
			fmt.Fprintln(os.Stderr, "note: overviews skipped for --normalize; image editors load them as extra layers")
		}
	}
	var ovAverage bool
	switch o.OvResample {
	case "average":
		ovAverage = true
	case "nearest":
	default:
		return fmt.Errorf("unknown --ov-resample %q", o.OvResample)
	}

	// Normalized output is laid out for image editors: interleaved samples,
	// RGB for three bands, and an alpha channel for no-data.
	lay := geotiff.Layout{
		Levels:      geotiff.OverviewLevels(j.grid.W, j.grid.H, o.Tile, overviews),
		Tile:        o.Tile,
		Samples:     len(j.bands),
		Type:        j.typ,
		Compression: j.comp,
		Predictor:   j.pred,
		Metadata:    j.gdalMetadata(),
	}
	if j.norm != nil {
		lay.Interleaved = true
		lay.Alpha = j.norm.alpha
		if lay.Alpha {
			lay.Samples++
		}
		if len(j.bands) == 3 {
			lay.Photometric = geotiff.RGB
		}
	}
	if j.hasNoData {
		lay.NoData = formatNoData(j.nodata, j.typ)
	}
	if j.proj != nil {
		lay.Georef = &geotiff.Georef{Grid: j.grid, Proj: j.proj}
	}
	est := float64(j.grid.W) * float64(j.grid.H) * float64(lay.Samples*j.typ.Size())
	if overviews {
		est *= 4.0 / 3
	}
	switch strings.ToLower(o.BigTIFF) {
	case "auto", "if_needed", "if_safer":
		lay.BigTIFF = est > 3.8e9 // uncompressed size, with some margin
	case "yes", "true", "1":
		lay.BigTIFF = true
	case "no", "false", "0":
	default:
		return fmt.Errorf("unknown --bigtiff %q", o.BigTIFF)
	}

	enc, err := geotiff.NewEncoder(geotiff.EncoderConfig{
		Type: j.typ, NoData: j.nodata, HasNoData: j.hasNoData, Stretch: j.stretch,
		Interleaved: lay.Interleaved, Alpha: lay.Alpha,
		Compression: j.comp, Level: j.level, Predictor: j.pred, Tile: o.Tile,
	})
	if err != nil {
		return err
	}
	if j.norm != nil {
		enc.Normalize = j.norm.mapping()
	}

	if err := os.MkdirAll(filepath.Dir(j.out), 0o755); err != nil {
		return err
	}
	tmp := j.out + ".partial"
	w, err := geotiff.Create(tmp, lay)
	if err != nil {
		return err
	}
	p := &Pipeline{src: j.src, w: w, enc: enc, threads: max(o.Threads, 1), ovAverage: ovAverage}
	if !o.Quiet {
		fmt.Fprintf(os.Stderr, "  %s -> %s\n", filepath.Base(j.in), j.out)
		if j.warp {
			fmt.Fprintf(os.Stderr, "  %s -> %s\n", j.c.Proj, j.proj)
		}
		p.progress = progressBar()
	}
	if err := p.Run(); err != nil {
		w.Abort()
		return err
	}
	if err := w.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Remove(j.out)
	if err := os.Rename(tmp, j.out); err != nil {
		return err
	}
	if j.norm != nil {
		lbl := labelPath(j.out, j.in)
		if err := writeNormLabel(lbl, j); err != nil {
			return fmt.Errorf("writing %s: %w", lbl, err)
		}
	}
	return nil
}

func (j *job) report(elapsed time.Duration) {
	if j.o.Quiet {
		return
	}
	c := j.c
	inMB := float64(c.W) * float64(c.H) * float64(len(j.bands)*c.Type.Size()) / 1e6
	outMB := 0.0
	if fi, err := os.Stat(j.out); err == nil {
		outMB = float64(fi.Size()) / 1e6
	}
	res := ""
	if j.proj != nil {
		unit := "m"
		if j.proj.Geo {
			unit = "deg"
		}
		res = fmt.Sprintf(", %s %s/px", raster.FormatNum(raster.Round6(j.grid.ResX)), unit)
	}
	sec := elapsed.Seconds()
	fmt.Fprintf(os.Stderr, "\r  %dx%dx%d %s%s, %.1f MB -> %.1f MB in %.2fs (%.0f MB/s)\n",
		j.grid.W, j.grid.H, len(j.bands), j.typ, res, inMB, outMB, sec, inMB/sec)
	if n := j.norm; n != nil {
		alpha := ""
		if n.alpha {
			alpha = " + alpha for no-data"
		}
		fmt.Fprintf(os.Stderr, "  normalized %s .. %s -> 0 .. %s%s; value = DN * %s + %s (in %s)\n",
			raster.FormatNum(raster.Round6(n.physLo)), raster.FormatNum(raster.Round6(n.physHi)), raster.FormatNum(n.dnMax), alpha,
			strconv.FormatFloat(n.mult, 'g', 8, 64), strconv.FormatFloat(n.base, 'g', 8, 64), filepath.Base(labelPath(j.out, j.in)))
	}
}

// defaultNoData is the ISIS Null value for each type.
func defaultNoData(t raster.PixType) float64 {
	switch t {
	case raster.Int8, raster.Int16, raster.Int32:
		lo, _ := t.Range()
		return lo
	case raster.Float32:
		return isis.Null4
	case raster.Float64:
		return isis.Null8
	}
	return 0 // unsigned types
}

// formatNoData prints the value so that parsing it back gives the same
// sample (GDAL compares float32 no-data after a round trip through float32).
func formatNoData(v float64, t raster.PixType) string {
	if math.IsNaN(v) {
		return "nan"
	}
	if t == raster.Float32 {
		return strconv.FormatFloat(float64(float32(v)), 'g', -1, 32)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// gdalMetadata is the GDAL_METADATA XML: target name, band descriptions and,
// when the stored values are not physical units, scale and offset.
func (j *job) gdalMetadata() string {
	c := j.c
	var items []string
	if c.Mapping != nil {
		if t := c.Mapping.Str("TargetName"); t != "" {
			items = append(items, fmt.Sprintf(`<Item name="TARGET_NAME">%s</Item>`, xmlEscape(t)))
		}
	}
	scale, offset, scaled := 0.0, 0.0, false
	switch {
	case j.norm != nil:
		scale, offset, scaled = j.norm.mult, j.norm.base, true
	case j.o.Raw && j.stretch == nil && c.HasScaling():
		scale, offset, scaled = c.Mult, c.Base, true
	}
	for i, b := range j.bands {
		if scaled {
			items = append(items,
				fmt.Sprintf(`<Item name="OFFSET" sample="%d" role="offset">%s</Item>`, i, strconv.FormatFloat(offset, 'g', -1, 64)),
				fmt.Sprintf(`<Item name="SCALE" sample="%d" role="scale">%s</Item>`, i, strconv.FormatFloat(scale, 'g', -1, 64)))
		}
		if c.BandInfo != nil {
			items = append(items, fmt.Sprintf(`<Item name="DESCRIPTION" sample="%d" role="description">%s</Item>`, i, xmlEscape(c.BandInfo[b])))
		}
	}
	if len(items) == 0 {
		return ""
	}
	return "<GDALMetadata>\n  " + strings.Join(items, "\n  ") + "\n</GDALMetadata>\n"
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

func xmlEscape(s string) string { return xmlEscaper.Replace(s) }
