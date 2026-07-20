package scan

import (
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ParseProblemRef parses a handwritten problem reference from the problem-ID box:
// an optional prefix (Q, P, 問, 第, #), a number, and an optional suffix (., ), :,
// 題). NFKC folds full-width forms first. Anything else — including trailing
// garbage — fails: the matcher must never guess a problem (spec §6 fail-safe).
// Numbers are capped at 3 digits; an assessment with 1000+ problems is not a
// thing, and long digit runs are OCR noise.
func ParseProblemRef(s string) (int, bool) {
	folded := strings.TrimSpace(norm.NFKC.String(s))
	rs := []rune(folded)
	i := 0
	for i < len(rs) && (isProblemPrefix(rs[i]) || unicode.IsSpace(rs[i])) {
		i++
	}
	j := i
	for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
		j++
	}
	if j == i || j-i > 3 {
		return 0, false
	}
	k := j
	for k < len(rs) && (isProblemSuffix(rs[k]) || unicode.IsSpace(rs[k])) {
		k++
	}
	if k != len(rs) {
		return 0, false
	}
	n, err := strconv.Atoi(string(rs[i:j]))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func isProblemPrefix(r rune) bool {
	switch r {
	case 'Q', 'q', 'P', 'p', '問', '第', '#':
		return true
	}
	return false
}

func isProblemSuffix(r rune) bool {
	switch r {
	case '.', ')', ':', '題':
		return true
	}
	return false
}
