package transcribe

import (
	"fmt"
	"sort"
	"strings"
)

// SECURITY INVARIANT (latex-transcription-export spec §2.1)
//
// Transcriptions are model output derived from student handwriting, so hostile
// content must be assumed. internal/report/typstmarkup.go can neutralise such
// text by wrapping it in Typst's literal-string token; LaTeX has no equivalent
// primitive — math mode exists precisely to let commands through, and TeX is
// Turing-complete, so `\def\x{\x}\x` is a two-token hang rather than a compile
// error (the same failure class D70 had to kill with a subprocess timeout).
//
// Therefore the math path is an ALLOW-LIST of command names, never an escape
// function. A fragment containing anything not on the list is demoted to
// literal escaped text and flagged; it is never dropped and never passed
// through. This makes injection defense and hang defense the same mechanism:
// a macro bomb cannot be built from an alphabet with no macro-definition
// primitive.
//
// Prose takes the other path: total escaping of every TeX special.
//
// Keep every emission path inside escapeProse / ValidateMath so the invariant
// stays auditable.

// allowedCommands is the math-mode allow-list, without the leading backslash.
// Adding an entry is a security decision: it must be a typesetting command
// that cannot define a macro, read or write a file, alter catcodes, or loop.
var allowedCommands = map[string]bool{
	// structure
	"begin": true, "end": true, "left": true, "right": true,
	"big": true, "Big": true, "bigg": true, "Bigg": true,
	"bigl": true, "bigr": true, "Bigl": true, "Bigr": true,
	"biggl": true, "biggr": true, "Biggl": true, "Biggr": true,
	"quad": true, "qquad": true, "hline": true,
	"limits": true, "nolimits": true,
	"displaystyle": true, "textstyle": true, "scriptstyle": true, "scriptscriptstyle": true,

	// fractions, roots, operators with limits
	"frac": true, "dfrac": true, "tfrac": true, "sqrt": true, "binom": true,
	"sum": true, "prod": true, "int": true, "oint": true, "coprod": true,
	"lim": true, "limsup": true, "liminf": true, "inf": true, "sup": true,
	"max": true, "min": true, "arg": true, "det": true, "dim": true,
	"ker": true, "deg": true, "gcd": true, "hom": true,

	// named functions
	"log": true, "ln": true, "lg": true, "exp": true,
	"sin": true, "cos": true, "tan": true, "cot": true, "sec": true, "csc": true,
	"arcsin": true, "arccos": true, "arctan": true,
	"sinh": true, "cosh": true, "tanh": true, "coth": true,
	"bmod": true, "pmod": true, "mod": true, "operatorname": true,

	// relations
	"leq": true, "geq": true, "le": true, "ge": true, "neq": true, "ne": true,
	"approx": true, "equiv": true, "sim": true, "simeq": true, "cong": true,
	"propto": true, "ll": true, "gg": true, "asymp": true, "doteq": true,
	"subset": true, "supset": true, "subseteq": true, "supseteq": true,
	"sqsubseteq": true, "in": true, "notin": true, "ni": true, "mid": true,
	"parallel": true, "perp": true, "models": true, "vdash": true,
	// amssymb proof/relation symbols (2026-07-30 handwriting pilot): all are
	// zero-argument glyphs — no macro, I/O, catcode, or loop capability.
	"therefore": true, "because": true, "nmid": true, "nparallel": true,
	"nleq": true, "ngeq": true, "subsetneq": true, "supsetneq": true,

	// logic and sets
	"forall": true, "exists": true, "nexists": true, "neg": true, "lnot": true,
	"land": true, "lor": true, "wedge": true, "vee": true, "cup": true,
	"cap": true, "bigcup": true, "bigcap": true, "setminus": true,
	"emptyset": true, "varnothing": true, "complement": true,

	// arrows
	"to": true, "gets": true, "mapsto": true, "implies": true, "iff": true,
	"rightarrow": true, "leftarrow": true, "leftrightarrow": true,
	"Rightarrow": true, "Leftarrow": true, "Leftrightarrow": true,
	"longrightarrow": true, "longleftarrow": true, "uparrow": true, "downarrow": true,
	"Uparrow": true, "Downarrow": true,

	// binary operators
	"cdot": true, "times": true, "div": true, "pm": true, "mp": true,
	"ast": true, "star": true, "circ": true, "bullet": true,
	"oplus": true, "ominus": true, "otimes": true, "odot": true,

	// dots and spacing. \vspace is deliberately absent: it is illegal in math
	// mode with ANY argument (\vadjust path), so no argument check can save it.
	"cdots": true, "ldots": true, "vdots": true, "ddots": true, "dots": true,
	"hspace": true, "phantom": true,

	// delimiters
	"langle": true, "rangle": true, "lceil": true, "rceil": true,
	"lfloor": true, "rfloor": true, "lvert": true, "rvert": true,
	"lVert": true, "rVert": true, "vert": true, "Vert": true, "backslash": true,

	// accents and decorations
	"overline": true, "underline": true, "hat": true, "widehat": true,
	"bar": true, "vec": true, "tilde": true, "widetilde": true,
	"dot": true, "ddot": true, "overbrace": true, "underbrace": true,
	"overrightarrow": true, "stackrel": true, "substack": true,
	// amsmath stacking (2026-07-30 handwriting pilot): same class as stackrel —
	// they typeset their two arguments and nothing else.
	"overset": true, "underset": true,

	// fonts (safe: they take an argument and typeset it)
	"mathbb": true, "mathcal": true, "mathrm": true, "mathbf": true,
	"mathit": true, "mathsf": true, "mathtt": true, "mathfrak": true,
	"boldsymbol": true, "text": true, "textrm": true, "textbf": true,
	"textit": true, "textsf": true, "texttt": true, "mbox": true,

	// misc symbols
	"infty": true, "partial": true, "nabla": true, "prime": true,
	"angle": true, "triangle": true, "square": true, "aleph": true,
	"Re": true, "Im": true, "wp": true, "ell": true, "hbar": true,
	"blacksquare": true, // amssymb QED tombstone; zero-argument glyph

	// greek
	"alpha": true, "beta": true, "gamma": true, "delta": true, "epsilon": true,
	"varepsilon": true, "zeta": true, "eta": true, "theta": true, "vartheta": true,
	"iota": true, "kappa": true, "lambda": true, "mu": true, "nu": true,
	"xi": true, "pi": true, "varpi": true, "rho": true, "varrho": true,
	"sigma": true, "varsigma": true, "tau": true, "upsilon": true, "phi": true,
	"varphi": true, "chi": true, "psi": true, "omega": true,
	"Gamma": true, "Delta": true, "Theta": true, "Lambda": true, "Xi": true,
	"Pi": true, "Sigma": true, "Upsilon": true, "Phi": true, "Psi": true, "Omega": true,
}

// mathContext distinguishes the two places the emitter puts a validated
// fragment. amsmath legality differs between them, so the environment check
// has to know which one it is guarding.
type mathContext int

const (
	displayMath mathContext = iota // \[ … \] (≡ equation* under amsmath)
	inlineMath                     // $ … $
)

// innerEnvironments restricts \begin{…} to amsmath's INNER building blocks —
// the forms designed to sit inside an enclosing math environment, which is
// the only place this emitter ever puts a fragment. The top-level display
// environments (align, gather, equation, multline) are deliberately absent:
// the emitter supplies the enclosing display itself, so admitting them is
// admitting a guaranteed "erroneous nesting" compile failure (2026-07-30
// audit). Notably absent as ever: verbatim/filecontents/tikzpicture and
// anything else that changes catcodes or touches the filesystem.
var innerEnvironments = map[string]bool{
	"aligned": true, "alignedat": true, "gathered": true,
	"array": true, "cases": true,
	"matrix": true, "pmatrix": true, "bmatrix": true, "Bmatrix": true,
	"vmatrix": true, "Vmatrix": true, "smallmatrix": true,
}

// displayOnlyEnvironments are additionally legal under \[…\] (which amsmath
// makes an equation*), but error inside $…$.
var displayOnlyEnvironments = map[string]bool{
	"split": true,
}

// ampersandEnvironments are the environments whose innermost scope makes a
// bare & a legal alignment tab. gathered is single-column: & inside it is the
// same "misplaced alignment tab" error as & outside any environment.
var ampersandEnvironments = map[string]bool{
	"aligned": true, "alignedat": true, "array": true, "cases": true, "split": true,
	"matrix": true, "pmatrix": true, "bmatrix": true, "Bmatrix": true,
	"vmatrix": true, "Vmatrix": true, "smallmatrix": true,
}

// envArgSpec names the environments whose \begin takes a MANDATORY argument,
// and the shape that argument must have. Omitting the argument is a hard TeX
// error, so validation demands it up front.
var envArgSpec = map[string]string{
	"array":     "cols",   // {[lcr|]+}
	"alignedat": "digits", // {pair count}
}

// allowedSymbols are the control symbols (backslash + non-letter) that may
// appear in math: escaped literals and spacing. `\\` is NOT here — a row
// break is only legal at an environment's own level, so it is gated on the
// group stack instead.
var allowedSymbols = map[string]bool{
	`\,`: true, `\;`: true, `\:`: true, `\!`: true, `\ `: true,
	`\{`: true, `\}`: true, `\|`: true, `\%`: true, `\$`: true,
	`\#`: true, `\&`: true, `\_`: true,
}

// spacingSymbols are glue, not glyphs: they cannot serve as a ^/_ operand and
// do not end the atom being built (x^2\,^3 is still a double superscript).
var spacingSymbols = map[string]bool{
	`\,`: true, `\;`: true, `\:`: true, `\!`: true, `\ `: true,
}

// scriptOperandCommands are the commands TeX's scan_math accepts as a BARE
// ^/_ operand: macros expanding to a single glyph or group (greek, symbol
// glyphs, the \frac family, math alphabets, the \text family). Operators,
// accents, radicals, spacing, and structure commands directly after ^ or _
// are hard TeX errors ("Missing { inserted"), so they may not complete a
// pending script.
var scriptOperandCommands = map[string]bool{
	"frac": true, "dfrac": true, "tfrac": true, "binom": true,
	"mathbb": true, "mathcal": true, "mathrm": true, "mathbf": true,
	"mathit": true, "mathsf": true, "mathtt": true, "mathfrak": true,
	"boldsymbol": true, "text": true, "textrm": true, "textbf": true,
	"textit": true, "textsf": true, "texttt": true, "mbox": true,
	"infty": true, "partial": true, "nabla": true, "prime": true,
	"ell": true, "hbar": true, "aleph": true, "emptyset": true, "varnothing": true,
	"alpha": true, "beta": true, "gamma": true, "delta": true, "epsilon": true,
	"varepsilon": true, "zeta": true, "eta": true, "theta": true, "vartheta": true,
	"iota": true, "kappa": true, "lambda": true, "mu": true, "nu": true,
	"xi": true, "pi": true, "varpi": true, "rho": true, "varrho": true,
	"sigma": true, "varsigma": true, "tau": true, "upsilon": true, "phi": true,
	"varphi": true, "chi": true, "psi": true, "omega": true,
	"Gamma": true, "Delta": true, "Theta": true, "Lambda": true, "Xi": true,
	"Pi": true, "Sigma": true, "Upsilon": true, "Phi": true, "Psi": true, "Omega": true,
}

// bigFamily sizes a delimiter and therefore REQUIRES one; \big with nothing
// after it is TeX's "Missing delimiter" error.
var bigFamily = map[string]bool{
	"big": true, "Big": true, "bigg": true, "Bigg": true,
	"bigl": true, "bigr": true, "Bigl": true, "Bigr": true,
	"biggl": true, "biggr": true, "Biggl": true, "Biggr": true,
}

// delimChars and delimCommands are the tokens \left and \right accept as
// their delimiter operand ("." is the invisible delimiter). A \left with no
// delimiter is a hard TeX error, so the operand is checked at the \left.
// '<' and '>' are deliberately absent: LaTeX declares them as relations, not
// delimiters, so \left< raises "Missing delimiter" — \langle is the form.
var delimChars = map[rune]bool{
	'(': true, ')': true, '[': true, ']': true, '.': true,
	'|': true, '/': true,
}

var delimCommands = map[string]bool{
	"langle": true, "rangle": true, "lceil": true, "rceil": true,
	"lfloor": true, "rfloor": true, "lvert": true, "rvert": true,
	"lVert": true, "rVert": true, "vert": true, "Vert": true,
	"uparrow": true, "downarrow": true, "Uparrow": true, "Downarrow": true,
	"backslash": true,
}

// MathResult reports the outcome of validating one math fragment.
type MathResult struct {
	// LaTeX is the fragment as supplied (unchanged — validation never rewrites).
	LaTeX string
	// Rejected names every offending token, deduplicated and ordered, so the
	// manifest can explain a demotion instead of silently losing content.
	Rejected []string
}

// ValidateMath reports whether a math fragment destined for \[…\] consists
// solely of allow-listed typesetting commands with balanced braces, matched
// environments, and no token that TeX would misread as structure (comment,
// math shift, alignment tab, paragraph break). It never modifies the
// fragment; callers demote rejected fragments to escaped literal text.
func ValidateMath(s string) (MathResult, bool) { return validateMath(s, displayMath) }

// ValidateMathInline is ValidateMath for fragments destined for $…$, where
// the display-only environments (split) are additionally illegal.
func ValidateMathInline(s string) (MathResult, bool) { return validateMath(s, inlineMath) }

// groupKind names what a group-stack record was opened by.
type groupKind int

const (
	gBrace groupKind = iota
	gEnv
	gLeft
)

func validateMath(s string, ctx mathContext) (MathResult, bool) {
	res := MathResult{LaTeX: s}
	seen := map[string]bool{}
	reject := func(tok string) {
		if !seen[tok] {
			seen[tok] = true
			res.Rejected = append(res.Rejected, tok)
		}
	}

	// An empty fragment is not "trivially safe": the display wrapper would
	// place a blank line — \par, a compile abort — inside \[…\], so a vacuous
	// ok here hands every caller that footgun (2026-07-30 re-audit).
	if strings.TrimSpace(s) == "" {
		reject("<empty>")
	}

	r := []rune(s)

	// TeX has ONE group stack: brace groups, environments, and \left/\right
	// all push onto it and must close in nesting order — three independent
	// counters accepted interleavings ( \frac{\left(a}{b\right)} ) that
	// hard-error (2026-07-30 re-audit). Each record also carries the script
	// state of the atom being built at that level (at most one ^ and one _ —
	// more is TeX's "double superscript"), so x^{a^b} tracks inner and outer
	// atoms independently. A gBrace record notes WHY it was opened: a ^/_
	// operand or a command argument must not disturb the outer atom when it
	// closes, while a plain group IS the next atom and resets it.
	type group struct {
		kind     groupKind
		env      string // gEnv only
		sup, sub bool
		operand  bool // gBrace: the group is a ^/_ operand
		argGroup bool // gBrace: the group is a command's argument
		rowOK    bool // gBrace: \substack's group — row breaks are legal
	}
	stack := []group{{kind: gBrace}}
	top := func() *group { return &stack[len(stack)-1] }
	clearAtom := func() { t := top(); t.sup, t.sub = false, false }
	dropPending := func(pending *rune) {
		if *pending != 0 {
			reject(string(*pending))
			*pending = 0
		}
	}

	var pending rune       // '^' or '_' still awaiting its operand, else 0
	afterCmd := false      // previous token was a command: next braces are arguments
	afterSubstack := false // previous command was \substack: its group takes rows

	for i := 0; i < len(r); i++ {
		c := r[i]
		wasAfterCmd, wasAfterSubstack := afterCmd, afterSubstack
		afterCmd, afterSubstack = false, false
		switch {
		case c == '\\':
			if i+1 >= len(r) {
				reject(`\`)
				continue
			}
			if isASCIILetter(r[i+1]) {
				j := i + 1
				for j < len(r) && isASCIILetter(r[j]) {
					j++
				}
				name := string(r[i+1 : j])
				switch {
				case !allowedCommands[name]:
					reject(`\` + name)
				case name == "begin" || name == "end":
					env, ok := environmentAt(r, j)
					switch {
					case !ok:
						reject(`\` + name + `{` + env + `}`)
					case name == "begin":
						if !envAllowed(env, ctx) {
							reject(`\begin{` + env + `}`)
						} else if spec := envArgSpec[env]; spec != "" && !envArgOK(r, j+len(env)+2, spec) {
							reject(`\begin{` + env + `}`)
						}
						// Push even when rejected, so the body's & / \\ and
						// the matching \end do not cascade into noise.
						stack = append(stack, group{kind: gEnv, env: env})
					case top().kind == gEnv && top().env == env:
						stack = stack[:len(stack)-1]
					default:
						// \end with no matching open environment at the top:
						// crossed groups or a stray closer.
						reject(`\end{` + env + `}`)
					}
				case name == "left":
					if !delimiterAt(r, j) {
						reject(`\left`)
					}
					stack = append(stack, group{kind: gLeft})
				case name == "right":
					if top().kind != gLeft {
						// \right while a brace group or environment is the
						// innermost open group — TeX pairs \left/\right per
						// group, so crossing is "Missing \right. inserted".
						reject(`\right`)
					} else {
						stack = stack[:len(stack)-1]
					}
					if !delimiterAt(r, j) {
						reject(`\right`)
					}
				case bigFamily[name]:
					if !delimiterAt(r, j) {
						reject(`\` + name)
					}
				case name == "hline":
					if t := top(); t.kind != gEnv || t.env != "array" {
						reject(`\hline`)
					}
				case name == "hspace":
					if !hspaceArgOK(r, j) {
						reject(`\hspace`)
					}
				case name == "substack":
					afterSubstack = true
				}
				// Scripts: a command completes a pending ^/_ only when it is
				// a glyph-like form scan_math accepts as a bare field; \left,
				// \quad, \sum, \hat etc. directly after ^ are hard errors.
				if pending != 0 {
					if !scriptOperandCommands[name] {
						reject(string(pending))
					}
					pending = 0
				} else {
					clearAtom()
				}
				afterCmd = true
				i = j - 1
				continue
			}
			sym := string(r[i : i+2])
			switch {
			case sym == `\\`:
				// A row break is legal only at an environment's own level (or
				// inside \substack's group); anywhere else TeX swallows it or
				// errors — including inside a brace group within a cell.
				if t := top(); !(t.kind == gEnv || (t.kind == gBrace && t.rowOK)) {
					reject(`\\`)
				}
				dropPending(&pending)
				clearAtom()
			case !allowedSymbols[sym]:
				reject(sym)
			case spacingSymbols[sym]:
				dropPending(&pending)
			default: // escaped literal (\%, \$, …): an ordinary glyph
				if pending != 0 {
					pending = 0
				} else {
					clearAtom()
				}
			}
			i++
		case c == '{':
			stack = append(stack, group{
				kind:     gBrace,
				operand:  pending != 0,
				argGroup: pending == 0 && wasAfterCmd,
				rowOK:    wasAfterSubstack,
			})
			pending = 0
		case c == '}':
			dropPending(&pending) // {x^} — script with nothing to attach to
			if len(stack) == 1 || top().kind != gBrace {
				// Underflow, or a } arriving while an environment or \left is
				// still open — TeX's "Extra }, or forgotten \endgroup/\right".
				reject(`}`)
				continue
			}
			wasOperand, wasArg := top().operand, top().argGroup
			stack = stack[:len(stack)-1]
			switch {
			case wasArg:
				afterCmd = true // \frac{1}{2}: the next brace is another argument
			case !wasOperand:
				// A closed plain group is a fresh atom: scripts after it
				// attach to the group, so the outer state resets.
				clearAtom()
			}
		case c == '%':
			// Catcode 14: TeX deletes the rest of the source line and keeps
			// compiling, so the professor grades a truncated answer with no
			// diagnostic anywhere. The single worst token in the audit.
			reject(`%`)
		case c == '$':
			reject(`$`)
		case c == '#':
			reject(`#`)
		case c == '&':
			// TeX recognises an alignment tab only at the alignment's own
			// brace level; inside a group or \text{} it is a hard error even
			// within an alignment environment. gathered is single-column.
			if t := top(); t.kind != gEnv || !ampersandEnvironments[t.env] {
				reject(`&`)
			}
			dropPending(&pending)
			clearAtom()
		case c == '^' && i+1 < len(r) && r[i+1] == '^':
			// TeX's catcode-7 escape: ^^41 is "A", ^^M is a carriage return.
			// A door into arbitrary character injection; never legitimate here.
			reject(`^^`)
			i++
		case c == '^' || c == '_':
			dropPending(&pending) // x^ ^2 / x^_2: operand never arrived
			t := top()
			if (c == '^' && t.sup) || (c == '_' && t.sub) {
				reject(string(c)) // double superscript / double subscript
			}
			if c == '^' {
				t.sup = true
			} else {
				t.sub = true
			}
			pending = c
		case c == '\n':
			// One newline is a space. Two — even with blank space between —
			// is \par, and \par in math mode aborts the compile.
			j := i + 1
			for j < len(r) && (r[j] == ' ' || r[j] == '\t') {
				j++
			}
			if j < len(r) && r[j] == '\n' {
				reject("<blank line>")
				i = j
			}
			afterCmd, afterSubstack = wasAfterCmd, wasAfterSubstack
		case c == '\r':
			reject("<CR>") // a line-end to TeX: CRLF pairs manufacture \par
		case c == '\f':
			reject("<FF>")
		case c < 0x20 && c != '\t':
			reject("<control>")
		case c == ' ' || c == '\t':
			// Spaces are invisible to math mode: they neither end an atom
			// (x^2 ^3 is still a double superscript) nor separate a command
			// from its argument braces.
			afterCmd, afterSubstack = wasAfterCmd, wasAfterSubstack
		case !renderableInMath(c):
			// The math fonts are 8-bit: an unmapped rune is a SILENT
			// missing-glyph drop with a clean compile — worse than an error.
			// Allow-list, not block-list: ASCII plus the CJK ranges xeCJK
			// renders; everything else (α, ℝ, ′, ², ≤, …) must arrive as its
			// command form or demote (2026-07-30 re-audit).
			reject(string(c))
		default:
			if pending != 0 {
				pending = 0
			} else {
				clearAtom()
			}
		}
	}
	if pending != 0 {
		reject(string(pending))
	}
	for k := len(stack) - 1; k >= 1; k-- {
		switch stack[k].kind {
		case gBrace:
			reject(`{`)
		case gEnv:
			reject(`\begin{` + stack[k].env + `}`)
		case gLeft:
			reject(`\left`)
		}
	}
	return res, len(res.Rejected) == 0
}

func envAllowed(env string, ctx mathContext) bool {
	return innerEnvironments[env] || (ctx == displayMath && displayOnlyEnvironments[env])
}

// envArgOK checks the mandatory {argument} that must follow \begin{array} or
// \begin{alignedat}, starting at i (the index just past the env's closing
// brace). An optional [t]/[b]/[c] position argument may precede it. Column
// specs are restricted to l/c/r/| with spaces (which \@mkpream ignores), and
// must declare at least one COLUMN — an all-rules spec compiles to a
// zero-column preamble ("Missing # inserted"). alignedat's pair count must
// be at least 1: {0} errors on any content.
func envArgOK(r []rune, i int, spec string) bool {
	skip := func() {
		for i < len(r) && (r[i] == ' ' || r[i] == '\t' || r[i] == '\n') {
			i++
		}
	}
	skip()
	if i < len(r) && r[i] == '[' {
		for i < len(r) && r[i] != ']' {
			i++
		}
		if i < len(r) {
			i++
		}
		skip()
	}
	if i >= len(r) || r[i] != '{' {
		return false
	}
	n := 0 // cols: l/c/r seen; digits: nonzero digits seen
	for j := i + 1; j < len(r); j++ {
		c := r[j]
		if c == '}' {
			return n > 0
		}
		switch spec {
		case "cols":
			switch c {
			case 'l', 'c', 'r':
				n++
			case '|', ' ', '\t':
			default:
				return false
			}
		case "digits":
			if c < '0' || c > '9' {
				return false
			}
			if c != '0' {
				n++
			}
		}
	}
	return false
}

// delimiterAt reports whether the token at/after i (whitespace skipped — a
// newline is a space to TeX) is a legal \left/\right/\big delimiter operand.
func delimiterAt(r []rune, i int) bool {
	for i < len(r) && (r[i] == ' ' || r[i] == '\t' || r[i] == '\n') {
		i++
	}
	if i >= len(r) {
		return false
	}
	if delimChars[r[i]] {
		return true
	}
	if r[i] != '\\' || i+1 >= len(r) {
		return false
	}
	if r[i+1] == '{' || r[i+1] == '}' || r[i+1] == '|' {
		return true
	}
	j := i + 1
	for j < len(r) && isASCIILetter(r[j]) {
		j++
	}
	return delimCommands[string(r[i+1:j])]
}

// hspaceArgOK checks \hspace's mandatory {<dimen>} argument: a missing,
// empty, unit-less, or absurdly large dimension is a hard TeX error
// ("Illegal unit of measure" / "Dimension too large").
func hspaceArgOK(r []rune, i int) bool {
	for i < len(r) && (r[i] == ' ' || r[i] == '\t') {
		i++
	}
	if i >= len(r) || r[i] != '{' {
		return false
	}
	j := i + 1
	if j < len(r) && (r[j] == '-' || r[j] == '+') {
		j++
	}
	intDigits, digits := 0, 0
	for j < len(r) && ((r[j] >= '0' && r[j] <= '9') || r[j] == '.') {
		if r[j] != '.' {
			digits++
			if !strings.ContainsRune(string(r[i+1:j]), '.') {
				intDigits++
			}
		}
		j++
	}
	if digits == 0 || intDigits > 4 { // >9999 of any unit exceeds \maxdimen territory
		return false
	}
	for _, u := range []string{"pt", "em", "ex", "cm", "mm", "in", "bp", "pc", "mu", "sp"} {
		k := j + len(u)
		if k < len(r) && string(r[j:k]) == u && r[k] == '}' {
			return true
		}
	}
	return false
}

// renderableInMath reports whether a rune may pass into math mode. The math
// fonts are 8-bit: an unmapped rune is a SILENT missing-glyph drop with a
// clean compile, so this is an allow-list — ASCII plus the CJK blocks xeCJK
// actually renders — rather than an enumeration of known-bad symbol ranges.
func renderableInMath(c rune) bool {
	if c >= 0x20 && c < 0x7F {
		return true
	}
	return (c >= 0x2E80 && c <= 0x9FFF) || // CJK radicals, kana, ideographs, punctuation
		(c >= 0xF900 && c <= 0xFAFF) || // CJK compatibility ideographs
		(c >= 0xFF00 && c <= 0xFFEF) // fullwidth forms
}

// environmentAt reads the {name} immediately following \begin or \end.
func environmentAt(r []rune, i int) (string, bool) {
	if i >= len(r) || r[i] != '{' {
		return "", false
	}
	for j := i + 1; j < len(r); j++ {
		if r[j] == '}' {
			return string(r[i+1 : j]), true
		}
		if r[j] == '\\' || r[j] == '{' {
			return "", false
		}
	}
	return "", false
}

func isASCIILetter(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// escapeProse neutralises every TeX special so a run of student prose types
// out literally. CJK and other non-ASCII pass through untouched — xeCJK
// handles them, and escaping them would corrupt the text.
func escapeProse(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, c := range s {
		switch c {
		case '\\':
			b.WriteString(`\textbackslash{}`)
		case '~':
			b.WriteString(`\textasciitilde{}`)
		case '^':
			b.WriteString(`\textasciicircum{}`)
		case '&', '%', '$', '#', '_', '{', '}':
			b.WriteByte('\\')
			b.WriteRune(c)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// renderInline renders prose that may contain $…$ inline math. Text is
// escaped; math spans are validated and passed through verbatim on success, or
// demoted to escaped text (with a flag) on failure.
//
// A $ opens a span only when the pairing looks like math rather than money
// (2026-07-30 audit: students write about dollars, and a mis-paired span
// silently typesets the prose between two prices as math):
//   - the closer must sit on the SAME LINE as the opener;
//   - the opener is followed by a non-space and the closer preceded by one
//     (real spans hug their content: $O(n)$, never $ O(n) $ or $5 and $);
//   - the span is non-empty — a literal "$$" is TeX's display-math toggle,
//     which would swallow the rest of the paragraph;
//   - the closer is not immediately followed by a digit ($5-$10 is money).
//
// A $ that opens no valid span is a literal dollar and is escaped.
func renderInline(s string) (string, []string) {
	var b strings.Builder
	var flags []string

	for i := 0; i < len(s); {
		if s[i] != '$' {
			j := strings.IndexByte(s[i:], '$')
			if j < 0 {
				b.WriteString(escapeProse(s[i:]))
				break
			}
			b.WriteString(escapeProse(s[i : i+j]))
			i += j
			continue
		}
		end := closingDollar(s, i)
		if end < 0 {
			b.WriteString(escapeProse("$"))
			if mathLikeAfter(s, i) {
				// The refused span contained TeX commands: its escaped source
				// will show in the PDF, which the manifest must mention.
				flags = append(flags, "kept literal $ next to TeX-like content")
			}
			i++
			continue
		}
		body := s[i+1 : end]
		if res, ok := ValidateMathInline(body); ok {
			b.WriteString("$" + body + "$")
		} else {
			// Demote, never drop: the delimiters are student-written bytes,
			// so they escape along with the body.
			b.WriteString(escapeProse("$" + body + "$"))
			flags = append(flags, "demoted inline math ("+strings.Join(res.Rejected, " ")+")")
		}
		i = end + 1
	}
	return b.String(), flags
}

// closingDollar returns the index of the $ closing a span opened at i under
// the pairing rules above, or -1 when the opener is literal. The FIRST
// unescaped $ after the opener decides: either it qualifies as a closer or
// the opener is literal — scanning past a disqualified $ is how a money $
// used to steal a later real span's closer (2026-07-30 re-audit).
func closingDollar(s string, i int) int {
	if i+1 >= len(s) {
		return -1
	}
	switch s[i+1] {
	case ' ', '\t', '\n', '$':
		return -1
	}
	for j := i + 2; j < len(s) && s[j] != '\n'; j++ {
		if s[j] != '$' {
			continue
		}
		if backslashEscaped(s, j, i) {
			continue // \$ is span content, not a delimiter
		}
		if prev := s[j-1]; prev == ' ' || prev == '\t' {
			return -1
		}
		if j+1 < len(s) && s[j+1] >= '0' && s[j+1] <= '9' {
			return -1
		}
		return j
	}
	return -1
}

// backslashEscaped reports whether s[j] sits behind an odd run of
// backslashes (scanning back no further than lo).
func backslashEscaped(s string, j, lo int) bool {
	n := 0
	for k := j - 1; k > lo && s[k] == '\\'; k-- {
		n++
	}
	return n%2 == 1
}

// mathLikeAfter reports whether the rest of the opener's line contains a
// backslash — the signature of a span the pairing rules refused (padded
// edges, a break across lines) rather than plain currency.
func mathLikeAfter(s string, i int) bool {
	for j := i + 1; j < len(s) && s[j] != '\n'; j++ {
		if s[j] == '\\' {
			return true
		}
	}
	return false
}

// Options tunes the emitted document. The zero value is valid and resolves the
// CJK font by family name, which requires it to be installed system-wide.
type Options struct {
	// CJKFontFile is the bundled Traditional Chinese face (the app ships
	// data/fonts/NotoSansTC-Regular.ttf and already points ADAMARKER_REPORT_FONT
	// at it). When set, the preamble loads the font BY PATH, so compilation does
	// not depend on the host having any font installed — the same guarantee
	// --font-path gives the Typst renderer. When empty, the family name is used.
	CJKFontFile string
}

// buildPreamble is fixed and tested. XeLaTeX + xeCJK is mandatory rather than
// stylistic: mixed Traditional Chinese and math cannot be typeset by pdflatex,
// and without xeCJK every Chinese glyph is silently dropped.
func buildPreamble(opt Options) string {
	font := "\\setCJKmainfont{Noto Sans TC}\n"
	if opt.CJKFontFile != "" {
		dir, file := splitFontPath(opt.CJKFontFile)
		// Path= must end in a separator; Extension is split out so fontspec can
		// find the bold/italic siblings if they are ever added beside it.
		base, ext := file, ""
		if i := strings.LastIndex(file, "."); i > 0 {
			base, ext = file[:i], file[i:]
		}
		font = fmt.Sprintf("\\setCJKmainfont[Path=%s, Extension=%s]{%s}\n", dir, ext, base)
	}
	return `\documentclass[11pt,a4paper]{article}
\usepackage{amsmath}
\usepackage{amssymb}
\usepackage{fontspec}
\usepackage{xeCJK}
` + font + `\usepackage[margin=2.5cm]{geometry}
\setlength{\parindent}{0pt}
\setlength{\parskip}{0.6em}
`
}

// splitFontPath returns the directory (with trailing separator) and file name.
func splitFontPath(p string) (dir, file string) {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "./", p
	}
	return p[:i+1], p[i+1:]
}

// Preamble returns the document preamble for the given options. Exported so a
// consumer assembling many answers into ONE document (the export bundle's
// _all.tex) can emit the preamble once and follow it with N bodies, instead of
// cutting standalone documents back apart with string surgery.
func Preamble(opt Options) string { return buildPreamble(opt) }

// EmitTeX renders a Doc as a standalone, compilable LaTeX document using the
// default options.
func EmitTeX(d Doc) (string, []string) { return EmitTeXWith(d, Options{}) }

// EmitTeXWith renders a Doc as a standalone, compilable LaTeX document. It is a
// pure function: identical input yields byte-identical output, with no
// timestamps or map iteration. The returned flags list every demotion so the
// export manifest can report reduced-fidelity answers rather than hiding them.
func EmitTeXWith(d Doc, opt Options) (string, []string) {
	body, flags := EmitBody(d)
	return Preamble(opt) + "\\begin{document}\n" + body + "\\end{document}\n", flags
}

// EmitBody renders a Doc as document CONTENT only — no preamble, no document
// environment — so several answers can share one preamble. Same purity and
// flag semantics as EmitTeXWith.
func EmitBody(d Doc) (string, []string) {
	var b strings.Builder
	var flags []string

	if d.Title != "" {
		// Title is app-controlled, but the emitter must not rely on its
		// caller: a raw newline or CR inside \section*{…} is a runaway-
		// argument compile error, so whitespace collapses to single spaces.
		title := strings.Join(strings.Fields(normalizeEOL(d.Title)), " ")
		fmt.Fprintf(&b, "\\section*{%s}\n", escapeProse(title))
	}

	// emitItems renders items wherever they appear — their own list block or
	// misfiled beside other content — so no field of a block is ever dropped.
	emitItems := func(items []string) {
		if len(items) == 0 {
			// \begin{itemize} with no \item is a hard TeX error, and an
			// empty list carries nothing to lose.
			return
		}
		b.WriteString("\\begin{itemize}\n")
		for _, item := range items {
			out, f := renderInline(normalizeEOL(item))
			flags = append(flags, f...)
			if strings.HasPrefix(out, "[") {
				// \item scans for an optional [label]; interval notation
				// and the prompt's own [illegible] marker must not be
				// captured as one. The empty group ends the scan.
				out = "{}" + out
			}
			b.WriteString("  \\item " + out + "\n")
		}
		b.WriteString("\\end{itemize}\n\n")
	}

	for _, blk := range d.Blocks {
		switch blk.Kind {
		case BlockProse:
			out, f := renderInline(normalizeEOL(blk.Text))
			flags = append(flags, f...)
			b.WriteString(out)
			b.WriteString("\n\n")
			emitItems(blk.Items)

		case BlockMath:
			// Edge whitespace is trimmed because the wrapper supplies its own
			// newlines: a trailing newline in the text would otherwise place
			// a blank line — \par, a compile abort — inside \[…\].
			text := strings.TrimSpace(normalizeEOL(blk.Text))
			if text == "" {
				// Nothing to lose, and \[ around nothing wraps a blank line —
				// \par in math mode, a compile abort.
				emitItems(blk.Items)
				continue
			}
			if res, ok := ValidateMath(text); ok {
				b.WriteString("\\[\n" + text + "\n\\]\n\n")
			} else {
				// Demote, never drop: the student wrote something, and a
				// silent gap would read as "wrote nothing" when grading.
				b.WriteString(escapeProse(text))
				b.WriteString("\n\n")
				flags = append(flags, "demoted display math ("+strings.Join(res.Rejected, " ")+")")
			}
			emitItems(blk.Items)

		case BlockCode:
			code, defused := sanitiseVerbatim(expandTabs(normalizeEOL(blk.Text)))
			if defused {
				// The one emission path that rewrites student content; saying
				// so in the manifest keeps "never silently altered" true.
				flags = append(flags, `code block contained \end{verbatim}; defused`)
			}
			b.WriteString("\\begin{verbatim}\n")
			b.WriteString(code)
			b.WriteString("\n\\end{verbatim}\n\n")
			emitItems(blk.Items)

		case BlockList:
			if strings.TrimSpace(blk.Text) != "" {
				// A misfiled lead-in ("two reasons:") is student content.
				out, f := renderInline(normalizeEOL(blk.Text))
				flags = append(flags, f...)
				b.WriteString(out)
				b.WriteString("\n\n")
			}
			emitItems(blk.Items)

		default:
			// ParseResponse rejects unknown kinds, but the emitter must not
			// rely on its caller: demote to escaped text and flag, never a
			// silent gap.
			text := blk.Text
			if len(blk.Items) > 0 {
				text = strings.Join(append([]string{text}, blk.Items...), "\n")
			}
			b.WriteString(escapeProse(strings.TrimSpace(normalizeEOL(text))))
			b.WriteString("\n\n")
			flags = append(flags, "demoted block of unrecognised kind "+string(blk.Kind))
		}
	}

	return b.String(), dedupeStable(flags)
}

// sanitiseVerbatim removes the only sequence that can escape a verbatim
// environment, reporting whether it had to. Everything else — including
// backslashes and braces — is inert inside verbatim, which is why pseudocode
// is emitted this way rather than escaped: indentation and operators survive
// exactly as the student wrote them.
func sanitiseVerbatim(s string) (string, bool) {
	out := strings.ReplaceAll(s, `\end{verbatim}`, `\end {verbatim}`)
	return out, out != s
}

// normalizeEOL folds CRLF and lone CR to LF. A raw CR is a line end to TeX,
// so an un-normalized CRLF pair silently manufactures a blank line — \par —
// wherever the model's output used Windows line endings.
func normalizeEOL(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// expandTabs widens tabs to four spaces for verbatim, which would otherwise
// render each tab as a SINGLE space and flatten the indentation levels the
// design promises to preserve exactly.
func expandTabs(s string) string {
	return strings.ReplaceAll(s, "\t", "    ")
}

// dedupeStable removes duplicate flags while preserving first-seen order, so
// an answer with the same demotion twenty times reports it once and the
// manifest stays readable.
func dedupeStable(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// SortedRejections is a helper for tests and manifest rendering where a stable
// alphabetical order is wanted rather than first-seen order.
func SortedRejections(r []string) []string {
	out := append([]string(nil), r...)
	sort.Strings(out)
	return out
}
