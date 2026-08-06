package offline

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/localocr"
)

// synthPage builds a w×h image whose left half is red and right half is blue,
// so a crop can be checked for BOTH its size and the pixels it actually took.
func synthPage(t *testing.T, w, h int) image.Image {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				img.Set(x, y, color.RGBA{R: 255, A: 255})
			} else {
				img.Set(x, y, color.RGBA{B: 255, A: 255})
			}
		}
	}
	return img
}

func synthPageJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, synthPage(t, w, h), &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode synthetic page: %v", err)
	}
	return buf.Bytes()
}

// TestCropRegion_PixelBounds pins the normalized-rect → pixel-rect math on a
// known-size image, including the floor/ceil asymmetry (x0 floors, x1 ceils) and
// padding clamped at the page edge.
func TestCropRegion_PixelBounds(t *testing.T) {
	src := synthPage(t, 200, 100)

	tests := []struct {
		name         string
		region       Region
		wantW, wantH int
	}{
		{
			// x: floor(0.25*200)=50 .. ceil(0.75*200)=150 => 100
			// y: floor(0.50*100)=50 .. ceil(0.75*100)=75  => 25
			name:   "exact rect, no padding",
			region: Region{X: 0.25, Y: 0.5, W: 0.5, H: 0.25},
			wantW:  100, wantH: 25,
		},
		{
			// Padding pushes x0/y0 negative; the rect clamps to the page edge
			// instead of erroring. Coordinates are binary-exact so the
			// expectation is the arithmetic, not a float-rounding accident.
			// x: floor(-12.5)=-13 -> 0 .. ceil(0.1875*200)=38 => 38
			// y: floor(-6.25)= -7 -> 0 .. ceil(0.3125*100)=32 => 32
			name:   "padding clamps at the top-left corner",
			region: Region{X: 0, Y: 0, W: 0.125, H: 0.25, Padding: 0.0625},
			wantW:  38, wantH: 32,
		},
		{
			// And at the bottom-right, where the far edge overshoots the page.
			// x: floor(0.8125*200)=162 .. ceil(1.0625*200)=213 -> 200 => 38
			// y: floor(0.6875*100)= 68 .. ceil(1.0625*100)=107 -> 100 => 32
			name:   "padding clamps at the bottom-right corner",
			region: Region{X: 0.875, Y: 0.75, W: 0.125, H: 0.25, Padding: 0.0625},
			wantW:  38, wantH: 32,
		},
		{
			// Fractional edges round outward on both sides, never inward: a
			// half-pixel of a glyph is worth more than a tidy rectangle.
			// x: floor(0.101*200)=20 .. ceil(0.301*200)=61 => 41
			name:   "fractional edges round outward",
			region: Region{X: 0.101, Y: 0.101, W: 0.2, H: 0.2},
			wantW:  41, wantH: 21,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			crop, err := cropRegion(src, tc.region, 90)
			if err != nil {
				t.Fatalf("cropRegion: %v", err)
			}
			img, err := jpeg.Decode(bytes.NewReader(crop.JPEG()))
			if err != nil {
				t.Fatalf("crop does not decode as JPEG: %v", err)
			}
			if got := img.Bounds().Dx(); got != tc.wantW {
				t.Errorf("crop width = %d, want %d", got, tc.wantW)
			}
			if got := img.Bounds().Dy(); got != tc.wantH {
				t.Errorf("crop height = %d, want %d", got, tc.wantH)
			}
			if crop.SHA256() == "" {
				t.Error("crop carries no SHA256")
			}
		})
	}
}

// TestCropRegion_TakesTheRightPixels checks the crop is of the region asked
// for, not merely of the right size: the right half of the synthetic page is
// blue, the left half red.
func TestCropRegion_TakesTheRightPixels(t *testing.T) {
	src := synthPage(t, 200, 100)

	right, err := cropRegion(src, Region{X: 0.6, Y: 0.2, W: 0.3, H: 0.3}, 95)
	if err != nil {
		t.Fatalf("cropRegion(right): %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(right.JPEG()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r, g, b, _ := img.At(img.Bounds().Dx()/2, img.Bounds().Dy()/2).RGBA()
	if b < 0x8000 || r > 0x4000 || g > 0x4000 {
		t.Errorf("crop of the right half should be blue; got r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
}

// TestCropRegion_FullyOutside pins the imaging contract we rely on: a region
// that lands nowhere on the page is an error, never an empty image handed to
// the OCR.
func TestCropRegion_FullyOutside(t *testing.T) {
	src := synthPage(t, 200, 100)
	if _, err := cropRegion(src, Region{X: 2, Y: 2, W: 0.1, H: 0.1}, 90); err == nil {
		t.Fatal("want an error for a region off the page, got nil")
	}
}

// fakeReader is a latticeReader that records what it was handed.
type fakeReader struct {
	calls int
	crops []imaging.IDCrop
	lines []localocr.LineLattice
	err   error
}

func (f *fakeReader) ReadLattices(ctx context.Context, crop imaging.IDCrop) ([]localocr.LineLattice, error) {
	f.calls++
	f.crops = append(f.crops, crop)
	if f.err != nil {
		return nil, f.err
	}
	return f.lines, nil
}

func testPage(t *testing.T, index int) Page {
	t.Helper()
	return Page{Index: index, SourcePDF: "scan.pdf", SourcePage: index, JPEG: synthPageJPEG(t, 400, 200)}
}

func oneLine(text string) []localocr.LineLattice {
	return []localocr.LineLattice{{
		Lattice:    localocr.NewLattice([][]float32{{0.1, 0.7, 0.1, 0.1}}, 0),
		Text:       text,
		Confidence: 0.9,
	}}
}

// TestReadIdentity_ThreeRegions: a loaded region set reads each configured kind
// separately and writes one crop artifact per kind.
func TestReadIdentity_ThreeRegions(t *testing.T) {
	dir := t.TempDir()
	cropsDir := filepath.Join(dir, "crops")
	regions := RegionSet{regions: map[Kind]Region{
		KindStudentID: {Kind: KindStudentID, X: 0.6, Y: 0.02, W: 0.3, H: 0.06, Padding: 0.004},
		KindName:      {Kind: KindName, X: 0.1, Y: 0.02, W: 0.4, H: 0.06, Padding: 0.004},
		KindProblemID: {Kind: KindProblemID, X: 0.1, Y: 0.12, W: 0.2, H: 0.05, Padding: 0.004},
	}}
	fr := &fakeReader{lines: oneLine("B10902066")}

	id, err := ReadIdentity(context.Background(), fr, testPage(t, 7), regions, cropsDir)
	if err != nil {
		t.Fatalf("ReadIdentity: %v", err)
	}
	if fr.calls != 3 {
		t.Errorf("ReadLattices called %d times, want 3 (one per configured region)", fr.calls)
	}
	for _, kind := range []Kind{KindStudentID, KindName, KindProblemID} {
		fl, ok := id.Fields[kind]
		if !ok {
			t.Errorf("Identity is missing kind %q", kind)
			continue
		}
		if len(fl.Lines) != 1 {
			t.Errorf("kind %q: got %d lines, want 1", kind, len(fl.Lines))
		}
		name := filepath.Join(cropsDir, "p0007-"+string(kind)+".jpg")
		if _, err := os.Stat(name); err != nil {
			t.Errorf("expected crop artifact %s: %v", name, err)
		}
	}
	// The crop bytes on disk are the bytes the OCR saw.
	onDisk, err := os.ReadFile(filepath.Join(cropsDir, "p0007-student_id.jpg"))
	if err != nil {
		t.Fatalf("read crop: %v", err)
	}
	if !bytes.Equal(onDisk, fr.crops[0].JPEG()) {
		t.Error("the crop written to disk is not the crop handed to the OCR")
	}
}

// TestReadIdentity_PartialRegionSet: a region file may configure only student_id
// and name; the missing kind is simply absent, not an empty entry that would
// look "read but blank" to the scorer.
func TestReadIdentity_PartialRegionSet(t *testing.T) {
	dir := t.TempDir()
	regions := RegionSet{regions: map[Kind]Region{
		KindStudentID: {Kind: KindStudentID, X: 0.6, Y: 0.02, W: 0.3, H: 0.06},
		KindName:      {Kind: KindName, X: 0.1, Y: 0.02, W: 0.4, H: 0.06},
	}}
	fr := &fakeReader{lines: oneLine("x")}

	id, err := ReadIdentity(context.Background(), fr, testPage(t, 1), regions, filepath.Join(dir, "crops"))
	if err != nil {
		t.Fatalf("ReadIdentity: %v", err)
	}
	if len(id.Fields) != 2 {
		t.Errorf("got %d fields, want 2", len(id.Fields))
	}
	if _, ok := id.Fields[KindProblemID]; ok {
		t.Error("problem_id was not configured; it must not appear in Identity")
	}
}

// TestReadIdentity_BandAliases pins the band-mode contract: ONE crop, ONE OCR
// pass, and all three kinds referencing the same lattices.
func TestReadIdentity_BandAliases(t *testing.T) {
	dir := t.TempDir()
	cropsDir := filepath.Join(dir, "crops")
	fr := &fakeReader{lines: oneLine("05 王小明")}

	id, err := ReadIdentity(context.Background(), fr, testPage(t, 3), BandRegions(0.18), cropsDir)
	if err != nil {
		t.Fatalf("ReadIdentity: %v", err)
	}
	if fr.calls != 1 {
		t.Errorf("band mode ran the OCR %d times, want 1 (one crop serves all three kinds)", fr.calls)
	}
	if len(id.Fields) != 3 {
		t.Fatalf("got %d fields, want 3", len(id.Fields))
	}
	idLines := id.Fields[KindStudentID].Lines
	for _, kind := range []Kind{KindName, KindProblemID} {
		other := id.Fields[kind].Lines
		if len(other) != len(idLines) {
			t.Fatalf("kind %q: %d lines, want %d", kind, len(other), len(idLines))
		}
		if &other[0] != &idLines[0] {
			t.Errorf("kind %q must ALIAS the student_id lattices, not copy them", kind)
		}
	}
	// Exactly one artifact, named for the band rather than for a kind.
	entries, err := os.ReadDir(cropsDir)
	if err != nil {
		t.Fatalf("read crops dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "p0003-band.jpg" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("band mode wrote %v, want exactly [p0003-band.jpg]", names)
	}
}

// TestReadIdentity_EmptyLinesIsNotAnError: a crop with no ink reads as zero
// lines. The scorer treats that as "this field contributes nothing", which is a
// score, not a failure — the page still gets matched on the other fields.
func TestReadIdentity_EmptyLinesIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeReader{lines: nil}

	id, err := ReadIdentity(context.Background(), fr, testPage(t, 2), BandRegions(0.18), filepath.Join(dir, "crops"))
	if err != nil {
		t.Fatalf("an empty crop must not be an error: %v", err)
	}
	if got := len(id.Fields[KindStudentID].Lines); got != 0 {
		t.Errorf("got %d lines, want 0", got)
	}
	if _, ok := id.Fields[KindStudentID]; !ok {
		t.Error("the field must still be present (read, but blank)")
	}
}

// TestReadIdentity_LowConfidenceLinesSurvive: the read stage never filters. A
// line the OCR is unsure of is exactly the line the lattice scorer exists to
// rescue, so dropping it here would defeat the whole closed-set match.
func TestReadIdentity_LowConfidenceLinesSurvive(t *testing.T) {
	dir := t.TempDir()
	lines := oneLine("illegible")
	lines[0].Confidence = 0.01
	fr := &fakeReader{lines: lines}

	id, err := ReadIdentity(context.Background(), fr, testPage(t, 4), BandRegions(0.18), filepath.Join(dir, "crops"))
	if err != nil {
		t.Fatalf("ReadIdentity: %v", err)
	}
	got := id.Fields[KindStudentID].Lines
	if len(got) != 1 || got[0].Confidence != 0.01 {
		t.Errorf("low-confidence line was dropped: %+v", got)
	}
}

func TestReadIdentity_Errors(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("boom")

	t.Run("undecodable page", func(t *testing.T) {
		page := Page{Index: 9, SourcePDF: "scan.pdf", SourcePage: 9, JPEG: []byte("not a jpeg")}
		_, err := ReadIdentity(context.Background(), &fakeReader{}, page, BandRegions(0.18), filepath.Join(dir, "c1"))
		if err == nil {
			t.Fatal("want an error, got nil")
		}
		if ExitCode(err) != ExitScan {
			t.Errorf("ExitCode = %d, want %d (*ScanError)", ExitCode(err), ExitScan)
		}
	})

	t.Run("ocr failure", func(t *testing.T) {
		_, err := ReadIdentity(context.Background(), &fakeReader{err: boom}, testPage(t, 5), BandRegions(0.18), filepath.Join(dir, "c2"))
		if err == nil {
			t.Fatal("want an error, got nil")
		}
		if ExitCode(err) != ExitOCR {
			t.Errorf("ExitCode = %d, want %d (*OCRError)", ExitCode(err), ExitOCR)
		}
		if !errors.Is(err, boom) {
			t.Error("the underlying OCR error must stay reachable through errors.Is")
		}
	})

	t.Run("region off the page", func(t *testing.T) {
		regions := RegionSet{regions: map[Kind]Region{
			KindStudentID: {Kind: KindStudentID, X: 3, Y: 3, W: 0.1, H: 0.1},
		}}
		_, err := ReadIdentity(context.Background(), &fakeReader{}, testPage(t, 6), regions, filepath.Join(dir, "c3"))
		if err == nil {
			t.Fatal("want an error, got nil")
		}
		if ExitCode(err) != ExitRegions {
			t.Errorf("ExitCode = %d, want %d (*RegionsError)", ExitCode(err), ExitRegions)
		}
	})
}
