package report

import "strings"

// typstComment converts a comment / criterion-name string (which may embed
// LaTeX math per the grading template's mandate — prompt.go: "LaTeX for math")
// into Typst markup for the Typst report renderer.
//
// SECURITY INVARIANT (typst-report spec 2026-07-20): comments are model/TA
// text derived from student answers, so hostile content must be assumed. The
// returned markup consists ONLY of these tokens:
//
//	#"<escaped string>"      literal text
//	#mi("<escaped latex>")   inline math   (mitex)
//	#mitex("<escaped latex>") display math (mitex)
//	#parbreak()              paragraph break
//
// User text therefore never lands in Typst markup position — `#import`,
// backticks, and friends render as literal characters. Keep every emission
// path inside emitText/emitMath so the invariant stays auditable.
//
// Math spans recognized: $$…$$ and \[…\] (display), \(…\) and $…$ (inline).
// An opener with no matching closer is treated as literal text.
func typstComment(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	var text strings.Builder // pending literal text, flushed on math/paragraph

	emitText := func() {
		if text.Len() == 0 {
			return
		}
		b.WriteString(`#"`)
		b.WriteString(escapeTypstString(text.String()))
		b.WriteString(`"`)
		text.Reset()
	}
	emitMath := func(latex string, display bool) {
		emitText()
		if display {
			b.WriteString(`#mitex("`)
		} else {
			b.WriteString(`#mi("`)
		}
		b.WriteString(escapeTypstString(latex))
		b.WriteString(`")`)
	}

	// Openers, longest first so $$ wins over $ and \[ over a lone backslash.
	type delim struct {
		open, close string
		display     bool
	}
	delims := []delim{
		{"$$", "$$", true},
		{`\[`, `\]`, true},
		{`\(`, `\)`, false},
		{"$", "$", false},
	}

	i := 0
	for i < len(s) {
		// Paragraph break: two-or-more newlines (blank line), spaces allowed.
		if s[i] == '\n' {
			j := i
			newlines := 0
			for j < len(s) && (s[j] == '\n' || s[j] == ' ' || s[j] == '\t') {
				if s[j] == '\n' {
					newlines++
				}
				j++
			}
			if newlines >= 2 {
				emitText()
				b.WriteString("#parbreak()")
				i = j
				continue
			}
		}
		matched := false
		for _, d := range delims {
			if !strings.HasPrefix(s[i:], d.open) {
				continue
			}
			body := s[i+len(d.open):]
			end := strings.Index(body, d.close)
			if end < 0 || (end == 0 && d.open == "$") {
				continue // no closer (or an empty "$$" seen as two lone $): literal
			}
			emitMath(body[:end], d.display)
			i += len(d.open) + end + len(d.close)
			matched = true
			break
		}
		if matched {
			continue
		}
		text.WriteByte(s[i])
		i++
	}
	emitText()
	return b.String()
}

// escapeTypstString escapes a value for a double-quoted Typst string literal:
// backslash and quote are the only metacharacters; newlines become \n so a
// multi-line comment stays one token.
func escapeTypstString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
