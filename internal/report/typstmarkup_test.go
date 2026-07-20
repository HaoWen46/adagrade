package report

import (
	"strings"
	"testing"
)

// The injection-safety invariant (typst-report spec): typstComment's output
// may consist ONLY of #"…", #mi("…"), #mitex("…"), and #parbreak() tokens —
// user-controlled text must never land bare in Typst markup position.
func assertOnlySafeTokens(t *testing.T, out string) {
	t.Helper()
	rest := out
	for rest != "" {
		rest = strings.TrimLeft(rest, " \n")
		if rest == "" {
			break
		}
		switch {
		case strings.HasPrefix(rest, `#parbreak()`):
			rest = rest[len("#parbreak()"):]
		case strings.HasPrefix(rest, `#mi("`), strings.HasPrefix(rest, `#mitex("`), strings.HasPrefix(rest, `#"`):
			// Consume the quoted string body, honoring escapes; then a closing
			// paren for the mi/mitex forms.
			isCall := !strings.HasPrefix(rest, `#"`)
			i := strings.Index(rest, `"`) + 1
			for i < len(rest) {
				if rest[i] == '\\' {
					i += 2
					continue
				}
				if rest[i] == '"' {
					break
				}
				i++
			}
			if i >= len(rest) {
				t.Fatalf("unterminated string token in %q", out)
			}
			rest = rest[i+1:]
			if isCall {
				if !strings.HasPrefix(rest, ")") {
					t.Fatalf("mi/mitex call not closed in %q", out)
				}
				rest = rest[1:]
			}
		default:
			t.Fatalf("unsafe token at %q (full output %q)", rest[:min(20, len(rest))], out)
		}
	}
}

func TestTypstComment_PlainTextIsEscapedString(t *testing.T) {
	out := typstComment(`Solid proof. #import "@preview/evil" would be bad, as would ` + "`raw`" + ` and "quotes" and \emph{stray}.`)
	// assertOnlySafeTokens is the real guarantee that #import cannot appear in
	// markup position; the checks below pin the escaping details.
	assertOnlySafeTokens(t, out)
	if !strings.Contains(out, `\"quotes\"`) {
		t.Fatalf("quotes must be escaped: %q", out)
	}
	if !strings.Contains(out, `\\emph`) {
		t.Fatalf("backslashes outside math must be escaped: %q", out)
	}
}

func TestTypstComment_InlineMathVariants(t *testing.T) {
	for _, src := range []string{
		`bound is \(O(n \log n)\) here`,
		`bound is $O(n \log n)$ here`,
	} {
		out := typstComment(src)
		assertOnlySafeTokens(t, out)
		if !strings.Contains(out, `#mi("O(n \\log n)")`) {
			t.Fatalf("inline math must reach mi() with escaped backslashes: %q -> %q", src, out)
		}
		if strings.Contains(out, `$`) {
			t.Fatalf("math delimiters must not leak into markup: %q", out)
		}
	}
}

func TestTypstComment_DisplayMathVariants(t *testing.T) {
	for _, src := range []string{
		`recurrence: $$T(n)=2T(n/2)+O(n)$$ done`,
		`recurrence: \[T(n)=2T(n/2)+O(n)\] done`,
	} {
		out := typstComment(src)
		assertOnlySafeTokens(t, out)
		if !strings.Contains(out, `#mitex("T(n)=2T(n/2)+O(n)")`) {
			t.Fatalf("display math must reach mitex(): %q -> %q", src, out)
		}
	}
}

func TestTypstComment_UnbalancedDelimitersStayLiteral(t *testing.T) {
	for _, src := range []string{
		`the price is $5 with no closer`,
		`open \( never closed`,
		`stray $$ alone`,
	} {
		out := typstComment(src)
		assertOnlySafeTokens(t, out)
		if strings.Contains(out, "#mi(") || strings.Contains(out, "#mitex(") {
			t.Fatalf("unbalanced delimiters must not become math: %q -> %q", src, out)
		}
	}
}

func TestTypstComment_ParagraphsAndCJK(t *testing.T) {
	out := typstComment("第一段：正確使用 $O(n^2)$。\n\n第二段照舊。")
	assertOnlySafeTokens(t, out)
	if !strings.Contains(out, "#parbreak()") {
		t.Fatalf("blank line must become a parbreak: %q", out)
	}
	if !strings.Contains(out, `#mi("O(n^2)")`) {
		t.Fatalf("math inside CJK text must render: %q", out)
	}
	if !strings.Contains(out, "第一段") {
		t.Fatalf("CJK text must survive: %q", out)
	}
}

func TestTypstComment_EmptyIsEmpty(t *testing.T) {
	if out := typstComment(""); out != "" {
		t.Fatalf("empty comment must produce no markup, got %q", out)
	}
}
