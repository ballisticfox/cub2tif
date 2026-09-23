# cub2tif

Fast, standalone ISIS3 cube (`.cub`) → GeoTIFF converter with built-in reprojection.
A single ~3 MB executable with no GDAL, Python or ISIS install needed.

Running `build.bat` (see [Building](#building)) produces:

```
dist\cub2tif.exe            Windows x64 (copy it anywhere)
dist\cub2tif-linux-amd64    Linux
dist\cub2tif-macos-arm64    macOS Apple Silicon
dist\cub2tif-macos-amd64    macOS Intel
```

## Guided mode

Double-click `cub2tif.exe`, or drop cubes or a folder onto it, and it walks you through the conversion:

1. **Input.** Drag a cube, several cubes, or a folder onto the window, or paste a path. It lists what it found (size, pixel type, projection, body, resolution) before anything is written, and flags files it can't read.
2. **Map projection.** Pick one with the arrow keys: keep the input's projection (optionally cropped), geographic, polar stereographic north or south, orthographic, sinusoidal, Mercator, Lambert azimuthal or conformal, transverse Mercator, or a custom spec. It only asks the questions that choice needs: centre point, latitude limit, standard parallels, lon/lat box, pixel size. Defaults come from the input, and the resulting output size is shown.
3. **Output.** A file, or a folder for batches. It warns before replacing anything.
4. **Review.** `[enter]` converts, `[e]` edits compression, overviews, 8-bit stretch, resampling and threads, and `[q]` quits.
5. **Afterwards** it prints the equivalent command line, so you can repeat or batch the same conversion without the wizard.

The wizard never starts when input or output is redirected, so scripts and agents get the usage text and a non-zero exit instead of a prompt. `cub2tif map.cub` typed into a shell converts straight away. `--wizard` starts guided mode on purpose, and `--no-pause` skips the "press any key" at the end of an Explorer-launched run.

## Quick start (command line)

```bat
cub2tif map.cub                                   :: -> map.tif next to it
cub2tif cubs\ -o out\                             :: every .cub in a folder
cub2tif *.cub -o out\ --compress zstd --overviews :: glob, zstd, internal pyramids
cub2tif map.cub --info                            :: print label / projection summary
```

## Reprojection

Any of `-p/--proj`, `--res`, `--bounds`, `--extent` or `--size` switches on warping.

```bat
cub2tif dtm.cub -p ps:north --bounds -180,60,180,90 --res 500
cub2tif dtm.cub -p ps:south                        :: whole southern hemisphere
cub2tif map.cub -p geographic                      :: plain lon/lat degree grid
cub2tif map.cub -p ortho:clat=30,clon=120 -r cubic
cub2tif map.cub -p "+proj=stere +lat_0=90 +lat_ts=70 +lon_0=45"
cub2tif map.cub --bounds -30,-10,30,20             :: crop, same projection, pixel-aligned
```

| Name (aliases)                        | Parameters (defaults come from the input) |
|---------------------------------------|-------------------------------------------|
| `geographic` (`geo`, `longlat`)       | `clon` (longitude the grid centres on)    |
| `equirectangular` (`eqc`)             | `clat` (true-scale lat), `clon`           |
| `simplecylindrical` (`simplecyl`)     | `clon`                                    |
| `sinusoidal` (`sinu`)                 | `clon`                                    |
| `orthographic` (`ortho`)              | `clat`, `clon`                            |
| `polarstereographic` (`ps`, `stere`)  | `north`/`south` or `clat` (±90, or a latitude of true scale), `clon`, `k` |
| `mercator` (`merc`)                   | `clat` (true-scale lat), `clon`           |
| `transversemercator` (`tm`, `tmerc`)  | `clat`, `clon`, `k`                       |
| `lambertconformal` (`lcc`)            | `par1`, `par2`, `clat`, `clon`            |
| `lambertazimuthal` (`laea`)           | `clat`, `clon`                            |

`R=`, `a=`, `b=` (meters) override the target body shape. PROJ strings (`+proj=eqc|sinu|ortho|stere|merc|tmerc|lcc|laea|longlat`) are accepted too.

Warp options:
- `--res` sets the pixel size, in meters or degrees. By default it matches the input.
- `--resample` is `nearest`, `bilinear` (default) or `cubic`.
- `--lat-type ocentric|ographic` sets the output latitude convention. It defaults to the input's and only matters for non-spherical bodies such as Mars.
- `--exact` turns off the 0.125-pixel transform approximation.

## Output options

| Option | Default | Notes |
|---|---|---|
| `--compress deflate\|zstd\|none` | deflate | `--level N`; float/int predictors are applied automatically |
| `--overviews` | off | internal reduced-resolution levels (`--ov-resample average\|nearest`) |
| `--type` | native | `uint8 int8 int16 uint16 int32 uint32 float32 float64` |
| `--stretch auto\|minmax\|LO,HI` | – | 8-bit browse image (0 = nodata) |
| `--normalize` | off | exact 0–1 range for image editors, plus a `.lbl` with Base/Multiplier (see below) |
| `--nodata V\|nan\|none` | ISIS Null for the type | |
| `--raw` | off | keep DNs; Base/Multiplier are written as GDAL scale/offset |
| `--bands 1,3` | all | band names from `BandBin` become band descriptions |
| `--tile N`, `--bigtiff auto\|yes\|no`, `--threads N`, `--cache MB` | 512, auto, all cores, 1024 | |

## Normalizing for GIMP and other image editors

GIMP only edits floating-point images in the 0–1 range, and most planetary data (elevations, radar backscatter) falls outside it. `--normalize` maps each cube's exact value range onto 0–1. The wizard offers this whenever it finds values outside 0–1.

```bat
cub2tif dtm.cub --normalize                     :: dtm.tif + dtm.lbl
cub2tif dtm.cub --normalize --type uint16       :: 16-bit instead of float32
```

What you get:
- **The values:** `DN = (value − min) / (max − min)`, stored as float32 0–1. With `--type uint8/uint16` they span the type's full range instead.
- **No-data:** becomes an alpha channel, transparent in GIMP and a mask in QGIS/GDAL. It's only added when the output can contain missing pixels.
- **Layout:** pixel-interleaved gray+alpha or RGB+alpha, the way image editors expect it. Overviews are skipped, because GIMP would open each level as another layer.
- **A `.lbl` next to the `.tif`:** a PVL label in ISIS style that records how to get back to real units:

  ```
  Group = Pixels
    Type          = Real
    Base          = -2001.6179199218750
    Multiplier    = 3143.1540527343750
    DnRange       = (0.0, 1.0)
    ValidMinimum  = -2001.617919921875
    ValidMaximum  = 1141.5361328125
    AlphaChannel  = Yes
  End_Group
  ```

  The conversion is `physical = DN × Multiplier + Base`, the same convention as an ISIS Pixels group. The label also holds the georeferencing (PROJ string, corner, resolution, body radii, and the ISIS Mapping group when the projection is unchanged), because image editors drop GeoTIFF tags when they save.
- **GDAL scale/offset:** the TIFF carries the same numbers, so QGIS/GDAL show physical values directly.

How accurate it is:
- **float32:** recovered values match the originals to within float32 precision (about 7.6e-5 m on a 2.5 km range).
- **uint16:** within half a quantization step.
- **After a GIMP round trip:** loading a normalized file in GIMP 3 and saving it again leaves the DNs and alpha byte-identical, so the `.lbl` still applies to the edited file.

The range always covers the whole source cube. A cropped or reprojected output can therefore use slightly less than the full 0–1 span.

## What it does with ISIS data

- **Layouts:** reads Tile and BandSequential cubes, in either byte order. All pixel types work (UnsignedByte, SignedByte, SignedWord, UnsignedWord, SignedInteger, UnsignedInteger, Real, Double). Detached labels (`^Core`) are supported.
- **Special pixels:** all ISIS special pixels (Null, Lrs, Lis, His, Hrs) become nodata.
- **Scaling:** `Base`/`Multiplier` are applied, and scaled data is written as float32. Use `--raw` to keep the stored DNs instead.
- **Georeferencing:** comes from the `Mapping` group, following ISIS's own projection math. That covers planetocentric vs planetographic latitude, PositiveWest longitudes and the equirectangular local radius. The CRS is written as standard GeoTIFF keys that GDAL, QGIS and ArcGIS read, for example `+proj=eqc ... +R=2575000`.
- **Unprojected cubes:** level-1 cubes with no Mapping group are converted without georeferencing.
- **Global maps:** these wrap across the antimeridian when warping, so there's no seam.

## Validation (against GDAL 3.12 / PROJ)

- **Straight conversion:** tested on all five sample Titan cubes. Pixels, nodata masks, geotransforms and CRS are bit-identical to GDAL's ISIS3 driver.
- **Reprojection:** all nine projections were compared with gdalwarp using exact transforms and nearest neighbour. Every pixel both tools fill is identical; the only differences are exact half-pixel ties.
- **Mars:** synthetic Mars cubes (ellipsoid, ocentric/ographic, PositiveWest) were checked against PROJ with 20k random points per projection.
- **Formats:** synthetic cubes cover int16 BSQ big-endian with Base/Multiplier, uint8 tiled, float64 detached, uint16 unprojected, and multi-band. GDAL can't open Double cubes at all.
- **Unit tests:** `go test` round-trips every projection on a sphere and an ellipsoid.

## Speed (Ryzen 7 3700X, 16 threads, 265 MB float32 cube)

| Job | cub2tif | GDAL |
|---|---|---|
| Straight convert, deflate | 0.35 s | – |
| Straight convert, uncompressed | 0.3 s | – |
| → polar stereographic (7334²) | 0.5 s | 1.6 s |
| → orthographic, cubic | 0.4 s | 8.8 s |
| → sinusoidal (exact) | 0.9 s | 52 s |

The first run on a file stored in OneDrive can be slower, because the file has to be downloaded or read from disk first.

## Building

You need Go 1.27 or newer (https://go.dev/dl/).

- `build.bat` runs vet and the tests, then builds every platform into `dist\`.
- `go build ./cmd/cub2tif` builds for the current platform only.
- `go test ./...` runs the tests.

The code follows the standard Go layout. The command lives in `cmd/`, and the libraries it uses live in `internal/`. Each package depends only on the ones listed above it in this table.

| Package | Contents |
|---|---|
| `internal/raster` | Shared basics: pixel types, grids, number formatting |
| `internal/pvl` | ISIS PVL label parser |
| `internal/proj` | Map projections (forward and inverse, sphere and ellipsoid), latitude conventions, GeoTIFF and PROJ descriptions |
| `internal/isis` | Cube reader: tiled and BSQ layouts, byte orders, special pixels, the Mapping group |
| `internal/geotiff` | Tiled (Big)TIFF writer, GeoKeys, the tile encoder (predictors, deflate/zstd, alpha) |
| `internal/convert` | The conversion engine: options, output-grid planning, reprojection, the parallel tile and overview pipeline, normalizing, and `.lbl` labels |
| `cmd/cub2tif` | The command line, the guided wizard, and the console handling |

The unit tests cover:
- label parsing
- reading every cube layout and special-pixel value
- round trips through the TIFF writer, checked with a minimal TIFF reader
- forward/inverse round trips for every projection on a sphere and an ellipsoid
- projection-spec parsing, output naming and input expansion

End-to-end accuracy was checked against GDAL 3.12 / PROJ, and against GIMP 3 for normalized output (see Validation above).
