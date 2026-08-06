package export

import (
	"archive/zip"
	"bytes"
	"image/color"
	"io"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/imaging"
)

// The contract these tests exist for: the directory view and the ZIP are the
// SAME bundle. The offline CLI writes a directory, the web app serves a ZIP,
// and a professor comparing the two must not be able to tell which produced
// which. Asserting "Files looks about right" would leave the two free to drift;
// every assertion below is against BuildZIP's actual output, unzipped.

// unzipFiles reads an archive back into the same shape Files returns, in the
// archive's own entry order (zip.Reader preserves the central directory, which
// is the write order).
func unzipFiles(t *testing.T, zipBytes []byte) []File {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	out := make([]File, 0, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %q: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read zip entry %q: %v", f.Name, err)
		}
		out = append(out, File{Name: f.Name, Body: body})
	}
	return out
}

// TestFiles_IsExactlyWhatBuildZIPWrites is the load-bearing test for this file:
// same names, same bodies, same order — the whole claim, checked against real
// ZIP bytes rather than against a second expectation list that could drift with
// the first.
func TestFiles_IsExactlyWhatBuildZIPWrites(t *testing.T) {
	in := sampleInput(t)

	zipBytes, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	want := unzipFiles(t, zipBytes)

	got, err := Files(in)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Files returned %d entries, the archive has %d\n got: %v\nwant: %v",
			len(got), len(want), fileNames(got), fileNames(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name {
			t.Fatalf("entry %d name = %q, want %q (order must match too)\n got: %v\nwant: %v",
				i, got[i].Name, want[i].Name, fileNames(got), fileNames(want))
		}
		if !bytes.Equal(got[i].Body, want[i].Body) {
			t.Errorf("entry %q body differs: %d bytes vs the archive's %d",
				got[i].Name, len(got[i].Body), len(want[i].Body))
		}
	}

	// A bundle with no _all.tex and no MANIFEST.csv would pass every assertion
	// above vacuously if both sides regressed to nothing.
	if len(got) == 0 {
		t.Fatal("Files returned no entries")
	}
}

// TestFiles_MatchesBuildZIPForAnUnusualCohort re-runs the equality claim over
// the shapes most likely to expose an assembly path the directory view skipped:
// a single-answer bundle, a >999 pseudonym-width cohort boundary, and a failed
// Typst verdict (which changes the manifest header).
func TestFiles_MatchesBuildZIPForAnUnusualCohort(t *testing.T) {
	cases := map[string]func(in *Input){
		"single answer":  func(in *Input) { in.Answers = in.Answers[:1] },
		"typst failed":   func(in *Input) { in.TypstVerdict = "failed" },
		"typst verified": func(in *Input) { in.TypstVerdict = "verified" },
		"cjk assessment": func(in *Input) { in.AssessmentName = "微積分 期中考（2026）" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := sampleInput(t)
			mutate(&in)

			zipBytes, err := BuildZIP(in)
			if err != nil {
				t.Fatalf("BuildZIP: %v", err)
			}
			want := unzipFiles(t, zipBytes)
			got, err := Files(in)
			if err != nil {
				t.Fatalf("Files: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("Files returned %d entries, the archive has %d", len(got), len(want))
			}
			for i := range want {
				if got[i].Name != want[i].Name || !bytes.Equal(got[i].Body, want[i].Body) {
					t.Fatalf("entry %d = %q (%d bytes), archive has %q (%d bytes)",
						i, got[i].Name, len(got[i].Body), want[i].Name, len(want[i].Body))
				}
			}
		})
	}
}

// TestFiles_IsDeterministic — the directory view inherits the re-export-is-free
// contract (spec §2); it would be worthless as a ZIP substitute otherwise.
func TestFiles_IsDeterministic(t *testing.T) {
	a, err := Files(sampleInput(t))
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	b, err := Files(sampleInput(t))
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("two calls returned %d and %d entries", len(a), len(b))
	}
	for i := range a {
		if a[i].Name != b[i].Name || !bytes.Equal(a[i].Body, b[i].Body) {
			t.Fatalf("entry %d differs between two identical calls (%q vs %q)", i, a[i].Name, b[i].Name)
		}
	}
}

// TestImageName_IsTheNamingRuleTheBundleUses pins the exported namer against
// the names the bundle actually contains, not against a restatement of the
// rule — the caller writing the directory needs to predict an image's path
// before the bundle exists (to point a report at it), and a namer that agreed
// with the rule but not with the bundle would send it to a missing file.
func TestImageName_IsTheNamingRuleTheBundleUses(t *testing.T) {
	in := sampleInput(t)
	got, err := Files(in)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	inBundle := map[string]bool{}
	for _, f := range got {
		if i := strings.Index(f.Name, "/images/"); i >= 0 {
			inBundle[f.Name[i+len("/images/"):]] = true
		}
	}
	if len(inBundle) == 0 {
		t.Fatal("fixture produced no image entries")
	}
	for _, a := range in.Answers {
		for pageIdx := range a.Pages {
			name := ImageName(a.Identity.StudentID, pageIdx, len(a.Pages))
			if !inBundle[name] {
				t.Errorf("ImageName(_, %d, %d) = %q, which is not in the bundle (have %v)",
					pageIdx, len(a.Pages), name, inBundle)
			}
			delete(inBundle, name)
		}
	}
	if len(inBundle) != 0 {
		t.Errorf("bundle images ImageName cannot name: %v", inBundle)
	}

	// The disambiguation rule itself: -pN appears only when it has to.
	if got, want := ImageName("b09901007", 0, 1), "b09901007.jpg"; got != want {
		t.Errorf("single-page ImageName = %q, want %q", got, want)
	}
	if got, want := ImageName("b09901002", 1, 2), "b09901002-p2.jpg"; got != want {
		t.Errorf("multi-page ImageName = %q, want %q", got, want)
	}
}

// TestFiles_RejectsWhateverBuildZIPRejects — the directory view must not be the
// lenient door into the bundle. Validation is where the privacy and
// collision rules live, so an input BuildZIP refuses must fail here with the
// same message, not silently produce a directory the ZIP would never contain.
func TestFiles_RejectsWhateverBuildZIPRejects(t *testing.T) {
	cases := map[string]func(in *Input){
		"unsafe id":            func(in *Input) { in.Answers[0].Identity.StudentID = "../../etc/passwd" },
		"duplicate id":         func(in *Input) { in.Answers[1].Identity.StudentID = in.Answers[0].Identity.StudentID },
		"bad status":           func(in *Input) { in.Answers[0].Status = "maybe" },
		"bad source":           func(in *Input) { in.Answers[0].Source = "vibes" },
		"absent with pages":    func(in *Input) { in.Answers[3].Pages = []imaging.MaskedImage{maskedPage(t, color.RGBA{A: 255})} },
		"no answers":           func(in *Input) { in.Answers = nil },
		"bad problem number":   func(in *Input) { in.ProblemNumber = 0 },
		"bad typst verdict":    func(in *Input) { in.TypstVerdict = "probably" },
		"zero page in answer":  func(in *Input) { in.Answers[0].Pages = []imaging.MaskedImage{{}} },
		"empty student id str": func(in *Input) { in.Answers[0].Identity.StudentID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := sampleInput(t)
			mutate(&in)

			_, zipErr := BuildZIP(in)
			if zipErr == nil {
				t.Fatalf("BuildZIP accepted %s — fixture no longer covers a rejection path", name)
			}
			files, err := Files(in)
			if err == nil {
				t.Fatalf("Files accepted %s that BuildZIP rejected (%d entries)", name, len(files))
			}
			if err.Error() != zipErr.Error() {
				t.Errorf("Files error = %q, BuildZIP error = %q — the two doors must reject identically",
					err.Error(), zipErr.Error())
			}
			if files != nil {
				t.Error("Files must return no entries alongside an error")
			}
		})
	}
}

// TestFiles_EnforcesTheImagePreflightCap — the cap exists to keep one export
// off the heap, which is exactly as true for a directory as for an archive.
func TestFiles_EnforcesTheImagePreflightCap(t *testing.T) {
	orig := MaxZipBytes
	MaxZipBytes = 32
	defer func() { MaxZipBytes = orig }()

	in := sampleInput(t)
	_, zipErr := BuildZIP(in)
	if zipErr == nil {
		t.Fatal("BuildZIP must refuse to build past MaxZipBytes")
	}
	_, err := Files(in)
	if err == nil {
		t.Fatal("Files must refuse to assemble past MaxZipBytes")
	}
	if err.Error() != zipErr.Error() {
		t.Errorf("Files error = %q, BuildZIP error = %q", err.Error(), zipErr.Error())
	}
}

func fileNames(fs []File) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}
