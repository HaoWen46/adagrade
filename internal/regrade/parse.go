package regrade

import (
	"regexp"
	"strconv"
	"strings"
)

// Block is one contested problem parsed out of a student's regrade reply — the
// verbatim text between a matched <pN> ... </pN> pair, N as printed on the tag.
type Block struct {
	Number int    // the problem number as written in the tag, e.g. 4 for <p4>.
	Text   string // everything between the open and close tags, verbatim.
}

// pTagPattern matches a <pN> or </pN> tag tolerantly (spec §2 D55 as amended
// 2026-07-10): each tag character accepts its full-width variant (＜ ／ ｐ ＞,
// digits ０-９) and 'p' accepts uppercase, in any mix — CJK input methods emit
// these forms and the widened match keeps such replies filing. Only the TAG is
// tolerant: the captured number goes through normalizeTagDigits so ｐ１ names
// the same problem as p1, while the complaint body between tags stays verbatim
// (never normalized). Everything else — inner whitespace, non-p letters, other
// scripts (a Cyrillic р is still a different rune) — still fails to match, per
// the strict-format contract. The close-marker group is `([/／]?)` rather than
// an optional group so it always participates (empty when absent) and the
// submatch indices stay valid.
var pTagPattern = regexp.MustCompile(`[<＜]([/／]?)[pPｐＰ]([0-9０-９]+)[>＞]`)

// normalizeTagDigits maps full-width digits ０-９ to ASCII 0-9 so a captured
// tag number parses with strconv.Atoi regardless of width. It is applied ONLY
// to tag numbers, never to complaint text (D55: body text is verbatim).
func normalizeTagDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '０' && r <= '９' {
			return '0' + (r - '０')
		}
		return r
	}, s)
}

// ParseBlocks extracts every validly-tagged problem complaint from a regrade
// reply body, per spec §2 (D54/D55):
//
//   - '>'-quoted lines are stripped BEFORE any tag matching, so a reply that
//     merely quotes our own template (mail-client "on ... wrote:" quoting) can
//     never self-match.
//   - Tags match tolerantly (D55 as amended 2026-07-10): full-width brackets/
//     slash/'p'/digits and uppercase 'P' are accepted in any mix, and full-width
//     tag digits normalize to the same problem numbers as ASCII. Any other
//     deviation — inner spaces, non-p letters, other scripts — still fails to
//     match with no best-effort recovery and no notice (D55). Normalization
//     applies to the tags ONLY; the complaint text between them is verbatim.
//   - An <pN> with no matching </pN> (or vice versa) is silently ignored.
//   - Duplicate <pN> blocks concatenate, in arrival order, into one Block per
//     distinct N — first-seen order determines the returned slice's order.
//   - Text outside all recognized tag pairs (greetings, signatures, stray
//     quote fragments) is ignored.
//
// Unknown-N filtering (no such problems.number in the token's assessment) is
// NOT this function's job — that is the translation layer (spec §3), which
// needs the assessment context this pure parser deliberately does not have.
func ParseBlocks(text string) []Block {
	stripped := stripQuotedLines(text)

	matches := pTagPattern.FindAllStringSubmatchIndex(stripped, -1)
	if len(matches) == 0 {
		return nil
	}

	var (
		order []int // first-seen order of problem numbers with >=1 valid block
		texts = map[int]string{}
	)

	// Walk the tag matches pairing each open tag with the NEXT tag encountered
	// (by position) that is a close tag for the SAME number. Any open tag whose
	// next same-number tag isn't a close (or has no following tag at all) is
	// unclosed and ignored, per spec. We scan left to right, and for every open
	// tag search forward for its closing partner among the remaining matches.
	used := make([]bool, len(matches)) // matches already consumed as a close tag
	for i, m := range matches {
		isClose := stripped[m[2]:m[3]] != "" // "/" or full-width "／"
		if isClose {
			continue // close tags are only ever consumed by a preceding open tag
		}
		n, err := strconv.Atoi(normalizeTagDigits(stripped[m[4]:m[5]]))
		if err != nil {
			continue // unreachable given the pattern's digit class, but fail closed
		}

		// Find the next unused tag (by position) with the same number that IS a
		// close tag — that is this open tag's partner. If the next same-number
		// tag we encounter is itself an open tag (or none exists), this open
		// tag is unclosed and is ignored (no nesting/redefinition support, per
		// the strict format — the spec shows only flat, non-nested blocks).
		closeIdx := -1
		for j := i + 1; j < len(matches); j++ {
			if used[j] {
				continue
			}
			mj := matches[j]
			jNum, err := strconv.Atoi(normalizeTagDigits(stripped[mj[4]:mj[5]]))
			if err != nil || jNum != n {
				continue
			}
			jIsClose := stripped[mj[2]:mj[3]] != ""
			if jIsClose {
				closeIdx = j
			}
			break // first same-number tag encountered decides open/unclosed
		}
		if closeIdx == -1 {
			continue // unclosed — silently ignored
		}
		used[closeIdx] = true

		openEnd := m[1]                    // end of the <pN> open tag
		closeStart := matches[closeIdx][0] // start of the </pN> close tag
		body := stripped[openEnd:closeStart]

		if _, seen := texts[n]; !seen {
			order = append(order, n)
		}
		texts[n] += body
	}

	if len(order) == 0 {
		return nil
	}
	blocks := make([]Block, 0, len(order))
	for _, n := range order {
		blocks = append(blocks, Block{Number: n, Text: texts[n]})
	}
	return blocks
}

// stripQuotedLines removes every line that is mail-client-quoted BEFORE any tag
// matching runs, per spec §2: "our own template quoted in the reply must never
// self-match." A line is quoted if its first byte is '>' — quoting is
// conventionally column-0, and the common single- and multi-level ("> >", ">>")
// forms both start with '>' there. The removed line's content is dropped
// entirely (not just the marker) since a quoted copy of our template must never
// contribute matchable tag text.
func stripQuotedLines(text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, ">") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
