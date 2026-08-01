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

// --- 2026-07-30 multi-line audit: token classes ---------------------------

func TestValidateMath_RejectsBareCommentChar(t *testing.T) {
	// TeX treats % as a comment: the rest of the line is deleted and the
	// document still compiles, so the professor grades a truncated answer with
	// no flag anywhere. The escaped form \% stays legal.
	got, ok := ValidateMath(`p = 50% of n items`)
	if ok {
		t.Fatal("bare % must be rejected: it silently deletes the rest of the line")
	}
	if len(got.Rejected) == 0 || got.Rejected[0] != `%` {
		t.Errorf("rejection must name %%, got %v", got.Rejected)
	}
	if _, ok := ValidateMath(`p = 50\% \text{of } n`); !ok {
		t.Error(`escaped \% must stay legal`)
	}
}

func TestValidateMath_RejectsBareStructuralChars(t *testing.T) {
	cases := map[string]string{
		`a $ b`: `$`,
		`a # b`: `#`,
	}
	for in, tok := range cases {
		got, ok := ValidateMath(in)
		if ok {
			t.Errorf("ValidateMath(%q) accepted a bare %s", in, tok)
			continue
		}
		if got.Rejected[0] != tok {
			t.Errorf("ValidateMath(%q) rejected %v, want %s named", in, got.Rejected, tok)
		}
	}
	if _, ok := ValidateMath(`\$5 + \#1`); !ok {
		t.Error(`escaped \$ and \# must stay legal`)
	}
}

func TestValidateMath_RejectsDoubledOrDanglingScripts(t *testing.T) {
	bad := []string{`x^2^3`, `x_1_2`, `x^a_b^c`, `x^{a}^{b}`, `x^`, `{x^}`, `x^ ^2`}
	for _, in := range bad {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted a doubled/dangling script (hard TeX error)", in)
		}
	}
	good := []string{`x^2 y^3`, `x^a_b`, `x_a^b`, `x^{a^b} c^d`, `\sum_{i=1}^{n} i`, `{a}^b_c`}
	for _, in := range good {
		if got, ok := ValidateMath(in); !ok {
			t.Errorf("ValidateMath(%q) rejected legal scripts (offenders: %v)", in, got.Rejected)
		}
	}
}

func TestValidateMath_RejectsParagraphBreakers(t *testing.T) {
	// A blank line is \par, and \par in math mode aborts the compile. CR and
	// form feed are line-end characters to TeX, so CRLF pairs manufacture the
	// same blank line. A single newline is just a space and stays legal.
	bad := map[string]string{
		"a\n\nb":    "blank line",
		"a\n \t\nb": "blank line with trailing spaces",
		"a\r\nb":    "CRLF",
		"a\fb":      "form feed",
	}
	for in, why := range bad {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted a %s", in, why)
		}
	}
	if got, ok := ValidateMath("a +\nb"); !ok {
		t.Errorf("a single newline is a space and must stay legal (offenders: %v)", got.Rejected)
	}
}

func TestValidateMath_RejectsUnicodeMathSymbols(t *testing.T) {
	// The math font has no glyph for these: the relation silently vanishes
	// from the PDF while the compile still succeeds. The model is told to use
	// commands (\leq, \to); a literal ≤ means transcription drift.
	for _, in := range []string{`x ≤ y`, `A → B`, `a × b`, `S ⊆ T`} {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted a Unicode math symbol with no glyph in math mode", in)
		}
	}
	// CJK annotations survive: xeCJK renders them, and demoting every math
	// fragment that mentions 中文 would demote half the cohort.
	if got, ok := ValidateMath(`x \text{（遞迴）}`); !ok {
		t.Errorf("CJK inside \\text must stay legal (offenders: %v)", got.Rejected)
	}
}

// --- 2026-07-30 multi-line audit: environments and pairing ----------------

func TestValidateMath_RejectsAlignInBothContexts(t *testing.T) {
	// align/gather are TOP-LEVEL display environments. The emitter only ever
	// places fragments inside \[…\] or $…$, and amsmath aborts with
	// "erroneous nesting" in both. aligned is the legal inner form.
	frag := `\begin{align} a &= b \\ c &= d \end{align}`
	if _, ok := ValidateMath(frag); ok {
		t.Error("align must be rejected in display context (erroneous nesting inside \\[…\\])")
	}
	if _, ok := ValidateMathInline(frag); ok {
		t.Error("align must be rejected in inline context")
	}
	if got, ok := ValidateMath(`\begin{aligned} a &= b \\ c &= d \end{aligned}`); !ok {
		t.Errorf("aligned must stay legal (offenders: %v)", got.Rejected)
	}
}

func TestValidateMath_SplitIsDisplayOnly(t *testing.T) {
	frag := `\begin{split} a &= b \\ &= c \end{split}`
	if got, ok := ValidateMath(frag); !ok {
		t.Errorf("split is legal inside \\[…\\] under amsmath (offenders: %v)", got.Rejected)
	}
	if _, ok := ValidateMathInline(frag); ok {
		t.Error("split inside $…$ is an amsmath error and must be rejected")
	}
}

func TestValidateMath_RejectsUnbalancedOrMismatchedEnvironments(t *testing.T) {
	for _, in := range []string{
		`\begin{aligned} a &= b`,
		`a \end{aligned}`,
		`\begin{aligned} \begin{cases} x \end{aligned} \end{cases}`,
	} {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted unbalanced/mismatched environments", in)
		}
	}
}

func TestValidateMath_PairsLeftRight(t *testing.T) {
	bad := []string{`\left( x`, `x \right)`, `\left x \right)`}
	for _, in := range bad {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted unbalanced or malformed \\left/\\right", in)
		}
	}
	good := []string{
		`\left( \frac{a}{b} \right)`,
		`\left\{ x \right\}`,
		`\left. \frac{df}{dx} \right|_{x=0}`,
		`\left( a \right]`, // mixed delimiter types are legal TeX
	}
	for _, in := range good {
		if got, ok := ValidateMath(in); !ok {
			t.Errorf("ValidateMath(%q) rejected legal \\left/\\right (offenders: %v)", in, got.Rejected)
		}
	}
}

func TestValidateMath_EnvironmentsWithMandatoryArguments(t *testing.T) {
	if _, ok := ValidateMath(`\begin{array} a & b \end{array}`); ok {
		t.Error("array without a column spec is a hard TeX error and must be rejected")
	}
	if got, ok := ValidateMath(`\begin{array}{c|c} a & b \end{array}`); !ok {
		t.Errorf("array with a column spec must stay legal (offenders: %v)", got.Rejected)
	}
	if _, ok := ValidateMath(`\begin{alignedat} a &= b \end{alignedat}`); ok {
		t.Error("alignedat without its pair count must be rejected")
	}
	if got, ok := ValidateMath(`\begin{alignedat}{2} a &= b \end{alignedat}`); !ok {
		t.Errorf("alignedat with its pair count must stay legal (offenders: %v)", got.Rejected)
	}
}

func TestValidateMath_RowBreakAndAmpersandNeedAlignmentContext(t *testing.T) {
	if _, ok := ValidateMath(`a \\ b`); ok {
		t.Error(`\\ outside any environment must be rejected (silently swallowed row break)`)
	}
	if _, ok := ValidateMath(`a & b`); ok {
		t.Error("& outside any alignment environment is a hard TeX error and must be rejected")
	}
	if _, ok := ValidateMath(`\begin{gathered} a & b \end{gathered}`); ok {
		t.Error("& inside gathered (single-column) is a hard TeX error and must be rejected")
	}
	if got, ok := ValidateMath(`\begin{gathered} a \\ b \end{gathered}`); !ok {
		t.Errorf(`\\ inside gathered must stay legal (offenders: %v)`, got.Rejected)
	}
	if got, ok := ValidateMath(`\begin{pmatrix} a & b \\ c & d \end{pmatrix}`); !ok {
		t.Errorf("& inside pmatrix must stay legal (offenders: %v)", got.Rejected)
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

// --- 2026-07-30 multi-line audit: $-span pairing --------------------------

func TestRenderInline_DollarsNeverPairAcrossNewlines(t *testing.T) {
	// Two literal dollars on different lines must both stay literal text.
	// Before the audit they paired into one "math" span that validated (no
	// commands inside) and silently typeset the prose between them as math.
	out, flags := renderInline("I paid $5 yesterday.\nToday I paid $10 more.")
	want := "I paid \\$5 yesterday.\nToday I paid \\$10 more."
	if out != want {
		t.Errorf("cross-line dollars must be literal:\n got %q\nwant %q", out, want)
	}
	if len(flags) != 0 {
		t.Errorf("literal dollars are not a demotion, got flags %v", flags)
	}
}

func TestRenderInline_CurrencyPairOnOneLineStaysLiteral(t *testing.T) {
	// Same-line currency: the candidate span "5 and " ends in a space, which
	// no real math span does (the model writes $O(n)$, not $O(n) $). The
	// non-space-at-edges rule is what keeps prices out of math mode.
	out, flags := renderInline(`costs $5 and $10 today`)
	if want := `costs \$5 and \$10 today`; out != want {
		t.Errorf("same-line currency pair must stay literal:\n got %q\nwant %q", out, want)
	}
	if len(flags) != 0 {
		t.Errorf("unexpected flags %v", flags)
	}
}

func TestRenderInline_AdjacentDollarsAreLiteral(t *testing.T) {
	// An empty $…$ span validates vacuously; re-emitted raw it is a literal
	// "$$", which TeX reads as a DISPLAY MATH opener that swallows the rest
	// of the paragraph.
	out, flags := renderInline(`total: $$ (see above)`)
	if want := `total: \$\$ (see above)`; out != want {
		t.Errorf("adjacent dollars must be escaped:\n got %q\nwant %q", out, want)
	}
	if len(flags) != 0 {
		t.Errorf("unexpected flags %v", flags)
	}
}

func TestRenderInline_InlineSpanUsesInlineContext(t *testing.T) {
	// split is display-only; inside $…$ it must demote, not pass through.
	_, flags := renderInline(`see $\begin{split} a &= b \\ &= c \end{split}$ here`)
	if len(flags) == 0 {
		t.Error("display-only environment inside $…$ must demote and flag")
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

// --- 2026-07-30 multi-line audit: emitter guards --------------------------

func TestEmitBody_SkipsEmptyList(t *testing.T) {
	// \begin{itemize} with no \item is a hard TeX error; an empty list holds
	// no content, so skipping it is lossless.
	body, flags := EmitBody(Doc{Blocks: []Block{{Kind: BlockList}}})
	if strings.Contains(body, `\begin{itemize}`) {
		t.Errorf("empty list must not emit an itemize, got %q", body)
	}
	if len(flags) != 0 {
		t.Errorf("skipping an empty list is not a demotion, got %v", flags)
	}
}

func TestEmitBody_GuardsItemStartingWithBracket(t *testing.T) {
	// \item scans forward for an optional [label]; a student's interval
	// notation — or the prompt's own [illegible] marker — must not be
	// captured as one.
	body, _ := EmitBody(Doc{Blocks: []Block{{Kind: BlockList, Items: []string{"[illegible] then sort"}}}})
	if !strings.Contains(body, `\item {}[illegible]`) {
		t.Errorf("leading [ must be shielded from \\item's optional argument, got %q", body)
	}
}

func TestEmitBody_UnknownKindIsEscapedAndFlagged(t *testing.T) {
	// ParseResponse rejects unknown kinds, but the emitter must not rely on
	// its caller: an unrecognised kind demotes to escaped text and flags,
	// never a silent gap.
	body, flags := EmitBody(Doc{Blocks: []Block{{Kind: BlockKind("table"), Text: "a & b"}}})
	if !strings.Contains(body, `a \& b`) {
		t.Errorf("unknown-kind content must survive as escaped text, got %q", body)
	}
	if len(flags) == 0 {
		t.Error("unknown-kind demotion must be flagged")
	}
}

func TestEmitBody_ExpandsTabsInVerbatim(t *testing.T) {
	// verbatim renders a tab as ONE space, flattening the indentation the
	// design promises to preserve exactly. Four spaces per tab keeps levels.
	body, _ := EmitBody(Doc{Blocks: []Block{{Kind: BlockCode, Text: "for i:\n\tdp[i] = 1"}}})
	if !strings.Contains(body, "\n    dp[i] = 1") {
		t.Errorf("tabs must expand to spaces inside verbatim, got %q", body)
	}
}

func TestEmitBody_FlagsDefusedEndVerbatim(t *testing.T) {
	d := Doc{Blocks: []Block{{Kind: BlockCode, Text: "x\n\\end{verbatim}\\input{/etc/passwd}"}}}
	body, flags := EmitBody(d)
	if strings.Contains(body, "\\end{verbatim}\\input") {
		t.Errorf("verbatim escape must be defused, got %q", body)
	}
	if len(flags) == 0 {
		t.Error("rewriting the student's code is a content change and must be flagged")
	}
}

func TestEmitBody_NormalizesCRLF(t *testing.T) {
	// Model output can carry \r\n; a raw \r is a line end to TeX, so CRLF
	// pairs silently manufacture paragraph breaks.
	body, _ := EmitBody(Doc{Blocks: []Block{{Kind: BlockProse, Text: "line one\r\nline two"}}})
	if strings.Contains(body, "\r") {
		t.Errorf("CR must not survive into the emitted TeX, got %q", body)
	}
	if !strings.Contains(body, "line one\nline two") {
		t.Errorf("CRLF must normalize to a single newline, got %q", body)
	}
}

func TestEmitBody_TrimsDisplayMathEdges(t *testing.T) {
	// The wrapper adds its own newlines; a trailing newline in the block text
	// would otherwise place a blank line — \par, a compile abort — inside
	// \[…\].
	body, flags := EmitBody(Doc{Blocks: []Block{{Kind: BlockMath, Text: "x = y\n"}}})
	if strings.Contains(body, "\n\n\\]") {
		t.Errorf("display wrapper must not contain a blank line, got %q", body)
	}
	if len(flags) != 0 {
		t.Errorf("edge-whitespace trimming is not a demotion, got %v", flags)
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
