// This file adds the local-OCR extraction helpers used by IdentifyFile's local
// rung (D24): PickID and PickName pull a student-ID candidate and a name
// candidate out of raw OCR lines BEFORE anything reaches Match/NormalizeID.
// This ordering matters — NormalizeID keeps CJK letters (unicode.IsLetter(CJK)
// is true), so if an unsplit line like "B11902156 王小明" were normalized as one
// string, the name would glue onto the ID. Extraction happens first, on
// NFKC-width-folded text, splitting purely on rune class (ASCII alnum vs Han),
// so the two candidates never contaminate each other downstream.
package scan

import (
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/HaoWen46/adagrade/internal/ocr"
)

// isASCIIAlnum reports whether r is an ASCII letter or digit. Deliberately
// ASCII-only (not unicode.IsLetter/IsNumber): full-width forms are folded to
// ASCII by NFKC before this runs, and CJK letters must NOT count as part of an
// ID run — that is exactly the gluing bug PickID exists to avoid.
func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// isHan reports whether r is a Han (CJK ideograph) rune.
func isHan(r rune) bool {
	return unicode.Is(unicode.Han, r)
}

// runCandidate is one maximal run found on one OCR line, carried alongside the
// line's confidence and its position so PickID/PickName can apply the tie
// rules (longest run wins; ties broken by higher confidence, then by
// whichever run appears first).
type runCandidate struct {
	text       string
	confidence float64
	order      int // monotonic across all lines/runs in encounter order
}

// better reports whether candidate a should be preferred over b under the
// PickID/PickName tie rules: longer run wins; equal length falls back to
// higher confidence; equal confidence falls back to earlier encounter order.
func (a runCandidate) better(b runCandidate) bool {
	al, bl := len([]rune(a.text)), len([]rune(b.text))
	if al != bl {
		return al > bl
	}
	if a.confidence != b.confidence {
		return a.confidence > b.confidence
	}
	return a.order < b.order
}

// extractRuns scans NFKC-folded text for maximal runs of runes satisfying
// keep, breaking on ANY rune that fails the predicate (no gluing across
// spaces, hyphens, or punctuation — deliberately stricter than NormalizeID).
func extractRuns(folded string, keep func(rune) bool) []string {
	var runs []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			runs = append(runs, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range folded {
		if keep(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return runs
}

// pickLongest applies the shared tie rules across every run extracted from
// every line, returning the winning run's text (or "" when none clears
// minLen).
func pickLongest(lines []ocr.Line, keep func(rune) bool, minLen int) string {
	var best runCandidate
	haveBest := false
	order := 0
	for _, line := range lines {
		folded := norm.NFKC.String(line.Text)
		for _, run := range extractRuns(folded, keep) {
			cand := runCandidate{text: run, confidence: line.Confidence, order: order}
			order++
			if len([]rune(run)) < minLen {
				continue
			}
			if !haveBest || cand.better(best) {
				best = cand
				haveBest = true
			}
		}
	}
	if !haveBest {
		return ""
	}
	return best.text
}

// PickID extracts a student-ID candidate from raw OCR lines: each line's text
// is NFKC width-folded (so full-width forms like "Ｂ１１９０２１５６" become
// ASCII first), then split into maximal runs of ASCII letters+digits — a run
// breaks on ANY non-ASCII-alnum rune (space, hyphen, CJK, punctuation). The
// longest run with length >= 5 wins across all lines; ties go to the
// higher-confidence line, then to whichever run was encountered first. Returns
// "" when no run clears the length-5 floor.
func PickID(lines []ocr.Line) string {
	return pickLongest(lines, isASCIIAlnum, 5)
}

// PickName extracts a name candidate from raw OCR lines: the longest maximal
// run of Han runes with length >= 2, using the same tie rules as PickID.
// Returns "" when no run clears the length-2 floor.
func PickName(lines []ocr.Line) string {
	return pickLongest(lines, isHan, 2)
}
