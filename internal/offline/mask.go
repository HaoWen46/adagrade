package offline

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	xdraw "golang.org/x/image/draw"

	"github.com/HaoWen46/adagrade/internal/imaging"
)

// ---------------------------------------------------------------------------
// PII file modes.
//
// Everything the post-match stages write — masked pages, the mask preview, the
// transcription bundle — carries student identity and handwriting BY
// CONSTRUCTION. There is no version of these artifacts that is safe to leave
// world-readable on a shared machine, so they are written 0600 under 0700
// directories, and the mode is re-asserted after the write: os.WriteFile and
// os.MkdirAll only apply their mode when they CREATE, so a re-run with --force
// over a previous run's (or an operator's) looser file would otherwise keep
// whatever permissions it already had.
//
// The rendered pages and crops of the match stage keep their own modes; this
// rule governs the files these four stages write.
// ---------------------------------------------------------------------------

const (
	privateFileMode fs.FileMode = 0o600
	privateDirMode  fs.FileMode = 0o700
)

// mkdirPrivate creates dir (with parents) and pins its mode. Returns the raw
// error; callers wrap it in the typed error their stage owes the operator.
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, privateDirMode); err != nil {
		return err
	}
	return os.Chmod(dir, privateDirMode)
}

// writePrivate writes body to path and pins the file's mode.
func writePrivate(path string, body []byte) error {
	if err := os.WriteFile(path, body, privateFileMode); err != nil {
		return err
	}
	return os.Chmod(path, privateFileMode)
}

// MaskedPage pairs a rendered page with its masked derivative.
//
// The split is the D10 seal made concrete: Page.JPEG is the ORIGINAL raster,
// which stays on this machine (it is what the professor's bundle ships), and
// Masked is the only thing that may be handed to a provider — imaging.
// MaskedImage's fields are unexported, so a stage that tries to send the
// original does not compile.
type MaskedPage struct {
	Page   Page
	Masked imaging.MaskedImage
}

// MaskPages paints over every page's identity regions and writes each masked
// page to maskedDir/pNNNN.jpg — the same %04d numbering as pages/, so
// pages/p0007.jpg and masked/p0007.jpg are the same sheet of paper.
//
// regions.MaskRegions() decides what is covered: student_id and name, never
// problem_id (D66 — the problem number is not identity, and covering it would
// break grading to protect nothing).
//
// A page that cannot be masked FAILS THE RUN (*ScanError, exit 4). There is no
// best-effort skip here on purpose: masking is the privacy gate, and a "skip"
// would leave the orchestrator holding a page with no masked twin, one zip
// field away from sending the original.
func MaskPages(pages []Page, regions RegionSet, quality int, maskedDir string) ([]MaskedPage, error) {
	if err := mkdirPrivate(maskedDir); err != nil {
		return nil, newOutDirError(err, "cannot create masked-page directory %s", maskedDir)
	}
	maskRegions := regions.MaskRegions()

	out := make([]MaskedPage, 0, len(pages))
	for _, page := range pages {
		masked, err := imaging.Mask(page.JPEG, maskRegions, quality)
		if err != nil {
			return nil, newScanError(err, "cannot mask page %d (%s page %d): it is not a decodable JPEG",
				page.Index, page.SourcePDF, page.SourcePage)
		}
		path := filepath.Join(maskedDir, PageFilename(page.Index))
		if err := writePrivate(path, masked.JPEG()); err != nil {
			return nil, newOutDirError(err, "cannot write masked page %s", path)
		}
		out = append(out, MaskedPage{Page: page, Masked: masked})
	}
	return out, nil
}

// Contact-sheet geometry. A sheet is for one job: scanning down a column of
// identity strips to spot a name the mask missed. 420px is wide enough to read
// a handwritten ID at a glance, six across fits a laptop screen, and 60 tiles
// keeps a sheet short enough to page through instead of scroll forever.
const (
	contactTileWidth     = 420
	contactTilesAcross   = 6
	contactTilesPerSheet = 60
	contactSheetQuality  = 85
)

// WriteContactSheet writes the mask preview: every page's identity area,
// AFTER masking, tiled into one image the operator can check at a glance.
//
// The crop is the union of the MASK regions (padded exactly as the mask itself
// is), so a tile shows the covered rectangle plus the slack around it — which
// is where an escaped name actually shows up. It deliberately crops the MASKED
// image, not the original: a preview of the originals would be a single file
// containing every student's identity, which is the one artifact this mode must
// not create.
//
// Sheets hold up to 60 tiles. The first is written to outPath and each overflow
// sheet gets a -02, -03, … suffix before the extension. Every path written is
// returned, in order.
//
// No pages means no file: an empty sheet would be a preview asserting "nothing
// was masked", which a later reader has no way to distinguish from a crash.
func WriteContactSheet(masked []MaskedPage, regions RegionSet, outPath string) ([]string, error) {
	if len(masked) == 0 {
		return nil, nil
	}
	if err := mkdirPrivate(filepath.Dir(outPath)); err != nil {
		return nil, newOutDirError(err, "cannot create the directory for the mask preview %s", outPath)
	}

	maskRegions := regions.MaskRegions()
	var paths []string
	for start, sheet := 0, 1; start < len(masked); start, sheet = start+contactTilesPerSheet, sheet+1 {
		end := start + contactTilesPerSheet
		if end > len(masked) {
			end = len(masked)
		}
		img, err := contactSheet(masked[start:end], maskRegions)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: contactSheetQuality}); err != nil {
			return nil, newOutDirError(err, "cannot encode the mask preview")
		}
		path := sheetPath(outPath, sheet)
		if err := writePrivate(path, buf.Bytes()); err != nil {
			return nil, newOutDirError(err, "cannot write the mask preview %s", path)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// sheetPath is outPath for sheet 1 and outPath with a -NN suffix before the
// extension for the rest, so the overflow sheets sort next to the first one.
func sheetPath(outPath string, sheet int) string {
	if sheet <= 1 {
		return outPath
	}
	ext := filepath.Ext(outPath)
	base := strings.TrimSuffix(outPath, ext)
	return fmt.Sprintf("%s-%02d%s", base, sheet, ext)
}

// contactSheet renders one sheet: each page's identity crop scaled to a
// fixed-width tile, laid out six across.
//
// Row heights are taken per row rather than assumed uniform. Pages of one scan
// are the same size in practice, but a mixed-DPI batch would otherwise write
// tiles over each other — silently, and in the one artifact whose whole job is
// to be trusted.
func contactSheet(masked []MaskedPage, maskRegions []imaging.Region) (image.Image, error) {
	tiles := make([]image.Image, 0, len(masked))
	for _, mp := range masked {
		tile, err := identityTile(mp, maskRegions)
		if err != nil {
			return nil, err
		}
		tiles = append(tiles, tile)
	}

	cols := len(tiles)
	if cols > contactTilesAcross {
		cols = contactTilesAcross
	}
	rowHeights := make([]int, 0, (len(tiles)+contactTilesAcross-1)/contactTilesAcross)
	total := 0
	for i := 0; i < len(tiles); i += contactTilesAcross {
		high := 0
		for j := i; j < i+contactTilesAcross && j < len(tiles); j++ {
			if h := tiles[j].Bounds().Dy(); h > high {
				high = h
			}
		}
		rowHeights = append(rowHeights, high)
		total += high
	}

	sheet := image.NewRGBA(image.Rect(0, 0, cols*contactTileWidth, total))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	y := 0
	for row, height := range rowHeights {
		for col := 0; col < contactTilesAcross; col++ {
			i := row*contactTilesAcross + col
			if i >= len(tiles) {
				break
			}
			b := tiles[i].Bounds()
			target := image.Rect(col*contactTileWidth, y, col*contactTileWidth+b.Dx(), y+b.Dy())
			draw.Draw(sheet, target, tiles[i], b.Min, draw.Src)
		}
		y += height
	}
	return sheet, nil
}

// identityTile crops one masked page to its identity area and scales it to the
// tile width, preserving aspect.
func identityTile(mp MaskedPage, maskRegions []imaging.Region) (image.Image, error) {
	src, err := jpeg.Decode(bytes.NewReader(mp.Masked.JPEG()))
	if err != nil {
		return nil, newScanError(err, "cannot decode the masked image of page %d for the mask preview", mp.Page.Index)
	}
	crop := maskUnionRect(src.Bounds(), maskRegions)
	height := int(math.Round(float64(crop.Dy()) * contactTileWidth / float64(crop.Dx())))
	if height < 1 {
		height = 1
	}
	tile := image.NewRGBA(image.Rect(0, 0, contactTileWidth, height))
	xdraw.ApproxBiLinear.Scale(tile, tile.Bounds(), src, crop, xdraw.Src, nil)
	return tile, nil
}

// maskUnionRect is the pixel rectangle covering every mask region, padded the
// same way the mask pads and clamped to the image.
//
// The normalized→pixel arithmetic (floor the origin, ceil the far edge, grow by
// padding, intersect with bounds) mirrors imaging.Mask's, so the preview frames
// exactly what was painted. imaging.CropImage is not reused here despite owning
// that math: it STACKS its regions vertically and seals the result as an
// IDCrop's JPEG bytes, which is the wrong shape twice over for a union crop
// that has to be scaled into a tile.
//
// With no mask regions at all — only reachable from a zero-value RegionSet,
// since both BandRegions and LoadRegions guarantee one — the whole page is the
// crop, which reads as "nothing was covered" rather than crashing.
func maskUnionRect(bounds image.Rectangle, regions []imaging.Region) image.Rectangle {
	wPx := float64(bounds.Dx())
	hPx := float64(bounds.Dy())
	union := image.Rectangle{}
	for _, r := range regions {
		x0 := int(math.Floor((r.X - r.Padding) * wPx))
		y0 := int(math.Floor((r.Y - r.Padding) * hPx))
		x1 := int(math.Ceil((r.X + r.W + r.Padding) * wPx))
		y1 := int(math.Ceil((r.Y + r.H + r.Padding) * hPx))
		rect := image.Rect(x0, y0, x1, y1).Add(bounds.Min).Intersect(bounds)
		if rect.Empty() {
			continue
		}
		if union.Empty() {
			union = rect
			continue
		}
		union = union.Union(rect)
	}
	if union.Empty() {
		return bounds
	}
	return union
}
