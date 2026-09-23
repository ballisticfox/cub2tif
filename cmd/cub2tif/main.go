// Command cub2tif converts ISIS3 cubes to GeoTIFF, optionally reprojecting
// them. Run with flags for scripting; with no flags (a double-click, or
// cubes dropped onto the exe) it starts a guided wizard.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"

	"cub2tif/internal/convert"
	"cub2tif/internal/isis"
	"cub2tif/internal/raster"
)

const version = convert.Version

const usage = `cub2tif %s - fast ISIS3 cube (.cub) to GeoTIFF converter

Usage:
  cub2tif [options] <input.cub | folder | pattern>...
  cub2tif                 (no arguments: guided mode)

Output:
  -o, --out PATH          Output .tif file, or a folder (default: next to input)
  --compress MODE         deflate (default), zstd, none
  --level N               Compression level (deflate 1-9, default 6; zstd 1-22, default 3)
  --predictor P           auto (default), none, 2 (integer), 3 (float)
  --tile N                Tile size in pixels, multiple of 16 (default 512)
  --overviews             Build internal overviews (pyramids) for fast display
  --ov-resample M         average (default) or nearest
  --bigtiff MODE          auto (default), yes, no
  --type T                Output type: uint8 int8 int16 uint16 int32 uint32 float32 float64
  --raw                   Keep raw DNs (don't apply ISIS Base/Multiplier; scale/offset
                          are written as GDAL metadata instead)
  --stretch S             Linear stretch to 8-bit: auto (0.5-99.5%%), minmax, or LO,HI
  --normalize             Map each cube's exact value range onto 0-1 (float32; or the full
                          range of --type uint8/uint16) for image editors like GIMP. No-data
                          becomes an alpha channel, and a .lbl beside the .tif records
                          value = DN * Multiplier + Base to recover the physical values
  --nodata V              NoData value, 'nan' or 'none' (default: ISIS Null for the type)
  --bands LIST            Bands to export, e.g. 1,3,5 (default: all)
  --threads N             Worker threads (default: all cores = %d)

Reprojection (any of these triggers a warp):
  -p, --proj SPEC         Target projection. Either a name with optional parameters:
                            geographic | equirectangular | simplecylindrical | sinusoidal |
                            orthographic | polarstereographic | mercator |
                            transversemercator | lambertconformal | lambertazimuthal
                          e.g.  ps:south   ortho:clat=30,clon=120   eqc:clat=45
                                lcc:par1=20,par2=60,clat=40,clon=-100   tm:clon=10,k=0.9996
                          keys: clat clon par1 par2 k R a b (degrees / meters)
                          or a PROJ string, e.g. "+proj=stere +lat_0=-90 +lon_0=0"
  --res R[,RY]            Output pixel size (meters, or degrees for geographic)
                          (default: same as the input)
  --bounds W,S,E,N        Limit output to a lon/lat box (degrees, east longitude)
  --extent XMIN,YMIN,XMAX,YMAX  Exact output extent in target units
  --size W,H              Output size in pixels (with --extent/--bounds)
  --resample M            nearest, bilinear (default), cubic
  --lat-type T            ocentric or ographic latitudes for the output (and --bounds)
                          (default: same as input; only matters for ellipsoidal bodies)
  --exact                 Transform every pixel exactly (default: 0.125 px approximation)
  --cache MB              Source block cache size for warping (default 1024)

Other:
  --wizard, -i            Guided mode (also what a double-click or drag-and-drop starts)
  --no-pause              Don't wait for a key before closing the window
  --info                  Print cube information and exit
  -q, --quiet             No progress output
  --version               Print version

Examples:
  cub2tif map.cub
  cub2tif *.cub -o out\ --compress zstd --overviews
  cub2tif dtm.cub -p ps:north --bounds -180,60,180,90 --res 500
  cub2tif mosaic.cub -p geographic --stretch auto -o browse.tif
`

func main() {
	code := run(os.Args[1:])
	pauseIfNeeded(os.Args[1:])
	os.Exit(code)
}

// cliFlags are the command-line-only switches; everything else lands in
// convert.Options.
type cliFlags struct {
	info, version, wizard, noPause bool
	cpuProfile                     string
}

// newFlagSet binds every flag. Defaults come from convert.Defaults, so the
// wizard and the command line cannot disagree.
func newFlagSet(o *convert.Options, f *cliFlags) *flag.FlagSet {
	d := convert.Defaults()
	fs := flag.NewFlagSet("cub2tif", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, version, runtime.NumCPU()) }
	str := func(p *string, def string, names ...string) {
		for _, n := range names {
			fs.StringVar(p, n, def, "")
		}
	}
	boolean := func(p *bool, names ...string) {
		for _, n := range names {
			fs.BoolVar(p, n, false, "")
		}
	}
	str(&o.Out, "", "o", "out")
	str(&o.Compress, d.Compress, "compress")
	fs.IntVar(&o.Level, "level", d.Level, "")
	str(&o.Predictor, d.Predictor, "predictor")
	fs.IntVar(&o.Tile, "tile", d.Tile, "")
	boolean(&o.Overviews, "overviews")
	str(&o.OvResample, d.OvResample, "ov-resample")
	str(&o.BigTIFF, d.BigTIFF, "bigtiff")
	fs.IntVar(&o.Threads, "threads", d.Threads, "")
	str(&o.Type, "", "type")
	boolean(&o.Raw, "raw")
	str(&o.Stretch, "", "stretch")
	boolean(&o.Normalize, "normalize")
	str(&o.NoData, "", "nodata")
	str(&o.Bands, "", "bands")
	str(&o.Proj, "", "p", "proj", "t_srs")
	str(&o.Res, "", "res")
	str(&o.Bounds, "", "bounds")
	str(&o.Extent, "", "extent")
	str(&o.Size, "", "size")
	str(&o.Resample, d.Resample, "r", "resample")
	str(&o.LatType, "", "lat-type")
	boolean(&o.Exact, "exact")
	fs.IntVar(&o.CacheMB, "cache", d.CacheMB, "")
	boolean(&o.Quiet, "q", "quiet")
	boolean(&f.info, "info")
	boolean(&f.version, "version")
	boolean(&f.wizard, "wizard", "i")
	boolean(&f.noPause, "no-pause") // acted on by pauseIfNeeded
	str(&f.cpuProfile, "", "cpuprofile")
	return fs
}

func run(args []string) int {
	o := convert.Defaults()
	var cli cliFlags
	fs := newFlagSet(&o, &cli)
	flagArgs, pos, err := splitArgs(fs, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if err := fs.Parse(flagArgs); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if cli.version {
		fmt.Println("cub2tif", version)
		return 0
	}

	// No flags means a person, not a script: a double-click (no arguments)
	// or cubes dropped onto the exe from Explorer. A bare path typed into a
	// shell stays a plain conversion. Redirected streams never reach the
	// wizard; they get the usage text instead of a prompt nobody answers.
	wantsWizard := cli.wizard || len(args) == 0 || (len(flagArgs) == 0 && launchedFromExplorer())
	if wantsWizard {
		if interactive() {
			return runWizard(pos)
		}
		if cli.wizard || len(args) == 0 {
			fs.Usage()
			return 2
		}
	}

	inputs, err := expandInputs(pos)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if len(inputs) == 0 {
		fs.Usage()
		return 2
	}
	if cli.info {
		return printInfoAll(inputs)
	}
	if cli.cpuProfile != "" {
		if f, err := os.Create(cli.cpuProfile); err == nil {
			pprof.StartCPUProfile(f)
			defer pprof.StopCPUProfile()
		}
	}
	if convert.Batch(inputs, &o) > 0 {
		return 1
	}
	return 0
}

// splitArgs separates flags from positional arguments so they can be mixed
// ("cub2tif in.cub -o out.tif"), which the flag package alone does not allow.
func splitArgs(fs *flag.FlagSet, args []string) (flags, pos []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		// "-" alone and negative numbers are values, not flags
		if len(a) < 2 || a[0] != '-' || (a[1] >= '0' && a[1] <= '9') {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if name == "h" || name == "help" {
			flags = append(flags, "-h")
			continue
		}
		name, _, hasValue := strings.Cut(name, "=")
		f := fs.Lookup(name)
		if f == nil {
			return nil, nil, fmt.Errorf("unknown option %s (see --help)", a)
		}
		flags = append(flags, "-"+strings.TrimLeft(a, "-"))
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); hasValue || (ok && bf.IsBoolFlag()) {
			continue
		}
		if i+1 >= len(args) {
			return nil, nil, fmt.Errorf("option %s needs a value", a)
		}
		flags = append(flags, args[i+1])
		i++
	}
	return flags, pos, nil
}

// expandInputs turns the arguments into a list of cubes: existing files as
// they are, folders to the .cub files in them, and anything else as a
// wildcard pattern. Existing paths are never treated as patterns, so names
// with [brackets] work, and matching ignores case as Windows does.
func expandInputs(args []string) ([]string, error) {
	var out []string
	for _, p := range args {
		st, err := os.Stat(p)
		switch {
		case err == nil && st.IsDir():
			m, err := matchDir(p, "*.cub")
			if err != nil {
				return nil, err
			}
			if len(m) == 0 {
				return nil, fmt.Errorf("no .cub files in %s", p)
			}
			out = append(out, m...)
		case err == nil:
			out = append(out, p)
		case strings.ContainsAny(filepath.Base(p), "*?["):
			m, err := matchDir(filepath.Dir(p), filepath.Base(p))
			if err != nil {
				return nil, err
			}
			if len(m) == 0 {
				return nil, fmt.Errorf("no files match %s", p)
			}
			out = append(out, m...)
		default:
			return nil, fmt.Errorf("%s: file not found", p)
		}
	}
	return out, nil
}

// matchDir lists the files in dir whose names match pattern, ignoring case.
func matchDir(dir, pattern string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	pattern = strings.ToLower(pattern)
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ok, err := filepath.Match(pattern, strings.ToLower(e.Name()))
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %v", pattern, err)
		}
		if ok {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func printInfoAll(inputs []string) int {
	failed := 0
	for i, in := range inputs {
		if i > 0 {
			fmt.Println()
		}
		c, err := isis.Open(in)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", in, err)
			failed++
			continue
		}
		printInfo(c)
		c.Close()
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func printInfo(c *isis.Cube) {
	num := raster.FormatNum
	fmt.Printf("File:        %s\n", c.Path)
	if c.DataPath != c.Path {
		fmt.Printf("Data file:   %s\n", c.DataPath)
	}
	fmt.Printf("Size:        %d samples x %d lines x %d bands\n", c.W, c.H, c.B)
	format := "BandSequential"
	if c.Tiled {
		format = fmt.Sprintf("Tile %dx%d", c.TS, c.TL)
	}
	order := "Lsb"
	if c.BigEnd {
		order = "Msb"
	}
	fmt.Printf("Pixels:      %s (%s), %s, base=%g multiplier=%g\n", c.Type, order, format, c.Base, c.Mult)
	if c.BandInfo != nil {
		fmt.Printf("Bands:       %s\n", strings.Join(c.BandInfo, ", "))
	}
	m := c.Mapping
	if m == nil {
		fmt.Println("Mapping:     none (not map projected)")
		return
	}
	fmt.Printf("Target:      %s (a=%s m, b=%s m)\n", m.Str("TargetName"), m.Str("EquatorialRadius"), m.Str("PolarRadius"))
	if c.Proj == nil {
		fmt.Printf("Projection:  %s (%v)\n", m.Str("ProjectionName"), c.ProjErr)
		return
	}
	fmt.Printf("Projection:  %s\n", c.Proj)
	fmt.Printf("PROJ:        %s\n", c.Proj.PROJ())
	fmt.Printf("Resolution:  %s m/px", num(c.Grid.ResX))
	if s := m.Str("Scale"); s != "" {
		fmt.Printf(" (%s px/deg)", s)
	}
	fmt.Println()
	fmt.Printf("Extent:      x %s .. %s, y %s .. %s\n", num(c.Grid.X0), num(c.Grid.X0+float64(c.W)*c.Grid.ResX),
		num(c.Grid.Y0-float64(c.H)*c.Grid.ResY), num(c.Grid.Y0))
	fmt.Printf("Lat/Lon:     lat %s .. %s, lon %s .. %s (%s, %s, domain %s)\n", m.Str("MinimumLatitude"), m.Str("MaximumLatitude"),
		m.Str("MinimumLongitude"), m.Str("MaximumLongitude"), m.Str("LatitudeType"), m.Str("LongitudeDirection"), m.Str("LongitudeDomain"))
}
