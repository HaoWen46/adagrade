package offline

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/localocr"
)

// cropQuality is the JPEG quality for identity crops. Crops are small and are
// the OCR's only view of the page, so they are encoded above the page quality:
// a compression artifact on a stroke costs a character.
const cropQuality = 92

// FieldLattices is everything the local OCR read out of ONE field's crop: the
// per-line CTC lattices, in reading order.
//
// Nothing is filtered. A line the recognizer scored 0.01 stays, because the
// closed-set scorer (score.go) is precisely the thing that can rescue a line
// greedy decoding got wrong — dropping it here would throw away the evidence
// before the only stage able to use it.
type FieldLattices struct {
	Lines []localocr.LineLattice
}

// Identity is one page's identity read: the lattices per configured region kind.
//
// A kind is present only if the region set configured it. Absent means "there
// was nothing to read", which the scorer must not confuse with "read and blank"
// — a page whose problem_id region simply does not exist scores the same as one
// whose problem box was left empty, but they arrive here differently.
//
// In band mode (RegionSet.Banded) all three kinds ALIAS one FieldLattices: one
// crop of the top strip is read once, and the same Lines slice is referenced by
// student_id, name and problem_id. Callers must treat Lines as read-only, since
// mutating it through one key changes all three.
type Identity struct {
	Fields map[Kind]FieldLattices
}

// latticeReader is the local-OCR seam this stage needs: crop in, lattices out.
// It is deliberately narrower than *localocr.Engine so unit tests need no ONNX
// runtime, no model files and no dictionary.
type latticeReader interface {
	ReadLattices(ctx context.Context, crop imaging.IDCrop) ([]localocr.LineLattice, error)
}

// CropFilename is the artifact name for one page's crop of one kind:
// p0007-student_id.jpg. Band mode uses bandCropFilename instead.
func CropFilename(index int, kind Kind) string { return fmt.Sprintf("p%04d-%s.jpg", index, kind) }

// bandCropFilename names the single band-mode crop. It is deliberately NOT
// three copies of one image under three kind names: the artifact directory is
// the audit trail, and three identical files would suggest three regions were
// read when only one was.
func bandCropFilename(index int) string { return fmt.Sprintf("p%04d-band.jpg", index) }

// ReadIdentity crops the page's identity regions and runs the local OCR over
// each one, writing every crop to cropsDir as an artifact.
//
// The page raster is decoded ONCE and every crop is taken from it (imaging's F8
// path), so a three-region page costs one JPEG decode rather than three.
//
// Failures are typed by what the operator would have to fix: an undecodable
// page is a *ScanError (exit 4), a region that lands off the page is a
// *RegionsError (exit 7), and an OCR failure is an *OCRError (exit 6). A crop
// that contains no ink is NOT a failure — it reads as zero lines, and the
// scorer treats the field as contributing nothing.
func ReadIdentity(ctx context.Context, ocr latticeReader, page Page, regions RegionSet, cropsDir string) (Identity, error) {
	src, err := jpeg.Decode(bytes.NewReader(page.JPEG))
	if err != nil {
		return Identity{}, newScanError(err, "page %d (%s page %d) is not a decodable JPEG", page.Index, page.SourcePDF, page.SourcePage)
	}
	if err := os.MkdirAll(cropsDir, 0o755); err != nil {
		return Identity{}, newOutDirError(err, "cannot create crop directory %s", cropsDir)
	}

	id := Identity{Fields: make(map[Kind]FieldLattices, len(kindOrder))}

	// Band mode: one rectangle answers for every kind, so it is cropped and read
	// once and the result is shared. Reading it three times would be three
	// identical inference passes over the same pixels.
	if regions.Banded() {
		region, ok := regions.Get(KindStudentID)
		if !ok {
			return Identity{}, newRegionsError(nil, "band region set carries no rectangle: this is a bug in --id-band handling")
		}
		lines, err := readOneCrop(ctx, ocr, src, page, region, filepath.Join(cropsDir, bandCropFilename(page.Index)))
		if err != nil {
			return Identity{}, err
		}
		shared := FieldLattices{Lines: lines}
		for _, kind := range kindOrder {
			id.Fields[kind] = shared
		}
		return id, nil
	}

	for _, kind := range kindOrder {
		region, ok := regions.Get(kind)
		if !ok {
			continue
		}
		lines, err := readOneCrop(ctx, ocr, src, page, region, filepath.Join(cropsDir, CropFilename(page.Index, kind)))
		if err != nil {
			return Identity{}, err
		}
		id.Fields[kind] = FieldLattices{Lines: lines}
	}
	return id, nil
}

// readOneCrop crops one region out of the decoded page, writes the crop to
// cropPath and runs the OCR over it. The bytes written are the bytes the OCR
// saw, so the artifact answers "what did the recognizer actually look at".
func readOneCrop(ctx context.Context, ocr latticeReader, src image.Image, page Page, region Region, cropPath string) ([]localocr.LineLattice, error) {
	crop, err := cropRegion(src, region, cropQuality)
	if err != nil {
		return nil, newRegionsError(err, "cannot crop the %q region out of page %d (%s page %d): check the region coordinates",
			region.Kind, page.Index, page.SourcePDF, page.SourcePage)
	}
	if err := os.WriteFile(cropPath, crop.JPEG(), 0o644); err != nil {
		return nil, newOutDirError(err, "cannot write crop artifact %s", cropPath)
	}
	lines, err := ocr.ReadLattices(ctx, crop)
	if err != nil {
		return nil, newOCRError(err, "local OCR failed on the %q region of page %d (%s page %d)",
			region.Kind, page.Index, page.SourcePDF, page.SourcePage)
	}
	return lines, nil
}

// cropRegion extracts one normalized region from an already-decoded page as a
// sealed imaging.IDCrop.
//
// It is a thin delegation to imaging.CropImage on purpose. The normalized ->
// pixel math (floor the origin, ceil the far edge, grow by padding, intersect
// with the page) lives in imaging and is shared with the server's scan path, so
// the same drawn rectangle produces the same crop in both modes; and IDCrop is
// sealed by design (D19), constructible only by imaging.Crop/CropImage or the
// audited storage read-back. Doing the pixel math here would mean either
// re-deriving it (two regimes to keep in sync) or minting the sealed type
// through the read-back gate with a fake storage key, which is exactly the hole
// the seal exists to close.
func cropRegion(src image.Image, r Region, quality int) (imaging.IDCrop, error) {
	return imaging.CropImage(src, []imaging.Region{toImagingRegion(r)}, quality)
}
