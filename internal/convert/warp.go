package convert

import (
	"container/list"
	"math"
	"sync"

	"cub2tif/internal/isis"
	"cub2tif/internal/proj"
	"cub2tif/internal/raster"
)

type resampling int

const (
	nearest resampling = iota
	bilinear
	cubic
)

// CubeSource copies pixels straight from the cube, without reprojection.
// Each worker reads a whole window as stored, then decodes it tile by tile.
type CubeSource struct {
	cube  *isis.Cube
	bands []int
	raw   bool
}

// cubeWindow is one worker's copy of the stored bytes under a window.
type cubeWindow struct {
	x0, y0, w int
	raw       [][]byte // per band, packed rows of w samples
}

func (s *CubeSource) Bands() int    { return len(s.bands) }
func (s *CubeSource) NewState() any { return &cubeWindow{raw: make([][]byte, len(s.bands))} }

// Window asks for reads of at least 16 KB per row, up to 8 tiles and 8 MB
// per worker.
func (s *CubeSource) Window(tile int) int {
	bps := s.cube.Type.Size()
	k := 1
	for k < 8 && k*tile*bps < 16<<10 {
		k *= 2
	}
	for k > 1 && k*tile*tile*bps*len(s.bands) > 8<<20 {
		k /= 2
	}
	return k
}

func (s *CubeSource) Load(state any, x0, y0, w, h int) error {
	st := state.(*cubeWindow)
	st.x0, st.y0, st.w = x0, y0, w
	n := w * h * s.cube.Type.Size()
	for i, b := range s.bands {
		if cap(st.raw[i]) < n {
			st.raw[i] = make([]byte, n)
		}
		if err := s.cube.ReadRaw(b, x0, y0, w, h, st.raw[i][:n]); err != nil {
			return err
		}
	}
	return nil
}

func (s *CubeSource) Fill(state any, x0, y0, w, h int, dst [][]float64, stride int) error {
	st := state.(*cubeWindow)
	bps := s.cube.Type.Size()
	for i := range s.bands {
		for r := 0; r < h; r++ {
			o := ((y0-st.y0+r)*st.w + x0 - st.x0) * bps
			s.cube.Decode(st.raw[i][o:o+w*bps], dst[i][r*stride:r*stride+w], s.raw)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// block cache over the source cube

type block struct {
	w, h int
	data [][]float64
}

type cacheEntry struct {
	id   int
	once sync.Once
	blk  *block
	err  error
	elem *list.Element
}

type blockCache struct {
	cube     *isis.Cube
	bands    []int
	raw      bool
	bw, bh   int
	nbx, nby int
	capacity int
	mu       sync.Mutex
	m        map[int]*cacheEntry
	lru      *list.List
}

func newBlockCache(c *isis.Cube, bands []int, raw bool, capBytes int64, threads int) *blockCache {
	bc := &blockCache{cube: c, bands: bands, raw: raw, m: map[int]*cacheEntry{}, lru: list.New()}
	if c.Tiled && c.TS*c.TL >= 64*64 && c.TS*c.TL <= 1024*1024 {
		bc.bw, bc.bh = c.TS, c.TL
	} else if c.Tiled {
		bc.bw, bc.bh = max(c.TS, 256), max(c.TL, 256)
	} else {
		bc.bw, bc.bh = min(c.W, 1024), 128
	}
	bc.nbx = (c.W + bc.bw - 1) / bc.bw
	bc.nby = (c.H + bc.bh - 1) / bc.bh
	per := int64(bc.bw*bc.bh*len(bands)) * 8
	bc.capacity = int(max(capBytes/per, int64(4*threads), 16))
	return bc
}

func (bc *blockCache) get(id int) (*block, error) {
	bc.mu.Lock()
	e := bc.m[id]
	if e == nil {
		e = &cacheEntry{id: id}
		e.elem = bc.lru.PushFront(e)
		bc.m[id] = e
		for bc.lru.Len() > bc.capacity {
			old := bc.lru.Back()
			oe := old.Value.(*cacheEntry)
			bc.lru.Remove(old)
			delete(bc.m, oe.id)
		}
	} else {
		bc.lru.MoveToFront(e.elem)
	}
	bc.mu.Unlock()
	e.once.Do(func() { e.blk, e.err = bc.load(id) })
	return e.blk, e.err
}

func (bc *blockCache) load(id int) (*block, error) {
	bx, by := id%bc.nbx, id/bc.nbx
	x0, y0 := bx*bc.bw, by*bc.bh
	w, h := min(bc.bw, bc.cube.W-x0), min(bc.bh, bc.cube.H-y0)
	b := &block{w: w, h: h, data: make([][]float64, len(bc.bands))}
	for i, band := range bc.bands {
		b.data[i] = make([]float64, w*h)
		if err := bc.cube.ReadRegion(band, x0, y0, w, h, b.data[i], w, bc.raw); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// warp source

type WarpSource struct {
	cube     *isis.Cube
	bands    []int
	sp, dp   *proj.Projection
	sg, dg   raster.Grid
	resample resampling
	approx   float64
	periodPx float64
	wrap     bool
	cache    *blockCache
}

type l1Entry struct {
	id  int
	blk *block
}

type warpState struct {
	u, v   []float64
	ok     []bool
	l1     [16]l1Entry // recent blocks, looked up without the cache lock; kept small, as it holds blocks the cache may have dropped
	lastID int
	last   *block
	err    error
}

func newWarpSource(c *isis.Cube, bands []int, raw bool, dp *proj.Projection, dg raster.Grid, resample resampling, approx float64, cacheBytes int64, threads int) *WarpSource {
	ws := &WarpSource{cube: c, bands: bands, sp: c.Proj, dp: dp, sg: c.Grid, dg: dg, resample: resample, approx: approx}
	if c.Proj.Period > 0 {
		ws.periodPx = c.Proj.Period / c.Grid.ResX
		ws.wrap = math.Abs(ws.periodPx-float64(c.W)) < 0.01
	}
	ws.cache = newBlockCache(c, bands, raw, cacheBytes, threads)
	return ws
}

func (s *WarpSource) Bands() int { return len(s.bands) }

// Window is a single tile: the block cache already turns scattered source
// access into block-sized reads.
func (s *WarpSource) Window(int) int                     { return 1 }
func (s *WarpSource) Load(any, int, int, int, int) error { return nil }

func (s *WarpSource) NewState() any {
	st := &warpState{lastID: -1}
	for i := range st.l1 {
		st.l1[i].id = -1
	}
	return st
}

// point maps an output projected coordinate to source pixel coordinates.
func (s *WarpSource) point(x, y float64) (float64, float64, bool) {
	lon, lat, ok := s.dp.Inverse(x, y)
	if !ok {
		return 0, 0, false
	}
	sx, sy, ok := s.sp.Forward(lon, lat)
	if !ok {
		return 0, 0, false
	}
	u := (sx - s.sg.X0) / s.sg.ResX
	v := (s.sg.Y0 - sy) / s.sg.ResY
	if s.periodPx > 0 {
		k := math.Floor((u-float64(s.cube.W)/2)/s.periodPx + 0.5)
		u -= k * s.periodPx
	}
	return u, v, true
}

func (s *WarpSource) transformRow(st *warpState, x0 int, y float64, n int) {
	u, v, ok := st.u[:n], st.v[:n], st.ok[:n]
	xAt := func(c int) float64 { return s.dg.X0 + (float64(x0+c)+0.5)*s.dg.ResX }
	exact := func(c int) { u[c], v[c], ok[c] = s.point(xAt(c), y) }
	if s.approx <= 0 || n < 3 {
		for c := 0; c < n; c++ {
			exact(c)
		}
		return
	}
	exact(0)
	exact(n - 1)
	var rec func(a, b int)
	rec = func(a, b int) {
		if b-a < 2 {
			return
		}
		m := (a + b) / 2
		exact(m)
		if ok[a] && ok[b] && ok[m] {
			t := float64(m-a) / float64(b-a)
			if math.Abs(u[a]+(u[b]-u[a])*t-u[m]) <= s.approx && math.Abs(v[a]+(v[b]-v[a])*t-v[m]) <= s.approx {
				du, dv := (u[b]-u[a])/float64(b-a), (v[b]-v[a])/float64(b-a)
				for i := a + 1; i < b; i++ {
					if i != m {
						u[i] = u[a] + du*float64(i-a)
						v[i] = v[a] + dv*float64(i-a)
						ok[i] = true
					}
				}
				return
			}
		}
		rec(a, m)
		rec(m, b)
	}
	rec(0, n-1)
}

func (s *WarpSource) Fill(state any, x0, y0, w, h int, dst [][]float64, stride int) error {
	st := state.(*warpState)
	if len(st.u) < w {
		st.u, st.v, st.ok = make([]float64, w), make([]float64, w), make([]bool, w)
	}
	nan := math.NaN()
	for r := 0; r < h; r++ {
		y := s.dg.Y0 - (float64(y0+r)+0.5)*s.dg.ResY
		s.transformRow(st, x0, y, w)
		for c := 0; c < w; c++ {
			o := r*stride + c
			if !st.ok[c] {
				for b := range dst {
					dst[b][o] = nan
				}
				continue
			}
			s.sample(st, st.u[c], st.v[c], dst, o)
		}
		if st.err != nil {
			return st.err
		}
	}
	return nil
}

// fetch returns the block holding source pixel (ix,iy) and the index within it.
// ix must already be wrapped/in range.
func (s *WarpSource) fetch(st *warpState, ix, iy int) (*block, int) {
	bc := s.cache
	bx, by := ix/bc.bw, iy/bc.bh
	id := by*bc.nbx + bx
	blk := st.last
	if id != st.lastID {
		e := &st.l1[id&(len(st.l1)-1)]
		if e.id == id {
			blk = e.blk
		} else {
			b, err := bc.get(id)
			if err != nil {
				if st.err == nil {
					st.err = err
				}
				return nil, 0
			}
			e.id, e.blk = id, b
			blk = b
		}
		st.lastID, st.last = id, blk
	}
	return blk, (iy-by*bc.bh)*blk.w + (ix - bx*bc.bw)
}

// col maps a (possibly out-of-range) column index to a valid one, or -1.
func (s *WarpSource) col(ix int) int {
	W := s.cube.W
	if ix >= 0 && ix < W {
		return ix
	}
	if s.wrap {
		ix %= W
		if ix < 0 {
			ix += W
		}
		return ix
	}
	return -1
}

func (s *WarpSource) sample(st *warpState, u, v float64, dst [][]float64, o int) {
	W, H := float64(s.cube.W), float64(s.cube.H)
	nan := math.NaN()
	if v < 0 || v >= H || (!s.wrap && (u < 0 || u >= W)) {
		for b := range dst {
			dst[b][o] = nan
		}
		return
	}
	if s.wrap && (u < 0 || u >= W) {
		u = math.Mod(u, W)
		if u < 0 {
			u += W
		}
		if u >= W {
			u = 0
		}
	}
	if s.resample == nearest {
		blk, i := s.fetch(st, int(u), int(v))
		for b := range dst {
			if blk == nil {
				dst[b][o] = nan
			} else {
				dst[b][o] = blk.data[b][i]
			}
		}
		return
	}
	if s.resample == cubic {
		for b := range dst {
			dst[b][o] = s.cubic(st, b, u, v)
		}
		return
	}
	for b := range dst {
		dst[b][o] = s.bilinear(st, b, u, v)
	}
}

// centerValid reports whether the source pixel containing (u,v) has data;
// like GDAL, interpolation never grows data into no-data areas.
func (s *WarpSource) centerValid(st *warpState, band int, u, v float64) bool {
	blk, i := s.fetch(st, int(u), int(v))
	return blk != nil && blk.data[band][i] == blk.data[band][i]
}

func (s *WarpSource) bilinear(st *warpState, band int, u, v float64) float64 {
	if !s.centerValid(st, band, u, v) {
		return math.NaN()
	}
	uc, vc := u-0.5, v-0.5
	fx0, fy0 := math.Floor(uc), math.Floor(vc)
	ix, iy := int(fx0), int(fy0)
	fx, fy := uc-fx0, vc-fy0
	var sum, wsum float64
	for dy := 0; dy < 2; dy++ {
		ry := iy + dy
		wy := fy
		if dy == 0 {
			wy = 1 - fy
		}
		if ry < 0 || ry >= s.cube.H || wy == 0 {
			continue
		}
		for dx := 0; dx < 2; dx++ {
			wx := fx
			if dx == 0 {
				wx = 1 - fx
			}
			if wx == 0 {
				continue
			}
			cx := s.col(ix + dx)
			if cx < 0 {
				continue
			}
			blk, i := s.fetch(st, cx, ry)
			if blk == nil {
				return math.NaN()
			}
			val := blk.data[band][i]
			if val == val {
				sum += val * wx * wy
				wsum += wx * wy
			}
		}
	}
	if wsum < 1e-9 {
		return math.NaN()
	}
	return sum / wsum
}

func cubicW(t float64) float64 {
	const a = -0.5
	t = math.Abs(t)
	if t <= 1 {
		return ((a+2)*t-(a+3))*t*t + 1
	}
	if t < 2 {
		return ((a*t-5*a)*t+8*a)*t - 4*a
	}
	return 0
}

func (s *WarpSource) cubic(st *warpState, band int, u, v float64) float64 {
	if !s.centerValid(st, band, u, v) {
		return math.NaN()
	}
	uc, vc := u-0.5, v-0.5
	fx0, fy0 := math.Floor(uc), math.Floor(vc)
	ix, iy := int(fx0), int(fy0)
	fx, fy := uc-fx0, vc-fy0
	var wx, wy [4]float64
	for k := 0; k < 4; k++ {
		wx[k] = cubicW(fx - float64(k-1))
		wy[k] = cubicW(fy - float64(k-1))
	}
	var sum float64
	for dy := 0; dy < 4; dy++ {
		ry := iy + dy - 1
		if ry < 0 || ry >= s.cube.H {
			return s.bilinear(st, band, u, v)
		}
		var rs float64
		for dx := 0; dx < 4; dx++ {
			cx := s.col(ix + dx - 1)
			if cx < 0 {
				return s.bilinear(st, band, u, v)
			}
			blk, i := s.fetch(st, cx, ry)
			if blk == nil {
				return math.NaN()
			}
			val := blk.data[band][i]
			if val != val {
				return s.bilinear(st, band, u, v)
			}
			rs += val * wx[dx]
		}
		sum += rs * wy[dy]
	}
	return sum
}
