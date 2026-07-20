// Package studentid owns the ONE identifier-normalization regime for roster
// student IDs and names (roster-lifecycle plan 2026-07-10, fix 4). It was
// extracted verbatim from internal/scan's OCR matching so that every consumer
// — scan matching, filename ingest fallback, quarantine resolve, orphan manual
// assign, roster import duplicate detection — folds IDs the same way instead
// of each growing its own regime. Imports stdlib + x/text only; everything
// (scan, ingest, httpapi) may import this package, never the reverse.
package studentid

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Normalize folds s into a canonical roster-ID comparison key: NFKC
// normalization (which folds full-width forms like "ｂ１０９０２０６６" down
// to their ASCII equivalents), uppercased, with every non-alphanumeric rune
// (spaces, hyphens, punctuation) stripped. Roster ID formats are never
// hardcoded here — the roster itself is ground truth; this only removes
// incidental OCR/typing noise around whatever the ID actually is.
func Normalize(s string) string {
	if s == "" {
		return ""
	}
	folded := norm.NFKC.String(s)
	var b strings.Builder
	b.Grow(len(folded))
	for _, r := range folded {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// NormalizeName folds s into a canonical name comparison key: NFKC
// normalization, every whitespace rune stripped (including the ideographic
// space U+3000, since Chinese names are often OCR'd or typed with gaps
// between characters), and Latin letters case-folded. CJK characters have
// no case, so they compare exactly once whitespace is removed.
func NormalizeName(s string) string {
	if s == "" {
		return ""
	}
	folded := norm.NFKC.String(s)
	var b strings.Builder
	b.Grow(len(folded))
	for _, r := range folded {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
