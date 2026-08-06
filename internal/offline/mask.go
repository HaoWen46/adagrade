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
// EVERYTHING this mode writes carries student identity and handwriting BY
// CONSTRUCTION — the rendered pages, the identity crops, the match report, the
// masked pages, the mask preview, the transcription bundle. There is no version
// of these artifacts that is safe to leave world-readable on a shared machine,
// so they are written 0600 under 0700 directories, and the mode is re-asserted
// after the write: os.WriteFile and os.MkdirAll only apply their mode when they
// CREATE, so a re-run with --force over a previous run's (or an operator's)
// looser file would otherwise keep whatever permissions it already had.
//
// The two helpers below are declared here, with the rule, and are used by every
// stage that puts a byte on disk. The one place the rule stops at the door is an
// --out directory that already EXISTS: PrepareOutDir narrows only a directory it
// creates itself, since the files inside are 0600 either way.
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
//
// A region set with nothing to mask is refused outright (*RegionsError, exit 7)
// before anything is written. imaging.Mask accepts an empty region slice and
// returns a re-encoded copy — a perfectly valid MaskedImage carrying the
// untouched original. That is the one input that would let this stage CERTIFY
// an unmasked page as safe to send, so it is a run failure rather than a
// silently identity-preserving pass.
func MaskPages(pages []Page, regions RegionSet, quality int, maskedDir string) ([]MaskedPage, error) {
	maskRegions := regions.MaskRegions()
	if len(maskRegions) == 0 {
		return nil, newRegionsError(nil,
			"the region set has nothing to mask: masking an empty region set would re-encode the identity straight through and label it masked; pass --id-regions with a %q or %q region, or drop it to use the --id-band top strip",
			KindStudentID, KindName)
	}
	if err := mkdirPrivate(maskedDir); err != nil {
		return nil, newOutDirError(err, "cannot create masked-page directory %s", maskedDir)
	}

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
// The crop is the mask-region union GROWN BY CONTEXT (see maskContextRect), and
// the context is the entire point. A tile cropped to the union alone would, in
// band mode, be 100% mask fill on every page — a grid of identical gray
// rectangles that cannot answer the only question it is asked. The question is
// "did anything identifying end up OUTSIDE the gray", so the tile has to show
// the margin around the covered rectangle: a name written above the box, a
// student number that overflowed to the right.
//
// It crops the MASKED image, not the original: a preview built from the
// originals would be a single file containing every student's identity, which
// is the one artifact this mode must not create. What an operator sees is
// therefore exactly what the provider would see of that area.
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
	crop := maskContextRect(src.Bounds(), maskUnionRect(src.Bounds(), maskRegions))
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
// With no mask regions at all the whole page is the crop. That case cannot
// arrive through the pipeline — MaskPages refuses an empty mask-region set
// outright, so nothing downstream of it can hold pages masked with none — and
// the fallback exists only so a direct call degrades to "show everything"
// instead of dividing by a zero-width crop.
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

// Context margins for a preview tile, as fractions of the union's height and of
// the PAGE's width.
//
// Vertically the margin scales with the covered band, because that is the
// direction identity escapes in: a name written just above the box, or a second
// line that ran below it. Half the band's height above and below shows that
// margin without swallowing the answer area.
//
// Horizontally it is a fraction of the PAGE rather than of the union, because
// the union is often the full page width already (band mode) and 5% of a page is
// a legible margin at any DPI, whereas 5% of a narrow ID box would be a few
// pixels of nothing.
const (
	contactContextY = 0.5  // of the union's height, per side
	contactContextX = 0.05 // of the page's width, per side
)

// maskContextRect grows the mask union by the context margins, clamped to the
// page. Clamping is why a top-of-page band still produces a legible tile: the
// growth simply stops at the paper edge instead of the crop running off it.
func maskContextRect(bounds, union image.Rectangle) image.Rectangle {
	growY := int(math.Round(float64(union.Dy()) * contactContextY))
	growX := int(math.Round(float64(bounds.Dx()) * contactContextX))
	grown := image.Rect(
		union.Min.X-growX, union.Min.Y-growY,
		union.Max.X+growX, union.Max.Y+growY,
	).Intersect(bounds)
	if grown.Empty() {
		return union
	}
	return grown
}
