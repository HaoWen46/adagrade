package transcribe

import (
	"strings"
	"testing"
)

// Round 2 of the 2026-07-30 hardening: gaps the adversarial re-audit proved
// against the first round. Every case here was demonstrated on the round-1
// validator before being fixed.

// --- unified group stack ---------------------------------------------------

func TestValidateMath_RejectsInterleavedGroups(t *testing.T) {
	// TeX has ONE group stack: braces, environments, and \left/\right must
	// nest. Round 1 tracked them as three independent counters, so balanced-
	// but-interleaved fragments passed and hard-errored at compile time.
	bad := []string{
		`{\left( x } \right)`,
		`\frac{\left( a+b}{c \right)}`,
		`\begin{aligned} \left( a \\ b \right) \end{aligned}`, // \left spans a row break
		`\begin{pmatrix} \left( a & b \right) \end{pmatrix}`,  // \left spans a cell
		`{\begin{aligned}} a \end{aligned}`,
		`\begin{aligned} \left( a \end{aligned} \right)`,
	}
	for _, in := range bad {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted interleaved groups (hard TeX error)", in)
		}
	}
	good := []string{
		`\left( \begin{aligned} a \\ b \end{aligned} \right)`,
		`\left\{ \begin{array}{ll} 1 & x \\ 0 & y \end{array} \right.`,
	}
	for _, in := range good {
		if got, ok := ValidateMath(in); !ok {
			t.Errorf("ValidateMath(%q) rejected properly nested groups (offenders: %v)", in, got.Rejected)
		}
	}
}

func TestValidateMath_AmpersandAndRowBreakRespectBraceScope(t *testing.T) {
	// TeX recognises & and \\ only at the alignment's own brace level; inside
	// a group or \text{} they are hard errors even within an environment.
	bad := []string{
		`\begin{cases} {a & b} \end{cases}`,
		`\begin{cases} \text{profit & loss} & 1 \end{cases}`,
		`\begin{aligned} \frac{a \\ b}{c} \end{aligned}`,
	}
	for _, in := range bad {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted & or \\\\ inside a brace group", in)
		}
	}
	if got, ok := ValidateMath(`\begin{cases} 1 & \text{if } x \\ 0 & \text{else} \end{cases}`); !ok {
		t.Errorf("idiomatic cases must stay legal (offenders: %v)", got.Rejected)
	}
}

// --- script operands -------------------------------------------------------

func TestValidateMath_ScriptOperandMustBeGlyphLike(t *testing.T) {
	// TeX's scan_math accepts only character-producing material as a bare
	// math field; \left, spacing, \mathop/\mathbin/\mathaccent/\radical
	// commands all hard-error after ^ or _.
	bad := []string{
		`x^\left(y\right)`,
		`x^\quad y`,
		`2^ \cdot 3`,
		`x^\sqrt{2}`,
		`x^\log n`,
		`x^\hat{a}`,
		`x^\,y`,
		`\begin{aligned} y &= x^ \end{aligned}`, // dangling into \end
	}
	for _, in := range bad {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted a non-glyph script operand (hard TeX error)", in)
		}
	}
	good := []string{`x^\alpha`, `x^\frac{1}{2}`, `x^\text{cm}`, `x^\mathbb{R}`, `x^2`, `x^{\sqrt{2}}`}
	for _, in := range good {
		if got, ok := ValidateMath(in); !ok {
			t.Errorf("ValidateMath(%q) rejected a legal script operand (offenders: %v)", in, got.Rejected)
		}
	}
}

func TestValidateMath_CommandArgumentBracesDoNotResetScriptState(t *testing.T) {
	// x^\frac{1}{2}^3 is TeX's "Double superscript": the \frac fills the sup
	// field, and its argument braces must not erase that record.
	bad := []string{`x^\frac{1}{2}^3`, `x^\mathbb{R}^2`, `x^\text{a}^\text{b}`}
	for _, in := range bad {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted a double script hidden behind a command argument", in)
		}
	}
	good := []string{`\alpha{a}^b`, `x^{a}\text{b}^{c}`, `x^\frac{1}{2} + y^2`}
	for _, in := range good {
		if got, ok := ValidateMath(in); !ok {
			t.Errorf("ValidateMath(%q) rejected legal scripts (offenders: %v)", in, got.Rejected)
		}
	}
}

// --- empties ---------------------------------------------------------------

func TestValidateMath_RejectsEmptyFragment(t *testing.T) {
	// An empty fragment validates vacuously, and the display wrapper then
	// emits \[ <blank line> \] — \par in math mode, a compile abort.
	for _, in := range []string{``, `  `, "\n", " \t\n "} {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) must reject empty/whitespace-only input", in)
		}
	}
}

func TestEmitBody_SkipsWhitespaceOnlyMathBlock(t *testing.T) {
	// Nothing to lose: skipping mirrors the empty-list precedent. Emitting
	// would place a blank line inside \[…\].
	for _, text := range []string{" ", "\n", "\t", " \r\n "} {
		body, flags := EmitBody(Doc{Blocks: []Block{{Kind: BlockMath, Text: text}}})
		if strings.Contains(body, `\[`) {
			t.Errorf("whitespace-only math (%q) must not emit a display wrapper, got %q", text, body)
		}
		if len(flags) != 0 {
			t.Errorf("skipping empty math is lossless, got flags %v", flags)
		}
	}
}

// --- renderInline round 2 --------------------------------------------------

func TestRenderInline_DemotionKeepsDollarDelimiters(t *testing.T) {
	// The student wrote both $ characters; demotion must escape them, not
	// delete them ("demote, never drop").
	out, flags := renderInline(`the bound $\def\x{\x}\x$ holds`)
	if strings.Count(out, `\$`) != 2 {
		t.Errorf("demotion must keep both $ as escaped literals, got %q", out)
	}
	if strings.Contains(out, `\def`) || len(flags) == 0 {
		t.Errorf("demotion semantics regressed: out=%q flags=%v", out, flags)
	}
}

func TestRenderInline_CurrencyCannotCascadeIntoRealMath(t *testing.T) {
	// A money $ must not scan past later openers and steal a real span's
	// closer. The first candidate $ decides: pair or go literal.
	out, _ := renderInline(`prices $5 and $10, plus $O(n)$ work`)
	if !strings.Contains(out, `$O(n)$`) {
		t.Errorf("the real math span must survive, got %q", out)
	}
	if !strings.Contains(out, `\$5`) || !strings.Contains(out, `\$10`) {
		t.Errorf("currency dollars must stay literal, got %q", out)
	}
}

func TestRenderInline_EscapedDollarIsNotACloser(t *testing.T) {
	out, flags := renderInline(`we get $c = \$5 + x$ total`)
	if !strings.Contains(out, `$c = \$5 + x$`) {
		t.Errorf("\\$ inside a span is content, not a closer: got %q (flags %v)", out, flags)
	}
}

func TestRenderInline_FlagsMathLikeUnpairedSpan(t *testing.T) {
	// `$ \alpha $` never pairs (padded edges); the output is escaped TeX
	// source in the PDF, which must reach the manifest, not pass silently.
	_, flags := renderInline(`let $ \alpha $ be small`)
	if len(flags) == 0 {
		t.Error("an unpaired span containing TeX commands must be flagged")
	}
	_, flags = renderInline(`costs $5 and $10 today`)
	if len(flags) != 0 {
		t.Errorf("plain currency must stay unflagged, got %v", flags)
	}
}

// --- both-fields blocks ----------------------------------------------------

func TestEmitBody_ListLeadInTextIsNotDropped(t *testing.T) {
	body, _ := EmitBody(Doc{Blocks: []Block{{Kind: BlockList, Text: "two reasons:", Items: []string{"a", "b"}}}})
	if !strings.Contains(body, "two reasons:") || !strings.Contains(body, `\item a`) {
		t.Errorf("a list's lead-in text and its items must both survive, got %q", body)
	}
}

func TestEmitBody_ProseItemsAreNotDropped(t *testing.T) {
	body, _ := EmitBody(Doc{Blocks: []Block{{Kind: BlockProse, Text: "intro", Items: []string{"stray item"}}}})
	if !strings.Contains(body, "intro") || !strings.Contains(body, "stray item") {
		t.Errorf("content in both fields must both survive, got %q", body)
	}
}

// --- Unicode as allow-list -------------------------------------------------

func TestValidateMath_RejectsNonRenderableUnicode(t *testing.T) {
	// The math font is 8-bit: any unmapped rune is a silent missing-glyph
	// drop with a clean compile. Only ASCII and the CJK ranges xeCJK renders
	// may pass; everything else demotes.
	for _, in := range []string{`α + β`, `f′(x)`, `x² + y²`, `ℝ`, "a b", `a … b`} {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted a rune the math font cannot render", in)
		}
	}
	for _, in := range []string{`x \text{（遞迴）}`, `\text{結果為 0}`} {
		if got, ok := ValidateMath(in); !ok {
			t.Errorf("CJK must stay legal (offenders: %v) for %q", got.Rejected, in)
		}
	}
}

// --- allow-list round 2 ----------------------------------------------------

func TestValidateMath_AllowsCommonTypesettingRound2(t *testing.T) {
	// Every one of these demoted in round 1 despite typesetting fine; several
	// (substack rows, array tables) are forms the allow-list already declares
	// intent to support.
	for _, in := range []string{
		`\lim\limits_{n \to \infty} a_n`,
		`\sum\limits_{i=1}^{n} i`,
		`\bigl( x + y \bigr)^2`,
		`\displaystyle \sum_{i=1}^{n} \frac{1}{i}`,
		`\begin{array}{|c|c|} \hline 1 & 2 \\ \hline 3 & 4 \\ \hline \end{array}`,
		`\sum_{\substack{i=1 \\ i \neq j}} a_i`,
		`a \ b`,
		`\begin{array}{ c c } a & b \end{array}`,
		`\begin{array}[t]{cc} a & b \end{array}`,
	} {
		if got, ok := ValidateMath(in); !ok {
			t.Errorf("ValidateMath(%q) rejected a legitimate form (offenders: %v)", in, got.Rejected)
		}
	}
}

func TestValidateMath_SpacingCommandArguments(t *testing.T) {
	// \vspace is illegal in math mode with ANY argument (\vadjust path);
	// \hspace is legal but its argument must be a dimension or TeX errors.
	bad := []string{`a \vspace{1cm} b`, `a \hspace{} b`, `a \hspace{x} b`, `a \hspace{3} b`}
	for _, in := range bad {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted a spacing command that cannot compile", in)
		}
	}
	good := []string{`a \hspace{1cm} b`, `a \hspace{0.5em} b`}
	for _, in := range good {
		if got, ok := ValidateMath(in); !ok {
			t.Errorf("ValidateMath(%q) rejected a well-formed \\hspace (offenders: %v)", in, got.Rejected)
		}
	}
}

func TestValidateMath_BigFamilyNeedsADelimiter(t *testing.T) {
	for _, in := range []string{`\big x`, `a \Big`, `\bigg + b`} {
		if _, ok := ValidateMath(in); ok {
			t.Errorf("ValidateMath(%q) accepted \\big with no delimiter (TeX: Missing delimiter)", in)
		}
	}
}

func TestValidateMath_ColspecMustDeclareAColumn(t *testing.T) {
	if _, ok := ValidateMath(`\begin{array}{|} a \end{array}`); ok {
		t.Error("an all-rules colspec declares zero columns (Missing # in alignment preamble)")
	}
	if _, ok := ValidateMath(`\begin{alignedat}{0} a \end{alignedat}`); ok {
		t.Error("alignedat{0} allows zero pairs and errors on any content")
	}
}

func TestValidateMath_AngleBracketsAreNotLeftRightDelimiters(t *testing.T) {
	// LaTeX declares < and > as relations, not delimiters; \left< raises
	// "Missing delimiter". \langle/\rangle are the legal spellings.
	if _, ok := ValidateMath(`\left< x \right>`); ok {
		t.Error(`\left< must be rejected; \langle is the delimiter form`)
	}
	if got, ok := ValidateMath(`\left\langle x \right\rangle`); !ok {
		t.Errorf(`\left\langle must stay legal (offenders: %v)`, got.Rejected)
	}
}

// --- title armor -----------------------------------------------------------

func TestEmitBody_TitleNewlinesCannotBreakTheSectionLine(t *testing.T) {
	// Title is app-controlled, but the emitter must not rely on its caller:
	// a raw newline or CR inside \section*{…} is a runaway-argument error.
	body, _ := EmitBody(Doc{Title: "Problem\r\n2", Blocks: []Block{{Kind: BlockProse, Text: "x"}}})
	line, _, _ := strings.Cut(body, "\n")
	if !strings.Contains(line, "Problem") || !strings.Contains(line, "2") {
		t.Errorf("title content must survive on one line, got %q", line)
	}
	if strings.Contains(body, "\r") {
		t.Errorf("CR must not survive into the emitted TeX, got %q", body)
	}
}
