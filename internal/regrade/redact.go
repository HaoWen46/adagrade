package regrade

import (
	"regexp"
	"strings"
	"unicode"
)

// Redaction (spec §8, D51 — the identity-XOR-content law for AI re-grade prompts).
// The student's request TEXT is the one piece of free-form student writing that would
// otherwise reach an LLM provider carrying identity. Before the text is assembled into
// a prompt, this pure helper mechanically excises the four things that tie it to a
// person — the roster name, the student id, the roster email, and any regrade+<token>@…
// reply address — and reports COUNTS ONLY. The counts are the only thing a caller may
// log; the text itself (and the identities) are never logged (CLAUDE.md PII rule).
//
// The contested record's original scores/comments come from the DB (the official
// grading record), NOT from this text — so redaction can be aggressive without losing
// grading signal: what remains is the student's argument ("problem 3's base case is
// right"), which is exactly what a stricter re-examination should weigh.

// Identity is the set of exact-match strings to remove, sourced from the roster row +
// the request's own token, never from the request text.
type Identity struct {
	Name      string // roster display name
	StudentID string // external student id (e.g. "b09901001")
	Email     string // current roster email
}

// RedactionCounts is how many occurrences of each identity kind were removed. It is a
// plain numeric struct on purpose (D51): it carries no content, so it is the ONLY thing
// derived from the redaction that is safe to log.
type RedactionCounts struct {
	Name      int `json:"name"`
	StudentID int `json:"student_id"`
	Email     int `json:"email"`
	Token     int `json:"token"`
}

// Total is the sum across all kinds — a convenient single figure for a log line.
func (c RedactionCounts) Total() int { return c.Name + c.StudentID + c.Email + c.Token }

// regradeTokenPattern matches a reply address of the shape regrade+<token>@<host> (the
// per-item mailbox-hash address students reply to). Scrubbed by PATTERN independent of
// the known roster email, since a student may quote the reply-to header verbatim and
// that token must never reach the provider. Local part after the '+' and the host are
// permissive (any non-space, non-'@' run) so provider-specific token/host shapes all
// match; case-insensitive because email is.
var regradeTokenPattern = regexp.MustCompile(`(?i)regrade\+[^\s@]+@[^\s]+`)

// redactedMarker replaces each excised identity occurrence. A fixed opaque token (not
// the original length) so nothing about the removed string leaks via spacing.
const redactedMarker = "[redacted]"

// Redact removes every occurrence of the identity's non-empty fields (case-insensitive,
// exact substring) and every regrade+…@… token from text, returning the scrubbed text
// and the per-kind counts. Empty identity fields are skipped (an empty needle must never
// behave like a wildcard). Order matters only for counting: the token pass runs first so
// a token that happens to contain the roster email substring is attributed to Token, not
// double-counted — but the roster-email pass still runs afterward over what remains.
func Redact(text string, id Identity) (string, RedactionCounts) {
	var counts RedactionCounts

	// 1. regrade+<token>@host — pattern-based, before the email pass.
	text, counts.Token = replaceAllCount(text, regradeTokenPattern)

	// 2-4. Exact-match identity fields (case-insensitive). Longest-first isn't needed:
	// these are distinct kinds, each removed independently over the current text.
	text, counts.Email = replaceFoldCount(text, id.Email)
	text, counts.Name = replaceFoldCount(text, id.Name)
	text, counts.StudentID = replaceFoldCount(text, id.StudentID)

	return text, counts
}

// replaceAllCount replaces every match of re with the marker and returns the count.
func replaceAllCount(s string, re *regexp.Regexp) (string, int) {
	matches := re.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s, 0
	}
	return re.ReplaceAllString(s, redactedMarker), len(matches)
}

// replaceFoldCount removes every case-insensitive occurrence of needle from s, returning
// the result and the number removed. An empty needle is a no-op (never a wildcard).
//
// PII-leak fix: matching happens on a LOWERCASED copy of s, but some runes change byte
// length when lowercased (U+212A KELVIN SIGN 3→1 bytes, U+0130 İ 2→1, U+023A Ⱥ 2→3), so
// a byte offset found in the lowered copy is NOT a valid offset into the original —
// slicing s with lowered offsets shifted every later match and let identity (a student
// id) survive "redaction" nearly intact. The original is therefore NEVER indexed with a
// lowered offset: the lowering pass below records, for every lowered byte, the original
// byte offset of the rune it came from, and every strings.Index hit is translated
// through that table before slicing.
func replaceFoldCount(s, needle string) (string, int) {
	if needle == "" {
		return s, 0
	}
	lowerNeedle := strings.ToLower(needle)

	// Lower s rune-by-rune (unicode.ToLower per rune — exactly what strings.ToLower
	// does) while building origAt: origAt[k] = the original byte offset of the rune
	// that produced lowered byte k. A trailing len(s) sentinel makes the one-past-the-
	// end boundary of a match at the very end translatable too.
	var lowered strings.Builder
	lowered.Grow(len(s))
	origAt := make([]int, 0, len(s)+1)
	for origOff, r := range s {
		lr := unicode.ToLower(r)
		n, _ := lowered.WriteRune(lr)
		for k := 0; k < n; k++ {
			origAt = append(origAt, origOff)
		}
	}
	origAt = append(origAt, len(s))
	lowerS := lowered.String()

	var b strings.Builder
	count := 0
	li := 0 // cursor in lowerS (lowered bytes)
	oi := 0 // cursor in s (original bytes), always == origAt[li]
	for {
		j := strings.Index(lowerS[li:], lowerNeedle)
		if j < 0 {
			b.WriteString(s[oi:])
			break
		}
		// Translate the lowered-space match [li+j, li+j+len(lowerNeedle)) back to
		// original-space offsets before touching s.
		start := origAt[li+j]
		end := origAt[li+j+len(lowerNeedle)]
		b.WriteString(s[oi:start])
		b.WriteString(redactedMarker)
		count++
		li += j + len(lowerNeedle)
		oi = end
	}
	return b.String(), count
}
