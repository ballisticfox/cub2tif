package convert

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"cub2tif/internal/geotiff"
	"cub2tif/internal/isis"
	"cub2tif/internal/raster"
)

// pattern is the test image: smooth values with scattered no-data.
func pattern(b, x, y int) float64 {
	if (x*7+y*3+b)%13 == 0 {
		return math.NaN()
	}
	return float64(x*5-y*3+b*1000) + 0.25*float64(b)
}

// fakeSource serves pattern and checks that every Fill lies inside the
// window last loaded by the same worker.
type fakeSource struct {
	bands []int
	k     int
	err   error
	mu    sync.Mutex
}

type fakeState struct{ x0, y0, w, h int }

func (s *fakeSource) Bands() int          { return len(s.bands) }
func (s *fakeSource) NewState() any       { return &fakeState{} }
func (s *fakeSource) Window(tile int) int { return s.k }
func (s *fakeSource) Load(state any, x0, y0, w, h int) error {
	*state.(*fakeState) = fakeState{x0, y0, w, h}
	return nil
}

func (s *fakeSource) Fill(state any, x0, y0, w, h int, dst [][]float64, stride int) error {
	st := state.(*fakeState)
	if x0 < st.x0 || y0 < st.y0 || x0+w > st.x0+st.w || y0+h > st.y0+st.h {
		s.mu.Lock()
		s.err = fmt.Errorf("fill (%d,%d %dx%d) outside window (%d,%d %dx%d)", x0, y0, w, h, st.x0, st.y0, st.w, st.h)
		s.mu.Unlock()
	}
	for i, b := range s.bands {
		for r := 0; r < h; r++ {
			for c := 0; c < w; c++ {
				dst[i][r*stride+c] = pattern(b, x0+c, y0+r)
			}
		}
	}
	return nil
}

// memSink keeps a copy of every tile written.
type memSink struct {
	mu    sync.Mutex
	tiles map[[4]int][]byte
	dups  int
}

func (m *memSink) WriteTile(level, plane, tx, ty int, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := [4]int{level, plane, tx, ty}
	if _, ok := m.tiles[key]; ok {
		m.dups++
	}
	m.tiles[key] = append([]byte(nil), data...)
	return nil
}

// reference computes every level of every band over the whole image at once,
// the way the old row-strip cascade did: 2x2 blocks, an odd last row or
// column paired with itself.
func reference(img [][]float64, W, H int, levels int, average bool) [][][]float64 {
	out := [][][]float64{img}
	for l := 1; l < levels; l++ {
		prev := out[l-1]
		w, h := (W+1)/2, (H+1)/2
		next := make([][]float64, len(prev))
		for b := range prev {
			next[b] = make([]float64, w*h)
			for y := 0; y < h; y++ {
				y1 := min(2*y+1, H-1)
				for x := 0; x < w; x++ {
					x1 := min(2*x+1, W-1)
					q := [4]float64{prev[b][2*y*W+2*x], prev[b][2*y*W+x1], prev[b][y1*W+2*x], prev[b][y1*W+x1]}
					if !average {
						next[b][y*w+x] = q[0]
						continue
					}
					sum, n := 0.0, 0
					for _, v := range q {
						if v == v {
							sum += v
							n++
						}
					}
					next[b][y*w+x] = math.NaN()
					if n > 0 {
						next[b][y*w+x] = sum / float64(n)
					}
				}
			}
		}
		out = append(out, next)
		W, H = w, h
	}
	return out
}

// runPipeline writes W x H through the pipeline into memory, as float64
// without compression so the tiles can be compared exactly.
func runPipeline(t *testing.T, W, H, T, threads int, groups [][]int, interleaved, overviews, average bool, newSource func([]int) TileSource) (*memSink, []*geotiff.Level) {
	t.Helper()
	var levels []*geotiff.Level
	for _, d := range geotiff.OverviewLevels(W, H, T, overviews) {
		levels = append(levels, &geotiff.Level{W: d[0], H: d[1], TilesX: (d[0] + T - 1) / T, TilesY: (d[1] + T - 1) / T})
	}
	enc, err := geotiff.NewEncoder(geotiff.EncoderConfig{Type: raster.Float64, Compression: geotiff.CompNone, Predictor: 1, Tile: T, Interleaved: interleaved})
	if err != nil {
		t.Fatal(err)
	}
	sink := &memSink{tiles: map[[4]int][]byte{}}
	p := &Pipeline{out: sink, levels: levels, tile: T, enc: enc, threads: threads, ovAverage: average}
	if err := p.Run(groups, newSource); err != nil {
		t.Fatal(err)
	}
	if p.done.Load() != p.total {
		t.Errorf("progress ended at %d of %d", p.done.Load(), p.total)
	}
	return sink, levels
}

// checkTiles compares every written tile with the reference.
func checkTiles(t *testing.T, sink *memSink, levels []*geotiff.Level, ref [][][]float64, T int, interleaved bool) {
	t.Helper()
	nb := len(ref[0])
	want := 0
	for li, l := range levels {
		for ty := 0; ty < l.TilesY; ty++ {
			for tx := 0; tx < l.TilesX; tx++ {
				for b := 0; b < nb; b++ {
					plane, spp, idx := b, 1, 0
					if interleaved {
						plane, spp, idx = 0, nb, b
					}
					tile, ok := sink.tiles[[4]int{li, plane, tx, ty}]
					if !ok {
						t.Fatalf("level %d plane %d tile (%d,%d) never written", li, plane, tx, ty)
					}
					for r := 0; r < min(T, l.H-ty*T); r++ {
						for c := 0; c < min(T, l.W-tx*T); c++ {
							got := math.Float64frombits(binary.LittleEndian.Uint64(tile[8*((r*T+c)*spp+idx):]))
							w := ref[li][b][(ty*T+r)*l.W+tx*T+c]
							if got != w && !(math.IsNaN(got) && math.IsNaN(w)) {
								t.Fatalf("level %d band %d pixel (%d,%d) = %v, want %v", li, b, tx*T+c, ty*T+r, got, w)
							}
						}
					}
				}
				if interleaved {
					want++
				} else {
					want += nb
				}
			}
		}
	}
	if len(sink.tiles) != want || sink.dups != 0 {
		t.Errorf("%d tiles written (%d twice), want %d", len(sink.tiles), sink.dups, want)
	}
}

func TestPipeline(t *testing.T) {
	const W, H, T = 300, 170, 16 // partial edge tiles; five overview levels
	cases := []struct {
		name                 string
		threads, k, perPass  int
		interleaved, average bool
		overviews            bool
	}{
		{"one-thread-average", 1, 1, 3, false, true, true},
		{"windows-average", 7, 4, 3, false, true, true},
		{"windows-nearest-passes", 5, 8, 2, false, false, true},
		{"interleaved", 4, 2, 3, true, true, true},
		{"no-overviews-passes", 3, 4, 1, false, true, false},
		{"wide-window", 2, 32, 3, false, true, true},
	}
	bands := []int{0, 1, 2}
	img := make([][]float64, len(bands))
	for b := range img {
		img[b] = make([]float64, W*H)
		for y := 0; y < H; y++ {
			for x := 0; x < W; x++ {
				img[b][y*W+x] = pattern(b, x, y)
			}
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var groups [][]int
			for i := 0; i < len(bands); i += tc.perPass {
				groups = append(groups, bands[i:min(i+tc.perPass, len(bands))])
			}
			var srcs []*fakeSource
			var mu sync.Mutex
			sink, levels := runPipeline(t, W, H, T, tc.threads, groups, tc.interleaved, tc.overviews, tc.average,
				func(b []int) TileSource {
					s := &fakeSource{bands: b, k: tc.k}
					mu.Lock()
					srcs = append(srcs, s)
					mu.Unlock()
					return s
				})
			for _, s := range srcs {
				if s.err != nil {
					t.Fatal(s.err)
				}
			}
			checkTiles(t, sink, levels, reference(img, W, H, len(levels), tc.average), T, tc.interleaved)
		})
	}
}

// writeTestCube writes a Real (float32) cube, band-sequential or tiled.
func writeTestCube(t *testing.T, path string, W, H, B, ts, tl int, val func(b, x, y int) float64) {
	t.Helper()
	format, tiles := "BandSequential", ""
	tx, ty := 1, 1
	if ts > 0 {
		format = "Tile"
		tiles = fmt.Sprintf("    TileSamples = %d\n    TileLines = %d\n", ts, tl)
		tx, ty = (W+ts-1)/ts, (H+tl-1)/tl
	} else {
		ts, tl = W, H
	}
	data := make([]byte, B*tx*ty*ts*tl*4)
	for b := 0; b < B; b++ {
		for y := 0; y < H; y++ {
			for x := 0; x < W; x++ {
				v := float32(val(b, x, y))
				if math.IsNaN(float64(v)) {
					v = float32(isis.Null4)
				}
				i := ((b*ty+y/tl)*tx+x/ts)*ts*tl + (y%tl)*ts + x%ts
				binary.LittleEndian.PutUint32(data[4*i:], math.Float32bits(v))
			}
		}
	}
	label := fmt.Sprintf("Object = IsisCube\n  Object = Core\n    StartByte = 1025\n    Format = %s\n%s"+
		"    Group = Dimensions\n      Samples = %d\n      Lines = %d\n      Bands = %d\n    End_Group\n"+
		"    Group = Pixels\n      Type = Real\n      ByteOrder = Lsb\n      Base = 0.0\n      Multiplier = 1.0\n    End_Group\n"+
		"  End_Object\nEnd_Object\nEnd\n", format, tiles, W, H, B)
	buf := make([]byte, 1024, 1024+len(data))
	copy(buf, label)
	if err := os.WriteFile(path, append(buf, data...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A real cube through CubeSource: windows read as stored bytes and decoded
// per tile must give exactly what ReadRegion gives for the whole image.
func TestCubeSourceWindows(t *testing.T) {
	const W, H, B, T = 300, 170, 2, 16
	dir := t.TempDir()
	for _, tc := range []struct {
		name   string
		ts, tl int
	}{{"bsq", 0, 0}, {"tile-13x7", 13, 7}, {"tile-64x64", 64, 64}} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name+".cub")
			writeTestCube(t, p, W, H, B, tc.ts, tc.tl, pattern)
			c, err := isis.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			img := make([][]float64, B)
			for b := range img {
				img[b] = make([]float64, W*H)
				if err := c.ReadRegion(b, 0, 0, W, H, img[b], W, false); err != nil {
					t.Fatal(err)
				}
			}
			bands := []int{0, 1}
			sink, levels := runPipeline(t, W, H, T, 6, [][]int{bands}, false, true, true,
				func(b []int) TileSource { return &CubeSource{cube: c, bands: b} })
			checkTiles(t, sink, levels, reference(img, W, H, len(levels), true), T, false)
		})
	}
}
