// Package geotiff writes tiled GeoTIFF and BigTIFF files: compressed tiles
// written concurrently as they are produced, optional internal overviews,
// and GeoTIFF keys for planetary (user-defined) coordinate systems.
package geotiff

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"

	"cub2tif/internal/proj"
	"cub2tif/internal/raster"
)

// Compression codes.
const (
	CompNone    = 1
	CompDeflate = 8
	CompZstd    = 50000
)

// Photometric interpretations.
const (
	MinIsBlack = 1
	RGB        = 2
)

// Layout describes the file to write.
type Layout struct {
	Levels      [][2]int // width/height of the full image, then of each overview
	Tile        int      // tile size in pixels (multiple of 16)
	Samples     int      // samples per pixel, alpha included
	Type        raster.PixType
	Compression int
	Predictor   int  // 1 none, 2 horizontal, 3 floating point
	BigTIFF     bool // 64-bit offsets, for files over 4 GiB
	Interleaved bool // all samples in one tile (PlanarConfig 1) rather than one tile set per band
	Photometric int  // MinIsBlack or RGB
	Alpha       bool // the last sample is unassociated alpha
	Georef      *Georef
	NoData      string // GDAL_NODATA value, "" for none
	Metadata    string // GDAL_METADATA XML, "" for none
}

// Georef places the image on the map.
type Georef struct {
	Grid raster.Grid
	Proj *proj.Projection
}

// OverviewLevels lists the image and, if wanted, each halving until the
// image fits in one tile.
func OverviewLevels(w, h, tile int, overviews bool) [][2]int {
	dims := [][2]int{{w, h}}
	if overviews {
		for max(w, h) > tile {
			w, h = (w+1)/2, (h+1)/2
			dims = append(dims, [2]int{w, h})
		}
	}
	return dims
}

// Level is one image directory: the full image or an overview.
type Level struct {
	W, H           int
	TilesX, TilesY int
	offsets        []uint64
	counts         []uint64
}

// Writer writes a tiled TIFF. Tiles are appended in whatever order they
// arrive; the directories go at the end, on Close.
type Writer struct {
	Layout
	Levels []*Level

	f   *os.File
	mu  sync.Mutex
	pos int64 // end of the data written so far
}

// Create starts a new file.
func Create(path string, l Layout) (*Writer, error) {
	if l.Photometric == 0 {
		l.Photometric = MinIsBlack
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := &Writer{Layout: l, f: f}
	planes := l.Samples
	if l.Interleaved {
		planes = 1
	}
	for _, d := range l.Levels {
		lv := &Level{W: d[0], H: d[1], TilesX: (d[0] + l.Tile - 1) / l.Tile, TilesY: (d[1] + l.Tile - 1) / l.Tile}
		n := lv.TilesX * lv.TilesY * planes
		lv.offsets, lv.counts = make([]uint64, n), make([]uint64, n)
		w.Levels = append(w.Levels, lv)
	}
	// header; the first-directory offset is patched in by Close
	hdr := []byte{'I', 'I', 42, 0, 0, 0, 0, 0}
	if l.BigTIFF {
		hdr = []byte{'I', 'I', 43, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	}
	if _, err := f.Write(hdr); err != nil {
		f.Close()
		return nil, err
	}
	w.pos = int64(len(hdr))
	return w, nil
}

// WriteTile stores one encoded tile; plane is the band for band-separate
// files and 0 for interleaved ones. Safe for concurrent use.
func (w *Writer) WriteTile(level, plane, tx, ty int, data []byte) error {
	lv := w.Levels[level]
	idx := plane*lv.TilesX*lv.TilesY + ty*lv.TilesX + tx
	w.mu.Lock()
	off := w.pos
	w.pos += int64(len(data))
	if !w.BigTIFF && w.pos > math.MaxUint32 {
		w.mu.Unlock()
		return fmt.Errorf("output exceeds 4 GiB; rerun with --bigtiff yes")
	}
	_, err := w.f.WriteAt(data, off)
	w.mu.Unlock()
	if err != nil {
		return err
	}
	// each tile index is written by exactly one goroutine, and Close runs
	// after all of them, so these need no lock
	lv.offsets[idx] = uint64(off)
	lv.counts[idx] = uint64(len(data))
	return nil
}

// Abort closes and deletes a partially written file.
func (w *Writer) Abort() {
	w.f.Close()
	os.Remove(w.f.Name())
}

// TIFF field types
const (
	tASCII  = 2
	tShort  = 3
	tLong   = 4
	tDouble = 12
	tLong8  = 16
)

type ifdEntry struct {
	tag, typ, count int
	data            []byte // little-endian values
}

// entries builds the tags of one directory, sorted as TIFF requires.
func (w *Writer) entries(level int) []ifdEntry {
	lv := w.Levels[level]
	var e []ifdEntry
	add := func(tag, typ, count int, data []byte) { e = append(e, ifdEntry{tag, typ, count, data}) }
	perSample := func(v int) []byte {
		r := make([]int, w.Samples)
		for i := range r {
			r[i] = v
		}
		return shorts(r...)
	}

	subfile := uint64(0)
	if level > 0 {
		subfile = 1 // reduced-resolution image
	}
	add(254, tLong, 1, longs(subfile))
	add(256, tLong, 1, longs(uint64(lv.W)))
	add(257, tLong, 1, longs(uint64(lv.H)))
	add(258, tShort, w.Samples, perSample(w.Type.Size()*8))
	add(259, tShort, 1, shorts(w.Compression))
	add(262, tShort, 1, shorts(w.Photometric))
	add(277, tShort, 1, shorts(w.Samples))
	planar := 1
	if w.Samples > 1 && !w.Interleaved {
		planar = 2
	}
	add(284, tShort, 1, shorts(planar))
	if w.Predictor > 1 {
		add(317, tShort, 1, shorts(w.Predictor))
	}
	add(322, tShort, 1, shorts(w.Tile))
	add(323, tShort, 1, shorts(w.Tile))
	if w.BigTIFF {
		add(324, tLong8, len(lv.offsets), long8s(lv.offsets))
		add(325, tLong8, len(lv.counts), long8s(lv.counts))
	} else {
		add(324, tLong, len(lv.offsets), longs(lv.offsets...))
		add(325, tLong, len(lv.counts), longs(lv.counts...))
	}
	if extra := w.extraSamples(); len(extra) > 0 {
		add(338, tShort, len(extra), shorts(extra...))
	}
	add(339, tShort, w.Samples, perSample(sampleFormat(w.Type)))
	if level == 0 && w.Georef != nil {
		g := w.Georef.Grid
		add(33550, tDouble, 3, doubles(g.ResX, g.ResY, 0))
		add(33922, tDouble, 6, doubles(0, 0, 0, g.X0, g.Y0, 0))
		keys, dbl, asc := geoKeys(w.Georef.Proj)
		add(34735, tShort, len(keys), shorts(keys...))
		if len(dbl) > 0 {
			add(34736, tDouble, len(dbl), doubles(dbl...))
		}
		add(34737, tASCII, len(asc)+1, ascii(asc))
	}
	if level == 0 && w.Metadata != "" {
		add(42112, tASCII, len(w.Metadata)+1, ascii(w.Metadata))
	}
	if w.NoData != "" {
		add(42113, tASCII, len(w.NoData)+1, ascii(w.NoData))
	}
	sort.Slice(e, func(i, j int) bool { return e[i].tag < e[j].tag })
	return e
}

// extraSamples describes the samples beyond the color channels: unspecified
// data (0), with the last one unassociated alpha (2) when Alpha is set.
func (w *Writer) extraSamples() []int {
	color := 1
	if w.Photometric == RGB {
		color = 3
	}
	n := w.Samples - color
	if n <= 0 {
		return nil
	}
	extra := make([]int, n)
	if w.Alpha {
		extra[n-1] = 2
	}
	return extra
}

func sampleFormat(t raster.PixType) int {
	switch {
	case t.IsFloat():
		return 3
	case t == raster.Int8 || t == raster.Int16 || t == raster.Int32:
		return 2
	}
	return 1
}

// Close writes every directory after the tile data and patches the header.
func (w *Writer) Close() error {
	defer w.f.Close()
	entrySize, countSize, offSize := 12, 2, 4
	if w.BigTIFF {
		entrySize, countSize, offSize = 20, 8, 8
	}
	type dir struct {
		at      int64
		entries []ifdEntry
	}
	// lay out each directory followed by its out-of-line values, word aligned
	pos := w.pos + w.pos%2
	var dirs []dir
	for level := range w.Levels {
		es := w.entries(level)
		dirs = append(dirs, dir{at: pos, entries: es})
		pos += int64(countSize + len(es)*entrySize + offSize)
		for _, e := range es {
			if len(e.data) > offSize {
				pos += int64(len(e.data) + len(e.data)%2)
			}
		}
	}
	if !w.BigTIFF && pos > math.MaxUint32 {
		return fmt.Errorf("output exceeds 4 GiB; rerun with --bigtiff yes")
	}

	out := bufio.NewWriterSize(&offsetWriter{f: w.f, off: dirs[0].at}, 1<<20)
	le := binary.LittleEndian
	putOff := func(b []byte, v uint64) []byte {
		if w.BigTIFF {
			return le.AppendUint64(b, v)
		}
		return le.AppendUint32(b, uint32(v))
	}
	for i, d := range dirs {
		values := d.at + int64(countSize+len(d.entries)*entrySize+offSize)
		var head, blob []byte
		if w.BigTIFF {
			head = le.AppendUint64(head, uint64(len(d.entries)))
		} else {
			head = le.AppendUint16(head, uint16(len(d.entries)))
		}
		for _, e := range d.entries {
			head = le.AppendUint16(head, uint16(e.tag))
			head = le.AppendUint16(head, uint16(e.typ))
			head = putOff(head, uint64(e.count))
			if len(e.data) <= offSize {
				// small values sit in the entry itself, left-justified
				head = append(head, e.data...)
				head = append(head, make([]byte, offSize-len(e.data))...)
				continue
			}
			head = putOff(head, uint64(values+int64(len(blob))))
			blob = append(blob, e.data...)
			if len(e.data)%2 == 1 {
				blob = append(blob, 0)
			}
		}
		next := uint64(0)
		if i+1 < len(dirs) {
			next = uint64(dirs[i+1].at)
		}
		head = putOff(head, next)
		out.Write(head)
		out.Write(blob)
	}
	if err := out.Flush(); err != nil {
		return err
	}
	first := putOff(nil, uint64(dirs[0].at))
	at := int64(4)
	if w.BigTIFF {
		at = 8
	}
	_, err := w.f.WriteAt(first, at)
	return err
}

type offsetWriter struct {
	f   *os.File
	off int64
}

func (o *offsetWriter) Write(p []byte) (int, error) {
	n, err := o.f.WriteAt(p, o.off)
	o.off += int64(n)
	return n, err
}

func shorts(v ...int) []byte {
	b := make([]byte, 2*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(x))
	}
	return b
}

func longs(v ...uint64) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], uint32(x))
	}
	return b
}

func long8s(v []uint64) []byte {
	b := make([]byte, 8*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint64(b[8*i:], x)
	}
	return b
}

func doubles(v ...float64) []byte {
	b := make([]byte, 8*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint64(b[8*i:], math.Float64bits(x))
	}
	return b
}

func ascii(s string) []byte { return append([]byte(s), 0) }
