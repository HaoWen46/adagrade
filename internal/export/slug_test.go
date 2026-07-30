package export

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSlug_LowercasesCollapsesAndTrims(t *testing.T) {
	cases := map[string]string{
		"Algorithms Midterm 2":            "algorithms-midterm-2",
		"  Midterm #2: Graphs & Trees!  ": "midterm-2-graphs-trees",
		"a---b":                           "a-b",
		"-- hello --":                     "hello",
		"UPPER lower 123":                 "upper-lower-123",
		"tabs\tand\nnewlines":             "tabs-and-newlines",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSlug_DropsNonASCIIRatherThanShippingAnUnextractableArchive pins the
// trade-off documented on Slug. macOS's bundled Info-ZIP refuses an entire
// archive whose entry names carry CJK, so the root directory is deliberately
// ASCII even though the target corpus is zh-Hant. The mixed case still keeps
// whatever ASCII the operator wrote.
func TestSlug_DropsNonASCIIRatherThanShippingAnUnextractableArchive(t *testing.T) {
	cases := map[string]string{
		"演算法 Midterm 2": "midterm-2",
		"線性代數期中考":       "assessment", // nothing ASCII survives: the fallback
		"微積分 期中考（2026）": "2026",
		"Café Test":     "caf-test", // é is dropped, not transliterated
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlug_IsAlwaysPureASCII(t *testing.T) {
	for _, in := range []string{"線性代數期中考", "演算法 Midterm 2", "Café", "Ω mega", "🎓 finals"} {
		got := Slug(in)
		for i := 0; i < len(got); i++ {
			if got[i] > 0x7f {
				t.Errorf("Slug(%q) = %q contains a non-ASCII byte at %d", in, got, i)
				break
			}
		}
	}
}

// TestSlug_NeverProducesAPathOrATraversal — the assessment name is
// operator-supplied text that becomes a directory name inside the archive.
func TestSlug_NeverProducesAPathOrATraversal(t *testing.T) {
	for _, in := range []string{
		"", "   ", "...", "..", ".", "///", "../../etc/passwd", "/", "\\", "~", "\x00",
	} {
		got := Slug(in)
		if got == "" {
			t.Errorf("Slug(%q) = %q; an empty slug would produce an entry name starting with '-'", in, got)
		}
		if strings.ContainsAny(got, `/\.`+"\x00") {
			t.Errorf("Slug(%q) = %q; must contain no path separators, dots, or NULs", in, got)
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("Slug(%q) = %q; must not begin or end with a dash", in, got)
		}
	}
}

func TestSlug_FallsBackWhenNothingSurvives(t *testing.T) {
	for _, in := range []string{"!!! ??? ###", "", "線性代數"} {
		if got := Slug(in); got != slugFallback {
			t.Errorf("Slug(%q) = %q, want the %q fallback", in, got, slugFallback)
		}
	}
}

func TestSlug_TruncatesRunawayNames(t *testing.T) {
	long := strings.Repeat("verylongword ", 40)
	got := Slug(long)
	if n := utf8.RuneCountInString(got); n > maxSlugRunes {
		t.Errorf("Slug truncated to %d runes, want at most %d", n, maxSlugRunes)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("Slug(%q…) = %q; truncation must not leave a trailing dash", long[:20], got)
	}
	if Slug(long) != got {
		t.Error("Slug must be a pure function")
	}
}

func TestSlug_IsIdempotent(t *testing.T) {
	for _, in := range []string{"Algorithms Midterm 2", "微積分 期中考（2026）", "!!!", "線性代數期中考"} {
		once := Slug(in)
		if twice := Slug(once); twice != once {
			t.Errorf("Slug is not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}
