package export

import (
	"archive/zip"
	"bytes"
	"image/color"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// The whole-exam bundle is the per-problem bundle N times over, side by side
// under one root:
//
//	{slug}/
//	├── {slug}-p1/…   exactly the tree BuildZIP produces for problem 1
//	└── {slug}-p3/…   …and for problem 3
//
// Everything the per-problem archive promises must survive the composition:
// determinism, pure-ASCII entry names, the size cap, and above all the
// identity-lives-in-filenames-never-in-bytes invariant (privacy_test.go).

// examInput builds a two-problem exam from the shared fixture. Problem 3 is
// listed FIRST so every ordering assertion tests the sort rather than the
// fixture, and problem 1 carries a different answer set so the two trees are
// distinguishable.
func examInput(t *testing.T) ExamInput {
	t.Helper()
	p3 := sampleInput(t)

	p1 := sampleInput(t)
	p1.ProblemNumber = 1
	p1.Answers = p1.Answers[:2]

	return ExamInput{
		AssessmentName: "Algorithms Midterm 2",
		Problems:       []Input{p3, p1},
	}
}

// ---- layout --------------------------------------------------------------

// TestBuildExamZIP_NestsEachProblemTreeUnderOneRoot is the folder contract: one
// top directory, each problem's existing tree unchanged beneath it, problems in
// number order.
func TestBuildExamZIP_NestsEachProblemTreeUnderOneRoot(t *testing.T) {
	out, err := BuildExamZIP(examInput(t))
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
	}
	got := zipEntryNames(t, out)
	want := []string{
		"algorithms-midterm-2/algorithms-midterm-2-p1/_all.tex",
		"algorithms-midterm-2/algorithms-midterm-2-p1/MANIFEST.csv",
		"algorithms-midterm-2/algorithms-midterm-2-p1/tex/b09901002.tex",
		"algorithms-midterm-2/algorithms-midterm-2-p1/tex/b09901007.tex",
		"algorithms-midterm-2/algorithms-midterm-2-p1/_all.typ",
		"algorithms-midterm-2/algorithms-midterm-2-p1/typ/b09901002.typ",
		"algorithms-midterm-2/algorithms-midterm-2-p1/typ/b09901007.typ",
		"algorithms-midterm-2/algorithms-midterm-2-p1/images/b09901002-p1.jpg",
		"algorithms-midterm-2/algorithms-midterm-2-p1/images/b09901002-p2.jpg",
		"algorithms-midterm-2/algorithms-midterm-2-p1/images/b09901007.jpg",
		"algorithms-midterm-2/algorithms-midterm-2-p3/_all.tex",
		"algorithms-midterm-2/algorithms-midterm-2-p3/MANIFEST.csv",
		"algorithms-midterm-2/algorithms-midterm-2-p3/tex/b09901002.tex",
		"algorithms-midterm-2/algorithms-midterm-2-p3/tex/b09901005.tex",
		"algorithms-midterm-2/algorithms-midterm-2-p3/tex/b09901007.tex",
		"algorithms-midterm-2/algorithms-midterm-2-p3/tex/b09901009.tex",
		"algorithms-midterm-2/algorithms-midterm-2-p3/_all.typ",
		"algorithms-midterm-2/algorithms-midterm-2-p3/typ/b09901002.typ",
		"algorithms-midterm-2/algorithms-midterm-2-p3/typ/b09901005.typ",
		"algorithms-midterm-2/algorithms-midterm-2-p3/typ/b09901007.typ",
		"algorithms-midterm-2/algorithms-midterm-2-p3/typ/b09901009.typ",
		"algorithms-midterm-2/algorithms-midterm-2-p3/images/b09901002-p1.jpg",
		"algorithms-midterm-2/algorithms-midterm-2-p3/images/b09901002-p2.jpg",
		"algorithms-midterm-2/algorithms-midterm-2-p3/images/b09901005.jpg",
		"algorithms-midterm-2/algorithms-midterm-2-p3/images/b09901007.jpg",
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

// TestBuildExamZIP_InnerTreesAreThePerProblemBundleVerbatim is the refactor's
// real contract: the exam bundle must be composed from the SAME entry writer as
// the per-problem bundle, so every inner file is byte-identical to what a
// per-problem download would hand over. If the two paths ever diverge, the
// professor's whole-exam download quietly stops matching his per-problem ones.
func TestBuildExamZIP_InnerTreesAreThePerProblemBundleVerbatim(t *testing.T) {
	exam, err := BuildExamZIP(examInput(t))
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
	}
	single, err := BuildZIP(sampleInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	for _, name := range zipEntryNames(t, single) {
		want := zipEntryBytes(t, single, name)
		got := zipEntryBytes(t, exam, "algorithms-midterm-2/"+name)
		if !bytes.Equal(got, want) {
			t.Errorf("exam entry %q differs from the per-problem bundle's bytes", name)
		}
	}
}

// TestBuildExamZIP_RootDirNamesTheDownload — the caller names the .zip after it.
func TestBuildExamZIP_RootDirNamesTheArchiveRoot(t *testing.T) {
	if got, want := examInput(t).RootDir(), "algorithms-midterm-2"; got != want {
		t.Errorf("RootDir() = %q, want %q", got, want)
	}
}

// ---- determinism ---------------------------------------------------------

// TestBuildExamZIP_IsByteIdenticalForIdenticalInput carries spec §2's
// re-export-is-free contract to the whole-exam archive, including the deliberate
// cross-second rebuild that is the only thing which catches an unpinned
// modification time (ZIP stamps have 1–2 second granularity).
func TestBuildExamZIP_IsByteIdenticalForIdenticalInput(t *testing.T) {
	a, err := BuildExamZIP(examInput(t))
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
	}
	b, err := BuildExamZIP(examInput(t))
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("two builds of identical input differ (%d vs %d bytes)", len(a), len(b))
	}

	if testing.Short() {
		t.Skip("skipping the cross-second-boundary rebuild under -short")
	}
	time.Sleep(1100 * time.Millisecond)
	c, err := BuildExamZIP(examInput(t))
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
	}
	if !bytes.Equal(a, c) {
		t.Fatal("a build one second later differs — a wall-clock stamp is leaking into the archive")
	}
}

func TestBuildExamZIP_PinsEntryModificationTimes(t *testing.T) {
	out, err := BuildExamZIP(examInput(t))
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
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

// TestBuildExamZIP_ProblemOrderDoesNotChangeOutput: the handler assembles the
// problem list from a query, and a query's ordering must not be load-bearing.
func TestBuildExamZIP_ProblemOrderDoesNotChangeOutput(t *testing.T) {
	a, err := BuildExamZIP(examInput(t))
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
	}
	shuffled := examInput(t)
	shuffled.Problems[0], shuffled.Problems[1] = shuffled.Problems[1], shuffled.Problems[0]
	b, err := BuildExamZIP(shuffled)
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("archive must not depend on the caller's problem ordering")
	}
}

// TestBuildExamZIP_SubInputAssessmentNameIsNotLoadBearing: the exam's name owns
// the whole archive. A per-problem Input carrying a stale or different name must
// not produce a subdirectory under a second slug.
func TestBuildExamZIP_SubInputAssessmentNameIsNotLoadBearing(t *testing.T) {
	in := examInput(t)
	in.Problems[0].AssessmentName = "Something Else Entirely"
	out, err := BuildExamZIP(in)
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
	}
	for _, name := range zipEntryNames(t, out) {
		if !strings.HasPrefix(name, "algorithms-midterm-2/algorithms-midterm-2-p") {
			t.Errorf("entry %q escaped the exam's own slug", name)
		}
	}
}

// ---- validation ----------------------------------------------------------

func TestBuildExamZIP_RejectsNoProblems(t *testing.T) {
	if _, err := BuildExamZIP(ExamInput{AssessmentName: "Algorithms Midterm 2"}); err == nil {
		t.Fatal("BuildExamZIP must reject an exam with no problems")
	}
}

// TestBuildExamZIP_RejectsDuplicateProblemNumbers: two p3 trees would occupy the
// same directory, silently interleaving two cohorts' files.
func TestBuildExamZIP_RejectsDuplicateProblemNumbers(t *testing.T) {
	in := examInput(t)
	in.Problems[1].ProblemNumber = in.Problems[0].ProblemNumber
	if _, err := BuildExamZIP(in); err == nil {
		t.Fatal("duplicate problem numbers would collide on disk")
	}
}

// TestBuildExamZIP_PropagatesPerProblemValidation: the per-problem rules are not
// relaxed by being nested — one bad row fails the whole exam bundle.
func TestBuildExamZIP_PropagatesPerProblemValidation(t *testing.T) {
	in := examInput(t)
	in.Problems[1].Answers[0].Identity.StudentID = "../../etc/passwd"
	if _, err := BuildExamZIP(in); err == nil {
		t.Fatal("BuildExamZIP must apply the per-problem validation to every problem")
	}

	in = examInput(t)
	in.Problems[1].Answers = nil
	if _, err := BuildExamZIP(in); err == nil {
		t.Fatal("a problem with no answers has no tree to write; the caller must drop it first")
	}
}

// TestBuildExamZIP_SizeCapAppliesToTheWholeArchive — the cap bounds ONE built
// archive, and for the exam bundle that archive is every problem at once. A cap
// applied per problem would let an 8-problem exam build 8x the ceiling.
func TestBuildExamZIP_SizeCapAppliesToTheWholeArchive(t *testing.T) {
	in := examInput(t)
	// Size the cap to exactly one problem's finished archive: big enough for
	// either tree alone, too small for both together.
	one, err := BuildZIP(in.Problems[1])
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}

	orig := MaxZipBytes
	defer func() { MaxZipBytes = orig }()
	MaxZipBytes = int64(len(one))

	if _, err := BuildExamZIP(in); err == nil {
		t.Fatal("BuildExamZIP must apply MaxZipBytes to the whole exam archive, not per problem")
	}
	// Non-vacuity: the same cap must still admit the single-problem bundle it
	// was sized for, or the assertion above proves nothing about the cap's scope.
	if _, err := BuildZIP(in.Problems[1]); err != nil {
		t.Fatalf("the cap was sized to admit one problem alone, but BuildZIP failed: %v", err)
	}
}

// TestBuildExamZIP_EveryEntryNameIsPureASCII: macOS's bundled Info-ZIP rejects
// an ENTIRE archive carrying one non-ASCII entry name, and the exam bundle
// repeats the slug twice per path — twice the exposure of the per-problem one.
func TestBuildExamZIP_EveryEntryNameIsPureASCII(t *testing.T) {
	in := examInput(t)
	in.AssessmentName = "微積分 期中考（2026）"
	out, err := BuildExamZIP(in)
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
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

// TestBuildExamZIP_PseudonymsAreScopedPerProblem records the honest consequence
// of composing per-problem trees: "Student 001" means a different person in
// p1/_all.tex than in p3/_all.tex whenever the two problems have different
// cohorts, because each tree is pseudonymised over its OWN sorted id set. Each
// tree carries its own MANIFEST.csv decoder ring, so nothing is ambiguous within
// a tree — but a reader must not carry a pseudonym across trees.
func TestBuildExamZIP_PseudonymsAreScopedPerProblem(t *testing.T) {
	out, err := BuildExamZIP(examInput(t))
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
	}
	p1 := manifestRowIn(t, out, "algorithms-midterm-2/algorithms-midterm-2-p1/MANIFEST.csv", "b09901007")
	p3 := manifestRowIn(t, out, "algorithms-midterm-2/algorithms-midterm-2-p3/MANIFEST.csv", "b09901007")
	if p1[1] == p3[1] {
		t.Skip("the fixture's cohorts happen to agree on this student's rank; nothing to assert")
	}
	if p1[1] != "Student 002" || p3[1] != "Student 003" {
		t.Errorf("pseudonyms = %q / %q, want each tree ranked over its own cohort", p1[1], p3[1])
	}
}

// ---- privacy -------------------------------------------------------------

// TestBuildExamZIP_NoExportedByteCarriesARosterIdentity runs the load-bearing
// privacy sweep over the whole-exam archive. The per-problem sweep passing does
// NOT imply this one: the exam path has its own composition step, and a leak
// introduced there (say, an exam-level index file listing students) would be
// invisible to privacy_test.go.
func TestBuildExamZIP_NoExportedByteCarriesARosterIdentity(t *testing.T) {
	in := examInput(t)
	// The B-C10 case: masking is rectangular, students write their name in the
	// margin, so the transcription itself can carry identity.
	for i := range in.Problems {
		in.Problems[i].Answers[0].Doc.Blocks = append(in.Problems[i].Answers[0].Doc.Blocks, blockNaming(
			"Grace Hopper (b09901007, hopper@example.edu) — continued on the back."))
		in.Problems[i].Answers[1].Doc.Blocks = append(in.Problems[i].Answers[1].Doc.Blocks, blockNaming(
			"see Ada Lovelace's earlier proof, b09901002"))
	}

	out, err := BuildExamZIP(in)
	if err != nil {
		t.Fatalf("BuildExamZIP: %v", err)
	}

	var needles []identityKind
	for _, p := range in.Problems {
		needles = append(needles, rosterNeedles(p)...)
	}

	manifests := 0
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	for _, f := range zr.File {
		body := zipEntryBytes(t, out, f.Name)
		isManifest := strings.HasSuffix(f.Name, "/MANIFEST.csv")
		if isManifest {
			manifests++
		}
		for _, n := range needles {
			if !containsFold(body, n.value) {
				continue
			}
			if isManifest && n.kind == "student id" {
				continue // the one documented exception: the decoder ring
			}
			t.Errorf("entry %q contains a %s in its BYTES", f.Name, n.kind)
		}
	}
	if manifests != len(in.Problems) {
		t.Errorf("found %d MANIFEST.csv files, want one per problem tree (%d)", manifests, len(in.Problems))
	}
}

// TestBuildExamZIP_ErrorsNeverNameAStudent — CLAUDE.md's PII rule applies to the
// exam path's own error messages too.
func TestBuildExamZIP_ErrorsNeverNameAStudent(t *testing.T) {
	cases := map[string]func(in *ExamInput){
		"unsafe id":  func(in *ExamInput) { in.Problems[0].Answers[0].Identity.StudentID = "../../etc/passwd" },
		"bad status": func(in *ExamInput) { in.Problems[1].Answers[0].Status = "maybe" },
		"absent page": func(in *ExamInput) {
			in.Problems[0].Answers[3].Pages = []imaging.MaskedImage{maskedPage(t, color.RGBA{A: 255})}
		},
		"dup problem": func(in *ExamInput) { in.Problems[1].ProblemNumber = in.Problems[0].ProblemNumber },
	}
	for name, mutate := range cases {
		in := examInput(t)
		mutate(&in)
		_, err := BuildExamZIP(in)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		msg := strings.ToLower(err.Error())
		for _, n := range rosterNeedles(sampleInput(t)) {
			if strings.Contains(msg, strings.ToLower(n.value)) {
				t.Errorf("%s: error message leaks a %s", name, n.kind)
			}
		}
	}
}

// ---- helpers -------------------------------------------------------------

func blockNaming(text string) transcribe.Block {
	return transcribe.Block{Kind: transcribe.BlockProse, Text: text}
}

// manifestRowIn reads one row out of a NAMED manifest, which the exam bundle
// needs because it carries several.
func manifestRowIn(t *testing.T, zipBytes []byte, manifestName, studentID string) []string {
	t.Helper()
	raw := zipEntryBytes(t, zipBytes, manifestName)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if fields[0] == studentID {
			return fields
		}
	}
	t.Fatalf("no row for the requested student in %q", manifestName)
	return nil
}

// compile-time proof that the exam bundle's answers are the same sealed
// MaskedImage values the per-problem bundle takes.
var _ = func() bool {
	var in ExamInput
	var a Answer
	var _ []Input = in.Problems
	var _ regrade.Identity = a.Identity
	var _ []imaging.MaskedImage = a.Pages
	return true
}()

// TestAllTeX_MatchesTheBundledEntry pins the compile gate's input to the
// shipped artifact: verifying AllTeX's output must be verifying the exact
// bytes the professor receives, or the gate checks a fiction.
func TestAllTeX_MatchesTheBundledEntry(t *testing.T) {
	in := sampleInput(t)
	tex, err := AllTeX(in)
	if err != nil {
		t.Fatalf("AllTeX: %v", err)
	}
	zipBytes, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	var bundled []byte
	for _, f := range zr.File {
		if f.Name == in.RootDir()+"/_all.tex" {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			bundled, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if tex != string(bundled) {
		t.Error("AllTeX must be byte-identical to the bundled _all.tex")
	}
}
