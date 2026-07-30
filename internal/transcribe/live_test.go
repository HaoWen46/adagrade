package transcribe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// These tests run a real tectonic compile. They are skipped when no engine is
// available, matching the repo's other live_test.go convention (opt-in, never
// a hard CI dependency). The first run downloads tectonic's support bundle;
// subsequent runs reuse the shared cache.

func engineOrSkip(t *testing.T) (bin, cache string) {
	t.Helper()
	bin = os.Getenv("ADAMARKER_TECTONIC_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("tectonic"); err != nil {
			t.Skip("tectonic not installed; skipping live compile test")
		}
	}
	cache = DefaultCacheDir()
	if cache == "" {
		t.Skip("cannot resolve tectonic cache dir")
	}
	return bin, cache
}

// bundledFont returns the repo's Traditional Chinese face.
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

// realisticDoc mixes everything an algorithms answer actually contains:
// Traditional Chinese prose, inline and display math, pseudocode whose
// indentation carries meaning, and a macro bomb the validator must demote.
func realisticDoc(t *testing.T) Doc {
	t.Helper()
	return Doc{
		Title: "Problem 2",
		Blocks: []Block{
			{Kind: BlockProse, Text: `這是一個貪婪演算法。The greedy runs in $O(n \log n)$ time, 時間複雜度為 $\Theta(n)$ overall.`},
			{Kind: BlockMath, Text: `T(n) = 2T\left(\frac{n}{2}\right) + O(n)`},
			{Kind: BlockCode, Text: "for i in 1..n:\n    dp[i] = min(dp[i-1] + 1, dp[i])"},
			{Kind: BlockList, Items: []string{`依結束時間排序`, `由左至右掃描，取 $j$ 為第一個可行者`}},
			{Kind: BlockMath, Text: `\def\x{\x}\x`}, // must be demoted, must not hang
		},
	}
}

func TestLive_EmittedTeXCompilesWithCJKAndMath(t *testing.T) {
	bin, cache := engineOrSkip(t)
	tex, flags := EmitTeXWith(realisticDoc(t), Options{CJKFontFile: bundledFont(t)})

	if len(flags) == 0 {
		t.Error("the macro bomb should have produced a demotion flag")
	}

	// Generous ceiling: the very first compile on a machine downloads the
	// support bundle over the network.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pdf, err := Compile(ctx, bin, cache, tex)
	if err != nil {
		t.Fatalf("emitted .tex failed to compile: %v\n--- source ---\n%s", err, tex)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Fatalf("output is not a PDF (%d bytes)", len(pdf))
	}
	t.Logf("compiled %d-byte PDF; flags=%v", len(pdf), flags)
}

func TestLive_CompileIsDeterministic(t *testing.T) {
	bin, cache := engineOrSkip(t)
	tex, _ := EmitTeXWith(realisticDoc(t), Options{CJKFontFile: bundledFont(t)})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	a, err := Compile(ctx, bin, cache, tex)
	if err != nil {
		t.Fatalf("first compile: %v", err)
	}
	b, err := Compile(ctx, bin, cache, tex)
	if err != nil {
		t.Fatalf("second compile: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("identical source must yield byte-identical PDFs (SOURCE_DATE_EPOCH pinning)")
	}
}

func TestLive_BrokenTeXReportsCompileFailureNotHang(t *testing.T) {
	bin, cache := engineOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Structurally invalid on purpose: an unclosed environment.
	_, err := Compile(ctx, bin, cache, `\documentclass{article}\begin{document}\begin{itemize}`)
	if err == nil {
		t.Fatal("expected a compile failure")
	}
	if !errors.Is(err, ErrCompileFailed) {
		t.Errorf("expected ErrCompileFailed, got %v", err)
	}
}

func TestCompile_NoEngineIsADistinctError(t *testing.T) {
	_, err := Compile(context.Background(), "", "", `\documentclass{article}`)
	if !errors.Is(err, ErrNoEngine) {
		t.Errorf("empty binary must yield ErrNoEngine, got %v", err)
	}
}
