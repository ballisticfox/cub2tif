// Package isis reads ISIS3 cubes: the PVL label, tiled or band-sequential
// pixel data in either byte order, special pixels and the map projection.
package isis

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"cub2tif/internal/proj"
	"cub2tif/internal/pvl"
	"cub2tif/internal/raster"
)

var isisTypes = map[string]raster.PixType{
	"unsignedbyte": raster.Uint8, "signedbyte": raster.Int8, "signedword": raster.Int16, "unsignedword": raster.Uint16,
	"signedinteger": raster.Int32, "unsignedinteger": raster.Uint32, "real": raster.Float32, "double": raster.Float64,
}

// ISIS special pixel values (isis/src/core/include/SpecialPixel.h). Floating
// point values below the valid minimum are Null, Lrs, Lis, His or Hrs.
var (
	validMin4 = float64(math.Float32frombits(0xFF7FFFFA))
	validMin8 = math.Float64frombits(0xFFEFFFFFFFFFFFFA)
	Null4     = float64(math.Float32frombits(0xFF7FFFFB)) // ISIS Null for Real
	Null8     = math.Float64frombits(0xFFEFFFFFFFFFFFFB)  // ISIS Null for Double
)

// Cube is an open ISIS3 cube. Its label is parsed on open; pixels are read
// on demand with ReadRegion, which is safe for concurrent use.
type Cube struct {
	Path     string // the label file
	DataPath string // the pixel file (differs for a detached label)
	f        *os.File
	W, H, B  int   // samples, lines, bands
	Tiled    bool  // Tile format; otherwise BandSequential
	TS, TL   int   // tile samples/lines
	Start    int64 // byte offset of the pixel data
	Type     raster.PixType
	BigEnd   bool    // Msb byte order
	Base     float64 // physical = stored * Mult + Base
	Mult     float64
	Label    *pvl.Node
	Mapping  *pvl.Node        // nil when not map projected
	Proj     *proj.Projection // nil when unprojected or unsupported
	ProjErr  error            // why Proj is nil despite a Mapping group
	Grid     raster.Grid      // valid when Proj != nil
	BandInfo []string         // per-band names from the BandBin group, if any

	tilesX, tilesY int
	bufPool        sync.Pool
}

// Open reads a cube's label and prepares it for reading.
func Open(path string) (*Cube, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	// read the label, growing the buffer until the End statement is found
	var root *pvl.Node
	n := int64(64 << 10)
	for {
		if n > st.Size() {
			n = st.Size()
		}
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, 0); err != nil && err != io.EOF {
			f.Close()
			return nil, err
		}
		var complete bool
		root, complete, err = pvl.Parse(string(buf))
		if err != nil {
			f.Close()
			return nil, err
		}
		if complete || n >= st.Size() || n >= 256<<20 {
			break
		}
		n *= 4
	}
	c := &Cube{Path: path, DataPath: path, f: f, Label: root}
	if err := c.parse(); err != nil {
		c.f.Close()
		return nil, err
	}
	return c, nil
}

func (c *Cube) Close() error { return c.f.Close() }

func (c *Cube) parse() error {
	isis := c.Label.Child("IsisCube")
	if isis == nil {
		return fmt.Errorf("not an ISIS3 cube (no IsisCube object in label)")
	}
	core := isis.Child("Core")
	if core == nil {
		return fmt.Errorf("label has no Core object")
	}
	dims := core.Child("Dimensions")
	pix := core.Child("Pixels")
	if dims == nil || pix == nil {
		return fmt.Errorf("label is missing Core Dimensions/Pixels")
	}
	var err error
	if c.W, err = dims.Int("Samples"); err != nil {
		return err
	}
	if c.H, err = dims.Int("Lines"); err != nil {
		return err
	}
	if c.B, err = dims.Int("Bands"); err != nil {
		return err
	}
	if c.W <= 0 || c.H <= 0 || c.B <= 0 {
		return fmt.Errorf("invalid dimensions %dx%dx%d", c.W, c.H, c.B)
	}
	t, ok := isisTypes[strings.ToLower(pix.Str("Type"))]
	if !ok {
		return fmt.Errorf("unsupported pixel type %q", pix.Str("Type"))
	}
	c.Type = t
	c.BigEnd = strings.EqualFold(pix.Str("ByteOrder"), "Msb")
	c.Base = pix.FloatOr("Base", 0)
	c.Mult = pix.FloatOr("Multiplier", 1)

	sb, err := core.Int("StartByte")
	if err != nil {
		sb = 1
	}
	c.Start = int64(sb) - 1
	if ptr := core.Str("^Core"); ptr != "" {
		// detached label: data lives in another file
		dp := ptr
		if !filepath.IsAbs(dp) {
			dp = filepath.Join(filepath.Dir(c.Path), dp)
		}
		df, err := os.Open(dp)
		if err != nil {
			return fmt.Errorf("opening detached data file: %w", err)
		}
		c.f.Close()
		c.f = df
		c.DataPath = dp
	}
	switch strings.ToLower(core.Str("Format")) {
	case "tile":
		c.Tiled = true
		c.TS, _ = core.Int("TileSamples")
		c.TL, _ = core.Int("TileLines")
		if c.TS <= 0 || c.TL <= 0 {
			return fmt.Errorf("invalid tile size %dx%d", c.TS, c.TL)
		}
		c.tilesX = (c.W + c.TS - 1) / c.TS
		c.tilesY = (c.H + c.TL - 1) / c.TL
	case "bandsequential", "bsq", "":
	default:
		return fmt.Errorf("unsupported cube format %q", core.Str("Format"))
	}

	// sanity check the file size
	if st, err := c.f.Stat(); err == nil {
		need := c.Start + int64(c.W)*int64(c.H)*int64(c.B)*int64(c.Type.Size())
		if c.Tiled {
			need = c.Start + int64(c.tilesX*c.tilesY)*int64(c.TS*c.TL)*int64(c.B)*int64(c.Type.Size())
		}
		if st.Size() < need {
			return fmt.Errorf("file is truncated: %d bytes, cube needs %d", st.Size(), need)
		}
	}

	if bb := isis.Child("BandBin"); bb != nil {
		for _, k := range []string{"FilterName", "Name", "Center", "OriginalBand"} {
			if kk := bb.Key(k); kk != nil && len(kk.Values) == c.B {
				c.BandInfo = kk.Values
				break
			}
		}
	}

	c.Mapping = isis.Child("Mapping")
	if c.Mapping != nil {
		c.Proj, c.ProjErr = projectionFromMapping(c.Mapping)
		if c.Proj != nil {
			res, err1 := c.Mapping.Float("PixelResolution")
			ulx, err2 := c.Mapping.Float("UpperLeftCornerX")
			uly, err3 := c.Mapping.Float("UpperLeftCornerY")
			if err1 != nil || err2 != nil || err3 != nil || res <= 0 {
				c.Proj, c.ProjErr = nil, fmt.Errorf("mapping group lacks PixelResolution/UpperLeftCornerX/Y")
			} else {
				c.Grid = raster.Grid{W: c.W, H: c.H, X0: ulx, Y0: uly, ResX: res, ResY: res}
			}
		}
	}
	return nil
}

// HasScaling reports whether Base/Multiplier alter the stored values.
func (c *Cube) HasScaling() bool { return c.Base != 0 || c.Mult != 1 }

func (c *Cube) getBuf(n int) []byte {
	if b, ok := c.bufPool.Get().(*[]byte); ok && cap(*b) >= n {
		return (*b)[:n]
	}
	return make([]byte, n)
}

func (c *Cube) putBuf(b []byte) { c.bufPool.Put(&b) }

// ReadRegion reads band (0-based) pixels [x0,x0+w) x [y0,y0+h) into dst with
// the given row stride. Special pixels become NaN. If raw is false the ISIS
// Base/Multiplier are applied.
func (c *Cube) ReadRegion(band, x0, y0, w, h int, dst []float64, stride int, raw bool) error {
	if x0 < 0 || y0 < 0 || x0+w > c.W || y0+h > c.H || band < 0 || band >= c.B {
		return fmt.Errorf("region out of bounds")
	}
	bps := c.Type.Size()
	if !c.Tiled {
		rowBytes := w * bps
		buf := c.getBuf(rowBytes)
		defer c.putBuf(buf)
		for r := 0; r < h; r++ {
			off := c.Start + ((int64(band)*int64(c.H)+int64(y0+r))*int64(c.W)+int64(x0))*int64(bps)
			if _, err := c.f.ReadAt(buf, off); err != nil {
				return err
			}
			c.decode(buf, dst[r*stride:r*stride+w], raw)
		}
		return nil
	}
	tileBytes := int64(c.TS*c.TL) * int64(bps)
	ty0, ty1 := y0/c.TL, (y0+h-1)/c.TL
	tx0, tx1 := x0/c.TS, (x0+w-1)/c.TS
	buf := c.getBuf(c.TS * c.TL * bps)
	defer c.putBuf(buf)
	for ty := ty0; ty <= ty1; ty++ {
		ra := max(y0, ty*c.TL) - ty*c.TL
		rb := min(y0+h, (ty+1)*c.TL) - ty*c.TL
		for tx := tx0; tx <= tx1; tx++ {
			ca := max(x0, tx*c.TS) - tx*c.TS
			cb := min(x0+w, (tx+1)*c.TS) - tx*c.TS
			tileIdx := (int64(band)*int64(c.tilesY)+int64(ty))*int64(c.tilesX) + int64(tx)
			off := c.Start + tileIdx*tileBytes + int64(ra*c.TS*bps)
			n := (rb - ra) * c.TS * bps
			b := buf[:n]
			if _, err := c.f.ReadAt(b, off); err != nil {
				return err
			}
			for r := ra; r < rb; r++ {
				src := b[((r-ra)*c.TS+ca)*bps : ((r-ra)*c.TS+cb)*bps]
				dr := ty*c.TL + r - y0
				dc := tx*c.TS + ca - x0
				c.decode(src, dst[dr*stride+dc:dr*stride+dc+(cb-ca)], raw)
			}
		}
	}
	return nil
}

// decode converts raw cube bytes to float64, mapping special pixels to NaN.
func (c *Cube) decode(src []byte, dst []float64, raw bool) {
	var bo binary.ByteOrder = binary.LittleEndian
	if c.BigEnd {
		bo = binary.BigEndian
	}
	nan := math.NaN()
	switch c.Type {
	case raster.Uint8:
		for i, v := range src[:len(dst)] {
			if v == 0 || v == 255 {
				dst[i] = nan
			} else {
				dst[i] = float64(v)
			}
		}
	case raster.Int8:
		for i := range dst {
			v := int8(src[i])
			if v < -126 {
				dst[i] = nan
			} else {
				dst[i] = float64(v)
			}
		}
	case raster.Int16:
		for i := range dst {
			v := int16(bo.Uint16(src[2*i:]))
			if v < -32752 {
				dst[i] = nan
			} else {
				dst[i] = float64(v)
			}
		}
	case raster.Uint16:
		for i := range dst {
			v := bo.Uint16(src[2*i:])
			if v < 3 || v > 65522 {
				dst[i] = nan
			} else {
				dst[i] = float64(v)
			}
		}
	case raster.Int32:
		for i := range dst {
			v := int32(bo.Uint32(src[4*i:]))
			if v < -2147483632 {
				dst[i] = nan
			} else {
				dst[i] = float64(v)
			}
		}
	case raster.Uint32:
		for i := range dst {
			v := bo.Uint32(src[4*i:])
			if v < 3 || v > 4294967282 {
				dst[i] = nan
			} else {
				dst[i] = float64(v)
			}
		}
	case raster.Float32:
		if !c.BigEnd {
			for i := range dst {
				v := float64(math.Float32frombits(binary.LittleEndian.Uint32(src[4*i:])))
				if v < validMin4 || v != v || math.IsInf(v, 0) {
					dst[i] = nan
				} else {
					dst[i] = v
				}
			}
		} else {
			for i := range dst {
				v := float64(math.Float32frombits(bo.Uint32(src[4*i:])))
				if v < validMin4 || v != v || math.IsInf(v, 0) {
					dst[i] = nan
				} else {
					dst[i] = v
				}
			}
		}
	case raster.Float64:
		for i := range dst {
			v := math.Float64frombits(bo.Uint64(src[8*i:]))
			if v < validMin8 || v != v || math.IsInf(v, 0) {
				dst[i] = nan
			} else {
				dst[i] = v
			}
		}
	}
	if !raw && (c.Base != 0 || c.Mult != 1) {
		for i, v := range dst {
			dst[i] = v*c.Mult + c.Base
		}
	}
}
