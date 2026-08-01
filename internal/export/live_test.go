package export

import (
	"bytes"
	"context"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/report"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// These tests run a real tectonic compile over the ASSEMBLED bundle, which is
// the only way to prove the claim that matters most about _all.tex: that it is
// ONE document with ONE preamble and N sections, rather than N concatenated
// standalone files that happen to look right in a substring assertion. A
// second \documentclass compiles to a hard error, so this test is the
// difference between "looks assembled" and "is assembled".
//
// Skipped when no engine is available, matching internal/transcribe's
// live_test.go convention (opt-in, never a hard CI dependency).

func engineOrSkip(t *testing.T) (bin, cache string) {
	t.Helper()
	bin = os.Getenv("ADAMARKER_TECTONIC_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("tectonic"); err != nil {
			t.Skip("tectonic not installed; skipping live compile test")
		}
	}
	cache = transcribe.DefaultCacheDir()
	if cache == "" {
		t.Skip("cannot resolve tectonic cache dir")
	}
	return bin, cache
}

func bundledFont(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../data/fonts/NotoSansTC-Regular.ttf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("bundled CJK font missing: %v", err)
	}
	return p
}

// liveInput mixes everything a real zh-Hant algorithms answer contains, plus a
// macro bomb that must be demoted rather than compiled, plus an answer whose
// transcription carries the student's own name (the B-C10 mask-leak case).
func liveInput(t *testing.T) Input {
	t.Helper()
	return Input{
		AssessmentName: "演算法 Midterm 2",
		ProblemNumber:  2,
		TeX:            transcribe.Options{CJKFontFile: bundledFont(t)},
		Answers: []Answer{
			{
				Identity: regrade.Identity{Name: "Grace Hopper", StudentID: "b09901007", Email: "hopper@example.edu"},
				Doc: transcribe.Doc{Blocks: []transcribe.Block{
					{Kind: transcribe.BlockProse, Text: `這是一個貪婪演算法。The greedy runs in $O(n \log n)$ time, 時間複雜度為 $\Theta(n)$ overall.`},
					{Kind: transcribe.BlockMath, Text: `T(n) = 2T\left(\frac{n}{2}\right) + O(n)`},
					{Kind: transcribe.BlockCode, Text: "for i in 1..n:\n    dp[i] = min(dp[i-1] + 1, dp[i])"},
					{Kind: transcribe.BlockList, Items: []string{`依結束時間排序`, `由左至右掃描，取 $j$ 為第一個可行者`}},
					{Kind: transcribe.BlockMath, Text: `\def\x{\x}\x`}, // must be demoted, must not hang
					{Kind: transcribe.BlockProse, Text: "Grace Hopper b09901007 — 續下頁"},
				}},
				Pages:      []imaging.MaskedImage{maskedPage(t, color.RGBA{R: 200, A: 255})},
				Status:     StatusOK,
				Source:     SourceDedicated,
				Confidence: "0.9",
			},
			{
				Identity: regrade.Identity{Name: "Ada Lovelace", StudentID: "b09901002", Email: "lovelace@example.edu"},
				Doc: transcribe.Doc{Blocks: []transcribe.Block{
					{Kind: transcribe.BlockProse, Text: `Counterexample: 令 $S = \{1,2,3\}$，則 100% 的情形下 a_b & c 皆成立。`},
				}},
				Pages:  []imaging.MaskedImage{maskedPage(t, color.RGBA{G: 200, A: 255})},
				Status: StatusOK,
				Source: SourceGradingCache,
			},
			{
				Identity: regrade.Identity{Name: "Barbara Liskov", StudentID: "b09901009", Email: "liskov@example.edu"},
				Status:   StatusAbsent,
			},
		},
	}
}

func TestLive_AllTexCompilesAsASingleDocument(t *testing.T) {
	bin, cache := engineOrSkip(t)

	out, err := BuildZIP(liveInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	all := zipEntryContent(t, out, "midterm-2-p2/_all.tex")

	// Generous ceiling: a cold machine downloads the support bundle first.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pdf, err := transcribe.Compile(ctx, bin, cache, all)
	if err != nil {
		// The source is withheld: it embeds student answer text (CLAUDE.md).
		t.Fatalf("_all.tex failed to compile (%d bytes, source withheld): %v", len(all), err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Fatalf("output is not a PDF (%d bytes)", len(pdf))
	}
	t.Logf("compiled the whole-cohort _all.tex to a %d-byte PDF", len(pdf))
}

func TestLive_PerStudentTexCompilesStandalone(t *testing.T) {
	bin, cache := engineOrSkip(t)

	out, err := BuildZIP(liveInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Including the absent student: an export the professor cannot compile in
	// bulk because one student never sat the exam is a broken export.
	for _, id := range []string{"b09901007", "b09901002", "b09901009"} {
		tex := zipEntryContent(t, out, "midterm-2-p2/tex/"+id+".tex")
		pdf, err := transcribe.Compile(ctx, bin, cache, tex)
		if err != nil {
			t.Fatalf("a per-student .tex failed to compile (%d bytes, source withheld): %v", len(tex), err)
		}
		if !bytes.HasPrefix(pdf, []byte("%PDF")) {
			t.Fatalf("output is not a PDF (%d bytes)", len(pdf))
		}
	}
}

// TestLive_ArchiveExtractsWithSystemUnzip is the test that decided Slug's
// ASCII rule. An earlier version kept CJK in the root directory name; this
// test caught macOS's bundled Info-ZIP 6.00 refusing the whole archive with
// "Illegal byte sequence", because it does not honour the UTF-8 name flag that
// archive/zip sets. Finder and Windows Explorer both accepted the same file,
// so nothing short of a real extractor would have found it.
func TestLive_ArchiveExtractsWithSystemUnzip(t *testing.T) {
	unzip, err := exec.LookPath("unzip")
	if err != nil {
		t.Skip("unzip not installed")
	}
	out, err := BuildZIP(liveInput(t))
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "export.zip")
	if err := os.WriteFile(archive, out, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if b, err := exec.CommandContext(ctx, unzip, "-qq", "-t", archive).CombinedOutput(); err != nil {
		t.Fatalf("system unzip rejected the archive: %v\n%s", err, b)
	}
	if b, err := exec.CommandContext(ctx, unzip, "-qq", archive, "-d", dir).CombinedOutput(); err != nil {
		t.Fatalf("system unzip failed to extract: %v\n%s", err, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "midterm-2-p2", "_all.tex")); err != nil {
		entries, _ := os.ReadDir(dir)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("extracted tree does not contain the CJK root dir: %v (have %v)", err, names)
	}
}

// TestLive_AllTypCompilesWithMitex proves the Typst mirror's central bet
// (spec 2026-07-30): that mitex covers the math allow-list as students
// actually exercise it — proof symbols, stacked decorations, matrices,
// cases, CJK in \text — so a verdict of "verified" means a PDF, not luck.
// Opt-in like every live test: skipped without a typst binary.
func TestLive_AllTypCompilesWithMitex(t *testing.T) {
	bin := os.Getenv("ADAMARKER_TYPST_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("typst"); err != nil {
			t.Skip("typst not installed; skipping live mirror compile test")
		}
	}

	in := sampleInput(t)
	in.Answers[0].Doc.Blocks = append(in.Answers[0].Doc.Blocks,
		transcribe.Block{Kind: transcribe.BlockMath, Text: `x > 1 \therefore x^2 > x \quad \because x \nmid 0`},
		transcribe.Block{Kind: transcribe.BlockMath, Text: `\overset{\text{def}}{=} \underset{x \to 0}{\lim} f(x)`},
		transcribe.Block{Kind: transcribe.BlockMath, Text: `\begin{pmatrix} a & b \\ c & d \end{pmatrix} \begin{cases} 1 & \text{if 偶數} \\ 0 & \text{else} \end{cases}`},
		transcribe.Block{Kind: transcribe.BlockProse, Text: `因此 $O(n \log n)$ 成立。`},
		transcribe.Block{Kind: transcribe.BlockMath, Text: `\text{[a] and @x} + \operatorname{argmax} + \mathrm{a(b)c}`},
	)
	typ, err := AllTyp(in)
	if err != nil {
		t.Fatalf("AllTyp: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pdf, err := report.CompileTypstSource(ctx, bin, "", typ)
	if err != nil {
		t.Fatalf("_all.typ must compile with mitex (allow-list coverage gap?): %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Fatalf("output is not a PDF (first bytes %q)", pdf[:min(8, len(pdf))])
	}
}
