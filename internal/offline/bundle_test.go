package offline

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/roster"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// Synthetic roster (D14). The name and email double as PII needles: no file in
// the bundle may carry either, and only MANIFEST.csv may carry the id.
func bundleRoster() []roster.Row {
	return []roster.Row{
		{StudentID: "AB01", Name: "Test Alpha", Email: "alpha@example.test", Line: 2},
		{StudentID: "AB02", Name: "Test Beta", Email: "beta@example.test", Line: 3},
	}
}

// bundleCell builds one successfully transcribed cell. Every page is a
// different size, so its ORIGINAL bytes, its masked bytes and every other
// cell's are distinguishable by comparison alone.
func bundleCell(t *testing.T, pageIndex int, row roster.Row, problem int, blocks ...transcribe.Block) CellDoc {
	t.Helper()
	original := bandPageJPEG(t, 100+pageIndex*4, 60)
	masked, err := imaging.Mask(original, BandRegions(0.18).MaskRegions(), 85)
	if err != nil {
		t.Fatalf("mask fixture page: %v", err)
	}
	if len(blocks) == 0 {
		blocks = []transcribe.Block{{Kind: transcribe.BlockProse, Text: "an answer"}}
	}
	return CellDoc{
		Result: MatchResult{
			Page:        Page{Index: pageIndex, SourcePDF: "scan.pdf", SourcePage: pageIndex, JPEG: original},
			StudentID:   row.StudentID,
			StudentName: row.Name,
			Problem:     problem,
			Method:      MethodLattice,
			Status:      StatusAuto,
		},
		Masked:     masked,
		Doc:        transcribe.Doc{Blocks: blocks},
		Confidence: "high",
	}
}

// bundleFixture is 2 students × 2 problems, one page each.
func bundleFixture(t *testing.T) []CellDoc {
	t.Helper()
	rows := bundleRoster()
	return []CellDoc{
		bundleCell(t, 1, rows[0], 1),
		bundleCell(t, 2, rows[0], 2),
		bundleCell(t, 3, rows[1], 1),
		bundleCell(t, 4, rows[1], 2),
	}
}

// walkBundle returns every regular file's slash-separated path relative to
// root, sorted.
func walkBundle(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func readBundleFile(t *testing.T, root, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return data
}

// parseManifest reads MANIFEST.csv, whose leading '#' notice lines are comments
// by the convention encoding/csv, pandas and R all share.
func parseManifest(t *testing.T, data []byte) [][]string {
	t.Helper()
	r := csv.NewReader(bytes.NewReader(data))
	r.Comment = '#'
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return rows
}

// manifestRowFor returns the manifest row for a student id (header excluded).
func manifestRowFor(t *testing.T, root, rel, studentID string) []string {
	t.Helper()
	rows := parseManifest(t, readBundleFile(t, root, rel))
	for _, row := range rows[1:] {
		if row[0] == studentID {
			return row
		}
	}
	t.Fatalf("manifest %s has no row for the student", rel)
	return nil
}

// --- layout ----------------------------------------------------------------

// TestWriteBundle_WritesTheExportLayoutUnderOneRootPerProblem pins the
// directory contract exactly — including the hazard this stage is one join away
// from: export.File.Name ALREADY carries the bundle root, so joining it onto a
// second root of our own would produce out/bundle/offline-exam-p1/offline-exam-p1/…
func TestWriteBundle_WritesTheExportLayoutUnderOneRootPerProblem(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBundle(dir, "Offline Exam", bundleFixture(t), bundleRoster(), 2); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	want := []string{
		"offline-exam-p1/MANIFEST.csv",
		"offline-exam-p1/_all.tex",
		"offline-exam-p1/_all.typ",
		"offline-exam-p1/images/AB01.jpg",
		"offline-exam-p1/images/AB02.jpg",
		"offline-exam-p1/tex/AB01.tex",
		"offline-exam-p1/tex/AB02.tex",
		"offline-exam-p1/typ/AB01.typ",
		"offline-exam-p1/typ/AB02.typ",
		"offline-exam-p2/MANIFEST.csv",
		"offline-exam-p2/_all.tex",
		"offline-exam-p2/_all.typ",
		"offline-exam-p2/images/AB01.jpg",
		"offline-exam-p2/images/AB02.jpg",
		"offline-exam-p2/tex/AB01.tex",
		"offline-exam-p2/tex/AB02.tex",
		"offline-exam-p2/typ/AB01.typ",
		"offline-exam-p2/typ/AB02.typ",
	}
	got := walkBundle(t, dir)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("bundle layout:\n got %v\nwant %v", got, want)
	}
}

// TestWriteBundle_MultiPageAnswerGetsNumberedImages — two pages matched to one
// cell are one answer with two images, named by the export's own rule.
func TestWriteBundle_MultiPageAnswerGetsNumberedImages(t *testing.T) {
	dir := t.TempDir()
	rows := bundleRoster()
	cells := []CellDoc{
		bundleCell(t, 5, rows[0], 1),
		bundleCell(t, 2, rows[0], 1), // same student and problem, earlier page
	}
	if err := WriteBundle(dir, "Offline Exam", cells, rows, 1); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	want := []string{"offline-exam-p1/images/AB01-p1.jpg", "offline-exam-p1/images/AB01-p2.jpg"}
	got := walkBundle(t, dir)
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("missing %s; got %v", w, got)
		}
	}
	// Page order, not the caller's slice order: -p1 is the earlier page.
	if !bytes.Equal(readBundleFile(t, dir, want[0]), cells[1].Result.Page.JPEG) {
		t.Error("images/AB01-p1.jpg is not the earlier page")
	}
	if !bytes.Equal(readBundleFile(t, dir, want[1]), cells[0].Result.Page.JPEG) {
		t.Error("images/AB01-p2.jpg is not the later page")
	}
}

// --- the images/ substitution ----------------------------------------------

// TestWriteBundle_ImagesAreTheOriginalPagesNotTheMasked is THE deviation from
// the web export, and the reason it is safe: the bundle never leaves this
// machine, and a professor grading from it needs to see whose paper this is.
// The masked derivative still goes into the export input, which is what keeps
// export's own privacy sweep meaningful.
func TestWriteBundle_ImagesAreTheOriginalPagesNotTheMasked(t *testing.T) {
	dir := t.TempDir()
	cells := bundleFixture(t)
	if err := WriteBundle(dir, "Offline Exam", cells, bundleRoster(), 2); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	for _, tc := range []struct {
		rel  string
		cell CellDoc
	}{
		{"offline-exam-p1/images/AB01.jpg", cells[0]},
		{"offline-exam-p2/images/AB01.jpg", cells[1]},
		{"offline-exam-p1/images/AB02.jpg", cells[2]},
		{"offline-exam-p2/images/AB02.jpg", cells[3]},
	} {
		got := readBundleFile(t, dir, tc.rel)
		if !bytes.Equal(got, tc.cell.Result.Page.JPEG) {
			t.Errorf("%s: not the ORIGINAL page bytes", tc.rel)
		}
		if bytes.Equal(got, tc.cell.Masked.JPEG()) {
			t.Errorf("%s: the masked derivative was written instead of the original", tc.rel)
		}
		// And the identity really is visible: the band still carries its ink.
		assertPixelIs(t, decodeJPEG(t, got), 20, 5, bandInk, tc.rel+" identity band")
	}
}

// --- privacy ---------------------------------------------------------------

// TestWriteBundle_NoFileButTheManifestCarriesAnIdentity mirrors the export
// package's own mechanical sweep (internal/export/privacy_test.go): every byte
// of every file, against every roster identity.
func TestWriteBundle_NoFileButTheManifestCarriesAnIdentity(t *testing.T) {
	dir := t.TempDir()
	rows := bundleRoster()
	cells := bundleFixture(t)
	// The B-C10 case: masking is rectangular, students write their name in the
	// margin, so the transcription itself can carry identity.
	cells[0].Doc.Blocks = append(cells[0].Doc.Blocks, transcribe.Block{
		Kind: transcribe.BlockProse,
		Text: "Test Alpha (AB01, alpha@example.test) — continued on the back.",
	})
	if err := WriteBundle(dir, "Offline Exam", cells, rows, 2); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	type needle struct{ kind, value string }
	var needles []needle
	for _, r := range rows {
		needles = append(needles,
			needle{"roster name", r.Name},
			needle{"student id", r.StudentID},
			needle{"roster email", r.Email},
		)
	}
	sawManifest := false
	for _, rel := range walkBundle(t, dir) {
		if strings.HasSuffix(rel, ".jpg") {
			continue // images are pixels, and carry identity on purpose
		}
		isManifest := strings.HasSuffix(rel, "/MANIFEST.csv")
		sawManifest = sawManifest || isManifest
		body := strings.ToLower(string(readBundleFile(t, dir, rel)))
		for _, n := range needles {
			if !strings.Contains(body, strings.ToLower(n.value)) {
				continue
			}
			if isManifest && n.kind == "student id" {
				continue // the documented decoder-ring exception
			}
			t.Errorf("%s contains a %s in its BYTES", rel, n.kind)
		}
	}
	if !sawManifest {
		t.Error("no MANIFEST.csv — the decoder ring is mandatory")
	}

	all := string(readBundleFile(t, dir, "offline-exam-p1/_all.tex"))
	if !strings.Contains(all, "Student 001") {
		t.Error("_all.tex is not pseudonymous: no \"Student 001\" heading")
	}
}

// TestWriteBundle_ScrubIsAppliedExactlyOnce — export scrubs identity out of the
// doc INSIDE problemEntries, which Files reaches. Scrubbing here as well would
// not double-redact (the marker is not a needle); it would report zero
// redactions in the manifest and DESTROY the mask-quality signal that count
// exists to carry (spec §4).
func TestWriteBundle_ScrubIsAppliedExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	rows := bundleRoster()
	cells := []CellDoc{bundleCell(t, 1, rows[0], 1, transcribe.Block{
		Kind: transcribe.BlockProse,
		Text: "Test Alpha AB01 wrote this in the margin",
	})}
	if err := WriteBundle(dir, "Offline Exam", cells, rows, 1); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	flags := manifestRowFor(t, dir, "offline-exam-p1/MANIFEST.csv", "AB01")[6]
	if !strings.Contains(flags, "identity-redacted: 2") {
		t.Errorf("manifest flags = %q, want an identity-redacted count of 2 (one name + one id, counted once)", flags)
	}
}

// TestWriteBundle_PrivateModes — the bundle is a directory of student
// handwriting and transcriptions on a shared machine.
func TestWriteBundle_PrivateModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	if err := WriteBundle(dir, "Offline Exam", bundleFixture(t), bundleRoster(), 2); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", path, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// --- what is left out ------------------------------------------------------

// TestWriteBundle_SkipsProblemsWithNothingToShow — an empty problem directory
// reads as "nobody answered", which is a claim about the cohort this run has no
// business making.
func TestWriteBundle_SkipsProblemsWithNothingToShow(t *testing.T) {
	dir := t.TempDir()
	rows := bundleRoster()
	cells := []CellDoc{
		bundleCell(t, 1, rows[0], 1),
		bundleCell(t, 2, rows[0], 2), // this one failed
	}
	cells[1].Err = errWriteBundleFixture
	cells[1].Doc = transcribe.Doc{}

	// --problems 3: problem 2 has only a failed cell, problem 3 has none at all.
	if err := WriteBundle(dir, "Offline Exam", cells, rows, 3); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	for _, rel := range walkBundle(t, dir) {
		if strings.HasPrefix(rel, "offline-exam-p2/") || strings.HasPrefix(rel, "offline-exam-p3/") {
			t.Errorf("wrote %s for a problem with no successful cells", rel)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "offline-exam-p1" {
		t.Errorf("out dir holds %v, want only offline-exam-p1", entries)
	}
}

var errWriteBundleFixture = errors.New("transcription failed")

// TestWriteBundle_NothingSucceededWritesNoBundle — the run already failed with
// a *ProviderError upstream; the bundle must not fabricate an empty one.
func TestWriteBundle_NothingSucceededWritesNoBundle(t *testing.T) {
	dir := t.TempDir()
	rows := bundleRoster()
	cells := []CellDoc{bundleCell(t, 1, rows[0], 1)}
	cells[0].Err = errWriteBundleFixture
	if err := WriteBundle(dir, "Offline Exam", cells, rows, 2); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if files := walkBundle(t, dir); len(files) != 0 {
		t.Errorf("wrote %v, want nothing", files)
	}
}

// TestWriteBundle_UnmatchedCellsNeverReachTheBundle — an unmatched page names
// nobody; filing it under a student would be exactly the misattribution the
// match report exists to prevent.
func TestWriteBundle_UnmatchedCellsNeverReachTheBundle(t *testing.T) {
	dir := t.TempDir()
	rows := bundleRoster()
	cells := []CellDoc{bundleCell(t, 1, rows[0], 1), bundleCell(t, 2, rows[1], 1)}
	cells[1].Result.Status = StatusUnmatched
	cells[1].Result.StudentID = ""
	cells[1].Result.StudentName = ""
	cells[1].Result.Problem = 0

	if err := WriteBundle(dir, "Offline Exam", cells, rows, 1); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	for _, rel := range walkBundle(t, dir) {
		if strings.Contains(rel, "AB02") {
			t.Errorf("wrote %s for an unmatched page", rel)
		}
	}
}

// TestWriteBundle_IllegibleAnswerIsRecordedAsSuch — four statuses exist because
// "wrote nothing" and "could not be read" call for different actions.
func TestWriteBundle_IllegibleAnswerIsRecordedAsSuch(t *testing.T) {
	dir := t.TempDir()
	rows := bundleRoster()
	cells := []CellDoc{bundleCell(t, 1, rows[0], 1), bundleCell(t, 2, rows[1], 1)}
	cells[0].Doc = transcribe.Doc{}
	cells[0].Confidence = "illegible"

	if err := WriteBundle(dir, "Offline Exam", cells, rows, 1); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	row := manifestRowFor(t, dir, "offline-exam-p1/MANIFEST.csv", "AB01")
	if row[3] != "illegible" {
		t.Errorf("status = %q, want %q", row[3], "illegible")
	}
	if row[5] != "illegible" {
		t.Errorf("confidence = %q, want the model's own verdict", row[5])
	}
	if ok := manifestRowFor(t, dir, "offline-exam-p1/MANIFEST.csv", "AB02"); ok[3] != "ok" {
		t.Errorf("a transcribed answer's status = %q, want %q", ok[3], "ok")
	}
}

// TestWriteBundle_StudentMissingFromTheRosterIsAnError — the identity fields
// feed export's redaction needles, so a row assembled without them would ship a
// bundle whose scrub had nothing to look for.
func TestWriteBundle_StudentMissingFromTheRosterIsAnError(t *testing.T) {
	dir := t.TempDir()
	rows := bundleRoster()
	cells := []CellDoc{bundleCell(t, 7, roster.Row{StudentID: "ZZ99", Name: "Test Gamma"}, 1)}
	err := WriteBundle(dir, "Offline Exam", cells, rows, 1)
	assertErrorType[*RosterError](t, err, "page 7")
	if strings.Contains(err.Error(), "ZZ99") || strings.Contains(err.Error(), "Test Gamma") {
		t.Errorf("error %q names a student (PII rule)", err.Error())
	}
}
