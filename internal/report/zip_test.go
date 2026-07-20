package report

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"sort"
	"strings"
	"testing"
)

// TestBuildZIP_ContainsExpectedFilenames asserts the ZIP fallback (spec §3,
// D45) has one problem-<n>-page-<m>.jpg per answer-page image (1-based, in
// problem/page order) plus grades.txt — nothing else.
func TestBuildZIP_ContainsExpectedFilenames(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{
		solidJPEG(t, 300, 400, color.RGBA{R: 200, A: 255}),
		solidJPEG(t, 300, 400, color.RGBA{R: 200, A: 255}),
	}
	in.Problems[1].Pages = [][]byte{
		solidJPEG(t, 300, 400, color.RGBA{B: 200, A: 255}),
	}

	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}

	names := zipEntryNames(t, out)
	sort.Strings(names)
	want := []string{
		"grades.txt",
		"problem-1-page-1.jpg",
		"problem-1-page-2.jpg",
		"problem-2-page-1.jpg",
	}
	sort.Strings(want)
	if len(names) != len(want) {
		t.Fatalf("zip entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("zip entries = %v, want %v", names, want)
			break
		}
	}
}

// TestBuildZIP_GradesTextContainsBreakdown asserts grades.txt carries the
// same per-criterion breakdown the email body has (spec §3: "the text body's
// breakdown"), scoped to the student's own content.
func TestBuildZIP_GradesTextContainsBreakdown(t *testing.T) {
	in := sampleInput()
	in.Problems[0].Pages = [][]byte{solidJPEG(t, 300, 400, color.RGBA{R: 200, A: 255})}
	in.Problems[1].Pages = [][]byte{solidJPEG(t, 300, 400, color.RGBA{B: 200, A: 255})}

	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}

	grades := zipEntryContent(t, out, "grades.txt")
	for _, want := range []string{
		"Midterm 2",
		"Ada Lovelace",
		"B11902999",
		"Problem 1: Determinants",
		"Setup: 5/5",
		"Sign error in row 2.",
		"Subtotal: 13/15",
		"Total: 27/30",
	} {
		if !strings.Contains(grades, want) {
			t.Errorf("grades.txt missing %q; got:\n%s", want, grades)
		}
	}
}

// TestBuildZIP_PageBytesAreDecodableJPEGs asserts each problem-*.jpg entry
// really is a valid JPEG (not just named .jpg), and at "compressed" quality
// respects the long-edge downscale.
func TestBuildZIP_PageBytesAreDecodableJPEGs(t *testing.T) {
	in := sampleInput()
	in.Quality = QualityCompressed
	in.Problems[0].Pages = [][]byte{solidJPEG(t, 3200, 2400, color.RGBA{R: 200, A: 255})}
	in.Problems[1].Pages = [][]byte{solidJPEG(t, 300, 400, color.RGBA{B: 200, A: 255})}

	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}

	page1 := zipEntryBytes(t, out, "problem-1-page-1.jpg")
	img := decodeJPEGBytes(t, page1)
	b := img.Bounds()
	if b.Dx() != compressedLongEdgePx {
		t.Errorf("compressed page width = %d, want long edge capped at %d", b.Dx(), compressedLongEdgePx)
	}
}

// TestBuildZIP_RejectsInvalidQuality mirrors Build's validation — BuildZIP
// shares the same ReportInput contract.
func TestBuildZIP_RejectsInvalidQuality(t *testing.T) {
	in := sampleInput()
	in.Quality = "bogus"
	in.Problems[0].Pages = [][]byte{solidJPEG(t, 300, 400, color.RGBA{R: 200, A: 255})}
	in.Problems[1].Pages = [][]byte{solidJPEG(t, 300, 400, color.RGBA{B: 200, A: 255})}
	if _, err := BuildZIP(in); err == nil {
		t.Fatal("BuildZIP should reject an invalid Quality value")
	}
}

func TestBuildZIP_RejectsNoProblems(t *testing.T) {
	in := sampleInput()
	in.Problems = nil
	if _, err := BuildZIP(in); err == nil {
		t.Fatal("BuildZIP should reject a ReportInput with no problems")
	}
}

// ---- zip test helpers ----

func zipEntryNames(t *testing.T, zipBytes []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func zipEntryBytes(t *testing.T, zipBytes []byte, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %q: %v", name, err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read zip entry %q: %v", name, err)
		}
		return b
	}
	t.Fatalf("zip entry %q not found", name)
	return nil
}

func zipEntryContent(t *testing.T, zipBytes []byte, name string) string {
	t.Helper()
	return string(zipEntryBytes(t, zipBytes, name))
}

func decodeJPEGBytes(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode JPEG: %v", err)
	}
	return img
}
