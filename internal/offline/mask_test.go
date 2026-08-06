package offline

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// The mask stage is the privacy gate: after it runs, the ONLY page bytes that
// may leave the machine are the ones it produced. So these tests assert PIXELS,
// not just the absence of an error — a mask that returned successfully while
// painting the wrong rectangle would pass any error-only test and leak every
// student's name to the provider.

// maskFill is imaging's default mask color (#4a4a4a). The offline region set
// deliberately leaves Region.Color empty so imaging applies it.
var maskFill = color.RGBA{R: 0x4a, G: 0x4a, B: 0x4a, A: 0xff}

// Box colors for the fixture pages. Each is far from maskFill in RGB space, so
// "was this covered" is decidable from one pixel.
var (
	bandInk    = color.RGBA{R: 220, G: 20, B: 60, A: 255}  // crimson identity band
	studentInk = color.RGBA{R: 240, G: 180, B: 20, A: 255} // student_id box
	nameInk    = color.RGBA{R: 20, G: 200, B: 90, A: 255}  // name box
	problemInk = color.RGBA{R: 30, G: 60, B: 230, A: 255}  // problem_id box (never masked, D66)
)

// fillRect paints a normalized rectangle onto img.
func fillRect(img *image.RGBA, x, y, w, h float64, c color.RGBA) {
	b := img.Bounds()
	r := image.Rect(
		int(x*float64(b.Dx())), int(y*float64(b.Dy())),
		int((x+w)*float64(b.Dx())), int((y+h)*float64(b.Dy())),
	).Intersect(b)
	for py := r.Min.Y; py < r.Max.Y; py++ {
		for px := r.Min.X; px < r.Max.X; px++ {
			img.Set(px, py, c)
		}
	}
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode fixture JPEG: %v", err)
	}
	return buf.Bytes()
}

// bandPageJPEG is a white page with a crimson identity strip across the top
// 18% — the shape the --id-band fallback assumes.
func bandPageJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fillRect(img, 0, 0, 1, 1, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	fillRect(img, 0, 0, 1, 0.18, bandInk)
	return encodeJPEG(t, img)
}

// boxPageJPEG is a white page with three separate identity boxes at the
// coordinates boxRegions describes: student_id and name up top, problem_id
// halfway down the page.
func boxPageJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fillRect(img, 0, 0, 1, 1, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	fillRect(img, 0.6, 0.02, 0.3, 0.06, studentInk)
	fillRect(img, 0.1, 0.02, 0.3, 0.06, nameInk)
	fillRect(img, 0.05, 0.5, 0.1, 0.05, problemInk)
	return encodeJPEG(t, img)
}

// boxRegions is the three-region set matching boxPageJPEG, loaded through the
// real LoadRegions path so the test exercises the file format operators use.
func boxRegions(t *testing.T) RegionSet {
	t.Helper()
	path := writeFile(t, t.TempDir(), "regions.json", `{"version":1,"regions":[
		{"kind":"student_id","x":0.6,"y":0.02,"w":0.3,"h":0.06},
		{"kind":"name","x":0.1,"y":0.02,"w":0.3,"h":0.06},
		{"kind":"problem_id","x":0.05,"y":0.5,"w":0.1,"h":0.05}
	]}`)
	set, err := LoadRegions(path)
	if err != nil {
		t.Fatalf("LoadRegions: %v", err)
	}
	return set
}

func decodeJPEG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode JPEG: %v", err)
	}
	return img
}

func pixel(img image.Image, x, y int) color.RGBA {
	r, g, b, _ := img.At(x, y).RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: 255}
}

// dist is the Chebyshev distance between two colors: one number that says "this
// pixel is/is not that color" under JPEG's per-channel wobble.
func dist(a, b color.RGBA) int {
	d := 0
	for _, p := range [][2]int{{int(a.R), int(b.R)}, {int(a.G), int(b.G)}, {int(a.B), int(b.B)}} {
		if v := p[0] - p[1]; v > d {
			d = v
		} else if -v > d {
			d = -v
		}
	}
	return d
}

func assertPixelIs(t *testing.T, img image.Image, x, y int, want color.RGBA, what string) {
	t.Helper()
	const tol = 14 // JPEG at q85 wobbles a few counts inside a solid region
	if got := pixel(img, x, y); dist(got, want) > tol {
		t.Errorf("%s: pixel (%d,%d) = %v, want ~%v", what, x, y, got, want)
	}
}

func assertPixelIsNot(t *testing.T, img image.Image, x, y int, unwanted color.RGBA, what string) {
	t.Helper()
	const minDist = 40
	if got := pixel(img, x, y); dist(got, unwanted) < minDist {
		t.Errorf("%s: pixel (%d,%d) = %v, must not be ~%v", what, x, y, got, unwanted)
	}
}

func maskFixturePages(t *testing.T, n int, jpegFor func(*testing.T, int, int) []byte, w, h int) []Page {
	t.Helper()
	pages := make([]Page, n)
	for i := range pages {
		pages[i] = Page{Index: i + 1, SourcePDF: "scan.pdf", SourcePage: i + 1, JPEG: jpegFor(t, w, h)}
	}
	return pages
}

// --- MaskPages -------------------------------------------------------------

// TestMaskPages_PaintsOverTheIdentityBand is the load-bearing assertion of this
// stage: the band that carried identity is solid mask fill afterwards, and the
// answer area below it is untouched.
func TestMaskPages_PaintsOverTheIdentityBand(t *testing.T) {
	dir := t.TempDir()
	pages := maskFixturePages(t, 2, bandPageJPEG, 800, 400)
	masked, err := MaskPages(pages, BandRegions(0.18), 85, filepath.Join(dir, "masked"))
	if err != nil {
		t.Fatalf("MaskPages: %v", err)
	}
	if len(masked) != 2 {
		t.Fatalf("masked pages = %d, want 2", len(masked))
	}

	for i, mp := range masked {
		if mp.Page.Index != i+1 {
			t.Errorf("masked[%d].Page.Index = %d, want %d", i, mp.Page.Index, i+1)
		}
		if mp.Masked.IsZero() {
			t.Fatalf("masked[%d] carries no masked bytes", i)
		}
		img := decodeJPEG(t, mp.Masked.JPEG())
		// Sample across the whole band width: a mask that only covered the left
		// half would pass a single centered probe.
		for _, x := range []int{20, 200, 400, 600, 780} {
			assertPixelIs(t, img, x, 30, maskFill, "inside the identity band")
			assertPixelIsNot(t, img, x, 30, bandInk, "inside the identity band")
		}
		// The answer area must survive: masking the whole page would "pass" every
		// privacy assertion above and destroy the thing being graded.
		assertPixelIs(t, img, 400, 300, color.RGBA{R: 255, G: 255, B: 255, A: 255}, "answer area")
	}

	// The originals are inputs, not scratch space: a caller still holds them.
	orig := decodeJPEG(t, pages[0].JPEG)
	assertPixelIs(t, orig, 400, 30, bandInk, "original page after masking")
}

// TestMaskPages_LeavesTheProblemRegionVisible pins D66: masking exists to keep
// IDENTITY out of the provider request, and the problem number is not identity
// — covering it would break grading to protect nothing.
func TestMaskPages_LeavesTheProblemRegionVisible(t *testing.T) {
	dir := t.TempDir()
	pages := maskFixturePages(t, 1, boxPageJPEG, 800, 400)
	masked, err := MaskPages(pages, boxRegions(t), 85, filepath.Join(dir, "masked"))
	if err != nil {
		t.Fatalf("MaskPages: %v", err)
	}
	img := decodeJPEG(t, masked[0].Masked.JPEG())

	assertPixelIs(t, img, 600, 20, maskFill, "student_id box") // 0.6..0.9 x, 0.02..0.08 y
	assertPixelIsNot(t, img, 600, 20, studentInk, "student_id box")
	assertPixelIs(t, img, 200, 20, maskFill, "name box") // 0.1..0.4 x
	assertPixelIsNot(t, img, 200, 20, nameInk, "name box")
	assertPixelIs(t, img, 80, 210, problemInk, "problem_id box") // 0.05..0.15 x, 0.5..0.55 y
}

// TestMaskPages_WritesPrivateArtifacts — these files are a student's handwriting
// on the operator's disk. Modes are 0600/0700, and the bytes on disk are the
// bytes that may reach the provider, so the artifact is auditable.
func TestMaskPages_WritesPrivateArtifacts(t *testing.T) {
	dir := t.TempDir()
	maskedDir := filepath.Join(dir, "masked")
	pages := maskFixturePages(t, 3, bandPageJPEG, 200, 100)
	masked, err := MaskPages(pages, BandRegions(0.18), 85, maskedDir)
	if err != nil {
		t.Fatalf("MaskPages: %v", err)
	}

	info, err := os.Stat(maskedDir)
	if err != nil {
		t.Fatalf("stat masked dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("masked dir mode = %04o, want 0700", got)
	}

	for i, mp := range masked {
		path := filepath.Join(maskedDir, PageFilename(mp.Page.Index))
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", path, got)
		}
		onDisk, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(onDisk, mp.Masked.JPEG()) {
			t.Errorf("masked[%d]: artifact bytes differ from the masked image handed to the provider", i)
		}
	}
}

// TestMaskPages_UndecodablePageIsFatal — masking is the privacy gate, so a page
// that cannot be masked stops the run. A best-effort skip would leave the
// orchestrator holding a page with no masked twin.
func TestMaskPages_UndecodablePageIsFatal(t *testing.T) {
	dir := t.TempDir()
	pages := []Page{
		{Index: 1, SourcePDF: "scan.pdf", SourcePage: 1, JPEG: bandPageJPEG(t, 200, 100)},
		{Index: 2, SourcePDF: "scan.pdf", SourcePage: 2, JPEG: []byte("not a jpeg at all")},
	}
	masked, err := MaskPages(pages, BandRegions(0.18), 85, filepath.Join(dir, "masked"))
	assertErrorType[*ScanError](t, err, "page 2", "scan.pdf")
	if masked != nil {
		t.Errorf("masked = %v, want nil on failure", masked)
	}
	if code := ExitCode(err); code != ExitScan {
		t.Errorf("ExitCode = %d, want %d", code, ExitScan)
	}
}

// TestMaskPages_EmptyRegionSetIsRefused — imaging.Mask accepts an empty region
// slice and returns a re-encoded copy: a valid MaskedImage wrapping the
// UNTOUCHED original. That is the single input that would let this stage
// certify an unmasked page as safe to send, so it must fail before anything is
// written.
func TestMaskPages_EmptyRegionSetIsRefused(t *testing.T) {
	dir := t.TempDir()
	maskedDir := filepath.Join(dir, "masked")
	pages := maskFixturePages(t, 2, bandPageJPEG, 200, 100)

	masked, err := MaskPages(pages, RegionSet{}, 85, maskedDir)
	assertErrorType[*RegionsError](t, err, "nothing to mask", "--id-band")
	if code := ExitCode(err); code != ExitRegions {
		t.Errorf("ExitCode = %d, want %d", code, ExitRegions)
	}
	if masked != nil {
		t.Errorf("masked = %v, want nil", masked)
	}
	if _, err := os.Stat(maskedDir); !os.IsNotExist(err) {
		t.Errorf("stat %s: err = %v, want not-exist — nothing may be written", maskedDir, err)
	}
}

// TestMaskPages_UnusableDirectoryIsAnOutDirError keeps the "which flag do I
// fix" mapping honest: a bad --out is exit 5, not a masking failure.
func TestMaskPages_UnusableDirectoryIsAnOutDirError(t *testing.T) {
	dir := t.TempDir()
	blocker := writeFile(t, dir, "masked", "not a directory")
	_, err := MaskPages(maskFixturePages(t, 1, bandPageJPEG, 200, 100), BandRegions(0.18), 85, blocker)
	assertErrorType[*OutDirError](t, err, "masked")
}

// --- WriteContactSheet -----------------------------------------------------

// Tile geometry for a band-masked 800x400 page, derived once here so the
// expectations below read as arithmetic rather than magic numbers:
//
//	union rect: x 0..800 (full width, padding clamped), y 0..ceil((0.18+0.004)*400)=74
//	context:    x ±round(0.05*800)=40 (clamped to 0..800), y ±round(0.5*74)=37 (clamped to 0..111)
//	crop:       800 x 111
//	tile:       420 wide, round(111*420/800) = 58 tall
const (
	bandTileW = 420
	bandTileH = 58
)

// TestMaskContextRect_GrowsTheUnionAndClampsAtTheEdges — the growth is what
// makes a tile answer "did anything identifying land OUTSIDE the gray"; without
// it a band-mode tile is 100% mask fill and answers nothing.
func TestMaskContextRect_GrowsTheUnionAndClampsAtTheEdges(t *testing.T) {
	page := image.Rect(0, 0, 800, 400)
	tests := []struct {
		name  string
		union image.Rectangle
		want  image.Rectangle
	}{
		{
			// Interior: room to grow on every side. x ±40 (5% of the page's
			// width), y ±14 (half the union's 28px height).
			name:  "interior region grows on all four sides",
			union: image.Rect(476, 198, 724, 226),
			want:  image.Rect(436, 184, 764, 240),
		},
		{
			// Top-of-page identity box: the upward growth stops at the paper
			// edge instead of running off it.
			name:  "top edge clamps upward growth",
			union: image.Rect(76, 6, 724, 34),
			want:  image.Rect(36, 0, 764, 48),
		},
		{
			// Full-width band: horizontal growth clamps on both sides, so the
			// tile is the page width and the aspect stays sane.
			name:  "full-width band clamps horizontally",
			union: image.Rect(0, 0, 800, 74),
			want:  image.Rect(0, 0, 800, 111),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskContextRect(page, tc.union); got != tc.want {
				t.Errorf("maskContextRect = %v, want %v", got, tc.want)
			}
		})
	}
}

func sheetSize(t *testing.T, path string) (int, int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	b := decodeJPEG(t, data).Bounds()
	return b.Dx(), b.Dy()
}

func maskedFixture(t *testing.T, n int, jpegFor func(*testing.T, int, int) []byte, w, h int, regions RegionSet) []MaskedPage {
	t.Helper()
	masked, err := MaskPages(maskFixturePages(t, n, jpegFor, w, h), regions, 85, filepath.Join(t.TempDir(), "masked"))
	if err != nil {
		t.Fatalf("MaskPages: %v", err)
	}
	return masked
}

// TestWriteContactSheet_TilesSixAcross pins the grid: one row of up to six
// tiles, then a new row.
func TestWriteContactSheet_TilesSixAcross(t *testing.T) {
	tests := []struct {
		pages        int
		wantW, wantH int
	}{
		{1, bandTileW, bandTileH},
		{6, 6 * bandTileW, bandTileH},
		{7, 6 * bandTileW, 2 * bandTileH},
		{12, 6 * bandTileW, 2 * bandTileH},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%d pages", tc.pages), func(t *testing.T) {
			masked := maskedFixture(t, tc.pages, bandPageJPEG, 800, 400, BandRegions(0.18))
			out := filepath.Join(t.TempDir(), "masked-preview.jpg")
			paths, err := WriteContactSheet(masked, BandRegions(0.18), out)
			if err != nil {
				t.Fatalf("WriteContactSheet: %v", err)
			}
			if len(paths) != 1 || paths[0] != out {
				t.Fatalf("paths = %v, want [%s]", paths, out)
			}
			w, h := sheetSize(t, out)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("sheet = %dx%d, want %dx%d", w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

// TestWriteContactSheet_OverflowsPastSixtyTiles — 61 pages must not produce one
// unreadably tall sheet.
func TestWriteContactSheet_OverflowsPastSixtyTiles(t *testing.T) {
	// Small pages: 61 of them, and the geometry is the same arithmetic.
	// union y 0..ceil(0.184*100)=19, x 0..200; context ±round(0.5*19)=10 and
	// ±round(0.05*200)=10, both clamped => crop 200x29
	// => tile 420 x round(29*420/200)=61
	const tileH = 61
	masked := maskedFixture(t, 61, bandPageJPEG, 200, 100, BandRegions(0.18))
	dir := t.TempDir()
	out := filepath.Join(dir, "masked-preview.jpg")
	paths, err := WriteContactSheet(masked, BandRegions(0.18), out)
	if err != nil {
		t.Fatalf("WriteContactSheet: %v", err)
	}
	want := []string{out, filepath.Join(dir, "masked-preview-02.jpg")}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths = %v, want %v", paths, want)
		}
	}
	if w, h := sheetSize(t, paths[0]); w != 6*bandTileW || h != 10*tileH {
		t.Errorf("sheet 1 = %dx%d, want %dx%d", w, h, 6*bandTileW, 10*tileH)
	}
	if w, h := sheetSize(t, paths[1]); w != bandTileW || h != tileH {
		t.Errorf("sheet 2 = %dx%d, want %dx%d", w, h, bandTileW, tileH)
	}
}

// TestWriteContactSheet_CropsTheMaskUnionPlusContext — the preview shows the
// identity area: the union of the MASK regions, grown by a fixed margin so the
// mask's EDGES are visible against the page (a tile cropped to the union alone
// cannot show a mask that fell short of the ink it was meant to cover). What it
// does NOT include is the unmasked problem_id region halfway down the page,
// which would make every tile a whole page and the sheet useless for checking
// mask placement.
func TestWriteContactSheet_CropsTheMaskUnionPlusContext(t *testing.T) {
	regions := boxRegions(t)
	masked := maskedFixture(t, 1, boxPageJPEG, 800, 400, regions)
	out := filepath.Join(t.TempDir(), "masked-preview.jpg")
	if _, err := WriteContactSheet(masked, regions, out); err != nil {
		t.Fatalf("WriteContactSheet: %v", err)
	}
	// union:   x floor((0.1-0.004)*800)=76 .. ceil((0.9+0.004)*800)=724  => 648
	//          y floor((0.02-0.004)*400)=6 .. ceil((0.08+0.004)*400)=34  => 28
	// context: x ±40 => 36..764 (728); y ±14 => -8..48, clamped to 0..48 (48)
	// tile:    420 wide, round(48*420/728) = 28 tall
	w, h := sheetSize(t, out)
	if w != 420 || h != 28 {
		t.Fatalf("sheet = %dx%d, want 420x28 (the mask union plus context, not the whole page)", w, h)
	}

	// And what it shows is MASKED: the student_id box maps to roughly x 254..397,
	// y 0..16 of the tile, which must read as fill rather than as the box's ink.
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read sheet: %v", err)
	}
	img := decodeJPEG(t, data)
	got := pixel(img, 325, 11)
	if dist(got, maskFill) >= dist(got, studentInk) {
		t.Errorf("contact-sheet pixel (325,11) = %v, closer to the student_id ink %v than to the mask fill %v — the preview is showing unmasked pixels",
			got, studentInk, maskFill)
	}
}

// TestWriteContactSheet_TileShowsTheMaskAndItsSurroundings is the reason the
// crop is grown at all. In band mode the mask covers the whole top strip, so a
// tile cropped to the union alone would be a solid gray rectangle on every
// page: an operator could not tell a well-placed mask from one whose band was
// too short and left a name showing just below it. The tile must carry both.
func TestWriteContactSheet_TileShowsTheMaskAndItsSurroundings(t *testing.T) {
	masked := maskedFixture(t, 1, bandPageJPEG, 800, 400, BandRegions(0.18))
	out := filepath.Join(t.TempDir(), "masked-preview.jpg")
	if _, err := WriteContactSheet(masked, BandRegions(0.18), out); err != nil {
		t.Fatalf("WriteContactSheet: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read sheet: %v", err)
	}
	img := decodeJPEG(t, data)

	// Scale is 420/800 = 0.525, so the masked band (source y 0..74) occupies
	// tile y 0..38 and the page below it occupies y 39..58.
	assertPixelIs(t, img, 210, 15, maskFill, "the masked band inside the tile")
	assertPixelIs(t, img, 210, 52, color.RGBA{R: 255, G: 255, B: 255, A: 255},
		"the page BELOW the mask — the context that makes an escaped name visible")
}

// TestWriteContactSheet_WritesPrivateFiles — same PII rule as every other
// artifact this stage produces.
func TestWriteContactSheet_WritesPrivateFiles(t *testing.T) {
	masked := maskedFixture(t, 2, bandPageJPEG, 200, 100, BandRegions(0.18))
	dir := filepath.Join(t.TempDir(), "out")
	out := filepath.Join(dir, "masked-preview.jpg")
	paths, err := WriteContactSheet(masked, BandRegions(0.18), out)
	if err != nil {
		t.Fatalf("WriteContactSheet: %v", err)
	}
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat out dir: %v", err)
	} else if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("out dir mode = %04o, want 0700", got)
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", p, got)
		}
	}
}

// TestWriteContactSheet_NoPagesWritesNothing — an empty sheet would be a file
// claiming "nothing was masked" that a later run silently reuses.
func TestWriteContactSheet_NoPagesWritesNothing(t *testing.T) {
	out := filepath.Join(t.TempDir(), "masked-preview.jpg")
	paths, err := WriteContactSheet(nil, BandRegions(0.18), out)
	if err != nil {
		t.Fatalf("WriteContactSheet: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want none", paths)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("stat %s: err = %v, want not-exist", out, err)
	}
}
