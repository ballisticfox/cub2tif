package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"cub2tif/internal/convert"
	"cub2tif/internal/isis"
	"cub2tif/internal/proj"
	"cub2tif/internal/raster"
)

// The guided flow, used when cub2tif is started with no flags: double-clicked,
// or with cubes dropped onto it. It asks for the input, reports what it found
// on disk before anything is written, walks through the map and output
// choices, shows a review screen, and ends by printing the equivalent command
// line so the same conversion can be scripted.
//
// The wizard only gathers an options struct; convertAll runs it, so a guided
// run and a scripted one execute the same code.

type wizCube struct {
	path string
	cube *isis.Cube // label only; the file handle is closed after scanning
	err  error
}

type wizard struct {
	o      convert.Options
	args   []string // inputs as the user gave them, for the echoed command line
	inputs []string // expanded list of cubes
	cubes  []wizCube
	ref    string // path of the mapped cube the map defaults come from
}

func runWizard(seed []string) int {
	enableVT()
	fmt.Println()
	fmt.Printf("  cub2tif %s\n", version)
	rule()
	fmt.Println("  Converts ISIS3 cubes (.cub) to GeoTIFF, optionally reprojecting them.")
	fmt.Println("  Run with --help for the scripted interface.")
	fmt.Println()

	w := &wizard{o: convert.Defaults()}
	if !w.askInputs(seed) {
		return 0
	}
	w.askNormalize()
	if w.ref != "" {
		if !w.askMap() {
			return 0
		}
	}
	if !w.askOutput() {
		return 0
	}
	if !w.review() {
		return 0
	}
	fmt.Println()
	failed := convert.Batch(w.inputs, &w.o)
	fmt.Println()
	fmt.Println("  Same conversion from the command line:")
	fmt.Println("      " + w.commandLine())
	if failed > 0 {
		return 1
	}
	return 0
}

// ── input ──────────────────────────────────────────────────────────────────

func (w *wizard) askInputs(seed []string) bool {
	for {
		args := seed
		seed = nil
		if len(args) == 0 {
			fmt.Println("  Input cube(s) or folder (drag onto this window, or paste the path):")
			line, ok := prompt()
			if !ok {
				return false
			}
			args = splitPaths(line)
			if len(args) == 0 {
				continue
			}
		}
		inputs, err := expandInputs(args)
		if err != nil {
			warn(err.Error())
			fmt.Println()
			continue
		}
		w.args, w.inputs = args, inputs
		if w.scan() {
			return true
		}
	}
}

// scan reads every label and shows what is there. False if nothing usable.
func (w *wizard) scan() bool {
	w.cubes = w.cubes[:0]
	w.ref = ""
	good := 0
	for _, p := range w.inputs {
		c, err := isis.Open(p)
		wc := wizCube{path: p, cube: c, err: err}
		if err == nil {
			c.Close()
			good++
			if w.ref == "" && c.Proj != nil {
				w.ref = p
			}
		}
		w.cubes = append(w.cubes, wc)
	}

	fmt.Println()
	if len(w.inputs) == 1 {
		fmt.Printf("  Found %s\n\n", w.inputs[0])
	} else {
		fmt.Printf("  Found %d files in %s\n\n", len(w.inputs), commonDir(w.inputs))
	}
	const maxRows = 15
	for i, wc := range w.cubes {
		if i == maxRows {
			fmt.Printf("      ... and %d more\n", len(w.cubes)-maxRows)
			break
		}
		name := filepath.Base(wc.path)
		if wc.err != nil {
			fmt.Printf("    ! %-24s %v\n", name, wc.err)
			continue
		}
		fmt.Printf("      %-24s %s\n", name, cubeSummary(wc.cube))
	}
	// errors past the visible rows still need to be seen
	for i, wc := range w.cubes {
		if i >= maxRows && wc.err != nil {
			fmt.Printf("    ! %-24s %v\n", filepath.Base(wc.path), wc.err)
		}
	}
	fmt.Println()

	if good == 0 {
		warn("none of these can be converted. Pick another input.")
		fmt.Println()
		return false
	}
	if bad := len(w.inputs) - good; bad > 0 {
		// drop the unreadable ones so the run and the echoed command agree
		var keep []string
		for _, wc := range w.cubes {
			if wc.err == nil {
				keep = append(keep, wc.path)
			}
		}
		if !confirm(fmt.Sprintf("  Skip the %d file(s) that can't be read and continue?", bad), true) {
			return false
		}
		w.inputs, w.args = keep, keep
		fmt.Println()
	}
	if w.ref == "" {
		fmt.Println("  No map projection in these cubes, so they are converted without georeferencing.")
		fmt.Println()
	}
	return true
}

func cubeSummary(c *isis.Cube) string {
	bands := "1 band "
	if c.B > 1 {
		bands = fmt.Sprintf("%d bands", c.B)
	}
	s := fmt.Sprintf("%6d x %-6d %-7s %s", c.W, c.H, c.Type, bands)
	switch {
	case c.Proj != nil:
		target := c.Proj.Body.Name
		s += fmt.Sprintf("  %-17s %-8s %s m/px", c.Proj.DisplayName(), target, raster.FormatNum(raster.Round6(c.Grid.ResX)))
	case c.Mapping != nil:
		s += fmt.Sprintf("  %s (not supported)", c.Mapping.Str("ProjectionName"))
	default:
		s += "  not map projected"
	}
	return s
}

func commonDir(paths []string) string {
	d := filepath.Dir(paths[0])
	for _, p := range paths[1:] {
		for !strings.HasPrefix(filepath.Dir(p)+string(filepath.Separator), d+string(filepath.Separator)) {
			nd := filepath.Dir(d)
			if nd == d {
				return d
			}
			d = nd
		}
	}
	if abs, err := filepath.Abs(d); err == nil {
		return abs
	}
	return d
}

func (w *wizard) refCube() *isis.Cube {
	c, err := isis.Open(w.ref)
	if err != nil {
		return nil
	}
	return c
}

// ── value range ──────────────────────────────────────────────────────────

// askNormalize offers to normalize when the values would land outside the 0-1
// range image editors work in. A sampled look is enough to decide; the
// conversion itself uses each cube's exact range.
func (w *wizard) askNormalize() {
	lo, hi := math.Inf(1), math.Inf(-1)
	checked, candidates := 0, 0
	for _, wc := range w.cubes {
		if wc.err != nil || checked == 25 {
			continue
		}
		c, err := isis.Open(wc.path)
		if err != nil {
			continue
		}
		checked++
		// unsigned integers open in editors as they are (0..max maps to 0..1)
		if pt, _ := convert.OutputType(c, &w.o); pt.IsFloat() || pt == raster.Int8 || pt == raster.Int16 || pt == raster.Int32 {
			bands := make([]int, c.B)
			for i := range bands {
				bands[i] = i
			}
			l, h := convert.SampledRange(c, bands, w.o.Raw)
			lo, hi = math.Min(lo, l), math.Max(hi, h)
			candidates++
		}
		c.Close()
	}
	if candidates == 0 || math.IsInf(lo, 1) || (lo >= 0 && hi <= 1) {
		return
	}
	where := "sampled"
	if len(w.inputs) > 1 {
		where = fmt.Sprintf("sampled across %d cubes", checked)
	}
	warn(fmt.Sprintf("Pixel values run from about %s to %s (%s).", raster.FormatNum(raster.Round6(lo)), raster.FormatNum(raster.Round6(hi)), where))
	fmt.Println("      GIMP and most image editors only handle 0-1 in floating point images.")
	fmt.Println("      Normalizing maps each cube's exact range onto 0-1 and writes a .lbl beside")
	fmt.Println("      each .tif with the scale and offset, so the real values stay recoverable:")
	fmt.Println("          value = DN * Multiplier + Base")
	fmt.Println("      No-data becomes transparency. Keep the values as they are for GIS use.")
	fmt.Println()
	w.o.Normalize = confirm("  Normalize to 0-1 for image editing?", false)
	fmt.Println()
}

// ── map projection ─────────────────────────────────────────────────────────

type projChoice struct {
	key, label, detail string
}

func (w *wizard) askMap() bool {
	c := w.refCube()
	if c == nil {
		return false
	}
	defer c.Close()

	choices := []projChoice{
		{"keep", "Keep " + c.Proj.DisplayName(), "no reprojection (optionally crop)"},
		{"geographic", "Geographic", "plain lon/lat degree grid"},
		{"ps:north", "Polar stereographic, north", "north polar map"},
		{"ps:south", "Polar stereographic, south", "south polar map"},
		{"orthographic", "Orthographic", "globe view centred on a point"},
		{"sinusoidal", "Sinusoidal", "equal-area global map"},
		{"mercator", "Mercator", "conformal, stops at +/-85 deg"},
		{"lambertazimuthal", "Lambert azimuthal equal area", "regional, equal area"},
		{"lambertconformal", "Lambert conformal conic", "mid-latitude regions"},
		{"transversemercator", "Transverse Mercator", "long north-south strips"},
		{"custom", "Custom...", "any projection spec or PROJ string"},
	}
	items := make([]menuItem, len(choices))
	for i, ch := range choices {
		items[i] = menuItem{ch.label, ch.detail}
	}
	if len(w.inputs) > 1 {
		fmt.Printf("  Map defaults below come from %s.\n", filepath.Base(w.ref))
	}
	def := 0
	for {
		pick := selectOne("Output map projection?", items, def)
		if pick < 0 {
			return false
		}
		def = pick
		ch := choices[pick]
		w.o.Proj, w.o.Res, w.o.Bounds = "", "", ""
		ok, back := w.askProjParams(c, ch)
		if !ok {
			return false
		}
		if back {
			continue
		}
		if w.askArea(c, ch.key) {
			return true
		}
	}
}

// askProjParams asks the parameters worth asking for the chosen projection.
// back is true when the user should return to the projection menu.
func (w *wizard) askProjParams(c *isis.Cube, ch projChoice) (ok, back bool) {
	clon, clat := c.Center()
	clat = c.Proj.Body.ConvertLat(clat*proj.D2R, proj.Ocentric, c.LatType()) * proj.R2D
	clon, clat = math.Round(clon*100)/100+0, math.Round(clat*100)/100+0
	// center asks for a projection center and returns the spec parameters,
	// empty when the defaults (the middle of the input) were kept.
	center := func() ([]string, bool) {
		la, ch1, ok := askFloat("    Center latitude ", clat, raster.FormatNum(clat), -90, 90)
		if !ok {
			return nil, false
		}
		lo, ch2, ok := askFloat("    Center longitude", clon, raster.FormatNum(clon), -360, 360)
		if !ok {
			return nil, false
		}
		if ch1 || ch2 {
			return []string{"clat=" + raster.FormatNum(la), "clon=" + raster.FormatNum(lo)}, true
		}
		return nil, true
	}
	spec := func(name string, params []string) string {
		if len(params) == 0 {
			return name
		}
		return name + ":" + strings.Join(params, ",")
	}

	switch ch.key {
	case "keep":
	case "geographic", "sinusoidal", "mercator":
		w.o.Proj = ch.key
	case "ps:north", "ps:south":
		w.o.Proj = ch.key
		north := ch.key == "ps:north"
		label, lim := "    Include down to latitude", 0.0
		if !north {
			label = "    Include up to latitude  "
		}
		v, changed, ok := askFloat(label, lim, "0 = the whole hemisphere", -89, 89)
		if !ok {
			return false, false
		}
		if changed {
			if north {
				w.o.Bounds = fmt.Sprintf("-180,%s,180,90", raster.FormatNum(v))
			} else {
				w.o.Bounds = fmt.Sprintf("-180,-90,180,%s", raster.FormatNum(v))
			}
		}
	case "orthographic", "lambertazimuthal", "transversemercator":
		params, ok := center()
		if !ok {
			return false, false
		}
		w.o.Proj = spec(ch.key, params)
	case "lambertconformal":
		lo := c.Mapping.FloatOr("MinimumLatitude", clat-10)
		hi := c.Mapping.FloatOr("MaximumLatitude", clat+10)
		p1 := math.Round((lo+(hi-lo)/6)*100) / 100
		p2 := math.Round((hi-(hi-lo)/6)*100) / 100
		for {
			s, ok := askLine("    Standard parallels (lat1,lat2)", raster.FormatNum(p1)+","+raster.FormatNum(p2))
			if !ok {
				return false, false
			}
			v, err := convert.ParseFloats(strings.ReplaceAll(s, " ", ""), 2)
			if err != nil || math.Abs(v[0]) >= 90 || math.Abs(v[1]) >= 90 || math.Abs(v[0]+v[1]) < 1e-6 {
				warn("Enter two latitudes like 20,60 (not symmetric about the equator).")
				continue
			}
			p1, p2 = v[0], v[1]
			break
		}
		params, ok := center()
		if !ok {
			return false, false
		}
		w.o.Proj = spec("lcc", append([]string{"par1=" + raster.FormatNum(p1), "par2=" + raster.FormatNum(p2)}, params...))
	case "custom":
		fmt.Println("    Examples:  ortho:clat=30,clon=120   eqc:clat=45   ps:south,clon=180")
		fmt.Println("               \"+proj=stere +lat_0=90 +lat_ts=70 +lon_0=45\"   (enter to go back)")
		for {
			s, ok := askLine("    Projection", "")
			if !ok {
				return false, false
			}
			s = strings.Trim(strings.TrimSpace(s), `"'`)
			if s == "" {
				fmt.Println()
				return true, true
			}
			if _, err := convert.ParseProjSpec(s, c, ""); err != nil {
				warn(err.Error())
				continue
			}
			w.o.Proj = s
			break
		}
	}
	return true, false
}

// askArea asks for resolution and an optional lon/lat box, then plans the
// grid to prove the choice works. False sends the user back to the menu.
func (w *wizard) askArea(c *isis.Cube, key string) bool {
	polar := strings.HasPrefix(key, "ps:")
	if !polar {
		verb := "Limit to"
		if key == "keep" {
			verb = "Crop to"
		}
		for {
			s, ok := askLine("    "+verb+" a lon/lat box W,S,E,N", "whole map")
			if !ok {
				return false
			}
			if s == "whole map" {
				w.o.Bounds = ""
				break
			}
			v, err := convert.ParseFloats(strings.ReplaceAll(s, " ", ""), 4)
			if err != nil || v[2] <= v[0] || v[3] <= v[1] || v[1] < -90 || v[3] > 90 {
				warn("Enter west,south,east,north in degrees, e.g. -30,-10,30,20.")
				continue
			}
			w.o.Bounds = strings.ReplaceAll(s, " ", "")
			break
		}
	}
	if key == "keep" && w.o.Bounds == "" {
		fmt.Println()
		return true // straight conversion: nothing to plan
	}

	p, grid, _, err := convert.PlanWarp(c, &w.o)
	if err != nil {
		fmt.Println()
		warn(err.Error())
		fmt.Println()
		return false
	}
	if key != "keep" {
		unit := "m"
		if p.Geo {
			unit = "deg"
		}
		def := grid.ResX
		v, changed, ok := askFloat(fmt.Sprintf("    Pixel size (%s)", unit), def, raster.FormatNum(raster.Round6(def))+" = same detail as input", 1e-9, 1e12)
		if !ok {
			return false
		}
		if changed {
			w.o.Res = raster.FormatNum(v)
			if _, grid, _, err = convert.PlanWarp(c, &w.o); err != nil {
				warn(err.Error())
				fmt.Println()
				return false
			}
		}
	}
	fmt.Println()
	fmt.Printf("  Output grid: %d x %d px\n", grid.W, grid.H)
	if float64(grid.W)*float64(grid.H) > 16*float64(c.W)*float64(c.H) {
		warn("that is much larger than the input; a coarser pixel size or a smaller area may be wiser.")
		if !confirm("  Keep it anyway?", false) {
			fmt.Println()
			return false
		}
	}
	fmt.Println()
	return true
}

// ── output ─────────────────────────────────────────────────────────────────

func (w *wizard) askOutput() bool {
	single := len(w.inputs) == 1
	for {
		if single {
			def, _ := convert.OutputPath(w.inputs[0], &convert.Options{}, false)
			fmt.Printf("  Output file (enter for %s):\n", def)
			s, ok := prompt()
			if !ok {
				return false
			}
			s = cleanPath(s)
			w.o.Out = ""
			if s != "" {
				if st, err := os.Stat(s); (err == nil && st.IsDir()) || strings.HasSuffix(s, "\\") || strings.HasSuffix(s, "/") {
					s = filepath.Join(s, filepath.Base(def))
				} else if ext := strings.ToLower(filepath.Ext(s)); ext != ".tif" && ext != ".tiff" {
					s += ".tif"
				}
				if abs, _ := filepath.Abs(s); abs != "" {
					if def2, _ := filepath.Abs(def); !strings.EqualFold(abs, def2) {
						w.o.Out = s
					}
				}
			}
		} else {
			fmt.Println("  Output folder (enter to write each .tif next to its cube):")
			s, ok := prompt()
			if !ok {
				return false
			}
			w.o.Out = cleanPath(s)
			if w.o.Out != "" {
				if st, err := os.Stat(w.o.Out); err == nil && !st.IsDir() {
					warn("that is a file, not a folder.")
					continue
				} else if err != nil {
					fmt.Println("      does not exist yet - it will be created.")
				}
			}
		}

		// what would be overwritten
		exists, clash := 0, false
		for _, in := range w.inputs {
			out := w.plannedOutput(in)
			a1, _ := filepath.Abs(out)
			a2, _ := filepath.Abs(in)
			if strings.EqualFold(a1, a2) {
				clash = true
			}
			if _, err := os.Stat(out); err == nil {
				exists++
			}
		}
		if clash {
			warn("that would overwrite an input cube. Pick another name.")
			continue
		}
		if exists > 0 {
			if single {
				warn("that file already exists and will be replaced.")
			} else {
				warn(fmt.Sprintf("%d of the output files already exist and will be replaced.", exists))
			}
			if !confirm("      Continue?", true) {
				continue
			}
		}
		fmt.Println()
		return true
	}
}

// plannedOutput is where the conversion will write the TIFF for in.
func (w *wizard) plannedOutput(in string) string {
	out, err := convert.OutputPath(in, &w.o, len(w.inputs) > 1)
	if err != nil {
		return ""
	}
	return out
}

// ── review + settings ──────────────────────────────────────────────────────

func (w *wizard) review() bool {
	for {
		rule()
		if len(w.inputs) == 1 {
			fmt.Printf("    Input     %s\n", w.inputs[0])
			fmt.Printf("    Output    %s\n", w.plannedOutput(w.inputs[0]))
		} else {
			fmt.Printf("    Input     %d cubes in %s\n", len(w.inputs), commonDir(w.inputs))
			out := w.o.Out
			if out == "" {
				out = "next to each cube"
			}
			fmt.Printf("    Output    %s\n", out)
		}
		c := w.refCube()
		if c == nil {
			c, _ = isis.Open(w.inputs[0])
		}
		if c != nil {
			if c.Proj != nil {
				if w.o.WantsWarp() {
					if p, g, _, err := convert.PlanWarp(c, &w.o); err == nil {
						unit := "m/px"
						if p.Geo {
							unit = "deg/px"
						}
						fmt.Printf("    Map       %s\n", c.Proj.DisplayName()+" -> "+p.String())
						fmt.Printf("    Grid      %d x %d px at %s %s", g.W, g.H, raster.FormatNum(raster.Round6(g.ResX)), unit)
						if w.o.Bounds != "" {
							fmt.Printf(", lon/lat box %s", w.o.Bounds)
						}
						fmt.Println()
					}
				} else {
					fmt.Printf("    Map       %s, unchanged\n", c.Proj.String())
				}
			}
			pt, _ := convert.OutputType(c, &w.o)
			pix := pt.String()
			if w.o.Normalize {
				pix = "float32 normalized 0-1 per cube, no-data as alpha; scale + offset in .lbl"
			} else if w.o.Stretch != "" {
				pix = "uint8, stretched " + w.o.Stretch + " (0 = no data)"
			} else if pt.IsFloat() {
				pix += ", ISIS special pixels -> nodata"
			}
			fmt.Printf("    Pixels    %s\n", pix)
			c.Close()
		}
		settings := []string{w.o.Compress}
		if w.o.Overviews && w.o.Normalize {
			settings = append(settings, "no overviews (they'd open as extra layers)")
		} else if w.o.Overviews {
			settings = append(settings, "overviews")
		} else {
			settings = append(settings, "no overviews")
		}
		if w.o.WantsWarp() {
			settings = append(settings, w.o.Resample)
		}
		settings = append(settings, fmt.Sprintf("%d threads", w.o.Threads))
		fmt.Printf("    Settings  %s\n", strings.Join(settings, " | "))
		rule()
		fmt.Println()
		fmt.Println("  [enter] convert   [e] edit settings   [q] quit")
		switch actionKey("e") {
		case "enter":
			return true
		case "e":
			fmt.Println()
			w.editSettings()
		default:
			return false
		}
	}
}

func (w *wizard) editSettings() {
	comps := []string{"deflate", "zstd", "none"}
	def := indexOf(comps, w.o.Compress)
	if i := selectOne("Compression?", []menuItem{
		{"deflate", "smallest compatible choice; every GIS reads it"},
		{"zstd", "faster and a bit smaller; needs GDAL 2.3+ / QGIS 3"},
		{"none", "largest files, fastest to write"},
	}, max(def, 0)); i >= 0 {
		w.o.Compress = comps[i]
	}

	w.o.Overviews = confirm("    Build overviews for fast display in QGIS/ArcGIS?", w.o.Overviews)
	fmt.Println()

	pixDef := 0
	switch {
	case w.o.Normalize:
		pixDef = 1
	case w.o.Stretch != "":
		pixDef = 2
	}
	if i := selectOne("Pixel values?", []menuItem{
		{"Keep values", "full precision, for analysis in GIS"},
		{"Normalized 0-1", "for GIMP / image editors; .lbl keeps the way back to real values"},
		{"8-bit stretched", "small browse image, 0.5-99.5% stretch"},
	}, pixDef); i >= 0 {
		w.o.Normalize = i == 1
		w.o.Stretch = ""
		if i == 2 {
			w.o.Stretch = "auto"
		}
	}

	if w.o.WantsWarp() {
		rs := []string{"nearest", "bilinear", "cubic"}
		if i := selectOne("Resampling?", []menuItem{
			{"nearest", "exact source values; use for classes/masks"},
			{"bilinear", "smooth; good default for DEMs and images"},
			{"cubic", "sharper; slowest"},
		}, max(indexOf(rs, w.o.Resample), 0)); i >= 0 {
			w.o.Resample = rs[i]
		}
	}
	w.o.Threads = askInt("    Worker threads", w.o.Threads, 1, 1024)
	fmt.Println()
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if strings.EqualFold(v, s) {
			return i
		}
	}
	return -1
}

// ── the equivalent command line ────────────────────────────────────────────

// commandLine prints the flags that reproduce this run. Only settings that
// differ from the defaults are emitted, so the line stays readable.
func (w *wizard) commandLine() string {
	d := convert.Defaults()
	o := &w.o
	parts := []string{"cub2tif"}
	for _, a := range w.args {
		parts = append(parts, shellQuote(a))
	}
	add := func(flag, v string) { parts = append(parts, flag, shellQuote(v)) }
	if o.Proj != "" {
		add("-p", o.Proj)
	}
	if o.Bounds != "" {
		add("--bounds", o.Bounds)
	}
	if o.Res != "" {
		add("--res", o.Res)
	}
	if o.WantsWarp() && o.Resample != d.Resample {
		add("--resample", o.Resample)
	}
	if o.Stretch != "" {
		add("--stretch", o.Stretch)
	}
	if o.Normalize {
		parts = append(parts, "--normalize")
	}
	if o.Compress != d.Compress {
		add("--compress", o.Compress)
	}
	if o.Overviews {
		parts = append(parts, "--overviews")
	}
	if o.Threads != d.Threads {
		add("--threads", fmt.Sprint(o.Threads))
	}
	if o.Out != "" {
		// no trailing separator: before a closing quote it would escape it
		add("-o", strings.TrimRight(o.Out, `\/`))
	}
	return strings.Join(parts, " ")
}
