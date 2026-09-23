package convert

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"cub2tif/internal/geotiff"
)

// TileSource produces float64 pixels (NaN = no data) for the output grid.
// Its methods may be called concurrently, each goroutine with its own state.
type TileSource interface {
	Bands() int
	NewState() any
	// Window is how many tiles of the given size across (a power of two) a
	// worker should take at a time. Load prepares state for the Fill calls
	// inside one such window, so that a source that reads its input by rows
	// can read the whole window in a few large reads.
	Window(tile int) int
	Load(state any, x0, y0, w, h int) error
	Fill(state any, x0, y0, w, h int, dst [][]float64, stride int) error
}

// tileSink receives encoded tiles; *geotiff.Writer is one.
type tileSink interface {
	WriteTile(level, plane, tx, ty int, data []byte) error
}

// Pipeline fills, encodes and writes every tile of every level. Memory
// depends on the tile size and thread count, not on the size of the image:
//
//   - Workers take windows: a run of tiles in one tile row. A window is read
//     in one go (wide reads are many times faster than tile-sized ones on a
//     band-sequential cube), then filled, encoded and written tile by tile.
//   - Overviews are built as a quadtree. Each finished tile is halved into
//     its quarter of the parent tile, and the parent is encoded as soon as
//     all its children are in. With overviews the windows are visited in
//     square blocks, the blocks in Z order, so only a few parents are ever
//     incomplete, however wide the image.
//   - Bands are written in passes, each from its own source, so a cube with
//     hundreds of bands never has them all in memory at once.
type Pipeline struct {
	out       tileSink
	levels    []*geotiff.Level // the image, then each overview
	tile      int
	enc       *geotiff.Encoder
	threads   int
	ovAverage bool
	progress  func(done, total int64)

	// the current pass
	src   TileSource
	plane int // output plane of the source's first band
	nb    int

	bufPool sync.Pool
	ovMu    sync.Mutex
	ov      map[tileKey]*ovTile // overview tiles still waiting for children
	errMu   sync.Mutex
	err     error
	failed  atomic.Bool
	done    atomic.Int64
	total   int64
}

type pworker struct {
	state any
	enc   geotiff.Scratch
}

type tileKey struct{ level, tx, ty int }

// ovTile is an overview tile being assembled from its (up to four) children.
type ovTile struct {
	bufs    [][]float64
	pending int // children still to come; guarded by ovMu
}

func (p *Pipeline) setErr(err error) {
	if err == nil {
		return
	}
	p.errMu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.errMu.Unlock()
	p.failed.Store(true)
}

// getBufs returns one tile buffer per band of the current pass.
func (p *Pipeline) getBufs() [][]float64 {
	bufs := make([][]float64, p.nb)
	for b := range bufs {
		if v, ok := p.bufPool.Get().(*[]float64); ok {
			bufs[b] = *v
		} else {
			bufs[b] = make([]float64, p.tile*p.tile)
		}
	}
	return bufs
}

func (p *Pipeline) putBufs(bufs [][]float64) {
	for _, b := range bufs {
		p.bufPool.Put(&b)
	}
}

// Run writes the bands group by group, each group in its own pass over the
// image, reading from the source newSource makes for it.
func (p *Pipeline) Run(groups [][]int, newSource func(bands []int) TileSource) error {
	for _, l := range p.levels {
		p.total += int64(l.TilesX*l.TilesY) * int64(len(groups))
	}
	defer p.showProgress()()
	for _, g := range groups {
		if err := p.pass(newSource(g)); err != nil {
			return err
		}
		p.plane += len(g)
	}
	return nil
}

// showProgress reports progress every 200 ms until the returned func is called.
func (p *Pipeline) showProgress() (stop func()) {
	if p.progress == nil {
		return func() {}
	}
	quit, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				p.progress(p.done.Load(), p.total)
				return
			case <-ticker.C:
				p.progress(p.done.Load(), p.total)
			}
		}
	}()
	return func() { close(quit); <-finished }
}

func (p *Pipeline) pass(src TileSource) error {
	p.src, p.nb = src, src.Bands()
	p.ov = map[tileKey]*ovTile{}
	k := p.windowSize(src.Window(p.tile))
	type window struct{ tx, ty int }
	windows := make(chan window, p.threads)
	var wg sync.WaitGroup
	for i := 0; i < p.threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &pworker{state: src.NewState()}
			for win := range windows {
				if !p.failed.Load() {
					p.window(w, win.tx, win.ty, k)
				}
			}
		}()
	}
	p.eachWindow(k, func(tx, ty int) bool {
		windows <- window{tx, ty}
		return !p.failed.Load()
	})
	close(windows)
	wg.Wait()
	return p.err
}

// windowSize settles how many tiles across a window is: what the source asks
// for, narrowed until there are enough windows to keep every worker busy.
func (p *Pipeline) windowSize(k int) int {
	l0 := p.levels[0]
	for k > 1 && (l0.TilesX+k-1)/k*l0.TilesY < 4*p.threads {
		k /= 2
	}
	return k
}

// eachWindow visits the full-resolution windows (k tiles across, from tile
// tx of row ty) until visit returns false. Without overviews they go row by
// row, which reads a band-sequential cube front to back. With overviews they
// go in square blocks of k x k tiles, the blocks in Z order, so that at every
// level a parent completes soon after its first child.
func (p *Pipeline) eachWindow(k int, visit func(tx, ty int) bool) {
	l0 := p.levels[0]
	nx := (l0.TilesX + k - 1) / k // windows per row, and blocks across
	if len(p.levels) == 1 {
		for ty := 0; ty < l0.TilesY; ty++ {
			for bx := 0; bx < nx; bx++ {
				if !visit(bx*k, ty) {
					return
				}
			}
		}
		return
	}
	ny := (l0.TilesY + k - 1) / k
	side := 1
	for side < max(nx, ny) {
		side *= 2
	}
	var quad func(bx, by, size int) bool
	quad = func(bx, by, size int) bool {
		if bx >= nx || by >= ny {
			return true
		}
		if size > 1 {
			h := size / 2
			return quad(bx, by, h) && quad(bx+h, by, h) && quad(bx, by+h, h) && quad(bx+h, by+h, h)
		}
		for ty := by * k; ty < min(by*k+k, l0.TilesY); ty++ {
			if !visit(bx*k, ty) {
				return false
			}
		}
		return true
	}
	quad(0, 0, side)
}

// window loads k tiles of row ty, from column tx0, and finishes them in turn.
func (p *Pipeline) window(w *pworker, tx0, ty, k int) {
	T, l0 := p.tile, p.levels[0]
	x0, y0 := tx0*T, ty*T
	h := min(T, l0.H-y0)
	if err := p.src.Load(w.state, x0, y0, min(k*T, l0.W-x0), h); err != nil {
		p.setErr(err)
		return
	}
	for tx := tx0; tx < min(tx0+k, l0.TilesX); tx++ {
		vw := min(T, l0.W-tx*T)
		bufs := p.getBufs()
		if err := p.src.Fill(w.state, tx*T, y0, vw, h, bufs, T); err != nil {
			p.setErr(err)
			p.putBufs(bufs)
			return
		}
		p.finish(w, 0, tx, ty, bufs, vw, h)
	}
}

// finish encodes a completed tile, folds it into its overview parent, and
// releases its buffers.
func (p *Pipeline) finish(w *pworker, level, tx, ty int, bufs [][]float64, vw, vh int) {
	p.encodeTile(w, level, tx, ty, bufs, vw, vh)
	p.done.Add(1)
	if level+1 < len(p.levels) && !p.failed.Load() {
		p.reduce(w, level, tx, ty, bufs, vw, vh)
	}
	p.putBufs(bufs)
}

func (p *Pipeline) encodeTile(w *pworker, level, tx, ty int, bufs [][]float64, vw, vh int) {
	if p.enc.Interleaved {
		data, err := p.enc.EncodeInterleaved(&w.enc, bufs, vw, vh)
		if err == nil {
			err = p.out.WriteTile(level, 0, tx, ty, data)
		}
		p.setErr(err)
		return
	}
	for b, buf := range bufs {
		data, err := p.enc.Encode(&w.enc, buf, vw, vh, b)
		if err == nil {
			err = p.out.WriteTile(level, p.plane+b, tx, ty, data)
		}
		if err != nil {
			p.setErr(err)
			return
		}
	}
}

// reduce halves a finished tile into its quarter of the parent tile, and
// finishes the parent if this was the last child to arrive.
func (p *Pipeline) reduce(w *pworker, level, tx, ty int, bufs [][]float64, vw, vh int) {
	T := p.tile
	child, parent := p.levels[level], p.levels[level+1]
	key := tileKey{level + 1, tx / 2, ty / 2}
	p.ovMu.Lock()
	t := p.ov[key]
	if t == nil {
		kids := min(2, child.TilesX-2*key.tx) * min(2, child.TilesY-2*key.ty)
		t = &ovTile{bufs: p.getBufs(), pending: kids}
		p.ov[key] = t
	}
	p.ovMu.Unlock()
	// the children write disjoint quarters; ovMu orders those writes before
	// the parent is read
	off := ty%2*(T/2)*T + tx%2*(T/2)
	for b := range bufs {
		p.halve(bufs[b], vw, vh, t.bufs[b][off:])
	}
	p.ovMu.Lock()
	t.pending--
	last := t.pending == 0
	if last {
		delete(p.ov, key)
	}
	p.ovMu.Unlock()
	if last {
		p.finish(w, level+1, key.tx, key.ty, t.bufs, min(T, parent.W-key.tx*T), min(T, parent.H-key.ty*T))
	}
}

// halve writes the 2x2 reduction of a vw x vh tile into dst (both with row
// stride T): the average of the valid pixels, or the top-left one for
// nearest. An odd last row or column pairs with itself.
func (p *Pipeline) halve(src []float64, vw, vh int, dst []float64) {
	T := p.tile
	for r := 0; r < (vh+1)/2; r++ {
		a, c := src[2*r*T:], src[min(2*r+1, vh-1)*T:]
		out := dst[r*T : r*T+(vw+1)/2]
		for x := range out {
			x0 := 2 * x
			x1 := min(x0+1, vw-1)
			if !p.ovAverage {
				out[x] = a[x0]
				continue
			}
			sum, n := 0.0, 0
			for _, v := range [4]float64{a[x0], a[x1], c[x0], c[x1]} {
				if v == v {
					sum += v
					n++
				}
			}
			if n == 0 {
				out[x] = math.NaN()
			} else {
				out[x] = sum / float64(n)
			}
		}
	}
}
