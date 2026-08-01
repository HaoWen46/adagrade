package transcribe

import (
	"fmt"
	"strings"
)

// Typst mirror of the LaTeX emitter (spec
// docs/superpowers/specs/2026-07-30-typst-transcription-mirror-design.md).
//
// SECURITY INVARIANT (inherits typst-report spec 2026-07-20, see
// internal/report/typstmarkup.go): student-derived bytes appear ONLY inside
// escaped Typst string literals — `#"…"`, `#raw("…")`, `#heading(…, "…")`,
// `#mi("…")`/`#mitex("…")` — never in markup position, so `#import`,
// backticks, and markup characters are inert. Math strings additionally
// passed the LaTeX allow-list (ValidateMath*), which is what makes handing
// them to mitex safe.
//
// FLAG PARITY INVARIANT: EmitTypstBody branches on the SAME validator
// verdicts as EmitBody and emits the SAME flag strings on the same inputs,
// so the manifest's one flags column stays truthful for both formats
// (pinned by TestEmitTypstBody_FlagParityWithLaTeX). When changing either
// emitter, change both.

// transcribeMitexVersion pins the mitex package for transcription mirrors.
// Keep equal to internal/report's mitexVersion — the two Typst paths should
// not drift apart in math rendering.
const transcribeMitexVersion = "0.2.5"

// TypstPreamble returns the standalone-document preamble: the mitex import
// plus page/text settings mirroring the report renderer's CJK stack. Shared
// by EmitTypst and the export bundle's _all.typ, so the standalone and
// aggregate documents can never drift apart.
func TypstPreamble() string {
	return `#import "@preview/mitex:` + transcribeMitexVersion + `": mi, mitex
#set page(paper: "a4", margin: (x: 1.6cm, y: 1.8cm))
#set text(size: 10pt, lang: "zh", region: "TW", font: ("Libertinus Serif", "Noto Sans TC", "Songti TC", "Noto Sans CJK TC"))
#set par(justify: true)
`
}

// EmitTypst renders a Doc as a standalone, compilable Typst document. Same
// purity guarantees as EmitTeX: identical input yields byte-identical output.
func EmitTypst(d Doc) (string, []string) {
	body, flags := EmitTypstBody(d)
	return TypstPreamble() + body, flags
}

// EmitTypstBody renders a Doc as document CONTENT only, mirroring EmitBody
// branch for branch (see the parity invariant above).
func EmitTypstBody(d Doc) (string, []string) {
	var b strings.Builder
	var flags []string

	if d.Title != "" {
		// Same title armor as EmitBody: whitespace collapses to spaces.
		title := strings.Join(strings.Fields(normalizeEOL(d.Title)), " ")
		fmt.Fprintf(&b, "#heading(level: 2, %s)\n", quotedTypstString(title))
	}

	emitItems := func(items []string) {
		if len(items) == 0 {
			return
		}
		b.WriteString("#list(\n")
		for _, item := range items {
			out, f := renderInlineTypst(normalizeEOL(item))
			flags = append(flags, f...)
			// No optional-argument hazard exists for #list items; the [
			// guard is a LaTeX-only concern and adds no flag there either.
			b.WriteString("  [" + out + "],\n")
		}
		b.WriteString(")\n#parbreak()\n")
	}

	for _, blk := range d.Blocks {
		switch blk.Kind {
		case BlockProse:
			out, f := renderInlineTypst(normalizeEOL(blk.Text))
			flags = append(flags, f...)
			b.WriteString(out)
			b.WriteString("\n#parbreak()\n")
			emitItems(blk.Items)

		case BlockMath:
			text := strings.TrimSpace(normalizeEOL(blk.Text))
			if text == "" {
				// Parity with EmitBody: nothing to lose, no flag.
				emitItems(blk.Items)
				continue
			}
			if res, ok := ValidateMath(text); ok {
				b.WriteString("#mitex(" + quotedTypstString(text) + ")\n#parbreak()\n")
			} else {
				// Demote, never drop — and the SAME flag string as EmitBody.
				b.WriteString(quotedTypstMarkup(text) + "\n#parbreak()\n")
				flags = append(flags, "demoted display math ("+strings.Join(res.Rejected, " ")+")")
			}
			emitItems(blk.Items)

		case BlockCode:
			code := expandTabs(normalizeEOL(blk.Text))
			if _, defused := sanitiseVerbatim(code); defused {
				// #raw has no escape class, so nothing is altered HERE — but
				// the flags column describes the primary .tex, where this
				// input WAS rewritten. Parity keeps the column truthful.
				flags = append(flags, `code block contained \end{verbatim}; defused`)
			}
			b.WriteString("#raw(" + quotedTypstString(code) + ", block: true)\n#parbreak()\n")
			emitItems(blk.Items)

		case BlockList:
			if strings.TrimSpace(blk.Text) != "" {
				out, f := renderInlineTypst(normalizeEOL(blk.Text))
				flags = append(flags, f...)
				b.WriteString(out)
				b.WriteString("\n#parbreak()\n")
			}
			emitItems(blk.Items)

		default:
			text := blk.Text
			if len(blk.Items) > 0 {
				text = strings.Join(append([]string{text}, blk.Items...), "\n")
			}
			b.WriteString(quotedTypstMarkup(strings.TrimSpace(normalizeEOL(text))) + "\n#parbreak()\n")
			flags = append(flags, "demoted block of unrecognised kind "+string(blk.Kind))
		}
	}

	return b.String(), dedupeStable(flags)
}

// renderInlineTypst is renderInline's Typst twin: the SAME span decisions
// (closingDollar, ValidateMathInline) with Typst emission — literal text as
// `#"…"`, accepted spans as `#mi("…")`, demotions escaped with their
// delimiters. Flag strings match renderInline's exactly (parity invariant).
func renderInlineTypst(s string) (string, []string) {
	var b strings.Builder
	var flags []string
	var text strings.Builder // pending literal text, flushed before math

	flush := func() {
		if text.Len() == 0 {
			return
		}
		b.WriteString(quotedTypstMarkup(text.String()))
		text.Reset()
	}

	for i := 0; i < len(s); {
		if s[i] != '$' {
			j := strings.IndexByte(s[i:], '$')
			if j < 0 {
				text.WriteString(s[i:])
				break
			}
			text.WriteString(s[i : i+j])
			i += j
			continue
		}
		end := closingDollar(s, i)
		if end < 0 {
			text.WriteString("$")
			if mathLikeAfter(s, i) {
				flags = append(flags, "kept literal $ next to TeX-like content")
			}
			i++
			continue
		}
		body := s[i+1 : end]
		if res, ok := ValidateMathInline(body); ok {
			flush()
			b.WriteString("#mi(" + quotedTypstString(body) + ")")
		} else {
			text.WriteString("$" + body + "$")
			flags = append(flags, "demoted inline math ("+strings.Join(res.Rejected, " ")+")")
		}
		i = end + 1
	}
	flush()
	return b.String(), flags
}

// quotedTypstString renders s as a double-quoted Typst string VALUE (for
// code position: function arguments).
func quotedTypstString(s string) string {
	return `"` + escapeTypstString(s) + `"`
}

// quotedTypstMarkup renders s as literal text in MARKUP position: a hash
// expression whose value is the escaped string.
func quotedTypstMarkup(s string) string {
	return `#"` + escapeTypstString(s) + `"`
}

// escapeTypstString escapes a value for a double-quoted Typst string
// literal: backslash and quote are the only metacharacters; newlines become
// \n so multi-line content stays one token. Duplicated from
// internal/report/typstmarkup.go (12 lines) rather than exporting it across
// an unrelated seam — keep the two in sync.
func escapeTypstString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, c := range s {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}
