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
	"quad": true, "qquad": true,

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

	// binary operators
	"cdot": true, "times": true, "div": true, "pm": true, "mp": true,
	"ast": true, "star": true, "circ": true, "bullet": true,
	"oplus": true, "ominus": true, "otimes": true, "odot": true,

	// dots and spacing
	"cdots": true, "ldots": true, "vdots": true, "ddots": true, "dots": true,
	"hspace": true, "vspace": true, "phantom": true,

	// delimiters
	"langle": true, "rangle": true, "lceil": true, "rceil": true,
	"lfloor": true, "rfloor": true, "lvert": true, "rvert": true,
	"lVert": true, "rVert": true,

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

// allowedEnvironments restricts \begin{…}. Everything here is a math-alignment
// environment; notably absent are verbatim/filecontents/tikzpicture and
// anything else that changes catcodes or touches the filesystem.
var allowedEnvironments = map[string]bool{
	"aligned": true, "align": true, "alignedat": true, "gathered": true,
	"array": true, "cases": true, "split": true,
	"matrix": true, "pmatrix": true, "bmatrix": true, "Bmatrix": true,
	"vmatrix": true, "Vmatrix": true, "smallmatrix": true,
}

// allowedSymbols are the control symbols (backslash + non-letter) that may
// appear in math. `\\` is a row break; the rest are escaped literals or thin
// spaces. Anything else — notably `\ ` variants used in catcode tricks — is
// rejected.
var allowedSymbols = map[string]bool{
	`\\`: true, `\,`: true, `\;`: true, `\:`: true, `\!`: true,
	`\{`: true, `\}`: true, `\|`: true, `\%`: true, `\$`: true,
	`\#`: true, `\&`: true, `\_`: true,
}

// MathResult reports the outcome of validating one math fragment.
type MathResult struct {
	// LaTeX is the fragment as supplied (unchanged — validation never rewrites).
	LaTeX string
	// Rejected names every offending token, deduplicated and ordered, so the
	// manifest can explain a demotion instead of silently losing content.
	Rejected []string
}

// ValidateMath reports whether a math fragment consists solely of allow-listed
// typesetting commands with balanced braces. It never modifies the fragment;
// callers demote rejected fragments to escaped literal text.
func ValidateMath(s string) (MathResult, bool) {
	res := MathResult{LaTeX: s}
	seen := map[string]bool{}
	reject := func(tok string) {
		if !seen[tok] {
			seen[tok] = true
			res.Rejected = append(res.Rejected, tok)
		}
	}

	r := []rune(s)
	depth := 0
	for i := 0; i < len(r); i++ {
		switch {
		case r[i] == '\\':
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
				if !allowedCommands[name] {
					reject(`\` + name)
				} else if name == "begin" || name == "end" {
					env, ok := environmentAt(r, j)
					if !ok || !allowedEnvironments[env] {
						reject(`\` + name + `{` + env + `}`)
					}
				}
				i = j - 1
				continue
			}
			sym := string(r[i : i+2])
			if !allowedSymbols[sym] {
				reject(sym)
			}
			i++
		case r[i] == '{':
			depth++
		case r[i] == '}':
			depth--
			if depth < 0 {
				reject(`}`)
				depth = 0
			}
		case r[i] == '^' && i+1 < len(r) && r[i+1] == '^':
			// TeX's catcode-7 escape: ^^41 is "A", ^^M is a carriage return.
			// A door into arbitrary character injection; never legitimate here.
			reject(`^^`)
			i++
		}
	}
	if depth != 0 {
		reject(`{`)
	}
	return res, len(res.Rejected) == 0
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
// demoted to escaped text (with a flag) on failure. A `$` with no closer is
// literal — students write about dollars, and an unterminated math span would
// otherwise swallow the rest of the answer.
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
		end := strings.IndexByte(s[i+1:], '$')
		if end < 0 {
			b.WriteString(escapeProse("$"))
			i++
			continue
		}
		body := s[i+1 : i+1+end]
		if res, ok := ValidateMath(body); ok {
			b.WriteString("$" + body + "$")
		} else {
			b.WriteString(escapeProse(body))
			flags = append(flags, "demoted inline math ("+strings.Join(res.Rejected, " ")+")")
		}
		i += end + 2
	}
	return b.String(), flags
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
		fmt.Fprintf(&b, "\\section*{%s}\n", escapeProse(d.Title))
	}

	for _, blk := range d.Blocks {
		switch blk.Kind {
		case BlockProse:
			out, f := renderInline(blk.Text)
			flags = append(flags, f...)
			b.WriteString(out)
			b.WriteString("\n\n")

		case BlockMath:
			if res, ok := ValidateMath(blk.Text); ok {
				b.WriteString("\\[\n" + blk.Text + "\n\\]\n\n")
			} else {
				// Demote, never drop: the student wrote something, and a
				// silent gap would read as "wrote nothing" when grading.
				b.WriteString(escapeProse(blk.Text))
				b.WriteString("\n\n")
				flags = append(flags, "demoted display math ("+strings.Join(res.Rejected, " ")+")")
			}

		case BlockCode:
			b.WriteString("\\begin{verbatim}\n")
			b.WriteString(sanitiseVerbatim(blk.Text))
			b.WriteString("\n\\end{verbatim}\n\n")

		case BlockList:
			b.WriteString("\\begin{itemize}\n")
			for _, item := range blk.Items {
				out, f := renderInline(item)
				flags = append(flags, f...)
				b.WriteString("  \\item " + out + "\n")
			}
			b.WriteString("\\end{itemize}\n\n")
		}
	}

	return b.String(), dedupeStable(flags)
}

// sanitiseVerbatim removes the only sequence that can escape a verbatim
// environment. Everything else — including backslashes and braces — is inert
// inside verbatim, which is why pseudocode is emitted this way rather than
// escaped: indentation and operators survive exactly as the student wrote them.
func sanitiseVerbatim(s string) string {
	return strings.ReplaceAll(s, `\end{verbatim}`, `\end {verbatim}`)
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
