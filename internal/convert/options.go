// Package convert turns ISIS cubes into GeoTIFFs: it resolves the options
// into a plan, reads and optionally reprojects the pixels in parallel tiles,
// and writes the result.
package convert

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Options is everything a conversion can be told. The command line and the
// wizard both fill one in; Defaults gives the starting values.
type Options struct {
	// output file
	Out        string // file, or folder for several inputs; "" = next to the input
	Compress   string // deflate, zstd, none
	Level      int    // compression level; 0 = default for the codec
	Predictor  string // auto, none, 2, 3
	Tile       int
	Overviews  bool
	OvResample string // average, nearest
	BigTIFF    string // auto, yes, no
	Threads    int

	// pixel values
	Type      string // output sample type; "" = from the cube
	Raw       bool   // keep stored DNs instead of applying Base/Multiplier
	Stretch   string // auto, minmax or LO,HI: 8-bit browse image
	Normalize bool   // exact range onto 0-1, for image editors, with a .lbl
	NoData    string // value, "nan" or "none"; "" = ISIS Null for the type
	Bands     string // 1-based, comma separated; "" = all

	// reprojection (any of Proj, Res, Bounds, Extent, Size warps)
	Proj     string // projection spec or PROJ string
	Res      string // "R" or "RX,RY"
	Bounds   string // W,S,E,N degrees
	Extent   string // XMIN,YMIN,XMAX,YMAX target units
	Size     string // W,H pixels
	Resample string // nearest, bilinear, cubic
	LatType  string // ocentric, ographic; "" = the cube's
	Exact    bool   // transform every pixel exactly
	CacheMB  int    // source block cache for warping

	Quiet bool
}

// Defaults are the settings used when nothing is specified.
func Defaults() Options {
	return Options{
		Compress:   "deflate",
		Predictor:  "auto",
		Tile:       512,
		OvResample: "average",
		BigTIFF:    "auto",
		Threads:    runtime.NumCPU(),
		Resample:   "bilinear",
		CacheMB:    1024,
	}
}

// WantsWarp reports whether the options reproject or regrid.
func (o *Options) WantsWarp() bool {
	return o.Proj != "" || o.Res != "" || o.Bounds != "" || o.Extent != "" || o.Size != ""
}

// OutputPath is where the TIFF for input in goes. With several inputs, or
// when Out is an existing folder or ends in a separator, Out is a folder.
func OutputPath(in string, o *Options, multi bool) (string, error) {
	name := strings.TrimSuffix(filepath.Base(in), filepath.Ext(in)) + ".tif"
	if o.Out == "" {
		return filepath.Join(filepath.Dir(in), name), nil
	}
	st, err := os.Stat(o.Out)
	isDir := (err == nil && st.IsDir()) || strings.HasSuffix(o.Out, "/") || strings.HasSuffix(o.Out, `\`)
	if isDir {
		return filepath.Join(o.Out, name), nil
	}
	if multi {
		if ext := strings.ToLower(filepath.Ext(o.Out)); ext == ".tif" || ext == ".tiff" {
			return "", fmt.Errorf("-o %s is a file name, but there are several inputs; give a folder", o.Out)
		}
		return filepath.Join(o.Out, name), nil
	}
	return o.Out, nil
}
