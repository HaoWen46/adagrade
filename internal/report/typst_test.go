package report

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// typstBinOrSkip mirrors the live-test gating pattern: these tests need a
// typst binary (and, once per machine, network for the mitex package cache).
// They skip — not fail — where that environment is absent.
func typstBinOrSkip(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("typst")
	if err != nil {
		t.Skip("typst binary not on PATH — skipping Typst renderer test")
	}
	// Probe that the mitex package is compilable here (cache or network).
	dir := t.TempDir()
	probe := filepath.Join(dir, "p.typ")
	if err := os.WriteFile(probe, []byte("#import \"@preview/mitex:"+mitexVersion+"\": mi\n#mi(\"x\")\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(bin, "compile", "--root", dir, probe, filepath.Join(dir, "p.pdf")).Run(); err != nil {
		t.Skipf("typst cannot compile mitex here (no package cache / network): %v", err)
	}
	return bin
}

func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < 4; i++ {
		img.Set(i, i, color.RGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func typstInput(t *testing.T) ReportInput {
	return ReportInput{
		AssessmentName: "Demo Exam（期末）",
		StudentName:    "王小明",
		StudentID:      "B11902001",
		Quality:        QualityOriginal,
		Total:          "17.5", Max: "20",
		Problems: []ProblemReport{{
			Label: "Problem 1: Binary search",
			Pages: [][]byte{tinyJPEG(t)},
			Criteria: []CriterionLine{
				{Name: `States the bound $O(\log n)$`, Score: "3", Max: "3"},
				{Name: "Solves the recurrence", Score: "2.5", Max: "4"},
			},
			Total: "5.5", Max: "7",
			Comment: `Good overall. The bound \(T(n) \leq 2T(n/2) + O(n)\) is right; ` +
				`#import "@preview/evil:1.0.0": * and ` + "`#read(\"/etc/hosts\")`" + ` must render as text.`,
		}},
	}
}

// The renderer must produce a real PDF, byte-deterministically, with hostile
// comment content rendered inert (the compile succeeding at all proves the
// injection attempt stayed literal text — bare #import of a nonexistent
// package would fail the compile).
func TestBuildTypst_DeterministicPDFWithHostileComment(t *testing.T) {
	bin := typstBinOrSkip(t)
	in := typstInput(t)

	a, err := BuildTypst(t.Context(), bin, "", in)
	if err != nil {
		t.Fatalf("BuildTypst: %v", err)
	}
	if !bytes.HasPrefix(a, []byte("%PDF")) {
		t.Fatalf("output is not a PDF (first bytes %q)", a[:min(8, len(a))])
	}
	b, err := BuildTypst(t.Context(), bin, "", in)
	if err != nil {
		t.Fatalf("BuildTypst (second run): %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("Typst report must be byte-deterministic: %d vs %d bytes", len(a), len(b))
	}
}

// The generated source itself must uphold the injection invariant: hostile
// text appears only inside escaped strings, never in markup position.
func TestTypstDocument_SourceKeepsHostileTextQuoted(t *testing.T) {
	dir := t.TempDir()
	doc, err := typstDocument(dir, typstInput(t))
	if err != nil {
		t.Fatalf("typstDocument: %v", err)
	}
	// Exactly one line-starting #import (the pinned mitex one); the hostile
	// text's "#import" substring survives only inside an escaped string, so it
	// never begins a line of markup.
	imports := 0
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "#import") {
			imports++
			if !strings.Contains(line, "@preview/mitex") {
				t.Fatalf("unexpected markup-position import: %q", line)
			}
		}
	}
	if imports != 1 {
		t.Fatalf("want exactly 1 markup-position #import (mitex), got %d:\n%s", imports, doc)
	}
	if !bytes.Contains([]byte(doc), []byte(`#mi("T(n) \\leq 2T(n/2) + O(n)")`)) {
		t.Fatalf("comment math must reach mi():\n%s", doc)
	}
}

// A hostile self-referential LaTeX macro inside a math span drives mitex's
// expander into unbounded recursion — the compile HANGS rather than erroring,
// which without a hard kill would never reach the sender's fpdf fallback and
// would wedge the single-worker email queue (adversarial review, high). The
// subprocess must be killed at typstCompileTimeout and surface as an error.
func TestBuildTypst_KillsRunawayMacroExpansion(t *testing.T) {
	bin := typstBinOrSkip(t)
	old := typstCompileTimeout
	typstCompileTimeout = 3 * time.Second
	t.Cleanup(func() { typstCompileTimeout = old })

	in := typstInput(t)
	in.Problems[0].Comment = `pathological: \(\newcommand{\bomb}{\bomb\bomb}\bomb\)`

	start := time.Now()
	_, err := BuildTypst(t.Context(), bin, "", in)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("runaway macro expansion must fail the build, not succeed")
	}
	if elapsed > 15*time.Second {
		t.Fatalf("compile was not killed promptly: took %s", elapsed)
	}
	if !strings.Contains(err.Error(), "killed") {
		t.Fatalf("error should say the compile was killed: %v", err)
	}
}

func TestBuildTypst_NoBinaryConfigured(t *testing.T) {
	if _, err := BuildTypst(t.Context(), "", "", typstInput(t)); err == nil {
		t.Fatal("empty binary path must error, not panic or succeed")
	}
}
