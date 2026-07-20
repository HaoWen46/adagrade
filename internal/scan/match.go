// Package scan proposes a roster match for a scanned exam page from its OCR'd
// identity-box contents (student ID + name, names usually Chinese) and its
// original filename (docs/DECISIONS.md D21). Matching is pure Go and
// table-tested: model/OCR output is never trusted directly into an
// assignment (same philosophy as D4's snap/clamp) — every match only ever
// *proposes*; a human confirms every proposal before it becomes an
// assignment (design doc §6, §7).
package scan

import (
	"strings"

	"github.com/HaoWen46/adagrade/internal/studentid"
)

// RosterEntry is one candidate student a scanned page could belong to.
// Callers are responsible for excluding withdrawn students (D23) before
// passing a roster slice to matchOCRID/matchOCRName — RosterEntry carries no
// withdrawn flag.
type RosterEntry struct {
	ID         int64
	ExternalID string
	Name       string
}

// StudentMatch is the page-level student resolution (spec §6, D64).
type StudentMatch struct {
	AgreedID       int64  // both rungs independently resolve here; 0 = no auto-assign
	ProposedID     int64  // partial signal for orphan pre-fill; 0 = none
	ProposalSource string // "ocr_agree" | "ocr_id" | "ocr_id_near" | "ocr_name" | "ocr_disagree" | ""
}

// matchOCRID reports the roster db id whose external id is the UNIQUE exact match for
// NormalizeID(ocrID), and whether a match was found. Two or more roster entries whose
// external ids collide under NormalizeID (a roster predating the import-time
// normalization guard) make the id ambiguous, so this fails rather than resolving to
// whichever row happens to come first — mirroring matchOCRName's duplicate-name refusal
// and ingest.ResolveStudentByExternalID's ErrAmbiguousStudentID posture.
func matchOCRID(ocrID string, roster []RosterEntry) (int64, bool) {
	normID := NormalizeID(ocrID)
	if normID == "" {
		return 0, false
	}
	var candidate int64
	found := 0
	for _, r := range roster {
		if NormalizeID(r.ExternalID) == normID {
			candidate = r.ID
			found++
			if found > 1 {
				return 0, false // ambiguous: 2+ roster ids share this normalized id
			}
		}
	}
	if found != 1 {
		return 0, false
	}
	return candidate, true
}

// matchOCRIDNear reports the roster db id whose external ID is the UNIQUE
// entry at edit distance exactly 1 from NormalizeID(ocrID) — a "near miss":
// the OCR read is one character off a single roster ID and no other. Two or
// more roster IDs at distance 1 make the read ambiguous (sequential student
// IDs routinely differ by one digit), so this fails rather than guessing. Any
// roster ID at distance 0 also fails: an exact hit is the exact rung's
// business, never a near miss.
//
// A near miss is only ever a PROPOSAL for the orphan queue (see MatchStudent)
// — it can never contribute to AgreedID/auto-assign.
func matchOCRIDNear(ocrID string, roster []RosterEntry) (int64, bool) {
	normID := NormalizeID(ocrID)
	if normID == "" {
		return 0, false
	}
	var candidate int64
	found := 0
	for _, r := range roster {
		switch levenshtein(normID, NormalizeID(r.ExternalID)) {
		case 0:
			return 0, false // exact hit: not this rung's business
		case 1:
			candidate = r.ID
			found++
			if found > 1 {
				return 0, false // ambiguous: 2+ roster IDs one edit away
			}
		}
	}
	if found != 1 {
		return 0, false
	}
	return candidate, true
}

// matchOCRIDEmbedded reports the roster db id whose external ID appears as a
// verbatim substring of NormalizeID(ocrID), UNIQUELY — the live-observed
// box-edge artifact shape, where OCR glues junk around an otherwise-perfect
// read (e.g. "1B11902003" for B11902003). Every character of the candidate ID
// is present verbatim, which is stronger evidence than an edit-distance
// mutation, so this rung sits ABOVE distance-1 in the proposal ladder — but
// it is still only ever a PROPOSAL (see MatchStudent's firewall comment).
//
// Refusals, all in the spirit of "never guess":
//   - the read exactly equals ANY roster ID: the exact rung's business, never
//     embedded (mirrors matchOCRIDNear's distance-0 refusal);
//   - two or more roster IDs appear as substrings: ambiguous;
//   - degenerate rosters — the one contained ID is itself a substring of a
//     DIFFERENT roster ID (e.g. "B0100" inside "AB0100C"): the junk-wrapped
//     read could equally be a partial/corrupted read of the longer ID, so
//     neither can be preferred.
func matchOCRIDEmbedded(ocrID string, roster []RosterEntry) (int64, bool) {
	normID := NormalizeID(ocrID)
	if normID == "" {
		return 0, false
	}
	norms := make([]string, len(roster))
	for i, r := range roster {
		norms[i] = NormalizeID(r.ExternalID)
	}
	var candidate int64
	var candidateNorm string
	found := 0
	for i, r := range roster {
		rn := norms[i]
		if rn == "" {
			continue
		}
		if rn == normID {
			return 0, false // exact hit: not this rung's business
		}
		if strings.Contains(normID, rn) {
			candidate = r.ID
			candidateNorm = rn
			found++
			if found > 1 {
				return 0, false // ambiguous: 2+ roster IDs embedded in this read
			}
		}
	}
	if found != 1 {
		return 0, false
	}
	// Degenerate-roster guard: if the contained ID also lives inside another
	// roster ID, a junk-wrapped read containing it could equally be intended
	// as (a partial read of) that longer ID — refuse rather than pick.
	for i := range roster {
		// Skip the candidate itself. (A second entry with the same normalized
		// ID would already have tripped found > 1 above, so norm equality
		// identifies the candidate uniquely here.)
		if norms[i] == candidateNorm {
			continue
		}
		if strings.Contains(norms[i], candidateNorm) {
			return 0, false
		}
	}
	return candidate, true
}

// matchOCRIDFar reports the roster db id whose external ID is the UNIQUE
// entry at edit distance exactly 2 from NormalizeID(ocrID), and ONLY when no
// other roster ID is within distance 3 — a uniqueness gap >= 2. Two edits is
// deep into wrong-student territory on real rosters (sequential student IDs
// mutually differ by 1-2 digits), so the gap gate makes this rung refuse on
// crowded neighborhoods: any OTHER roster ID at distance <= 3 kills the
// proposal. Any roster ID at distance 0 or 1 also refuses outright — those
// reads belong to the exact/embedded/near rungs, which by the same rule can
// never be outranked by a distance-2 candidate.
//
// Like every fuzzy rung, a hit here is only ever a PROPOSAL for the orphan
// queue (see MatchStudent) — it can never contribute to AgreedID/auto-assign.
func matchOCRIDFar(ocrID string, roster []RosterEntry) (int64, bool) {
	normID := NormalizeID(ocrID)
	if normID == "" {
		return 0, false
	}
	var candidate int64
	found := 0
	for _, r := range roster {
		switch d := levenshtein(normID, NormalizeID(r.ExternalID)); {
		case d < 2:
			return 0, false // exact or distance-1: earlier rungs' business
		case d == 2:
			candidate = r.ID
			found++
			if found > 1 {
				return 0, false // ambiguous: 2+ roster IDs two edits away
			}
		case d == 3:
			return 0, false // another ID within distance 3: gap < 2, refuse
		}
	}
	if found != 1 {
		return 0, false
	}
	return candidate, true
}

// matchOCRIDFuzzy runs the below-exact proposal ladder in strength order —
// embedded-unique (every candidate character present verbatim), then
// distance-1-unique, then gated distance-2 — returning the first rung's hit.
// All three surface as the SAME proposal source, "ocr_id_near" (migration
// 0033's CHECK constraint on scan_pages.proposal_source admits no new
// values), and all three are proposal-only.
func matchOCRIDFuzzy(ocrID string, roster []RosterEntry) (int64, bool) {
	if id, ok := matchOCRIDEmbedded(ocrID, roster); ok {
		return id, ok
	}
	if id, ok := matchOCRIDNear(ocrID, roster); ok {
		return id, ok
	}
	return matchOCRIDFar(ocrID, roster)
}

// matchOCRName reports the roster db id whose name is the unique exact
// normalized-name match for ocrName, and whether a match was found. Duplicate
// names in the roster make the name ambiguous, so this fails rather than
// guessing.
func matchOCRName(ocrName string, roster []RosterEntry) (int64, bool) {
	normName := NormalizeName(ocrName)
	if normName == "" {
		return 0, false
	}

	var candidate RosterEntry
	found := 0
	for _, r := range roster {
		if NormalizeName(r.Name) == normName {
			candidate = r
			found++
			if found > 1 {
				return 0, false // ambiguous duplicate name
			}
		}
	}
	if found != 1 {
		return 0, false
	}
	return candidate.ID, true
}

// MatchStudent resolves one page's student from the two independent OCR reads
// (spec §6, D64): auto-assign eligibility (AgreedID) requires the ID rung and the
// name rung to independently resolve to the SAME live student, and the ID rung
// that feeds AgreedID is exact-only — one OCR digit error is exactly how a page
// lands on the wrong real student, so fuzzy ID matching NEVER contributes to
// auto-assign. Fuzzy matching does exist below that bar, as a ladder ordered
// by evidence strength (matchOCRIDFuzzy): exact -> embedded-unique (one
// roster ID verbatim inside the read, junk around it) -> distance-1-unique ->
// gated distance-2. Whichever rung fires becomes a human-confirmed PROPOSAL
// ("ocr_id_near") for the orphan queue. Anything less than exact agreement
// yields at most such a pre-fill proposal; a human confirms every proposal.
func MatchStudent(ocrID, ocrName string, roster []RosterEntry) StudentMatch {
	idHit, idOK := matchOCRID(ocrID, roster)
	nameHit, nameOK := matchOCRName(ocrName, roster)
	switch {
	case idOK && nameOK && idHit == nameHit:
		return StudentMatch{AgreedID: idHit, ProposedID: idHit, ProposalSource: "ocr_agree"}
	case idOK && nameOK:
		// ID says one student, name says another — possibly a student who wrote
		// someone else's ID. Flag distinctly, pre-fill nothing (spec §6).
		return StudentMatch{ProposalSource: "ocr_disagree"}
	case idOK:
		return StudentMatch{ProposedID: idHit, ProposalSource: "ocr_id"}
	case nameOK:
		if fuzzyHit, fuzzyOK := matchOCRIDFuzzy(ocrID, roster); fuzzyOK && fuzzyHit == nameHit {
			// The name box and the fuzzy ID rung concur on the same student.
			// That upgrades confidence in the PROPOSAL — never into AgreedID:
			// the exact-only auto-assign rule is the wrong-student firewall
			// (one OCR digit error is exactly how a page lands on the wrong
			// real student, and a concurring name doesn't prove the ID read —
			// the same wrong digit that missed the roster may equally be the
			// digit that was truly written, i.e. someone else's ID). This
			// holds even for the embedded rung, whose candidate's every
			// character IS present verbatim in the read: the junk around it
			// proves the read is corrupted, and box-edge artifacts that ADD
			// characters can just as well EAT them — a read containing roster
			// ID X verbatim may still be a mangled read of a longer/different
			// written ID, so verbatim containment never clears the
			// auto-assign bar either. Surface it as "ocr_id_near" so the
			// orphan card demands the TA compare the written digits, not as
			// "ocr_name" (whose "ID box unreadable" copy would be false — the
			// box WAS read, close to but not exactly a roster ID).
			return StudentMatch{ProposedID: nameHit, ProposalSource: "ocr_id_near"}
		}
		// An exact, unique name hit outranks a fuzzy ID pointing elsewhere (or
		// nowhere): propose the name match exactly as before the fuzzy
		// rungs existed.
		return StudentMatch{ProposedID: nameHit, ProposalSource: "ocr_name"}
	default:
		// Fuzzy ladder: no exact ID hit, no name resolution. A unique
		// embedded / distance-1 / gated distance-2 roster ID becomes a
		// proposal ONLY — AgreedID is never set from a fuzzy rung (see the
		// firewall comment above).
		if fuzzyHit, fuzzyOK := matchOCRIDFuzzy(ocrID, roster); fuzzyOK {
			return StudentMatch{ProposedID: fuzzyHit, ProposalSource: "ocr_id_near"}
		}
		return StudentMatch{}
	}
}

// NormalizeID folds s into the canonical roster-ID comparison key. The
// implementation lives in internal/studentid (the ONE normalization regime,
// roster-lifecycle plan 2026-07-10) — this wrapper stays so scan's matching
// code keeps its local vocabulary and existing callers/tests are undisturbed.
func NormalizeID(s string) string {
	return studentid.Normalize(s)
}

// NormalizeName folds s into the canonical name comparison key (see
// studentid.NormalizeName — moved there unchanged).
func NormalizeName(s string) string {
	return studentid.NormalizeName(s)
}

// levenshtein returns the edit distance between a and b, counted over
// runes (not bytes), so multi-byte CJK/full-width characters each count as
// a single edit unit.
func levenshtein(a, b string) int {
	ra := []rune(a)
	rb := []rune(b)
	la, lb := len(ra), len(rb)

	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}
