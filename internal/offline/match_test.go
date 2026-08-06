package offline

import (
	"fmt"
	"math"
	"testing"

	"github.com/HaoWen46/adagrade/internal/localocr"
	"github.com/HaoWen46/adagrade/internal/roster"
)

// matchKeys is the toy dictionary the matching tests score in: the two letters
// the fixture student IDs use, 'Q' for problem labels, the digits, and three
// lowercase letters for names. Class 0 is the CTC blank, so 'A' is class 1.
var matchKeys = []rune{'A', 'B', 'Q', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', 'a', 'b', 'c'}

func matchCharset() localocr.Charset { return localocr.NewCharset(matchKeys, false) }

// textLine builds a lattice that reads text: one blank frame between every pair
// of characters (and at both ends), with spike of the probability mass on the
// intended class at each character frame and the remainder spread evenly.
func textLine(t *testing.T, text string, spike float32) localocr.LineLattice {
	t.Helper()
	cs := matchCharset()
	c := cs.NumClasses()
	rest := (1 - spike) / float32(c-1)
	var rows [][]float32
	push := func(class int) {
		row := make([]float32, c)
		for i := range row {
			row[i] = rest
		}
		row[class] = spike
		rows = append(rows, row)
	}
	push(0)
	for _, r := range text {
		cls, ok := cs.Class(r)
		if !ok {
			t.Fatalf("textLine: %q is not in the test charset", r)
		}
		push(cls)
		push(0)
	}
	return localocr.LineLattice{Lattice: localocr.NewLattice(rows, 0), Text: text, Confidence: float64(spike)}
}

// garbageLine says nothing: every class equally likely at every frame.
func garbageLine(frames int) localocr.LineLattice {
	c := matchCharset().NumClasses()
	rows := make([][]float32, frames)
	for i := range rows {
		rows[i] = make([]float32, c)
		for j := range rows[i] {
			rows[i][j] = 1 / float32(c)
		}
	}
	return localocr.LineLattice{Lattice: localocr.NewLattice(rows, 0), Confidence: 0.05}
}

// identity assembles an Identity from per-kind lines. A nil entry means the
// field was read and found blank, which is how a field ends up contributing
// nothing.
func identity(idLines, nameLines, probLines []localocr.LineLattice) Identity {
	return Identity{Fields: map[Kind]FieldLattices{
		KindStudentID: {Lines: idLines},
		KindName:      {Lines: nameLines},
		KindProblemID: {Lines: probLines},
	}}
}

func lines(l ...localocr.LineLattice) []localocr.LineLattice { return l }

// labelLine is a line the veto can read but the SCORER cannot: greedy text and a
// confidence, with no lattice behind them. It stands for the printed field label
// a header-box crop picks up next to the handwriting ("Student ID"), which is
// exactly what the veto must not mistake for a reading of an id.
//
// The empty lattice is the honest part rather than a shortcut: localocr's
// ScoreTarget refuses a frameless line ("this pairing has no opinion"), so the
// line contributes nothing to any candidate's score and the fixture isolates the
// veto's own input — Text and Confidence. It also lets these tests spell labels
// the toy matchKeys charset has no classes for.
func labelLine(text string, confidence float64) localocr.LineLattice {
	return localocr.LineLattice{Text: text, Confidence: confidence}
}

// fixtureRoster builds n students with structurally identical IDs and names:
// IDs AB01..ABnn and three-letter names with no repeated adjacent character.
// Structural uniformity is what makes the garbage anchor exact (see
// flatCandidates in score_test.go).
func fixtureRoster(t *testing.T, n int) []roster.Row {
	t.Helper()
	names := flatCandidates(n)
	if len(names) < n {
		t.Fatalf("fixtureRoster: only %d distinct names available, need %d", len(names), n)
	}
	rows := make([]roster.Row, n)
	for i := range rows {
		rows[i] = roster.Row{
			StudentID: fmt.Sprintf("AB%02d", i+1),
			Name:      names[i],
			Email:     fmt.Sprintf("s%02d@example.test", i+1),
			Line:      i + 2,
		}
	}
	return rows
}

func page(index int) Page {
	return Page{Index: index, SourcePDF: "scan.pdf", SourcePage: index, JPEG: []byte("jpeg")}
}

// --- cell packing ----------------------------------------------------------

// TestCellPacking_RoundTrip pins the one arithmetic every part of the matcher
// shares: a (student, problem) pair packs into a single column index and comes
// back out unchanged. An off-by-one here would silently assign every page to
// the wrong problem, and every downstream number would still look plausible.
func TestCellPacking_RoundTrip(t *testing.T) {
	for _, problems := range []int{1, 2, 3, 7} {
		for student := 0; student < 5; student++ {
			for problem := 1; problem <= problems; problem++ {
				cell := cellIndex(student, problem, problems)
				if got := cellStudent(cell, problems); got != student {
					t.Errorf("problems=%d cell(%d,%d)=%d: cellStudent = %d, want %d", problems, student, problem, cell, got, student)
				}
				if got := cellProblem(cell, problems); got != problem {
					t.Errorf("problems=%d cell(%d,%d)=%d: cellProblem = %d, want %d", problems, student, problem, cell, got, problem)
				}
			}
		}
	}
}

// TestCellPacking_IsDenseAndOrdered: the cells of a roster×problems grid are
// exactly 0..n-1 with no gaps and no collisions, which is what lets the cost
// matrix be indexed by cell directly.
func TestCellPacking_IsDenseAndOrdered(t *testing.T) {
	const students, problems = 4, 3
	seen := make(map[int]bool)
	for s := 0; s < students; s++ {
		for q := 1; q <= problems; q++ {
			cell := cellIndex(s, q, problems)
			if cell < 0 || cell >= students*problems {
				t.Fatalf("cell(%d,%d) = %d, outside [0,%d)", s, q, cell, students*problems)
			}
			if seen[cell] {
				t.Fatalf("cell(%d,%d) = %d collides with an earlier pair", s, q, cell)
			}
			seen[cell] = true
		}
	}
	if len(seen) != students*problems {
		t.Errorf("packed %d distinct cells, want %d", len(seen), students*problems)
	}
	// Student 1's problem 1 sits immediately after student 0's last problem.
	if got := cellIndex(1, 1, problems); got != problems {
		t.Errorf("cellIndex(1,1,%d) = %d, want %d", problems, got, problems)
	}
}

// --- weights ---------------------------------------------------------------

// TestWeights_SumToOne: the three weights are a partition of one page's
// evidence. If they ever stop summing to 1, every threshold in the CLI (and the
// 0.1375 garbage floor below) silently changes meaning.
func TestWeights_SumToOne(t *testing.T) {
	if sum := WeightStudentID + WeightName + WeightProblem; math.Abs(sum-1) > 1e-12 {
		t.Errorf("weights sum to %v, want 1", sum)
	}
	if !(WeightStudentID > WeightName && WeightName > WeightProblem) {
		t.Errorf("the ID must outweigh the name, and the name the problem number: %v/%v/%v",
			WeightStudentID, WeightName, WeightProblem)
	}
}

// TestCellComponents_HandComputed pins the weighted sum against arithmetic done
// by hand, and — the part that matters for wrong-student risk — pins that an
// UNREAD field contributes exactly zero instead of having its weight
// redistributed over the fields that were read.
func TestCellComponents_HandComputed(t *testing.T) {
	tests := []struct {
		name                                  string
		idPost, namePost, probPost            float64
		idRead, nameRead, probRead            bool
		wantScore, wantID, wantName, wantProb float64
	}{
		{
			// 0.45*0.8 + 0.30*0.5 + 0.25*0.4 = 0.36 + 0.15 + 0.10 = 0.61
			name: "all three read", idPost: 0.8, namePost: 0.5, probPost: 0.4,
			idRead: true, nameRead: true, probRead: true,
			wantScore: 0.61, wantID: 0.36, wantName: 0.15, wantProb: 0.10,
		},
		{
			// The ID crop was blank. S drops to 0.30*0.5 + 0.25*0.4 = 0.25.
			// A renormalizing implementation would report 0.30/0.55*0.5 +
			// 0.25/0.55*0.4 = 0.4545..., promoting a page nobody could
			// identify to the same standing as one that was read.
			name: "id unread contributes zero", idPost: 0.8, namePost: 0.5, probPost: 0.4,
			idRead: false, nameRead: true, probRead: true,
			wantScore: 0.25, wantID: 0, wantName: 0.15, wantProb: 0.10,
		},
		{
			// Only the ID was read: 0.45*1.0 = 0.45, nowhere near 1.0.
			name: "name and problem unread", idPost: 1, namePost: 1, probPost: 1,
			idRead: true, nameRead: false, probRead: false,
			wantScore: 0.45, wantID: 0.45, wantName: 0, wantProb: 0,
		},
		{
			name: "nothing read at all", idPost: 1, namePost: 1, probPost: 1,
			wantScore: 0, wantID: 0, wantName: 0, wantProb: 0,
		},
		{
			// The garbage anchor: 10 students, 4 problems, every field
			// uniform. 0.45*0.1 + 0.30*0.1 + 0.25*0.25 = 0.1375, which is
			// below the default --min-score of 0.15 by design.
			name: "uniform garbage", idPost: 0.1, namePost: 0.1, probPost: 0.25,
			idRead: true, nameRead: true, probRead: true,
			wantScore: 0.1375, wantID: 0.045, wantName: 0.03, wantProb: 0.0625,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cellComponents(tc.idPost, tc.namePost, tc.probPost, tc.idRead, tc.nameRead, tc.probRead)
			for _, c := range []struct {
				label     string
				got, want float64
			}{
				{"Score", got.Score, tc.wantScore},
				{"ID", got.ID, tc.wantID},
				{"Name", got.Name, tc.wantName},
				{"Problem", got.Problem, tc.wantProb},
			} {
				if math.Abs(c.got-c.want) > 1e-12 {
					t.Errorf("%s = %v, want %v", c.label, c.got, c.want)
				}
			}
			// The components are the summands, so the report can be audited.
			if sum := got.ID + got.Name + got.Problem; math.Abs(sum-got.Score) > 1e-12 {
				t.Errorf("components sum to %v but Score is %v", sum, got.Score)
			}
		})
	}
	if DefaultMinScore <= 0.1375 {
		t.Errorf("DefaultMinScore = %v must sit above the 0.1375 uniform-garbage score", DefaultMinScore)
	}
}

// --- MatchPages ------------------------------------------------------------

// TestMatchPages_AutoMatch is the happy path: one page that plainly reads a
// roster ID, that student's name and a problem number lands on the right cell
// with status "auto".
func TestMatchPages_AutoMatch(t *testing.T) {
	rows := fixtureRoster(t, 3)
	reads := []PageRead{{
		Page: page(1),
		ID: identity(
			lines(textLine(t, "AB02", 0.9)),
			lines(textLine(t, rows[1].Name, 0.9)),
			lines(textLine(t, "Q2", 0.9)),
		),
	}}

	results, err := MatchPages(reads, rows, 3, matchCharset(), DefaultMinScore, DefaultMinMargin)
	if err != nil {
		t.Fatalf("MatchPages: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	got := results[0]
	if got.Status != StatusAuto || got.Reason != "" {
		t.Errorf("status/reason = %q/%q, want %q/\"\"", got.Status, got.Reason, StatusAuto)
	}
	if got.StudentID != rows[1].StudentID || got.StudentName != rows[1].Name {
		t.Errorf("assigned %q/%q, want %q/%q", got.StudentID, got.StudentName, rows[1].StudentID, rows[1].Name)
	}
	if got.Problem != 2 {
		t.Errorf("problem = %d, want 2", got.Problem)
	}
	if got.Method != MethodLattice {
		t.Errorf("method = %q, want %q", got.Method, MethodLattice)
	}
	if got.Page.Index != 1 {
		t.Errorf("result lost its page: %+v", got.Page)
	}
	if sum := got.ScoreID + got.ScoreName + got.ScoreProblem; math.Abs(sum-got.Score) > 1e-12 {
		t.Errorf("components %v sum to %v, but Score is %v", []float64{got.ScoreID, got.ScoreName, got.ScoreProblem}, sum, got.Score)
	}
	if got.Margin <= DefaultMinMargin {
		t.Errorf("margin = %v, want comfortably above %v on an unambiguous page", got.Margin, DefaultMinMargin)
	}
}

// TestMatchPages_UniformGarbageIsLowScore is the anchor from the design: ten
// students, four problems, all three fields reading nothing. Every posterior is
// uniform, S is 0.1375 everywhere, and that sits below the default floor — so
// the page is set aside rather than force-matched onto whichever cell the
// solver happened to pick.
func TestMatchPages_UniformGarbageIsLowScore(t *testing.T) {
	rows := fixtureRoster(t, 10)
	reads := []PageRead{{
		Page: page(1),
		ID: identity(
			lines(garbageLine(12)),
			lines(garbageLine(12)),
			lines(garbageLine(12)),
		),
	}}

	results, err := MatchPages(reads, rows, 4, matchCharset(), DefaultMinScore, DefaultMinMargin)
	if err != nil {
		t.Fatalf("MatchPages: %v", err)
	}
	got := results[0]
	if got.Status != StatusUnmatched || got.Reason != ReasonLowScore {
		t.Fatalf("status/reason = %q/%q, want %q/%q (S=%v)", got.Status, got.Reason, StatusUnmatched, ReasonLowScore, got.Score)
	}
	if math.Abs(got.Score-0.1375) > 1e-9 {
		t.Errorf("Score = %v, want 0.1375 (0.45*0.1 + 0.30*0.1 + 0.25*0.25)", got.Score)
	}
	if got.StudentID != "" || got.StudentName != "" || got.Problem != 0 {
		t.Errorf("an unmatched page must name nobody: %q/%q/%d", got.StudentID, got.StudentName, got.Problem)
	}
	// The would-have-been components still travel, so the report can explain
	// itself.
	for _, c := range []struct {
		label string
		want  float64
		got   float64
	}{
		{"score_id", 0.045, got.ScoreID},
		{"score_name", 0.03, got.ScoreName},
		{"score_problem", 0.0625, got.ScoreProblem},
	} {
		if math.Abs(c.got-c.want) > 1e-9 {
			t.Errorf("%s = %v, want %v", c.label, c.got, c.want)
		}
	}
}

// TestMatchPages_Forced: two pages peak on the SAME cell. Only one can have it,
// so the global assignment moves the weaker claim to its next-best cell and
// marks it "forced" — the flag that says a human should look.
func TestMatchPages_Forced(t *testing.T) {
	rows := fixtureRoster(t, 2)
	reads := []PageRead{
		{
			Page: page(1),
			ID: identity(
				lines(textLine(t, "AB01", 0.95)),
				lines(textLine(t, rows[0].Name, 0.95)),
				lines(textLine(t, "Q1", 0.95)),
			),
		},
		{
			Page: page(2),
			ID: identity(
				lines(textLine(t, "AB01", 0.6)),
				lines(textLine(t, rows[0].Name, 0.6)),
				lines(textLine(t, "Q1", 0.6)),
			),
		},
	}

	results, err := MatchPages(reads, rows, 1, matchCharset(), DefaultMinScore, DefaultMinMargin)
	if err != nil {
		t.Fatalf("MatchPages: %v", err)
	}
	var autos, forced int
	seen := map[string]bool{}
	for _, r := range results {
		if r.Status == StatusUnmatched {
			t.Fatalf("page %d unexpectedly unmatched (%s, score %v)", r.Page.Index, r.Reason, r.Score)
		}
		key := fmt.Sprintf("%s/%d", r.StudentID, r.Problem)
		if seen[key] {
			t.Errorf("cell %s assigned twice", key)
		}
		seen[key] = true
		switch r.Status {
		case StatusAuto:
			autos++
		case StatusForced:
			forced++
		}
	}
	if autos != 1 || forced != 1 {
		t.Errorf("got %d auto and %d forced, want 1 and 1: %+v", autos, forced, results)
	}
	// The stronger read keeps the cell both pages wanted.
	if results[0].Status != StatusAuto {
		t.Errorf("the stronger page should keep its argmax cell, got %q", results[0].Status)
	}
	if results[1].Status != StatusForced {
		t.Errorf("the weaker page should be the forced one, got %q", results[1].Status)
	}
}

// TestMatchPages_Surplus: more pages than there are cells to put them in. The
// solver leaves the extras unassigned and they surface as unmatched/surplus,
// not as a duplicate assignment.
func TestMatchPages_Surplus(t *testing.T) {
	rows := fixtureRoster(t, 1)
	var reads []PageRead
	for i := 1; i <= 2; i++ {
		reads = append(reads, PageRead{
			Page: page(i),
			ID: identity(
				lines(textLine(t, "AB01", 0.9)),
				lines(textLine(t, rows[0].Name, 0.9)),
				lines(textLine(t, "Q1", 0.9)),
			),
		})
	}

	results, err := MatchPages(reads, rows, 1, matchCharset(), DefaultMinScore, DefaultMinMargin)
	if err != nil {
		t.Fatalf("MatchPages: %v", err)
	}
	var matched, surplus int
	for _, r := range results {
		switch {
		case r.Status == StatusUnmatched && r.Reason == ReasonSurplus:
			surplus++
			if r.Score == 0 {
				t.Error("a surplus page should still report the score it would have had")
			}
		case r.Status != StatusUnmatched:
			matched++
		default:
			t.Errorf("unexpected status/reason %q/%q", r.Status, r.Reason)
		}
	}
	if matched != 1 || surplus != 1 {
		t.Errorf("got %d matched and %d surplus, want 1 and 1", matched, surplus)
	}
}

// TestMatchPages_Ambiguous: two students share a name and the ID crop is blank,
// so the evidence splits evenly between them. The page scores well above the
// floor — and is still set aside, because a coin flip between two students is
// exactly the wrong-student risk this mode refuses to take silently.
func TestMatchPages_Ambiguous(t *testing.T) {
	rows := []roster.Row{
		{StudentID: "AB01", Name: "aba", Line: 2},
		{StudentID: "AB02", Name: "aba", Line: 3},
	}
	reads := []PageRead{{
		Page: page(1),
		ID: identity(
			nil, // the ID box was left blank
			lines(textLine(t, "aba", 0.9)),
			lines(textLine(t, "Q1", 0.9)),
		),
	}}

	results, err := MatchPages(reads, rows, 1, matchCharset(), DefaultMinScore, DefaultMinMargin)
	if err != nil {
		t.Fatalf("MatchPages: %v", err)
	}
	got := results[0]
	if got.Status != StatusUnmatched || got.Reason != ReasonAmbiguous {
		t.Fatalf("status/reason = %q/%q, want %q/%q (score %v, margin %v)",
			got.Status, got.Reason, StatusUnmatched, ReasonAmbiguous, got.Score, got.Margin)
	}
	if got.Score < DefaultMinScore {
		t.Errorf("score = %v: this page must fail on the MARGIN, not on the floor", got.Score)
	}
	if math.Abs(got.Margin) > 1e-9 {
		t.Errorf("margin = %v, want ~0 between two identically-named students", got.Margin)
	}
	if got.ScoreID != 0 {
		t.Errorf("score_id = %v, want 0: the ID crop was blank", got.ScoreID)
	}
}

// TestMatchPages_MarginIgnoresTheSameStudentsOtherProblems is the reason the
// margin is defined against a DIFFERENT student rather than against the
// runner-up cell.
//
// This page identifies its student beyond doubt but its problem number is
// blank, so the student's own problem-1 and problem-2 cells score identically.
// A runner-up margin would be 0 and would throw the page away as "ambiguous";
// the correct question — could this be a DIFFERENT student — has a decisive
// answer, so the page is matched.
func TestMatchPages_MarginIgnoresTheSameStudentsOtherProblems(t *testing.T) {
	rows := fixtureRoster(t, 2)
	reads := []PageRead{{
		Page: page(1),
		ID: identity(
			lines(textLine(t, "AB01", 0.95)),
			lines(textLine(t, rows[0].Name, 0.95)),
			nil, // no problem number written
		),
	}}

	results, err := MatchPages(reads, rows, 2, matchCharset(), DefaultMinScore, DefaultMinMargin)
	if err != nil {
		t.Fatalf("MatchPages: %v", err)
	}
	got := results[0]
	if got.Status == StatusUnmatched {
		t.Fatalf("page was dropped as %q; the margin must be measured against the OTHER student, not against this student's other problem (margin %v)",
			got.Reason, got.Margin)
	}
	if got.StudentID != rows[0].StudentID {
		t.Errorf("assigned %q, want %q", got.StudentID, rows[0].StudentID)
	}
	if got.Margin < 0.5 {
		t.Errorf("margin = %v, want a decisive gap over the other student", got.Margin)
	}
	if got.ScoreProblem != 0 {
		t.Errorf("score_problem = %v, want 0 when no problem number was read", got.ScoreProblem)
	}
	// Never renormalized: with the problem field silent the ceiling is
	// 0.45 + 0.30 = 0.75, not 1.
	if got.Score > WeightStudentID+WeightName+1e-12 {
		t.Errorf("score = %v exceeds the readable weight mass %v: the weights were renormalized",
			got.Score, WeightStudentID+WeightName)
	}
}

// TestMatchPages_ProblemVariants: a page may write its problem number as "Q3"
// or as bare "3", and both must find problem 3.
func TestMatchPages_ProblemVariants(t *testing.T) {
	rows := fixtureRoster(t, 2)
	for _, written := range []string{"Q3", "3"} {
		t.Run(written, func(t *testing.T) {
			reads := []PageRead{{
				Page: page(1),
				ID: identity(
					lines(textLine(t, "AB01", 0.95)),
					lines(textLine(t, rows[0].Name, 0.95)),
					lines(textLine(t, written, 0.95)),
				),
			}}
			results, err := MatchPages(reads, rows, 4, matchCharset(), DefaultMinScore, DefaultMinMargin)
			if err != nil {
				t.Fatalf("MatchPages: %v", err)
			}
			if results[0].Problem != 3 {
				t.Errorf("problem = %d, want 3 (page wrote %q)", results[0].Problem, written)
			}
		})
	}
}

// TestNameVariants pins the two spellings a roster name is matched by: the
// NFKC-folded raw name (case preserved, as a student would write it) and
// studentid.NormalizeName's case-folded key. Whitespace is stripped from both,
// because handwritten CJK names come apart at the spaces.
func TestNameVariants(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"latin name keeps both cases", "Ada Lovelace", []string{"AdaLovelace", "adalovelace"}},
		{"CJK name folds to one variant", "王 小明", []string{"王小明"}},
		{"ideographic space is whitespace", "王　小明", []string{"王小明"}},
		{"full-width latin folds under NFKC", "ＡＢ", []string{"AB", "ab"}},
		{"already lowercase folds to one", "aba", []string{"aba"}},
		{"empty name has no variants", "   ", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nameVariants(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("variant %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestMatchPages_Errors(t *testing.T) {
	rows := fixtureRoster(t, 2)
	cs := matchCharset()

	t.Run("no problems", func(t *testing.T) {
		_, err := MatchPages(nil, rows, 0, cs, DefaultMinScore, DefaultMinMargin)
		if err == nil {
			t.Fatal("want an error, got nil")
		}
		if ExitCode(err) != ExitUsage {
			t.Errorf("ExitCode = %d, want %d", ExitCode(err), ExitUsage)
		}
	})

	t.Run("empty roster", func(t *testing.T) {
		_, err := MatchPages(nil, nil, 2, cs, DefaultMinScore, DefaultMinMargin)
		if err == nil {
			t.Fatal("want an error, got nil")
		}
		if ExitCode(err) != ExitRoster {
			t.Errorf("ExitCode = %d, want %d", ExitCode(err), ExitRoster)
		}
	})

	t.Run("no pages is not an error", func(t *testing.T) {
		results, err := MatchPages(nil, rows, 2, cs, DefaultMinScore, DefaultMinMargin)
		if err != nil {
			t.Fatalf("no pages should not be an error: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("got %d results, want 0", len(results))
		}
	})
}

// --- the legible-ID veto ---------------------------------------------------

// vetoRoster is a roster with REAL-shaped ids: the veto's run-length gate is 5
// characters, so fixtureRoster's four-character AB01 ids could never trip it and
// would silently pass every case below.
func vetoRoster(t *testing.T) []roster.Row {
	t.Helper()
	names := []string{"abc", "bca", "cab"}
	rows := make([]roster.Row, len(names))
	for i, name := range names {
		rows[i] = roster.Row{
			StudentID: fmt.Sprintf("B1190200%d", i+1),
			Name:      name,
			Email:     fmt.Sprintf("s%d@example.test", i+1),
			Line:      i + 2,
		}
	}
	return rows
}

// TestMatchPages_LegibleWrongIDVetoesTheAssignment is the off-roster veto.
//
// The setup is the hazard it exists for, and it is not hypothetical: the page's
// NAME and PROBLEM boxes read student 1's decisively (0.30 + 0.25 of the weight),
// so the solver assigns it to student 1 — while the id box legibly reads an id
// that is nobody's. Closed-set scoring cannot object: posteriors normalize over
// the roster, so "not on this roster" is not a hypothesis it can express, and
// the assigned cell clears both --min-score and --min-margin comfortably.
//
// The gates are asserted one at a time because each one protects something
// different: confidence keeps ugly handwriting from vetoing a correct lattice
// match, run length keeps a "Q1" or a stray digit from counting as an id, the
// digit requirement keeps a PRINTED LABEL ("Student ID", which a crisp printer
// reads at higher confidence than the handwriting beside it) from vetoing every
// page in the batch, and distance >= 3 keeps a near-miss OCR of the RIGHT id
// (internal/scan's rungs call <= 2 a near miss) from discarding a correct match.
//
// The multi-line cases matter most: a header-box crop routinely yields a label
// line AND a value line, so the rule is over the SET of qualifying readings —
// fire only if at least one exists and all of them disagree.
func TestMatchPages_LegibleWrongIDVetoesTheAssignment(t *testing.T) {
	rows := vetoRoster(t)

	tests := []struct {
		name       string
		idLines    []localocr.LineLattice
		wantStatus string
		wantReason string
		why        string
	}{
		{
			name:       "legible id, three edits away",
			idLines:    lines(textLine(t, "B99999999", 0.95)),
			wantStatus: StatusUnmatched,
			wantReason: ReasonIDConflict,
			why:        "the id box legibly says somebody else",
		},
		{
			name:       "same id, low confidence",
			idLines:    lines(textLine(t, "B99999999", 0.60)),
			wantStatus: StatusAuto,
			why:        "a 0.60 read is a guess, and a guess must not veto the lattice",
		},
		{
			name:       "printed label alone",
			idLines:    lines(labelLine("STUDENT ID", 0.99)),
			wantStatus: StatusAuto,
			why:        "a printed field label has no digits, so it is not a reading of an id — and it out-confidences the handwriting on every labelled sheet",
		},
		{
			name:       "printed label beside an agreeing value",
			idLines:    lines(labelLine("STUDENT ID", 0.99), textLine(t, "B11902001", 0.95)),
			wantStatus: StatusAuto,
			why:        "the value line agrees with the assignment; the label is not evidence against it",
		},
		{
			// The clause under test is ALL qualifying readings must disagree, so
			// this label has to actually qualify: "FORM2024" is 8 runes with a
			// digit and 8 edits from B11902001. Spelled "FORM 2024" it would
			// split into two sub-5-rune runs, never qualify, and the case would
			// silently degenerate into a duplicate of the one above — passing
			// just as happily against a wrong "any disagreement vetoes" rule.
			name:       "qualifying label disagreeing beside an agreeing value",
			idLines:    lines(labelLine("FORM2024", 0.99), textLine(t, "B11902001", 0.95)),
			wantStatus: StatusAuto,
			why:        "one qualifying reading agreeing is enough: a preprinted form number is not a claim about who wrote this",
		},
		{
			name:       "printed label beside a disagreeing value",
			idLines:    lines(labelLine("STUDENT ID", 0.99), textLine(t, "B99999999", 0.95)),
			wantStatus: StatusUnmatched,
			wantReason: ReasonIDConflict,
			why:        "the label is ignored and the value line still legibly says somebody else",
		},
		{
			name:       "two disagreeing qualifying readings",
			idLines:    lines(textLine(t, "B99999999", 0.95), textLine(t, "B88888888", 0.95)),
			wantStatus: StatusUnmatched,
			wantReason: ReasonIDConflict,
			why:        "every qualifying reading disagrees, whichever one is the real id",
		},
		{
			name:       "illegible agreement beside a legible disagreement",
			idLines:    lines(textLine(t, "B11902001", 0.60), textLine(t, "B99999999", 0.95)),
			wantStatus: StatusUnmatched,
			wantReason: ReasonIDConflict,
			why:        "a sub-threshold line cannot rescue a legible conflict — the fail-safe direction is unmatched/ for a human, never a possibly-wrong student",
		},
		{
			name:       "near miss of the assigned id",
			idLines:    lines(textLine(t, "B11902084", 0.95)),
			wantStatus: StatusAuto,
			why:        "two edits from B11902001 is a misread of it, not a different student",
		},
		{
			name:       "short legible run",
			idLines:    lines(textLine(t, "Q1", 0.95)),
			wantStatus: StatusAuto,
			why:        "four characters or fewer cannot identify anybody",
		},
		{
			name:       "id field unread",
			idLines:    nil,
			wantStatus: StatusAuto,
			why:        "nothing was read, so nothing can conflict",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reads := []PageRead{{
				Page: page(1),
				ID: identity(
					tc.idLines,
					lines(textLine(t, rows[0].Name, 0.9)),
					lines(textLine(t, "Q1", 0.9)),
				),
			}}

			results, err := MatchPages(reads, rows, 1, matchCharset(), DefaultMinScore, DefaultMinMargin)
			if err != nil {
				t.Fatalf("MatchPages: %v", err)
			}
			got := results[0]
			if got.Status != tc.wantStatus || got.Reason != tc.wantReason {
				t.Fatalf("status/reason = %q/%q, want %q/%q (%s)", got.Status, got.Reason, tc.wantStatus, tc.wantReason, tc.why)
			}
			if tc.wantStatus == StatusUnmatched {
				// A vetoed row names nobody, in memory as well as in the
				// report: every downstream stage keys off these fields.
				if got.StudentID != "" || got.StudentName != "" || got.Problem != 0 {
					t.Errorf("a vetoed row still carries an assignment: %q/%q problem %d", got.StudentID, got.StudentName, got.Problem)
				}
				// The scores stay, because they are the evidence an operator
				// needs to see: this page looked confident, and that is the point.
				if got.Score <= 0 {
					t.Errorf("score = %v, want the vetoed cell's score kept for the report", got.Score)
				}
				return
			}
			if got.StudentID != rows[0].StudentID {
				t.Errorf("assigned %q, want %q (%s)", got.StudentID, rows[0].StudentID, tc.why)
			}
		})
	}
}

// TestLongestIDRun covers the extraction the veto reads its id out of. Band mode
// is the reason it exists: with --id-band the "student_id" field is the whole
// top strip, so the line the veto inspects carries the name and the problem
// label too.
func TestLongestIDRun(t *testing.T) {
	tests := []struct{ in, want string }{
		{"B11902001", "B11902001"},
		{"b11902001", "B11902001"},            // uppercased, like studentid.Normalize
		{"Ｂ１１９０２００１", "B11902001"},             // NFKC: full-width reads as ASCII
		{"B11902001 abc Q1", "B11902001"},     // band mode: the whole header strip
		{"座號 B11902001 姓名 王小明", "B11902001"}, // CJK ends a run rather than joining it
		{"B-1190-2001", "1190"},               // punctuation ends a run too
		{"Q1", "Q1"},                          // too short for the veto, but still the run
		{"", ""},
		{"王小明", ""},                 // a name alone yields no run
		{"12 B119020011", "B119020011"}, // the LONGEST run, not the first
	}
	for _, tc := range tests {
		if got := longestIDRun(tc.in); got != tc.want {
			t.Errorf("longestIDRun(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLevenshtein pins the local copy against the distances the veto's threshold
// is stated in.
func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"B11902001", "B11902001", 0},
		{"B11902001", "B11902081", 1}, // one substitution: a misread digit
		{"B11902001", "B11902084", 2}, // two: still a near miss
		{"B11902001", "B99999999", 7}, // a different identity
		{"B11902001", "", 9},
		{"", "", 0},
		{"丁一心", "丁一忄", 1}, // runes, not bytes
	}
	for _, tc := range tests {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
