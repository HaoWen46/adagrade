package transcribe

import (
	"strings"
	"testing"
)

// --- Prose escaping -------------------------------------------------------

func TestEscapeProse_EscapesTeXSpecials(t *testing.T) {
	// Exact expectations: a substring check would be wrong here, since a
	// correctly escaped `\_` still *contains* `_`.
	cases := map[string]string{
		`100%`:  `100\%`,
		`{x}`:   `\{x\}`,
		`a & b`: `a \& b`,
		`#1`:    `\#1`,
		`$5`:    `\$5`,
		`a_b`:   `a\_b`,
		`~`:     `\textasciitilde{}`,
		`^`:     `\textasciicircum{}`,
		`\`:     `\textbackslash{}`,
	}
	for in, want := range cases {
		if got := escapeProse(in); got != want {
			t.Errorf("escapeProse(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeProse_LeavesCJKUntouched(t *testing.T) {
	const cjk = "這是一個貪婪演算法，時間複雜度為線性。"
	if got := escapeProse(cjk); got != cjk {
		t.Errorf("CJK must pass through unchanged:\n got %q\nwant %q", got, cjk)
	}
}

// --- Math allow-list ------------------------------------------------------

func TestValidateMath_AllowsOrdinaryMathCommands(t *testing.T) {
	for _, in := range []string{
		`\frac{n}{2}`,
		`\sum_{i=1}^{n} i \leq n^2`,
		`O(n \log n)`,
		`\forall x \in S, \; x \geq 0`,
		`\begin{aligned} a &= b \\ c &= d \end{aligned}`,
		// proof and relation symbols from the 2026-07-30 handwriting pilot
		`x > 1 \therefore x^2 > x`,
		`\because 2 \mid a, \; 2 \mid a^2`,
		`\overset{\text{def}}{=}`,
		`\underset{x \to 0}{\lim} f(x)`,
		`A \subsetneq B \implies A \neq B`,
		`2 \nmid 7 \land \ell_1 \nparallel \ell_2`,
		`a \nleq b \lor a \ngeq b`,
		`\blacksquare`,
	} {
		got, ok := ValidateMath(in)
		if !ok {
			t.Errorf("ValidateMath(%q) rejected a legitimate fragment (offenders: %v)", in, got.Rejected)
		}
	}
}

func TestValidateMath_RejectsMacroDefinitionAndFileIO(t *testing.T) {
	// Each of these is either a hang (Turing-complete expansion) or an escape
	// from the sandbox. None may ever reach the compiler.
	bombs := map[string]string{
		"self-referential def": `\def\x{\x}\x`,
		"newcommand":           `\newcommand{\y}{\y}\y`,
		"csname":               `\csname relax\endcsname`,
		"expandafter":          `\expandafter\x\csname y\endcsname`,
		"input":                `\input{/etc/passwd}`,
		"write18":              `\write18{rm -rf /}`,
		"catcode":              `\catcode` + "`" + `\@=11`,
		"loop":                 `\loop\repeat`,
		"read":                 `\read16 to \x`,
		"output routine":       `\output{\x}`,
	}
	for name, in := range bombs {
		got, ok := ValidateMath(in)
		if ok {
			t.Errorf("%s: ValidateMath(%q) accepted a dangerous fragment", name, in)
		}
		if len(got.Rejected) == 0 {
			t.Errorf("%s: rejection must name the offending command(s)", name)
		}
	}
}

func TestValidateMath_RejectsUnbalancedBraces(t *testing.T) {
	for _, in := range []string{`\frac{a}{b`, `x^{2`, `}`} {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted unbalanced braces", in)
		}
	}
}

func TestValidateMath_RejectsUnknownCommandRatherThanPassingItThrough(t *testing.T) {
	got, ok := ValidateMath(`\usepackage{tikz} x`)
	if ok {
		t.Fatalf("unknown command must not be accepted")
	}
	if len(got.Rejected) == 0 || got.Rejected[0] != `\usepackage` {
		t.Errorf("expected \\usepackage to be named as rejected, got %v", got.Rejected)
	}
}

// --- Demotion, not silent loss -------------------------------------------

func TestRenderInline_DemotesUnsafeMathToLiteralTextAndFlags(t *testing.T) {
	out, flags := renderInline(`the bound $\def\x{\x}\x$ holds`)
	if strings.Contains(out, `\def`) {
		t.Errorf("unsafe command survived into output: %q", out)
	}
	if !strings.Contains(out, "the bound") || !strings.Contains(out, "holds") {
		t.Errorf("surrounding prose must be preserved, got %q", out)
	}
	if len(flags) == 0 {
		t.Error("demotion must be flagged, not silent")
	}
}

func TestRenderInline_KeepsSafeInlineMathAsMath(t *testing.T) {
	out, flags := renderInline(`runs in $O(n \log n)$ time`)
	if !strings.Contains(out, `$O(n \log n)$`) {
		t.Errorf("safe inline math must survive verbatim, got %q", out)
	}
	if len(flags) != 0 {
		t.Errorf("safe math must not raise flags, got %v", flags)
	}
}

func TestRenderInline_UnclosedDollarIsLiteralNotMath(t *testing.T) {
	out, _ := renderInline(`costs $5 to run`)
	if want := `costs \$5 to run`; out != want {
		t.Errorf("a lone $ must be escaped to text: got %q, want %q", out, want)
	}
}

// --- Emitter --------------------------------------------------------------

func testDoc() Doc {
	return Doc{
		Title: "Problem 2",
		Blocks: []Block{
			{Kind: BlockProse, Text: `Use a greedy choice; it runs in $O(n \log n)$.`},
			{Kind: BlockMath, Text: `T(n) = 2T(n/2) + O(n)`},
			{Kind: BlockCode, Text: "for i in 1..n:\n    dp[i] = min(dp[i-1] + 1, dp[i])"},
			{Kind: BlockList, Items: []string{"sort by end time", "scan left to right"}},
		},
	}
}

func TestEmitTeX_IsDeterministic(t *testing.T) {
	d := testDoc()
	a, _ := EmitTeX(d)
	b, _ := EmitTeX(d)
	if a != b {
		t.Error("EmitTeX must be byte-identical for identical input")
	}
}

func TestEmitTeX_ProducesAStandaloneDocument(t *testing.T) {
	out, _ := EmitTeX(testDoc())
	for _, want := range []string{`\documentclass`, `\begin{document}`, `\end{document}`} {
		if !strings.Contains(out, want) {
			t.Errorf("emitted .tex is missing %q", want)
		}
	}
}

func TestEmitTeX_LoadsCJKSupport(t *testing.T) {
	// Mixed zh-Hant + math requires XeLaTeX + xeCJK; without this the document
	// silently drops every Chinese glyph.
	out, _ := EmitTeX(testDoc())
	if !strings.Contains(out, "xeCJK") {
		t.Error("preamble must load xeCJK for Traditional Chinese")
	}
}

func TestEmitTeX_CodeBlockNeverExecutesTeX(t *testing.T) {
	d := Doc{Blocks: []Block{{Kind: BlockCode, Text: `\def\x{\x}\x`}}}
	out, _ := EmitTeX(d)
	// Inside a verbatim environment the backslash is inert, but the sequence
	// must not appear in a position where TeX would expand it.
	if strings.Contains(out, `\begin{verbatim}`) == false {
		t.Error("code blocks must be emitted verbatim")
	}
}

func TestEmitTeX_ReportsFlagsFromDemotedMath(t *testing.T) {
	d := Doc{Blocks: []Block{{Kind: BlockMath, Text: `\input{/etc/passwd}`}}}
	out, flags := EmitTeX(d)
	if strings.Contains(out, `\input`) {
		t.Errorf("unsafe display math leaked into output: %q", out)
	}
	if len(flags) == 0 {
		t.Error("EmitTeX must surface flags so the manifest can record them")
	}
}

func TestEmitTeXWith_LoadsBundledFontByPathNotFamilyName(t *testing.T) {
	// Resolving by family name only works if the face is installed on the host.
	// The app bundles the TTF, so compilation must not depend on that.
	out, _ := EmitTeXWith(testDoc(), Options{CJKFontFile: "/srv/data/fonts/NotoSansTC-Regular.ttf"})
	want := `\setCJKmainfont[Path=/srv/data/fonts/, Extension=.ttf]{NotoSansTC-Regular}`
	if !strings.Contains(out, want) {
		t.Errorf("preamble must load the bundled font by path.\n want: %s\n got:  %s", want, out[:260])
	}
}

func TestEmitBody_HasNoPreambleOrDocumentWrapper(t *testing.T) {
	// _all.tex needs N bodies under ONE preamble. Exposing the body directly
	// means consumers never have to cut a standalone document back apart.
	body, _ := EmitBody(testDoc())
	for _, forbidden := range []string{`\documentclass`, `\begin{document}`, `\end{document}`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("EmitBody must not emit %q", forbidden)
		}
	}
	if !strings.Contains(body, `\section*{Problem 2}`) {
		t.Errorf("EmitBody must still render content, got %q", body)
	}
}

func TestEmitTeXWith_IsPreamblePlusBody(t *testing.T) {
	// The composition must hold exactly, so a consumer assembling
	// Preamble + N bodies produces the same LaTeX the standalone path does.
	d := testDoc()
	opt := Options{CJKFontFile: "/f/NotoSansTC-Regular.ttf"}
	body, bodyFlags := EmitBody(d)
	whole, wholeFlags := EmitTeXWith(d, opt)

	want := Preamble(opt) + "\\begin{document}\n" + body + "\\end{document}\n"
	if whole != want {
		t.Errorf("EmitTeXWith != Preamble + body + wrapper\n got: %q\nwant: %q", whole, want)
	}
	if len(bodyFlags) != len(wholeFlags) {
		t.Errorf("flags must match: body=%v whole=%v", bodyFlags, wholeFlags)
	}
}
