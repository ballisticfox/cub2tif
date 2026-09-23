package convert

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"cub2tif/internal/geotiff"
)

// TileSource produces float64 pixels (NaN = no data) for the output grid.
// Fill may be called concurrently, each goroutine with its own state.
type TileSource interface {
	Bands() int
	NewState() any
	Fill(state any, x0, y0, w, h int, dst [][]float64, stride int) error
}

type Pipeline struct {
	src       TileSource
	w         *geotiff.Writer
	enc       *geotiff.Encoder
	threads   int
	ovAverage bool
	progress  func(done, total int64)

	tile    int
	nb      int
	bufPool sync.Pool
	jobs    chan func(*pworker)
	wg      sync.WaitGroup
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

type strip struct {
	ty      int
	h       int
	tiles   [][][]float64 // [tx][band] T*T buffers
	pending atomic.Int32
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

func (p *Pipeline) getBuf() []float64 {
	if b, ok := p.bufPool.Get().(*[]float64); ok {
		return *b
	}
	return make([]float64, p.tile*p.tile)
}

func (p *Pipeline) putBuf(b []float64) { p.bufPool.Put(&b) }

// Run fills and encodes every tile of every level. Full-resolution tiles
// are processed in parallel; when there are overviews, finished rows of
// tiles are handed in order to a cascade that averages them down level by
// level, so memory stays at a few tile rows per level however large the
// image is.
func (p *Pipeline) Run() error {
	T := p.w.Tile
	p.tile = T
	p.nb = p.src.Bands()
	for _, l := range p.w.Levels {
		p.total += int64(l.TilesX * l.TilesY)
	}
	p.jobs = make(chan func(*pworker), p.threads*4)
	var workers sync.WaitGroup
	for i := 0; i < p.threads; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := &pworker{state: p.src.NewState()}
			for j := range p.jobs {
				j(w)
			}
		}()
	}

	stopProgress := make(chan struct{})
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		if p.progress == nil {
			return
		}
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopProgress:
				p.progress(p.done.Load(), p.total)
				return
			case <-ticker.C:
				p.progress(p.done.Load(), p.total)
			}
		}
	}()

	l0 := p.w.Levels[0]
	overviews := len(p.w.Levels) > 1
	var stripCh chan *strip
	var cascadeDone chan struct{}
	var stripSem chan struct{}
	if overviews {
		stripCh = make(chan *strip, l0.TilesY)
		cascadeDone = make(chan struct{})
		inflight := max(2, (2*p.threads+l0.TilesX-1)/l0.TilesX+1)
		stripSem = make(chan struct{}, inflight)
		go p.cascade(stripCh, stripSem, cascadeDone)
	}

	var wg0 sync.WaitGroup // level-0 jobs only
	for ty := 0; ty < l0.TilesY && !p.failed.Load(); ty++ {
		var s *strip
		if overviews {
			stripSem <- struct{}{}
			s = &strip{ty: ty, h: min(T, l0.H-ty*T), tiles: make([][][]float64, l0.TilesX)}
			s.pending.Store(int32(l0.TilesX))
		}
		for tx := 0; tx < l0.TilesX; tx++ {
			p.wg.Add(1)
			wg0.Add(1)
			p.jobs <- func(w *pworker) {
				defer p.wg.Done()
				defer wg0.Done()
				x0, y0 := tx*T, ty*T
				vw, vh := min(T, l0.W-x0), min(T, l0.H-y0)
				bufs := make([][]float64, p.nb)
				for b := range bufs {
					bufs[b] = p.getBuf()
				}
				if !p.failed.Load() {
					if err := p.src.Fill(w.state, x0, y0, vw, vh, bufs, T); err != nil {
						p.setErr(err)
					} else {
						p.encodeTile(w, 0, tx, ty, bufs, vw, vh)
					}
				}
				p.done.Add(1)
				if s != nil {
					s.tiles[tx] = bufs
					if s.pending.Add(-1) == 0 {
						stripCh <- s
					}
				} else {
					for _, b := range bufs {
						p.putBuf(b)
					}
				}
			}
		}
	}
	if overviews {
		wg0.Wait()
		close(stripCh)
		<-cascadeDone
	}
	p.wg.Wait()
	close(p.jobs)
	workers.Wait()
	close(stopProgress)
	<-progressDone
	return p.err
}

func (p *Pipeline) encodeTile(w *pworker, level, tx, ty int, bufs [][]float64, vw, vh int) {
	if p.enc.Interleaved {
		data, err := p.enc.EncodeInterleaved(&w.enc, bufs, vw, vh)
		if err == nil {
			err = p.w.WriteTile(level, 0, tx, ty, data)
		}
		p.setErr(err)
		return
	}
	for b, buf := range bufs {
		data, err := p.enc.Encode(&w.enc, buf, vw, vh, b)
		if err != nil {
			p.setErr(err)
			return
		}
		if err := p.w.WriteTile(level, b, tx, ty, data); err != nil {
			p.setErr(err)
			return
		}
	}
}

// ovLevel accumulates rows of one overview level until a full strip of
// tiles is available.
type ovLevel struct {
	idx     int
	W, H    int
	acc     [][]float64 // per band, T rows x W
	rows    int
	stripTY int
}

func (p *Pipeline) cascade(in chan *strip, sem chan struct{}, done chan struct{}) {
	defer close(done)
	T := p.tile
	var levels []*ovLevel
	for i := 1; i < len(p.w.Levels); i++ {
		l := p.w.Levels[i]
		ol := &ovLevel{idx: i, W: l.W, H: l.H, acc: make([][]float64, p.nb)}
		for b := range ol.acc {
			ol.acc[b] = make([]float64, T*l.W)
		}
		levels = append(levels, ol)
	}
	l0 := p.w.Levels[0]
	pending := map[int]*strip{}
	next := 0
	rows := make([][]float64, p.nb)
	for b := range rows {
		rows[b] = make([]float64, T*l0.W)
	}
	for s := range in {
		pending[s.ty] = s
		for {
			s, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			if !p.failed.Load() {
				// assemble full-width rows of this strip
				for b := 0; b < p.nb; b++ {
					for tx, tb := range s.tiles {
						vw := min(T, l0.W-tx*T)
						for r := 0; r < s.h; r++ {
							copy(rows[b][r*l0.W+tx*T:r*l0.W+tx*T+vw], tb[b][r*T:r*T+vw])
						}
					}
				}
			}
			for _, tb := range s.tiles {
				for _, b := range tb {
					p.putBuf(b)
				}
			}
			if !p.failed.Load() {
				p.feed(levels, 0, rows, s.h, l0.W)
			}
			<-sem
		}
	}
	if !p.failed.Load() {
		for i := range levels {
			p.flush(levels, i)
		}
	}
}

// feed downsamples nrows x w rows into overview level li.
func (p *Pipeline) feed(levels []*ovLevel, li int, rows [][]float64, nrows, w int) {
	if li >= len(levels) {
		return
	}
	ol := levels[li]
	T := p.tile
	for r := 0; r < nrows; r += 2 {
		r2 := min(r+1, nrows-1)
		for b := 0; b < p.nb; b++ {
			src := rows[b]
			dst := ol.acc[b][ol.rows*ol.W : (ol.rows+1)*ol.W]
			a, c := src[r*w:(r+1)*w], src[r2*w:(r2+1)*w]
			for x := 0; x < ol.W; x++ {
				x0 := 2 * x
				x1 := min(x0+1, w-1)
				if !p.ovAverage {
					dst[x] = a[x0]
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
					dst[x] = math.NaN()
				} else {
					dst[x] = sum / float64(n)
				}
			}
		}
		ol.rows++
		if ol.rows == T {
			p.emit(levels, li)
		}
	}
}

func (p *Pipeline) flush(levels []*ovLevel, li int) {
	if levels[li].rows > 0 {
		p.emit(levels, li)
	}
}

// emit encodes the accumulated strip of level li and feeds the next level.
func (p *Pipeline) emit(levels []*ovLevel, li int) {
	ol := levels[li]
	T := p.tile
	level := p.w.Levels[ol.idx]
	ty := ol.stripTY
	nrows := ol.rows
	for tx := 0; tx < level.TilesX; tx++ {
		vw := min(T, ol.W-tx*T)
		bufs := make([][]float64, p.nb)
		for b := range bufs {
			buf := p.getBuf()
			for r := 0; r < nrows; r++ {
				copy(buf[r*T:r*T+vw], ol.acc[b][r*ol.W+tx*T:r*ol.W+tx*T+vw])
			}
			bufs[b] = buf
		}
		p.wg.Add(1)
		p.jobs <- func(w *pworker) {
			defer p.wg.Done()
			if !p.failed.Load() {
				p.encodeTile(w, ol.idx, tx, ty, bufs, vw, nrows)
			}
			for _, b := range bufs {
				p.putBuf(b)
			}
			p.done.Add(1)
		}
	}
	ol.stripTY++
	p.feed(levels, li+1, ol.acc, nrows, ol.W)
	ol.rows = 0
}
