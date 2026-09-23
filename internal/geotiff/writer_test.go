package geotiff

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"

	"cub2tif/internal/raster"
)

// A minimal TIFF reader: enough to check what the writer produced.
type tiffFile struct {
	data []byte
	big  bool
	tags map[int][]uint64 // first directory only
}

func readTIFF(t *testing.T, path string) *tiffFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	f := &tiffFile{data: b, tags: map[int][]uint64{}}
	if string(b[:2]) != "II" {
		t.Fatal("not little-endian TIFF")
	}
	var at uint64
	switch le.Uint16(b[2:]) {
	case 42:
		at = uint64(le.Uint32(b[4:]))
	case 43:
		f.big = true
		at = le.Uint64(b[8:])
	default:
		t.Fatal("bad magic")
	}
	if at%2 != 0 {
		t.Error("directory not word aligned")
	}
	n, entry, off := uint64(le.Uint16(b[at:])), uint64(12), uint64(2)
	if f.big {
		n, entry, off = le.Uint64(b[at:]), 20, 8
	}
	prev := -1
	for i := uint64(0); i < n; i++ {
		e := b[at+off+i*entry:]
		tag, typ := int(le.Uint16(e)), int(le.Uint16(e[2:]))
		if tag <= prev {
			t.Errorf("tags not sorted: %d after %d", tag, prev)
		}
		prev = tag
		var count uint64
		var val []byte
		if f.big {
			count, val = le.Uint64(e[4:]), e[12:20]
		} else {
			count, val = uint64(le.Uint32(e[4:])), e[8:12]
		}
		size := map[int]uint64{2: 1, 3: 2, 4: 4, 12: 8, 16: 8}[typ]
		if count*size > uint64(len(val)) {
			o := uint64(le.Uint32(val))
			if f.big {
				o = le.Uint64(val)
			}
			val = b[o:]
		}
		vals := make([]uint64, count)
		for j := range vals {
			switch size {
			case 1:
				vals[j] = uint64(val[j])
			case 2:
				vals[j] = uint64(le.Uint16(val[2*j:]))
			case 4:
				vals[j] = uint64(le.Uint32(val[4*j:]))
			case 8:
				vals[j] = le.Uint64(val[8*j:])
			}
		}
		f.tags[tag] = vals
	}
	return f
}

// tile returns the decompressed, un-predicted bytes of one tile.
func (f *tiffFile) tile(t *testing.T, i int) []byte {
	t.Helper()
	off, n := f.tags[324][i], f.tags[325][i]
	raw := f.data[off : off+n]
	var out []byte
	switch f.tags[259][0] {
	case CompNone:
		out = append([]byte(nil), raw...)
	case CompDeflate:
		r, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		out, _ = io.ReadAll(r)
	case CompZstd:
		d, _ := zstd.NewReader(nil)
		var err error
		if out, err = d.DecodeAll(raw, nil); err != nil {
			t.Fatal(err)
		}
	}
	T := int(f.tags[322][0])
	spp := 1
	if f.tags[284][0] == 1 {
		spp = int(f.tags[277][0])
	}
	bps := int(f.tags[258][0]) / 8
	pred := uint64(1)
	if p, ok := f.tags[317]; ok {
		pred = p[0]
	}
	row := T * spp * bps
	for r := 0; r < T; r++ {
		b := out[r*row : (r+1)*row]
		switch pred {
		case 2: // undo horizontal differencing, per channel
			n := T * spp
			for c := spp; c < n; c++ {
				switch bps {
				case 1:
					b[c] += b[c-spp]
				case 2:
					binary.LittleEndian.PutUint16(b[2*c:], binary.LittleEndian.Uint16(b[2*c:])+binary.LittleEndian.Uint16(b[2*(c-spp):]))
				}
			}
		case 3: // undo byte differencing, then reassemble byte planes
			for i := spp; i < len(b); i++ {
				b[i] += b[i-spp]
			}
			wc := T * spp
			tmp := append([]byte(nil), b...)
			for c := 0; c < wc; c++ {
				for k := 0; k < bps; k++ {
					b[bps*c+k] = tmp[(bps-1-k)*wc+c]
				}
			}
		}
	}
	return out
}

func TestWriterRoundTrip(t *testing.T) {
	const W, H, T = 40, 25, 16 // partial edge tiles on both axes
	band := func(b, x, y int) float64 {
		if (x+y+b)%11 == 0 {
			return math.NaN()
		}
		return float64(x*3-y*2+b*100) * 0.25
	}
	cases := []struct {
		name        string
		typ         raster.PixType
		bands       int
		interleaved bool
		alpha       bool
		comp, pred  int
		big         bool
		photometric int
		extra       []uint64 // expected ExtraSamples; nil = no tag
	}{
		{"float-planar-deflate-pred3", raster.Float32, 2, false, false, CompDeflate, 3, false, MinIsBlack, []uint64{0}},
		{"float-gray-alpha-zstd", raster.Float32, 1, true, true, CompZstd, 3, false, MinIsBlack, []uint64{2}},
		{"int16-planar-pred2-bigtiff", raster.Int16, 3, false, false, CompDeflate, 2, true, MinIsBlack, []uint64{0, 0}},
		{"uint16-rgb-alpha", raster.Uint16, 3, true, true, CompDeflate, 2, false, RGB, []uint64{2}},
		{"float-rgb-no-alpha", raster.Float32, 3, true, false, CompDeflate, 3, false, RGB, nil},
		{"uint8-uncompressed", raster.Uint8, 1, false, false, CompNone, 1, false, MinIsBlack, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			samples := tc.bands
			if tc.alpha {
				samples++
			}
			path := filepath.Join(t.TempDir(), "x.tif")
			w, err := Create(path, Layout{Levels: [][2]int{{W, H}}, Tile: T, Samples: samples, Type: tc.typ,
				Compression: tc.comp, Predictor: tc.pred, BigTIFF: tc.big, Interleaved: tc.interleaved,
				Photometric: tc.photometric, Alpha: tc.alpha})
			if err != nil {
				t.Fatal(err)
			}
			cfg := EncoderConfig{Type: tc.typ, Compression: tc.comp, Level: 6, Predictor: tc.pred, Tile: T,
				Interleaved: tc.interleaved, Alpha: tc.alpha, HasNoData: !tc.alpha, NoData: -9}
			if tc.typ == raster.Uint16 || tc.typ == raster.Uint8 {
				cfg.NoData = 0
			}
			enc, err := NewEncoder(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var s Scratch
			tilesX, tilesY := (W+T-1)/T, (H+T-1)/T
			for ty := 0; ty < tilesY; ty++ {
				for tx := 0; tx < tilesX; tx++ {
					vw, vh := min(T, W-tx*T), min(T, H-ty*T)
					bufs := make([][]float64, tc.bands)
					for b := range bufs {
						bufs[b] = make([]float64, T*T)
						for r := 0; r < vh; r++ {
							for c := 0; c < vw; c++ {
								v := band(b, tx*T+c, ty*T+r)
								if tc.typ == raster.Uint16 || tc.typ == raster.Uint8 {
									v = math.Abs(v)
								}
								bufs[b][r*T+c] = v
							}
						}
					}
					if tc.interleaved {
						data, err := enc.EncodeInterleaved(&s, bufs, vw, vh)
						if err != nil {
							t.Fatal(err)
						}
						w.WriteTile(0, 0, tx, ty, append([]byte(nil), data...))
						continue
					}
					for b := range bufs {
						data, err := enc.Encode(&s, bufs[b], vw, vh, b)
						if err != nil {
							t.Fatal(err)
						}
						w.WriteTile(0, b, tx, ty, append([]byte(nil), data...))
					}
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			f := readTIFF(t, path)
			if f.big != tc.big {
				t.Errorf("BigTIFF = %v", f.big)
			}
			if got := f.tags[338]; len(got) != len(tc.extra) || (len(got) > 0 && got[len(got)-1] != tc.extra[len(tc.extra)-1]) {
				t.Errorf("ExtraSamples = %v, want %v", got, tc.extra)
			}
			if f.tags[262][0] != uint64(tc.photometric) || f.tags[277][0] != uint64(samples) {
				t.Errorf("photometric/samples = %v/%v", f.tags[262], f.tags[277])
			}
			bps := tc.typ.Size()
			sample := func(b []byte, i int) float64 {
				switch tc.typ {
				case raster.Float32:
					return float64(math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:])))
				case raster.Int16:
					return float64(int16(binary.LittleEndian.Uint16(b[2*i:])))
				case raster.Uint16:
					return float64(binary.LittleEndian.Uint16(b[2*i:]))
				}
				return float64(b[i])
			}
			_ = bps
			for ty := 0; ty < tilesY; ty++ {
				for tx := 0; tx < tilesX; tx++ {
					for b := 0; b < tc.bands; b++ {
						var tile []byte
						idx := b
						if tc.interleaved {
							tile, idx = f.tile(t, ty*tilesX+tx), b
						} else {
							tile, idx = f.tile(t, b*tilesX*tilesY+ty*tilesX+tx), 0
						}
						step := 1
						if tc.interleaved {
							step = samples
						}
						for r := 0; r < min(T, H-ty*T); r++ {
							for c := 0; c < min(T, W-tx*T); c++ {
								want := band(b, tx*T+c, ty*T+r)
								if tc.typ == raster.Uint16 || tc.typ == raster.Uint8 {
									want = math.Abs(want)
								}
								got := sample(tile, (r*T+c)*step+idx)
								switch {
								case math.IsNaN(want) && tc.alpha:
									if a := sample(tile, (r*T+c)*step+samples-1); a != 0 && tc.bands == 1 {
										t.Fatalf("alpha at no-data = %v", a)
									}
								case math.IsNaN(want):
									if !tc.typ.IsFloat() && tc.typ != raster.Int16 {
										continue // unsigned test data has no no-data
									}
									if got != cfg.NoData {
										t.Fatalf("band %d (%d,%d): no-data written as %v", b, tx*T+c, ty*T+r, got)
									}
								default:
									if !tc.typ.IsFloat() {
										want = math.Round(want)
									}
									if got != want {
										t.Fatalf("band %d (%d,%d) = %v, want %v", b, tx*T+c, ty*T+r, got, want)
									}
									if tc.alpha {
										if a := sample(tile, (r*T+c)*step+samples-1); a == 0 {
											t.Fatalf("alpha 0 at a data pixel")
										}
									}
								}
							}
						}
					}
				}
			}
		})
	}
}

func TestOverviewLevels(t *testing.T) {
	got := OverviewLevels(1500, 700, 512, true)
	want := [][2]int{{1500, 700}, {750, 350}, {375, 175}}
	if len(got) != len(want) {
		t.Fatalf("levels = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("level %d = %v, want %v", i, got[i], want[i])
		}
	}
	if len(OverviewLevels(1500, 700, 512, false)) != 1 {
		t.Error("no overviews requested")
	}
}
