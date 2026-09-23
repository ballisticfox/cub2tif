package main

import (
	"os"
	"path/filepath"
	"testing"

	"cub2tif/internal/convert"
)

func TestExpandInputs(t *testing.T) {
	dir := t.TempDir()
	odd := filepath.Join(dir, "maps [v2]")
	for _, p := range []string{
		filepath.Join(dir, "a.cub"),
		filepath.Join(dir, "B.CUB"),
		filepath.Join(dir, "notes.txt"),
		filepath.Join(odd, "c.cub"),
	} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		arg  string
		want int
	}{
		{dir, 2},                             // folder: .cub in any case
		{filepath.Join(dir, "*.cub"), 2},     // pattern, case-insensitive
		{filepath.Join(odd, "c.cub"), 1},     // brackets in an existing path
		{odd, 1},                             // ... and in a folder
		{filepath.Join(odd, "*.cub"), 1},     // ... under a pattern
		{filepath.Join(dir, "notes.txt"), 1}, // an explicit file is taken as given
	}
	for _, tc := range cases {
		got, err := expandInputs([]string{tc.arg})
		if err != nil || len(got) != tc.want {
			t.Errorf("expandInputs(%q) = %v, %v; want %d files", tc.arg, got, err, tc.want)
		}
	}
	for _, arg := range []string{filepath.Join(dir, "missing.cub"), filepath.Join(dir, "*.img")} {
		if _, err := expandInputs([]string{arg}); err == nil {
			t.Errorf("expandInputs(%q) should fail", arg)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	defaults := convert.Defaults()
	var cli cliFlags
	fs := newFlagSet(&defaults, &cli)
	flags, pos, err := splitArgs(fs, []string{"in.cub", "-o", "out.tif", "--bounds", "-180,60,180,90", "--overviews", "--res=500", "other.cub"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 2 || pos[0] != "in.cub" || pos[1] != "other.cub" {
		t.Errorf("positional = %v", pos)
	}
	if err := fs.Parse(flags); err != nil {
		t.Fatal(err)
	}
	if defaults.Out != "out.tif" || defaults.Bounds != "-180,60,180,90" || !defaults.Overviews || defaults.Res != "500" {
		t.Errorf("parsed %+v", defaults)
	}
	if _, _, err := splitArgs(fs, []string{"--bogus"}); err == nil {
		t.Error("unknown flag accepted")
	}
}
