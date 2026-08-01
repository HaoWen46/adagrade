package transcribe

import (
	"reflect"
	"strings"
	"testing"
)

// The Typst mirror (spec 2026-07-30): same validator verdicts, second emitter.

// parityCorpus exercises every EmitBody branch and every demotion class, so
// the flag-parity pin below covers the paths a real transcription hits.
func parityCorpus() []Doc {
	return []Doc{
		{Title: "Problem 1", Blocks: []Block{
			{Kind: BlockProse, Text: `Greedy works; it runs in $O(n \log n)$ time.`},
			{Kind: BlockMath, Text: `T(n) = 2T(n/2) + O(n)`},
		}},
		{Blocks: []Block{
			{Kind: BlockProse, Text: `the bound $\def\x{\x}\x$ holds`}, // demoted inline
			{Kind: BlockMath, Text: `\input{/etc/passwd}`},             // demoted display
			{Kind: BlockProse, Text: "costs $5 and $10 today"},         // literal currency
			{Kind: BlockProse, Text: `let $ \alpha $ be small`},        // unpaired math-like
			{Kind: BlockMath, Text: " \n"},                             // empty math: skipped
			{Kind: BlockList, Text: "two reasons:", Items: []string{"[illegible] then sort", "scan"}},
			{Kind: BlockCode, Text: "for i:\n\tdp[i] = 1"},
			{Kind: BlockCode, Text: "x\n\\end{verbatim}\\input{/etc/passwd}"}, // defused + flagged
			{Kind: BlockKind("table"), Text: "a & b"},                         // unknown kind
			{Kind: BlockProse, Text: "intro", Items: []string{"stray item"}},  // both fields
		}},
		{Blocks: []Block{
			{Kind: BlockProse, Text: "第一步：排序，時間 $O(n \\log n)$。"},
			{Kind: BlockMath, Text: `\begin{aligned} a &= b \\ c &= d \end{aligned}`},
		}},
	}
}

func TestEmitTypstBody_FlagParityWithLaTeX(t *testing.T) {
	// One manifest flags column serves both formats, which is only honest if
	// both emitters reach identical verdicts on identical input.
	for i, d := range parityCorpus() {
		_, latexFlags := EmitBody(d)
		_, typstFlags := EmitTypstBody(d)
		if !reflect.DeepEqual(latexFlags, typstFlags) {
			t.Errorf("doc %d: flags diverged\n latex: %v\n typst: %v", i, latexFlags, typstFlags)
		}
	}
}

// stripTypstStrings removes the CONTENTS of every double-quoted Typst string
// literal (respecting \" and \\ escapes), leaving only structural markup.
func stripTypstStrings(s string) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\\' && i+1 < len(s) {
				i++ // escaped char stays inside the literal
				continue
			}
			if c == '"' {
				inStr = false
				b.WriteByte(c)
			}
			continue
		}
		if c == '"' {
			inStr = true
		}
		b.WriteByte(c)
	}
	return b.String()
}

func TestEmitTypstBody_StudentTextNeverInMarkupPosition(t *testing.T) {
	// The typst-report invariant: hostile bytes must be inert. Every marker
	// below must survive INTO the output (never dropped) but only inside
	// string literals — the structural remainder must not contain them.
	hostiles := []string{
		"run `rm -rf`",
		`#import "@preview/evil:1.0.0": *`,
		`#eval("1+1")`,
		`*bold* _italic_ = heading`,
		`quote " and backslash \ mix`,
		`] escape the content block`,
		`<label> @ref $math$`,
	}
	for _, h := range hostiles {
		body, _ := EmitTypstBody(Doc{Blocks: []Block{{Kind: BlockProse, Text: h}}})
		structural := stripTypstStrings(body)
		for _, marker := range []string{"#import", "#eval", "`", "*", "= heading", "@ref"} {
			if strings.Contains(h, marker) && strings.Contains(structural, marker) {
				t.Errorf("hostile %q leaked marker %q into markup position:\n%s", h, marker, body)
			}
		}
	}
	// Code blocks too: backticks in pseudocode must not open a raw fence.
	body, _ := EmitTypstBody(Doc{Blocks: []Block{{Kind: BlockCode, Text: "s = `cmd` ```fence```"}}})
	if strings.Contains(stripTypstStrings(body), "`") {
		t.Errorf("code backticks leaked into markup position:\n%s", body)
	}
}

func TestEmitTypst_IsDeterministic(t *testing.T) {
	for _, d := range parityCorpus() {
		a, af := EmitTypst(d)
		b, bf := EmitTypst(d)
		if a != b || !reflect.DeepEqual(af, bf) {
			t.Fatal("EmitTypst must be byte- and flag-identical for identical input")
		}
	}
}

func TestEmitTypstBody_MathReusesValidatedLaTeXViaMitex(t *testing.T) {
	body, flags := EmitTypstBody(Doc{Blocks: []Block{
		{Kind: BlockProse, Text: `runs in $O(n \log n)$ time`},
		{Kind: BlockMath, Text: `\frac{n}{2} \leq k`},
	}})
	if !strings.Contains(body, `#mi("O(n \\log n)")`) {
		t.Errorf("accepted inline math must pass to mitex verbatim (escaped), got %q", body)
	}
	if !strings.Contains(body, `#mitex("\\frac{n}{2} \\leq k")`) {
		t.Errorf("accepted display math must pass to mitex verbatim (escaped), got %q", body)
	}
	if len(flags) != 0 {
		t.Errorf("accepted math must not flag, got %v", flags)
	}
}

func TestTypstPreamble_ImportsMitexAndCJKFont(t *testing.T) {
	p := TypstPreamble()
	if !strings.Contains(p, `#import "@preview/mitex:`+transcribeMitexVersion+`": mi, mitex`) {
		t.Errorf("preamble must import mitex, got %q", p)
	}
	if !strings.Contains(p, "Noto Sans TC") {
		t.Error("preamble must set a Traditional Chinese font stack")
	}
	whole, _ := EmitTypst(Doc{Blocks: []Block{{Kind: BlockProse, Text: "x"}}})
	if !strings.HasPrefix(whole, p) {
		t.Error("EmitTypst must compose as TypstPreamble + body")
	}
}
