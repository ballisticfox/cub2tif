package convert

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"cub2tif/internal/isis"
)

// a small global equirectangular Titan cube, enough to parse specs against
func testCube(t *testing.T) *isis.Cube {
	t.Helper()
	label := `Object = IsisCube
  Object = Core
    StartByte = 1025
    Format = BandSequential
    Group = Dimensions
      Samples = 8
      Lines = 4
      Bands = 1
    End_Group
    Group = Pixels
      Type = Real
      ByteOrder = Lsb
      Base = 0.0
      Multiplier = 1.0
    End_Group
  End_Object
  Group = Mapping
    ProjectionName = Equirectangular
    CenterLongitude = 0.0
    TargetName = TITAN
    EquatorialRadius = 2575000.0 <meters>
    PolarRadius = 2575000.0 <meters>
    LatitudeType = Planetocentric
    LongitudeDirection = PositiveEast
    LongitudeDomain = 180
    MinimumLatitude = -90.0
    MaximumLatitude = 90.0
    MinimumLongitude = -180.0
    MaximumLongitude = 180.0
    UpperLeftCornerX = -8089601.0 <meters>
    UpperLeftCornerY = 4044800.5 <meters>
    PixelResolution = 2022400.0 <meters/pixel>
    CenterLatitude = 0.0
  End_Group
End_Object
End
`
	p := filepath.Join(t.TempDir(), "t.cub")
	buf := make([]byte, 1024+8*4*4)
	copy(buf, label)
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := isis.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestParseProjSpec(t *testing.T) {
	c := testCube(t)
	ok := []struct{ spec, wantPROJ string }{
		{"", "+proj=eqc"},
		{"ps:south", "+proj=stere +lat_0=-90"},
		{"polarstereographic:clat=-71", "+lat_ts=-71"},
		{"ortho:clat=30,clon=120", "+proj=ortho +lat_0=30 +lon_0=120"},
		{"lcc:par1=20,par2=60,clat=40,clon=-100", "+lat_1=20 +lat_2=60 +lat_0=40"},
		{"geographic", "+proj=longlat +R=2575000"},
		{"+proj=stere +lat_0=90 +lat_ts=70 +lon_0=45 +no_defs +type=crs", "+lat_ts=70 +lon_0=45"},
		{"+proj=merc +lat_0=30", "+proj=merc +lon_0=0 +k=1"}, // lat_0 is not Mercator's true-scale latitude
		{"+proj=eqc +lat_ts=45 +lat_0=10", "+lat_ts=45 +lat_0=10"},
		{"+proj=sinu +R=1737400", "+R=1737400"},
		{"+proj=laea +lat_0=-90 +a=3396190 +b=3376200", "+a=3396190 +b=3376200"},
	}
	for _, tc := range ok {
		p, err := ParseProjSpec(tc.spec, c, "")
		if err != nil {
			t.Errorf("%q: %v", tc.spec, err)
			continue
		}
		if !strings.Contains(p.PROJ(), tc.wantPROJ) {
			t.Errorf("%q -> %s, want it to contain %q", tc.spec, p.PROJ(), tc.wantPROJ)
		}
	}
	bad := []string{
		"robinson",
		"ortho:clat=10,foo=3",
		"ps:sideways",
		"+proj=longlat +ellps=WGS84", // an Earth ellipsoid would be silently ignored
		"+proj=stere +lat_0=45",      // oblique stereographic
		"+proj=tmerc +x_0=500000",
		"lcc:par1=30,par2=-30",
	}
	for _, spec := range bad {
		if _, err := ParseProjSpec(spec, c, ""); err == nil {
			t.Errorf("%q should be rejected", spec)
		}
	}
	if _, err := ParseProjSpec("", c, "ographic"); err == nil {
		t.Error("--lat-type without --proj should be rejected")
	}
}

func TestOutputPath(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "maps", "dtm.cub")
	cases := []struct {
		out     string
		multi   bool
		want    string
		wantErr bool
	}{
		{"", false, filepath.Join(dir, "maps", "dtm.tif"), false},
		{filepath.Join(dir, "x.tif"), false, filepath.Join(dir, "x.tif"), false},
		{dir, false, filepath.Join(dir, "dtm.tif"), false},                                    // existing folder
		{filepath.Join(dir, "new") + "/", false, filepath.Join(dir, "new", "dtm.tif"), false}, // trailing separator
		{filepath.Join(dir, "batch"), true, filepath.Join(dir, "batch", "dtm.tif"), false},
		{filepath.Join(dir, "all.tif"), true, "", true}, // a file name for several inputs
	}
	for _, tc := range cases {
		o := Defaults()
		o.Out = tc.out
		got, err := OutputPath(in, &o, tc.multi)
		if (err != nil) != tc.wantErr || (!tc.wantErr && filepath.Clean(got) != filepath.Clean(tc.want)) {
			t.Errorf("OutputPath(%q, multi=%v) = %q, %v; want %q", tc.out, tc.multi, got, err, tc.want)
		}
	}
}

func TestDefaultNoData(t *testing.T) {
	c := testCube(t)
	o := Defaults()
	typ, err := OutputType(c, &o)
	if err != nil || typ.String() != "float32" {
		t.Fatalf("OutputType = %v, %v", typ, err)
	}
	// the shortest float32 spelling must parse back to the ISIS Null exactly
	s := formatNoData(defaultNoData(typ), typ)
	v, err := strconv.ParseFloat(s, 32)
	if err != nil || float32(v) != float32(isis.Null4) {
		t.Errorf("float32 no-data %q does not round-trip to ISIS Null", s)
	}
}
