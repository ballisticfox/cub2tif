package isis

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeCube writes a one-band cube with an attached (or detached) label.
// vals are sample values in row-major order, already in storage units.
func writeCube(t *testing.T, path, typ, order, format string, w, h, ts, tl int, base, mult float64, vals []float64, detached bool) {
	t.Helper()
	var bo binary.ByteOrder = binary.LittleEndian
	if order == "Msb" {
		bo = binary.BigEndian
	}
	size := map[string]int{"SignedWord": 2, "Real": 4, "UnsignedByte": 1}[typ]
	put := func(b []byte, v float64) {
		switch typ {
		case "SignedWord":
			bo.PutUint16(b, uint16(int16(v)))
		case "Real":
			bo.PutUint32(b, math.Float32bits(float32(v)))
		case "UnsignedByte":
			b[0] = uint8(v)
		}
	}
	var data []byte
	tiles := ""
	if format == "Tile" {
		tx, ty := (w+ts-1)/ts, (h+tl-1)/tl
		data = make([]byte, tx*ty*ts*tl*size)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				tile := (y/tl)*tx + x/ts
				i := tile*ts*tl + (y%tl)*ts + x%ts
				put(data[i*size:], vals[y*w+x])
			}
		}
		tiles = fmt.Sprintf("    TileSamples = %d\n    TileLines = %d\n", ts, tl)
	} else {
		data = make([]byte, w*h*size)
		for i, v := range vals {
			put(data[i*size:], v)
		}
	}
	start, ptr := 1024, ""
	if detached {
		start = 0
		ptr = "    ^Core = " + filepath.Base(path) + ".dat\n"
	}
	label := fmt.Sprintf(`Object = IsisCube
  Object = Core
%s    StartByte = %d
    Format = %s
%s    Group = Dimensions
      Samples = %d
      Lines = %d
      Bands = 1
    End_Group
    Group = Pixels
      Type = %s
      ByteOrder = %s
      Base = %v
      Multiplier = %v
    End_Group
  End_Object
End_Object
End
`, ptr, start+1, format, tiles, w, h, typ, order, base, mult)
	if detached {
		must(t, os.WriteFile(path, []byte(label), 0o644))
		must(t, os.WriteFile(path+".dat", data, 0o644))
		return
	}
	buf := make([]byte, start)
	copy(buf, label)
	must(t, os.WriteFile(path, append(buf, data...), 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, path string, raw bool) (*Cube, []float64) {
	t.Helper()
	c, err := Open(path)
	must(t, err)
	t.Cleanup(func() { c.Close() })
	out := make([]float64, c.W*c.H)
	must(t, c.ReadRegion(0, 0, 0, c.W, c.H, out, c.W, raw))
	return c, out
}

func TestReadLayouts(t *testing.T) {
	const w, h = 37, 23 // not multiples of the tile size
	vals := make([]float64, w*h)
	for i := range vals {
		vals[i] = float64(i%251) - 100
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		name, order, format string
		detached            bool
	}{
		{"tile-lsb", "Lsb", "Tile", false},
		{"tile-msb", "Msb", "Tile", false},
		{"bsq-lsb", "Lsb", "BandSequential", false},
		{"bsq-msb-detached", "Msb", "BandSequential", true},
	} {
		p := filepath.Join(dir, tc.name+".cub")
		writeCube(t, p, "Real", tc.order, tc.format, w, h, 16, 10, 0, 1, vals, tc.detached)
		_, got := readAll(t, p, false)
		for i := range vals {
			if got[i] != vals[i] {
				t.Fatalf("%s: pixel %d = %v, want %v", tc.name, i, got[i], vals[i])
			}
		}
		// a window straddling tile boundaries, written with a wider stride
		c, _ := readAll(t, p, false)
		win := make([]float64, 20*9)
		must(t, c.ReadRegion(0, 11, 7, 12, 9, win, 20, false))
		for r := 0; r < 9; r++ {
			for col := 0; col < 12; col++ {
				if want := vals[(7+r)*w+11+col]; win[r*20+col] != want {
					t.Fatalf("%s: window (%d,%d) = %v, want %v", tc.name, col, r, win[r*20+col], want)
				}
			}
		}
	}
}

func TestSpecialPixelsAndScaling(t *testing.T) {
	dir := t.TempDir()
	// SignedWord: -32768 Null, -32767 Lrs, -32766 Lis, -32765 His, -32764 Hrs, -32752 lowest valid
	p := filepath.Join(dir, "i16.cub")
	writeCube(t, p, "SignedWord", "Msb", "BandSequential", 7, 1, 0, 0, 100, 0.5,
		[]float64{-32768, -32767, -32766, -32765, -32764, -32752, 10}, false)
	_, scaled := readAll(t, p, false)
	_, raw := readAll(t, p, true)
	for i := 0; i < 5; i++ {
		if !math.IsNaN(scaled[i]) || !math.IsNaN(raw[i]) {
			t.Errorf("special pixel %d not mapped to NaN: %v %v", i, scaled[i], raw[i])
		}
	}
	if scaled[6] != 105 || raw[6] != 10 {
		t.Errorf("Base/Multiplier: got %v (scaled) %v (raw), want 105, 10", scaled[6], raw[6])
	}
	if raw[5] != -32752 {
		t.Errorf("lowest valid SignedWord mapped to %v", raw[5])
	}

	// Real: the five special values sit just above -FLT_MAX
	p = filepath.Join(dir, "f32.cub")
	specials := []float64{}
	for bits := uint32(0xFF7FFFFB); bits <= 0xFF7FFFFF; bits++ {
		specials = append(specials, float64(math.Float32frombits(bits)))
	}
	writeCube(t, p, "Real", "Lsb", "Tile", 6, 1, 8, 8, 0, 1, append(specials, 1.5), false)
	_, f := readAll(t, p, false)
	for i := 0; i < 5; i++ {
		if !math.IsNaN(f[i]) {
			t.Errorf("Real special %d = %v, want NaN", i, f[i])
		}
	}
	if f[5] != 1.5 {
		t.Errorf("valid Real = %v", f[5])
	}

	// UnsignedByte: 0 is Null/Lrs/Lis, 255 is His/Hrs
	p = filepath.Join(dir, "u8.cub")
	writeCube(t, p, "UnsignedByte", "Lsb", "BandSequential", 3, 1, 0, 0, 0, 1, []float64{0, 255, 7}, false)
	_, u := readAll(t, p, false)
	if !math.IsNaN(u[0]) || !math.IsNaN(u[1]) || u[2] != 7 {
		t.Errorf("UnsignedByte specials: %v", u)
	}
}

func TestTruncatedCube(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.cub")
	writeCube(t, p, "Real", "Lsb", "Tile", 40, 40, 16, 16, 0, 1, make([]float64, 1600), false)
	b, _ := os.ReadFile(p)
	must(t, os.WriteFile(p, b[:len(b)-100], 0o644))
	if _, err := Open(p); err == nil {
		t.Error("a truncated cube must fail to open")
	}
}
