package offline

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/HaoWen46/adagrade/internal/assign"
	"github.com/HaoWen46/adagrade/internal/localocr"
	"github.com/HaoWen46/adagrade/internal/roster"
	"github.com/HaoWen46/adagrade/internal/studentid"
)

// The evidence weights. A page's score is
//
//	S = 0.45*P(student | id crop) + 0.30*P(student | name crop) + 0.25*P(problem | problem crop)
//
// The ID carries the most weight because it is the only field that is unique by
// construction: two students share a name far more often than an ID, and a
// misread digit is recoverable by the closed-set scorer while a misread name
// often is not. The problem number is worth the least because getting it wrong
// misfiles an answer, while getting the student wrong grades the wrong person.
//
// THE WEIGHTS ARE NEVER RENORMALIZED. When a field cannot be read its term is
// zero and the total simply cannot reach 1 — a page with an unreadable ID
// caps at 0.55, which is exactly the intent. Renormalizing over the fields that
// WERE read would let a page with no ID at all reach the same score as a fully
// read one on the strength of a name, and names are the field students share.
// That is the single most likely way this mode could hand one student's answers
// to another, so the zero is load-bearing, not an oversight.
const (
	WeightStudentID = 0.45
	WeightName      = 0.30
	WeightProblem   = 0.25
)

// Match statuses and reasons, as written to the report.
const (
	// MethodLattice is the only matching method this mode has: closed-set CTC
	// lattice scoring against the roster. The field exists so a report from a
	// future method (a QR code, a barcode) is still readable by the same tools.
	MethodLattice = "lattice"

	StatusAuto      = "auto"      // the solver's assignment agrees with the page's own best guess
	StatusForced    = "forced"    // the global assignment moved this page off its best guess
	StatusUnmatched = "unmatched" // set aside; see Reason

	ReasonSurplus    = "surplus"     // no cell left: more pages than roster×problems slots
	ReasonLowScore   = "low-score"   // the assigned cell scored below --min-score
	ReasonAmbiguous  = "ambiguous"   // another STUDENT scored within --min-margin
	ReasonIDConflict = "id-conflict" // the id crop LEGIBLY reads a different student's id
)

// The legible-ID veto's three gates. See vetoLegibleIDConflict.
const (
	// idVetoMinRunLength: fewer than five characters cannot identify anybody.
	// "Q1", a stray page number and a two-digit seat number all read as short
	// runs, and vetoing on them would throw away correctly matched pages.
	idVetoMinRunLength = 5
	// idVetoMinConfidence: the greedy decode has to be a READING, not a guess.
	// Handwriting that the recognizer is unsure of is exactly what the lattice
	// scorer is better at than greedy decoding, so a low-confidence line must
	// never be allowed to overrule it.
	idVetoMinConfidence = 0.90
	// idVetoMinDistance mirrors internal/scan's proposal rungs, where an edit
	// distance of 1 or 2 from a roster id is a NEAR MISS of that id (a misread
	// digit) and 3 is a different identity. Below this, vetoing would discard
	// pages whose id was simply read imperfectly.
	idVetoMinDistance = 3
)

// PageRead pairs a rendered page with what the local OCR read off it. It is the
// matcher's input unit: matching is global across pages, so the whole set
// arrives at once.
type PageRead struct {
	Page Page
	ID   Identity
}

// MatchResult is one page's outcome, and the row the report writes.
//
// Score/ScoreID/ScoreName/ScoreProblem describe the ASSIGNED cell for a matched
// page and the page's own best cell for an unmatched one, so the report always
// shows what the match would have been — an operator resolving an unmatched
// page needs to see the candidate it was closest to. One consequence to read
// deliberately: a page rejected by --min-score reports its ARGMAX score, which
// can sit above the threshold that rejected it, because the threshold is
// applied to the cell the solver actually assigned. The three Score* fields are
// the WEIGHTED terms, so they sum to Score and the report can be audited for
// the never-renormalize rule without re-running anything.
//
// Margin always describes the page's own best cell (see studentMargin), for
// matched and unmatched rows alike: it answers "could this page be a different
// student", which is a property of the page's evidence rather than of where the
// solver put it.
type MatchResult struct {
	Page        Page
	StudentID   string // empty when unmatched
	StudentName string // empty when unmatched
	Problem     int    // 0 when unmatched

	Score        float64
	ScoreID      float64
	ScoreName    float64
	ScoreProblem float64
	Margin       float64

	Method string
	Status string
	Reason string
}

// cellIndex packs a (student, problem) pair into one column of the cost matrix.
// Students own contiguous runs of problems: student 0 holds cells
// [0, problems), student 1 holds [problems, 2*problems), and so on. problem is
// 1-based, matching what is written on the page and printed in the report.
func cellIndex(student, problem, problems int) int { return student*problems + (problem - 1) }

// cellStudent and cellProblem unpack cellIndex. Every consumer must go through
// these rather than re-deriving the arithmetic: an inverted pair would assign
// pages to plausible-looking wrong students without any other symptom.
func cellStudent(cell, problems int) int { return cell / problems }
func cellProblem(cell, problems int) int { return cell%problems + 1 }

// components is one cell's score broken into the terms that make it up.
type components struct {
	Score   float64
	ID      float64
	Name    float64
	Problem float64
}

// cellComponents applies the weights. An unread field contributes exactly zero
// — see the weights' doc comment for why this must never become a
// renormalization.
func cellComponents(idPost, namePost, probPost float64, idRead, nameRead, probRead bool) components {
	c := components{}
	if idRead {
		c.ID = WeightStudentID * idPost
	}
	if nameRead {
		c.Name = WeightName * namePost
	}
	if probRead {
		c.Problem = WeightProblem * probPost
	}
	c.Score = c.ID + c.Name + c.Problem
	return c
}

// MatchPages force-matches every page to a (student, problem) cell.
//
// The assignment is GLOBAL, not per page. Each page's own best guess is only a
// starting point: two pages cannot both be student 5's problem 2, and the
// minimum-cost assignment over the whole batch resolves those collisions in the
// way that costs the least total confidence, rather than letting whichever page
// was rendered first take the cell. A page the solver moves off its own best
// guess is reported as "forced" precisely because that is the case a human
// should look at.
//
// Pages that survive the solver still face two floors. --min-score rejects a
// page whose assigned cell is not supported by the evidence at all (the uniform
// -garbage case scores 0.1375, below the 0.15 default). --min-margin rejects a
// page that another STUDENT explains nearly as well. Both produce unmatched
// rows rather than errors: an offline run's job is to place what it can and be
// honest about the rest.
//
// A batch where NOTHING could be placed is not an error here either: every page
// still gets a row saying why it was set aside. The whole-run verdict — zero
// matched pages is a failed run (*NoMatchError, exit 9) — belongs to Run, which
// reaches it only after those rows are on disk, because they are the diagnosis.
//
// The returned slice is parallel to pages, so callers can zip it back against
// their own bookkeeping.
func MatchPages(pages []PageRead, rows []roster.Row, problems int, cs localocr.Charset, minScore, minMargin float64) ([]MatchResult, error) {
	if problems < 1 {
		return nil, newUsageError(nil, "--problems must be at least 1, got %d", problems)
	}
	if len(rows) == 0 {
		return nil, newRosterError(nil, "the roster has no rows: there is nothing to match pages against")
	}
	if len(pages) == 0 {
		return []MatchResult{}, nil
	}

	// Candidate spellings are built once for the whole run: the roster does not
	// change between pages, and NFKC folding a few hundred names per page would
	// dominate the matcher's runtime.
	idTargets := make([][]string, len(rows))
	nameTargets := make([][]string, len(rows))
	for i, row := range rows {
		idTargets[i] = idVariants(row)
		nameTargets[i] = nameVariants(row.Name)
	}
	probTargets := make([][]string, problems)
	for q := 1; q <= problems; q++ {
		probTargets[q-1] = problemVariants(q)
	}

	cells := len(rows) * problems
	cost := make([][]float64, len(pages))
	scores := make([][]components, len(pages))
	argmax := make([]int, len(pages))

	for p, pr := range pages {
		idRes := ScoreField(pr.ID.Fields[KindStudentID], cs, idTargets)
		nameRes := ScoreField(pr.ID.Fields[KindName], cs, nameTargets)
		probRes := ScoreField(pr.ID.Fields[KindProblemID], cs, probTargets)

		row := make([]components, cells)
		costRow := make([]float64, cells)
		best := -1
		for s := range rows {
			for q := 1; q <= problems; q++ {
				cell := cellIndex(s, q, problems)
				c := cellComponents(
					posteriorOf(idRes, s), posteriorOf(nameRes, s), posteriorOf(probRes, q-1),
					idRes.Read, nameRes.Read, probRes.Read,
				)
				row[cell] = c
				// Minimum cost is maximum score.
				costRow[cell] = -c.Score
				if best < 0 || c.Score > row[best].Score {
					best = cell
				}
			}
		}
		scores[p] = row
		cost[p] = costRow
		argmax[p] = best
	}

	solution, err := assign.Solve(cost)
	if err != nil {
		// Deliberately UNTYPED (exit 1). assign.Solve rejects exactly three
		// things — a ragged matrix, a zero-width one, and a non-finite cost —
		// and all three are bugs in the loop above, not inputs the operator can
		// fix. Claiming a roster or a scan exit code would send someone to edit
		// a file that is not the problem.
		return nil, fmt.Errorf("offline: assignment failed over %d pages and %d cells: %w", len(pages), cells, err)
	}

	results := make([]MatchResult, len(pages))
	for p, pr := range pages {
		results[p] = classify(pr, scores[p], argmax[p], solution[p], rows, problems, minScore, minMargin)
		vetoLegibleIDConflict(&results[p], pr.ID.Fields[KindStudentID])
	}
	return results, nil
}

// vetoLegibleIDConflict demotes a matched page whose identity crop LEGIBLY reads
// an id that is not the assigned student's.
//
// It exists because closed-set scoring cannot say "nobody". Every posterior is
// normalized over the roster, so a page carrying an id that belongs to no
// student on it — a visiting student, a different section's sheet, a typo'd
// self-written id — still concentrates its mass on whichever roster id is
// nearest, and comes out looking as confident as a true match. Neither
// --min-score nor --min-margin can catch that: both measure the assigned cell
// against the OTHER CELLS, and the whole field is wrong together. The observed
// case is the demo pile's B99999999 page, which matched at score 0.697 with a
// margin of 0.450 against a student it has nothing to do with.
//
// The check is deliberately not part of the scoring: it uses the greedy decode
// (LineLattice.Text), which is a different, independent view of the same pixels
// — the lattice says "which roster entry best explains these strokes", the
// greedy decode says "what do these strokes actually spell". A conflict between
// the two is the signal, and folding it into the score would destroy it.
//
// All three gates must fire, and each guards a different false positive; see the
// idVeto* constants. A vetoed row names NOBODY afterwards (every later stage
// keys off Status and StudentID), but keeps its scores, because "this page
// looked confident and was thrown away anyway" is precisely what the operator
// has to see in the report.
func vetoLegibleIDConflict(res *MatchResult, f FieldLattices) {
	if res.Status == StatusUnmatched || res.StudentID == "" {
		return
	}
	line, ok := bestConfidenceLine(f.Lines)
	if !ok || line.Confidence < idVetoMinConfidence {
		return
	}
	read := longestIDRun(line.Text)
	if len([]rune(read)) < idVetoMinRunLength {
		return
	}
	if levenshtein(read, studentid.Normalize(res.StudentID)) < idVetoMinDistance {
		return
	}
	res.Status, res.Reason = StatusUnmatched, ReasonIDConflict
	res.StudentID, res.StudentName, res.Problem = "", "", 0
}

// bestConfidenceLine is the line the veto reads, and it is the most confident
// one rather than the first: a crop of a header box often splits into the
// printed label and the written value, and only one of them is an id.
func bestConfidenceLine(lines []localocr.LineLattice) (localocr.LineLattice, bool) {
	best, found := localocr.LineLattice{}, false
	for _, l := range lines {
		if !found || l.Confidence > best.Confidence {
			best, found = l, true
		}
	}
	return best, found
}

// longestIDRun returns the longest run of ASCII letters and digits in s, under
// studentid.Normalize's folding rules: NFKC first (so a full-width Ｂ１１ reads
// as B11) and uppercased.
//
// It applies those rules per rune rather than calling Normalize, and that is the
// whole point of the function: Normalize DELETES separators, which would fuse a
// whole header strip into one run — "B11902001 abc Q1" becomes "B11902001ABCQ1",
// an id that matches nobody at any distance and would veto every band-mode page.
// Here a space, a punctuation mark or a Chinese character ENDS a run instead.
// That matters because the field being read is not always just an id: with
// --id-band the "student_id" field is the entire top strip, and even a drawn box
// crop picks up its printed label.
func longestIDRun(s string) string {
	best, run := []rune(nil), []rune(nil)
	flush := func() {
		if len(run) > len(best) {
			best = run
		}
		run = nil
	}
	for _, r := range norm.NFKC.String(s) {
		u := unicode.ToUpper(r)
		if (u >= 'A' && u <= 'Z') || (u >= '0' && u <= '9') {
			run = append(run, u)
			continue
		}
		flush()
	}
	flush()
	return string(best)
}

// levenshtein returns the edit distance between a and b, counted over RUNES so
// a multi-byte character is one edit.
//
// It is a local copy of internal/scan/match.go's function rather than a shared
// one: that package's is unexported, and importing internal/scan here would drag
// the whole scan service — its store, its queue, its database — into a mode
// whose entire premise is that there is no database. Twenty lines of textbook
// dynamic programming is the cheaper dependency.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(min(prev[j]+1, curr[j-1]+1), prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

// classify turns one page's cell scores and the solver's verdict into a result
// row. The order of the checks is the contract: no cell at all, then too weak,
// then too close to another student, then matched.
func classify(pr PageRead, cells []components, argmax, assigned int, rows []roster.Row, problems int, minScore, minMargin float64) MatchResult {
	res := MatchResult{Page: pr.Page, Method: MethodLattice}
	res.Margin = studentMargin(cells, argmax, problems)

	// Report the assigned cell for a match, and the page's own best cell when
	// there is no match, so an unmatched row still says who it nearly was.
	report := func(cell int) {
		c := cells[cell]
		res.Score, res.ScoreID, res.ScoreName, res.ScoreProblem = c.Score, c.ID, c.Name, c.Problem
	}

	switch {
	case assigned < 0:
		// More pages than cells. The solver picked which pages to drop by total
		// cost, so the ones left over are the weakest claims in the batch.
		report(argmax)
		res.Status, res.Reason = StatusUnmatched, ReasonSurplus
		return res
	case cells[assigned].Score < minScore:
		report(argmax)
		res.Status, res.Reason = StatusUnmatched, ReasonLowScore
		return res
	case res.Margin < minMargin:
		report(argmax)
		res.Status, res.Reason = StatusUnmatched, ReasonAmbiguous
		return res
	}

	report(assigned)
	student := rows[cellStudent(assigned, problems)]
	res.StudentID = student.StudentID
	res.StudentName = student.Name
	res.Problem = cellProblem(assigned, problems)
	if assigned == argmax {
		res.Status = StatusAuto
	} else {
		res.Status = StatusForced
	}
	return res
}

// studentMargin is how much better the page's best cell is than the best cell
// belonging to a DIFFERENT student.
//
// Measuring against the runner-up CELL instead would be a different and useless
// question: a page that identifies its student beyond doubt but leaves the
// problem number blank has all of that student's problem cells tied at the top,
// so the runner-up gap would be zero and every such page would be thrown away
// as ambiguous. The risk this threshold guards against is grading the wrong
// STUDENT, so the comparison is against students.
//
// With a single-student roster there is no competitor at all; the runner-up
// counts as zero, which makes the margin the score itself.
func studentMargin(cells []components, argmax, problems int) float64 {
	top := cells[argmax].Score
	owner := cellStudent(argmax, problems)
	best := 0.0
	for cell, c := range cells {
		if cellStudent(cell, problems) == owner {
			continue
		}
		if c.Score > best {
			best = c.Score
		}
	}
	return top - best
}

// posteriorOf is FieldResult indexing that tolerates an unread field (whose
// Scores are all zero anyway) and an out-of-range candidate.
func posteriorOf(f FieldResult, i int) float64 {
	if i < 0 || i >= len(f.Scores) {
		return 0
	}
	return f.Scores[i].Posterior
}

// idVariants is the spelling a student ID is matched by: the same normalization
// the server's roster matching uses (NFKC, uppercased, non-alphanumerics
// dropped), which is how an ID is written by hand — the hyphens and spaces a
// roster export carries are not on the page.
func idVariants(row roster.Row) []string {
	id := studentid.Normalize(row.StudentID)
	if id == "" {
		return nil
	}
	return []string{id}
}

// nameVariants is the two spellings a roster name is matched by: the NFKC-folded
// raw name with whitespace removed (case preserved, as the student writes it)
// and studentid.NormalizeName's canonical key (the same thing case-folded).
//
// They differ only in Latin case, and both are offered because the recognizer
// distinguishes 'A' from 'a' as separate classes: scoring only the case-folded
// key would charge a real match for every capital letter in the name. A CJK
// name folds to a single variant, since those characters have no case — hence
// the deduplication rather than a fixed pair.
//
// NFKC folding is x/text's, reached through the same package the server uses;
// nothing here re-implements normalization.
func nameVariants(name string) []string {
	raw := stripSpace(norm.NFKC.String(name))
	folded := studentid.NormalizeName(name)
	switch {
	case raw == "":
		return nil
	case raw == folded:
		return []string{raw}
	}
	return []string{raw, folded}
}

// stripSpace removes every whitespace rune, including the ideographic space
// U+3000 that separates the characters of a handwritten CJK name.
func stripSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// problemVariants is how a problem number appears on a page: labelled ("Q3") or
// bare ("3"). Both are scored and the better one wins, because which convention
// a class uses is not knowable from the roster.
func problemVariants(q int) []string {
	n := strconv.Itoa(q)
	return []string{"Q" + n, n}
}
