package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/export"
	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// fakeTectonic writes a stand-in engine that fails exactly when the source
// contains the marker, mirroring tectonic's CLI shape (Compile passes
// -X compile --untrusted --outdir <dir> <src>).
func fakeTectonic(t *testing.T, marker string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-tectonic")
	script := "#!/bin/sh\noutdir=$5\nsrc=$6\nif grep -q " + marker + " \"$src\"; then\n  exit 1\nfi\nprintf 'PDF' > \"$outdir/doc.pdf\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestVerifyProblemTeX_AttributesFailureToOffendingAnswers is the 2026-07-30
// audit's finding 8: a failed bundle compile must name WHICH answer cannot
// compile, instead of refusing the whole cohort with an anonymous error.
func TestVerifyProblemTeX_AttributesFailureToOffendingAnswers(t *testing.T) {
	good := transcribe.Doc{Blocks: []transcribe.Block{{Kind: transcribe.BlockProse, Text: "a clean answer"}}}
	bad := transcribe.Doc{Blocks: []transcribe.Block{{Kind: transcribe.BlockProse, Text: "this line says GATEFAILHERE somewhere"}}}
	in := export.Input{
		AssessmentName: "Midterm",
		ProblemNumber:  1,
		Answers: []export.Answer{
			{Identity: regrade.Identity{StudentID: "AAA001"}, Doc: good, Status: export.StatusOK, Source: export.SourceDedicated},
			{Identity: regrade.Identity{StudentID: "BBB002"}, Doc: bad, Status: export.StatusOK, Source: export.SourceDedicated},
		},
	}
	s := &Server{cfg: config.Config{TectonicBinPath: fakeTectonic(t, "GATEFAILHERE")}}

	err := s.verifyProblemTeX(context.Background(), in)
	if err == nil {
		t.Fatal("gate must fail when an answer cannot compile")
	}
	var ge *gateError
	if !errors.As(err, &ge) {
		t.Fatalf("want *gateError with attribution, got %T: %v", err, err)
	}
	if len(ge.studentIDs) != 1 || ge.studentIDs[0] != "BBB002" {
		t.Errorf("attribution must name exactly the offending answer, got %v", ge.studentIDs)
	}
	if msg := err.Error(); strings.Contains(msg, "BBB002") || strings.Contains(msg, "GATEFAILHERE") {
		t.Errorf("gateError.Error() goes to logs and must stay content-free, got %q", msg)
	}
	if !errors.Is(err, transcribe.ErrCompileFailed) {
		t.Error("gateError must still be recognisable as a compile failure")
	}
}

func TestVerifyProblemTeX_CleanBundleStillPasses(t *testing.T) {
	in := export.Input{
		AssessmentName: "Midterm",
		ProblemNumber:  1,
		Answers: []export.Answer{
			{Identity: regrade.Identity{StudentID: "AAA001"}, Doc: transcribe.Doc{Blocks: []transcribe.Block{{Kind: transcribe.BlockProse, Text: "fine"}}}, Status: export.StatusOK, Source: export.SourceDedicated},
		},
	}
	s := &Server{cfg: config.Config{TectonicBinPath: fakeTectonic(t, "GATEFAILHERE")}}
	if err := s.verifyProblemTeX(context.Background(), in); err != nil {
		t.Fatalf("clean bundle must pass the gate, got %v", err)
	}
}

// TestVerifyProblemTeX_AllAnswersFailingIsEnvironmentFault: when every
// standalone fails, the fault is the shared preamble (font, package), not the
// students — blaming the entire cohort by id is blaming no one.
func TestVerifyProblemTeX_AllAnswersFailingIsEnvironmentFault(t *testing.T) {
	doc := transcribe.Doc{Blocks: []transcribe.Block{{Kind: transcribe.BlockProse, Text: "fine"}}}
	in := export.Input{
		AssessmentName: "Midterm",
		ProblemNumber:  1,
		Answers: []export.Answer{
			{Identity: regrade.Identity{StudentID: "AAA001"}, Doc: doc, Status: export.StatusOK, Source: export.SourceDedicated},
			{Identity: regrade.Identity{StudentID: "BBB002"}, Doc: doc, Status: export.StatusOK, Source: export.SourceDedicated},
		},
	}
	// "documentclass" appears in every document's shared preamble.
	s := &Server{cfg: config.Config{TectonicBinPath: fakeTectonic(t, "documentclass")}}

	err := s.verifyProblemTeX(context.Background(), in)
	if err == nil {
		t.Fatal("gate must still fail")
	}
	var ge *gateError
	if errors.As(err, &ge) {
		t.Errorf("an everyone-fails result must not attribute to students, got %v", err)
	}
	if !errors.Is(err, transcribe.ErrCompileFailed) {
		t.Errorf("the failure must still read as a compile failure, got %v", err)
	}
}

func TestVerifyProblemTeX_SkipsAbsentAnswersInAttribution(t *testing.T) {
	good := transcribe.Doc{Blocks: []transcribe.Block{{Kind: transcribe.BlockProse, Text: "fine"}}}
	bad := transcribe.Doc{Blocks: []transcribe.Block{{Kind: transcribe.BlockProse, Text: "has GATEFAILHERE inside"}}}
	in := export.Input{
		AssessmentName: "Midterm",
		ProblemNumber:  1,
		Answers: []export.Answer{
			{Identity: regrade.Identity{StudentID: "AAA001"}, Doc: good, Status: export.StatusOK, Source: export.SourceDedicated},
			{Identity: regrade.Identity{StudentID: "BBB002"}, Doc: bad, Status: export.StatusOK, Source: export.SourceDedicated},
			{Identity: regrade.Identity{StudentID: "CCC003"}, Status: export.StatusAbsent},
		},
	}
	s := &Server{cfg: config.Config{TectonicBinPath: fakeTectonic(t, "GATEFAILHERE")}}

	err := s.verifyProblemTeX(context.Background(), in)
	var ge *gateError
	if !errors.As(err, &ge) {
		t.Fatalf("want *gateError, got %T: %v", err, err)
	}
	if len(ge.studentIDs) != 1 || ge.studentIDs[0] != "BBB002" {
		t.Errorf("attribution must name the offending answer only, got %v", ge.studentIDs)
	}
	if ge.total != 2 {
		t.Errorf("an absent answer carries no student content and must not be counted, total=%d", ge.total)
	}
}

func TestGateFailureMessage_NamesStudentsAndDistinguishesTimeout(t *testing.T) {
	ge := &gateError{studentIDs: []string{"AAA001"}, total: 30}
	if msg := gateFailureMessage(4, ge); !strings.Contains(msg, "AAA001") {
		t.Errorf("the professor-facing message must name the offending answer, got %q", msg)
	}
	if msg := gateFailureMessage(4, context.DeadlineExceeded); !strings.Contains(msg, "timed out") {
		t.Errorf("a timeout is not a compile failure and must not be reported as one, got %q", msg)
	}
}

// --- secondary Typst gate (spec 2026-07-30) --------------------------------

// fakeTypst mirrors typst's CLI shape (source second-to-last arg, output
// last), failing exactly when the source contains the marker.
func fakeTypst(t *testing.T, marker string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-typst")
	script := "#!/bin/sh\nprev=\"\"; last=\"\"\nfor a in \"$@\"; do prev=\"$last\"; last=\"$a\"; done\nif grep -q " + marker + " \"$prev\"; then exit 1; fi\nprintf 'PDF' > \"$last\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func typstGateInput() export.Input {
	return export.Input{
		AssessmentName: "Midterm",
		ProblemNumber:  1,
		Answers: []export.Answer{{
			Identity: regrade.Identity{StudentID: "AAA001"},
			Doc:      transcribe.Doc{Blocks: []transcribe.Block{{Kind: transcribe.BlockProse, Text: "has TYPMARKER inside"}}},
			Status:   export.StatusOK,
			Source:   export.SourceDedicated,
		}},
	}
}

func TestTypstVerdict_BestEffortNeverBlocks(t *testing.T) {
	in := typstGateInput()

	// Failing mirror -> "failed": the caller ships the bundle regardless and
	// the manifest records the verdict (export tests pin that rendering).
	s := &Server{cfg: config.Config{TypstBinPath: fakeTypst(t, "TYPMARKER")}, log: discardLogger()}
	if got := s.typstVerdict(context.Background(), in); got != "failed" {
		t.Errorf("failing mirror must report failed, got %q", got)
	}

	// Clean mirror -> "verified".
	s = &Server{cfg: config.Config{TypstBinPath: fakeTypst(t, "NEVERPRESENT")}, log: discardLogger()}
	if got := s.typstVerdict(context.Background(), in); got != "verified" {
		t.Errorf("clean mirror must report verified, got %q", got)
	}

	// No binary -> "" (manifest renders "unverified").
	s = &Server{cfg: config.Config{}, log: discardLogger()}
	if got := s.typstVerdict(context.Background(), in); got != "" {
		t.Errorf("unconfigured mirror must report empty verdict, got %q", got)
	}

	// A canceled context (deadline spent, engine killed) means the mirror was
	// never CHECKED — "failed" would be untruthful and would make the archive
	// bytes depend on scheduler luck. Only a deterministic compile failure
	// may say failed; everything else is "" (unverified).
	s = &Server{cfg: config.Config{TypstBinPath: fakeTypst(t, "NEVERPRESENT")}, log: discardLogger()}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.typstVerdict(canceled, in); got != "" {
		t.Errorf("an unchecked mirror must report unverified, got %q", got)
	}
}
