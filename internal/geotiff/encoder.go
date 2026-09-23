package geotiff

import (
	"bytes"
	"encoding/binary"
	"math"

	"github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"

	"cub2tif/internal/raster"
)

// Stretch maps [Lo, Hi] linearly onto 1..255, keeping 0 for no data.
type Stretch struct{ Lo, Hi float64 }

// Normalize maps values linearly so that Lo lands on 0 and the top of the
// range on DNMax: DN = (v - Lo) * K.
type Normalize struct{ Lo, K, DNMax float64 }

// EncoderConfig says how float64 pixels become stored samples.
type EncoderConfig struct {
	Type        raster.PixType
	NoData      float64 // written for missing pixels when HasNoData
	HasNoData   bool
	Stretch     []Stretch  // per band; nil = none
	Normalize   *Normalize // nil = none
	Interleaved bool       // all bands in one tile, plus alpha if Alpha
	Alpha       bool       // append an alpha sample: opaque where any band has data
	Compression int
	Level       int // compression level
	Predictor   int
	Tile        int
}

// Encoder turns float64 tile buffers (NaN = no data) into compressed tiles.
// It is safe for concurrent use; each goroutine brings its own Scratch.
type Encoder struct {
	EncoderConfig
	zstd *zstd.Encoder
}

func NewEncoder(c EncoderConfig) (*Encoder, error) {
	e := &Encoder{EncoderConfig: c}
	if c.Compression == CompZstd {
		z, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(c.Level)), zstd.WithEncoderCRC(false))
		if err != nil {
			return nil, err
		}
		e.zstd = z
	}
	return e, nil
}

// Scratch holds one goroutine's reusable encoding buffers.
type Scratch struct {
	raw  []byte
	out  bytes.Buffer
	zw   *zlib.Writer
	zb   []byte
	zout []byte
}

// fill is what a missing sample is written as.
func (e *Encoder) fill() float64 {
	switch {
	case e.Alpha:
		return 0 // hidden by alpha; keep it inside the editor's range
	case e.HasNoData:
		return e.NoData
	case e.Type.IsFloat():
		return math.NaN()
	}
	return 0
}

// convert maps one valid (non-NaN) value to what is stored.
func (e *Encoder) convert(x float64, band int, lo, hi float64) float64 {
	switch {
	case e.Normalize != nil:
		x = (x - e.Normalize.Lo) * e.Normalize.K
		if x < 0 {
			x = 0
		} else if x > e.Normalize.DNMax { // cubic overshoot
			x = e.Normalize.DNMax
		}
		if !e.Type.IsFloat() {
			x = math.Round(x)
		}
		return x
	case e.Stretch != nil:
		st := e.Stretch[band]
		if st.Hi > st.Lo {
			x = 1 + (x-st.Lo)*254/(st.Hi-st.Lo)
		} else {
			x = 128
		}
		return math.Round(math.Min(math.Max(x, 1), 255))
	case !e.Type.IsFloat():
		return math.Min(math.Max(math.Round(x), lo), hi)
	case e.Type == raster.Float32 && math.Abs(x) > math.MaxFloat32:
		return math.Copysign(math.MaxFloat32, x)
	}
	return x
}

// put stores sample i (in samples, not bytes) little-endian.
func put(raw []byte, pt raster.PixType, i int, v float64) {
	switch pt {
	case raster.Uint8:
		raw[i] = uint8(v)
	case raster.Int8:
		raw[i] = uint8(int8(v))
	case raster.Int16:
		binary.LittleEndian.PutUint16(raw[2*i:], uint16(int16(v)))
	case raster.Uint16:
		binary.LittleEndian.PutUint16(raw[2*i:], uint16(v))
	case raster.Int32:
		binary.LittleEndian.PutUint32(raw[4*i:], uint32(int32(v)))
	case raster.Uint32:
		binary.LittleEndian.PutUint32(raw[4*i:], uint32(v))
	case raster.Float32:
		binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(float32(v)))
	case raster.Float64:
		binary.LittleEndian.PutUint64(raw[8*i:], math.Float64bits(v))
	}
}

func (s *Scratch) rawBuf(n int) []byte {
	if cap(s.raw) < n {
		s.raw = make([]byte, n)
	}
	return s.raw[:n]
}

// Encode converts a T x T buffer (valid area vw x vh) of one band.
func (e *Encoder) Encode(s *Scratch, buf []float64, vw, vh, band int) ([]byte, error) {
	T := e.Tile
	raw := s.rawBuf(T * T * e.Type.Size())
	fill := e.fill()
	lo, hi := e.Type.Range()
	for r := 0; r < T; r++ {
		row := buf[r*T : r*T+T]
		for c := 0; c < T; c++ {
			v := fill
			if r < vh && c < vw {
				if x := row[c]; x == x {
					v = e.convert(x, band, lo, hi)
				}
			}
			put(raw, e.Type, r*T+c, v)
		}
	}
	return e.finish(s, raw, 1)
}

// EncodeInterleaved interleaves all bands (plus alpha if enabled) into one tile.
func (e *Encoder) EncodeInterleaved(s *Scratch, bufs [][]float64, vw, vh int) ([]byte, error) {
	T := e.Tile
	nb := len(bufs)
	spp := nb
	if e.Alpha {
		spp++
	}
	raw := s.rawBuf(T * T * spp * e.Type.Size())
	fill := e.fill()
	lo, hi := e.Type.Range()
	opaque := 1.0
	if e.Normalize != nil {
		opaque = e.Normalize.DNMax
	} else if !e.Type.IsFloat() {
		opaque = hi
	}
	for r := 0; r < T; r++ {
		for c := 0; c < T; c++ {
			i := r*T + c
			o := i * spp
			hasData := false
			for b := 0; b < nb; b++ {
				v := fill
				if r < vh && c < vw {
					if x := bufs[b][i]; x == x {
						v = e.convert(x, b, lo, hi)
						hasData = true
					}
				}
				put(raw, e.Type, o+b, v)
			}
			if e.Alpha {
				a := 0.0
				if hasData {
					a = opaque
				}
				put(raw, e.Type, o+nb, a)
			}
		}
	}
	return e.finish(s, raw, spp)
}

// finish applies the predictor and compression to a raw tile.
func (e *Encoder) finish(s *Scratch, raw []byte, spp int) ([]byte, error) {
	if e.Compression == CompNone {
		return raw, nil
	}
	T, bps := e.Tile, e.Type.Size()
	switch e.Predictor {
	case 2:
		predictHorizontal(raw, T, bps, spp)
	case 3:
		n := T * spp * bps
		if cap(s.zb) < n {
			s.zb = make([]byte, n)
		}
		predictFloat(raw, T, bps, spp, s.zb[:n])
	}
	switch e.Compression {
	case CompDeflate:
		s.out.Reset()
		if s.zw == nil {
			zw, err := zlib.NewWriterLevel(&s.out, e.Level)
			if err != nil {
				return nil, err
			}
			s.zw = zw
		} else {
			s.zw.Reset(&s.out)
		}
		if _, err := s.zw.Write(raw); err != nil {
			return nil, err
		}
		if err := s.zw.Close(); err != nil {
			return nil, err
		}
		return s.out.Bytes(), nil
	case CompZstd:
		s.zout = e.zstd.EncodeAll(raw, s.zout[:0])
		return s.zout, nil
	}
	return raw, nil
}

// predictHorizontal applies TIFF predictor 2 in place (little-endian data).
// Each sample is differenced against the same channel one pixel to the left.
func predictHorizontal(raw []byte, T, bps, spp int) {
	n := T * spp // samples per row
	for r := 0; r < T; r++ {
		row := raw[r*n*bps : (r+1)*n*bps]
		switch bps {
		case 1:
			for c := n - 1; c >= spp; c-- {
				row[c] -= row[c-spp]
			}
		case 2:
			for c := n - 1; c >= spp; c-- {
				v := binary.LittleEndian.Uint16(row[2*c:]) - binary.LittleEndian.Uint16(row[2*(c-spp):])
				binary.LittleEndian.PutUint16(row[2*c:], v)
			}
		case 4:
			for c := n - 1; c >= spp; c-- {
				v := binary.LittleEndian.Uint32(row[4*c:]) - binary.LittleEndian.Uint32(row[4*(c-spp):])
				binary.LittleEndian.PutUint32(row[4*c:], v)
			}
		case 8:
			for c := n - 1; c >= spp; c-- {
				v := binary.LittleEndian.Uint64(row[8*c:]) - binary.LittleEndian.Uint64(row[8*(c-spp):])
				binary.LittleEndian.PutUint64(row[8*c:], v)
			}
		}
	}
}

// predictFloat applies TIFF predictor 3 (floating point) in place, as
// libtiff's fpDiff: split each row into byte planes (most significant first),
// then difference the bytes with a stride of samples-per-pixel.
func predictFloat(raw []byte, T, bps, spp int, tmp []byte) {
	wc := T * spp // samples per row
	for r := 0; r < T; r++ {
		row := raw[r*wc*bps : (r+1)*wc*bps]
		copy(tmp, row)
		if bps == 4 {
			p0, p1, p2, p3 := row[:wc], row[wc:2*wc], row[2*wc:3*wc], row[3*wc:4*wc]
			for c := 0; c < wc; c++ {
				s := tmp[4*c : 4*c+4]
				p0[c], p1[c], p2[c], p3[c] = s[3], s[2], s[1], s[0]
			}
		} else {
			for c := 0; c < wc; c++ {
				for b := 0; b < bps; b++ {
					row[(bps-1-b)*wc+c] = tmp[bps*c+b]
				}
			}
		}
		for i := len(row) - 1; i >= spp; i-- {
			row[i] -= row[i-spp]
		}
	}
}
