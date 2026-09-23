package convert

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"cub2tif/internal/isis"
	"cub2tif/internal/proj"
)

// ParseProjSpec builds the target projection from a --proj value: either
// "name[:key=value,...]" (see proj.CanonicalName for names) or a PROJ
// string. Parameters left out default from the cube: its body, latitude
// type, central longitude and, for centered projections, its middle.
// An empty spec means the cube's own projection.
func ParseProjSpec(spec string, c *isis.Cube, latType string) (*proj.Projection, error) {
	src := c.Proj
	spec = strings.TrimSpace(spec)
	if spec == "" {
		if latType != "" {
			return nil, fmt.Errorf("--lat-type needs --proj")
		}
		return src, nil
	}
	pp := proj.Params{Body: src.Body, LatType: c.LatType()}
	if latType != "" {
		lt, err := ParseLatType(latType)
		if err != nil {
			return nil, err
		}
		pp.LatType = lt
	}

	var kv map[string]string
	var flags []string
	var err error
	if strings.HasPrefix(spec, "+") || strings.Contains(spec, "+proj=") {
		kv, flags, err = parsePROJ(spec, &pp)
	} else {
		kv, flags, err = parseShortSpec(spec, &pp)
	}
	if err != nil {
		return nil, err
	}

	// take the first of several alias keys, dropping the rest
	take := func(keys ...string) (float64, bool, error) {
		var val float64
		found := false
		for _, k := range keys {
			v, ok := kv[k]
			if !ok {
				continue
			}
			delete(kv, k)
			if found {
				continue
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return 0, false, fmt.Errorf("bad value for %s: %q", k, v)
			}
			val, found = f, true
		}
		return val, found, nil
	}
	var hasPar1, hasPar2 bool
	var r, a, b float64
	var hasR, hasA, hasB bool
	for _, t := range []struct {
		dst  *float64
		has  *bool
		keys []string
	}{
		{&pp.CLat, &pp.HasCLat, []string{"clat", "lat_0", "centerlatitude", "lat"}},
		{&pp.CLon, &pp.HasCLon, []string{"clon", "lon_0", "centerlongitude", "lon"}},
		{&pp.Par1, &hasPar1, []string{"par1", "lat_1", "firststandardparallel"}},
		{&pp.Par2, &hasPar2, []string{"par2", "lat_2", "secondstandardparallel"}},
		{&pp.K0, new(bool), []string{"k", "k_0", "scale", "scalefactor"}},
		{&r, &hasR, []string{"r", "radius"}},
		{&a, &hasA, []string{"a"}},
		{&b, &hasB, []string{"b"}},
	} {
		if *t.dst, *t.has, err = take(t.keys...); err != nil {
			return nil, err
		}
	}
	// the body: +R is a sphere, +a alone too (as in PROJ), +b flattens it
	switch {
	case hasR:
		pp.Body.A, pp.Body.B = r, r
	case hasA:
		pp.Body.A, pp.Body.B = a, a
	}
	if hasB {
		pp.Body.B = b
	}
	if len(kv) > 0 {
		var unknown []string
		for k := range kv {
			unknown = append(unknown, k)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown projection parameter(s): %s", strings.Join(unknown, ", "))
	}
	for _, f := range flags {
		switch f {
		case "north", "n":
			pp.CLat, pp.HasCLat = 90, true
		case "south", "s":
			pp.CLat, pp.HasCLat = -90, true
		default:
			return nil, fmt.Errorf("unknown projection flag %q", f)
		}
	}
	applyDefaults(&pp, c, hasPar1, hasPar2)
	return proj.New(pp)
}

// parseShortSpec reads "name:key=value,key=value,flag".
func parseShortSpec(spec string, pp *proj.Params) (map[string]string, []string, error) {
	name, rest, _ := strings.Cut(spec, ":")
	cn, ok := proj.CanonicalName(name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown projection %q", name)
	}
	pp.Name = cn
	kv := map[string]string{}
	var flags []string
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok {
			kv[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		} else {
			flags = append(flags, strings.ToLower(part))
		}
	}
	return kv, flags, nil
}

// parsePROJ reads a PROJ string, translating PROJ's parameter names to the
// short spec's where their meaning differs.
func parsePROJ(spec string, pp *proj.Params) (map[string]string, []string, error) {
	kv := map[string]string{}
	var flags []string
	for _, tok := range strings.Fields(spec) {
		tok = strings.TrimPrefix(tok, "+")
		if k, v, ok := strings.Cut(tok, "="); ok {
			kv[strings.ToLower(k)] = v
		} else {
			flags = append(flags, strings.ToLower(tok))
		}
	}
	// the body comes from the cube unless +R/+a/+b say otherwise; named
	// ellipsoids and datums are Earth's and would silently be ignored
	for _, k := range []string{"ellps", "datum", "towgs84", "nadgrids"} {
		if _, ok := kv[k]; ok {
			return nil, nil, fmt.Errorf("+%s is not supported; give the body shape with +R or +a/+b", k)
		}
	}
	name := kv["proj"]
	delete(kv, "proj")
	switch name {
	case "eqc":
		pp.Name = "equirectangular"
		if v, ok := kv["lat_0"]; ok { // origin, not the true-scale latitude
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("bad lat_0 %q", v)
			}
			pp.Lat0 = f
			delete(kv, "lat_0")
		}
		renameKey(kv, "lat_ts", "clat")
	case "longlat", "latlong", "lonlat", "latlon":
		pp.Name = "geographic"
	case "sinu":
		pp.Name = "sinusoidal"
	case "ortho":
		pp.Name = "orthographic"
	case "stere", "ups":
		pp.Name = "polarstereographic"
		if l0, _ := strconv.ParseFloat(kv["lat_0"], 64); math.Abs(math.Abs(l0)-90) > 1e-9 {
			return nil, nil, fmt.Errorf("only polar stereographic (+lat_0=90 or -90) is supported")
		}
		if _, ok := kv["lat_ts"]; ok { // variant B: the true-scale latitude picks the pole too
			delete(kv, "lat_0")
			renameKey(kv, "lat_ts", "clat")
		}
	case "merc":
		pp.Name = "mercator"
		delete(kv, "lat_0") // PROJ ignores it for Mercator
		renameKey(kv, "lat_ts", "clat")
	case "tmerc", "etmerc":
		pp.Name = "transversemercator"
	case "lcc":
		pp.Name = "lambertconformal"
	case "laea":
		pp.Name = "lambertazimuthal"
	default:
		return nil, nil, fmt.Errorf("unsupported PROJ projection %q", name)
	}
	for _, k := range []string{"x_0", "y_0"} {
		if v, ok := kv[k]; ok {
			if f, _ := strconv.ParseFloat(v, 64); f != 0 {
				return nil, nil, fmt.Errorf("false easting/northing (+%s) is not supported", k)
			}
			delete(kv, k)
		}
	}
	for _, k := range []string{"units", "type", "lat_ts"} {
		delete(kv, k)
	}
	var keep []string
	for _, f := range flags {
		if f != "no_defs" && f != "wktext" {
			keep = append(keep, f)
		}
	}
	return kv, keep, nil
}

func renameKey(kv map[string]string, from, to string) {
	if v, ok := kv[from]; ok {
		kv[to] = v
		delete(kv, from)
	}
}

// applyDefaults fills in what the spec left out, from the cube.
func applyDefaults(pp *proj.Params, c *isis.Cube, hasPar1, hasPar2 bool) {
	src := c.Proj
	clon, clat := c.Center() // east degrees, planetocentric
	clatOut := src.Body.ConvertLat(clat*proj.D2R, proj.Ocentric, pp.LatType) * proj.R2D
	centered := pp.Name == "orthographic" || pp.Name == "lambertazimuthal" ||
		pp.Name == "transversemercator" || pp.Name == "lambertconformal"
	if !pp.HasCLon {
		if centered {
			pp.CLon = clon
		} else {
			pp.CLon = src.Lon0 * proj.R2D
		}
	}
	if !pp.HasCLat {
		switch {
		case centered:
			pp.CLat = clatOut
		case pp.Name == "polarstereographic":
			pp.CLat = 90
			if clat < 0 {
				pp.CLat = -90
			}
		case pp.Name == "equirectangular" && src.Name == "equirectangular":
			pp.CLat = src.Spec.CLat // keep the cube's true-scale latitude
		}
	}
	if pp.Name == "lambertconformal" {
		if !hasPar1 { // put the parallels at 1/6 and 5/6 of the latitude range
			lo := c.Mapping.FloatOr("MinimumLatitude", clatOut-10)
			hi := c.Mapping.FloatOr("MaximumLatitude", clatOut+10)
			pp.Par1, pp.Par2 = lo+(hi-lo)/6, hi-(hi-lo)/6
		} else if !hasPar2 {
			pp.Par2 = pp.Par1
		}
	}
}
