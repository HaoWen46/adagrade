package offline

import (
	"math"
	"testing"

	"github.com/HaoWen46/adagrade/internal/localocr"
)

// The toy dictionary every scoring test works in: class 0 is the CTC blank,
// 'a'=1, 'b'=2, 'c'=3, so a dense lattice row is four numbers that can be added
// up by hand. 'z' is absent on purpose, so an unspellable candidate has
// something to be unspellable with.
var testKeys = []rune{'a', 'b', 'c'}

func testCharset() localocr.Charset { return localocr.NewCharset(testKeys, false) }

// spikeLine builds a one-line field whose frames each put most of their mass on
// one character of text. frames is written as a string: '_' is a blank frame,
// any other rune is that character's spike.
func spikeLine(frames string, spike float32) localocr.LineLattice {
	rest := (1 - spike) / 3
	rows := make([][]float32, 0, len(frames))
	for _, r := range frames {
		row := []float32{rest, rest, rest, rest}
		switch r {
		case '_':
			row[0] = spike
		case 'a':
			row[1] = spike
		case 'b':
			row[2] = spike
		case 'c':
			row[3] = spike
		}
		rows = append(rows, row)
	}
	return localocr.LineLattice{Lattice: localocr.NewLattice(rows, 0), Text: frames, Confidence: 0.9}
}

// uniformLine is a line that says nothing: every class equally likely at every
// frame. It is the garbage the low-score floor exists to catch.
func uniformLine(frames int) localocr.LineLattice {
	rows := make([][]float32, frames)
	for i := range rows {
		rows[i] = []float32{0.25, 0.25, 0.25, 0.25}
	}
	return localocr.LineLattice{Lattice: localocr.NewLattice(rows, 0), Confidence: 0.1}
}

func sumPosteriors(scores []CandidateScore) float64 {
	var total float64
	for _, s := range scores {
		total += s.Posterior
	}
	return total
}

// TestScoreField_PosteriorsAreASoftmaxOverLogLik pins the core: posteriors are
// exp(LogLik - max) normalized, and they sum to 1 across the candidate list.
func TestScoreField_PosteriorsAreASoftmaxOverLogLik(t *testing.T) {
	f := FieldLattices{Lines: []localocr.LineLattice{spikeLine("_abc_", 0.9)}}
	targets := [][]string{{"abc"}, {"ab"}, {"cba"}}

	got := ScoreField(f, testCharset(), targets)
	if !got.Read {
		t.Fatal("Read = false, want true")
	}
	if len(got.Scores) != len(targets) {
		t.Fatalf("got %d scores, want %d (parallel to the candidate list)", len(got.Scores), len(targets))
	}

	// Recompute the softmax from the reported LogLiks.
	maxLL := math.Inf(-1)
	for _, s := range got.Scores {
		if s.LogLik > maxLL {
			maxLL = s.LogLik
		}
	}
	var denom float64
	for _, s := range got.Scores {
		denom += math.Exp(s.LogLik - maxLL)
	}
	for i, s := range got.Scores {
		want := math.Exp(s.LogLik-maxLL) / denom
		if math.Abs(s.Posterior-want) > 1e-12 {
			t.Errorf("candidate %d: Posterior = %.12f, want %.12f", i, s.Posterior, want)
		}
	}
	if total := sumPosteriors(got.Scores); math.Abs(total-1) > 1e-12 {
		t.Errorf("posteriors sum to %.12f, want 1", total)
	}
	// The line reads "abc": that candidate must win.
	if got.Scores[0].Posterior <= got.Scores[1].Posterior || got.Scores[0].Posterior <= got.Scores[2].Posterior {
		t.Errorf("the matching candidate should dominate: %v", got.Scores)
	}
}

// TestScoreField_RanksOnLogLikNotProb is the finding from the scorer's own doc
// comment: under AllowSurroundingText the wildcard flanks pay a bonus that
// scales with the LINE, so per-character Prob systematically favours SHORT
// candidates. Ranking on LogLik keeps the full string ahead of its prefix.
func TestScoreField_RanksOnLogLikNotProb(t *testing.T) {
	line := spikeLine("_ab_", 0.9)
	f := FieldLattices{Lines: []localocr.LineLattice{line}}
	cs := testCharset()

	got := ScoreField(f, cs, [][]string{{"ab"}, {"a"}})
	if got.Scores[0].LogLik <= got.Scores[1].LogLik {
		t.Errorf("LogLik(\"ab\") = %v must beat LogLik(\"a\") = %v", got.Scores[0].LogLik, got.Scores[1].LogLik)
	}
	if got.Scores[0].Posterior <= got.Scores[1].Posterior {
		t.Error("the posterior must inherit the LogLik ordering")
	}

	// And the trap it avoids: the per-character Prob really does rank the
	// prefix higher, so a scorer that used it would pick "a".
	full, _ := localocr.ScoreTarget(line.Lattice, cs, "ab", localocr.ScoreOptions{AllowSurroundingText: true})
	prefix, _ := localocr.ScoreTarget(line.Lattice, cs, "a", localocr.ScoreOptions{AllowSurroundingText: true})
	if prefix.Prob <= full.Prob {
		t.Logf("note: the flank bonus no longer inverts Prob on this fixture (full %v, prefix %v); the LogLik ordering above is the contract either way",
			full.Prob, prefix.Prob)
	}
}

// TestScoreField_SurroundingTextAllowed: a candidate that is only PART of the
// line still scores. Real pages read "座號 05 姓名 王小明" while the roster
// candidate is just the name.
func TestScoreField_SurroundingTextAllowed(t *testing.T) {
	f := FieldLattices{Lines: []localocr.LineLattice{spikeLine("_cc_ab_", 0.9)}}
	got := ScoreField(f, testCharset(), [][]string{{"ab"}, {"ba"}})
	if !got.Read {
		t.Fatal("Read = false, want true")
	}
	if got.Scores[0].LogLik <= got.Scores[1].LogLik {
		t.Errorf("the substring present in the line must win: %v", got.Scores)
	}
}

// TestScoreField_BestOverVariantsAndLines: a candidate's LogLik is the MAX over
// its variants crossed with every line of the field, so a name written on the
// second line of the crop scores exactly as well as one on the first.
func TestScoreField_BestOverVariantsAndLines(t *testing.T) {
	cs := testCharset()
	f := FieldLattices{Lines: []localocr.LineLattice{
		spikeLine("_cc_", 0.9),
		spikeLine("_ab_", 0.9),
	}}

	got := ScoreField(f, cs, [][]string{{"ab"}})
	single := ScoreField(FieldLattices{Lines: []localocr.LineLattice{f.Lines[1]}}, cs, [][]string{{"ab"}})
	if math.Abs(got.Scores[0].LogLik-single.Scores[0].LogLik) > 1e-9 {
		t.Errorf("multi-line LogLik = %v, want the best line's %v", got.Scores[0].LogLik, single.Scores[0].LogLik)
	}

	// Variants behave the same way: the better one sets the score.
	multi := ScoreField(f, cs, [][]string{{"cc", "ab"}})
	if multi.Scores[0].LogLik < single.Scores[0].LogLik-1e-9 {
		t.Errorf("variant max = %v, must be at least the best variant's %v", multi.Scores[0].LogLik, single.Scores[0].LogLik)
	}
}

// TestScoreField_UnscorableCandidates: a candidate the dictionary cannot spell
// is skipped, not an error. It ends at LogLik -Inf with posterior 0, and the
// candidates that CAN be spelled still normalize to 1 between them.
func TestScoreField_UnscorableCandidates(t *testing.T) {
	f := FieldLattices{Lines: []localocr.LineLattice{spikeLine("_ab_", 0.9)}}

	got := ScoreField(f, testCharset(), [][]string{{"ab"}, {"zz"}, {""}})
	if !got.Read {
		t.Fatal("Read = false, want true: one candidate was scorable")
	}
	for i := 1; i <= 2; i++ {
		if !math.IsInf(got.Scores[i].LogLik, -1) {
			t.Errorf("candidate %d: LogLik = %v, want -Inf", i, got.Scores[i].LogLik)
		}
		if got.Scores[i].Posterior != 0 {
			t.Errorf("candidate %d: Posterior = %v, want 0", i, got.Scores[i].Posterior)
		}
	}
	if math.Abs(got.Scores[0].Posterior-1) > 1e-12 {
		t.Errorf("the only scorable candidate should carry the whole posterior; got %v", got.Scores[0].Posterior)
	}
}

// TestScoreField_NotRead pins every way a field contributes nothing. Read=false
// is what stops match.go from crediting a page for a field it never saw.
func TestScoreField_NotRead(t *testing.T) {
	cs := testCharset()
	line := spikeLine("_ab_", 0.9)

	tests := []struct {
		name    string
		field   FieldLattices
		targets [][]string
	}{
		{"no lines", FieldLattices{}, [][]string{{"ab"}}},
		{"empty line slice", FieldLattices{Lines: []localocr.LineLattice{}}, [][]string{{"ab"}}},
		{"no candidates", FieldLattices{Lines: []localocr.LineLattice{line}}, nil},
		{"every candidate unspellable", FieldLattices{Lines: []localocr.LineLattice{line}}, [][]string{{"zz"}, {"z"}}},
		{"zero-frame lattice", FieldLattices{Lines: []localocr.LineLattice{{}}}, [][]string{{"ab"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreField(tc.field, cs, tc.targets)
			if got.Read {
				t.Errorf("Read = true, want false")
			}
			if len(got.Scores) != len(tc.targets) {
				t.Errorf("got %d scores, want %d (still parallel to the candidate list)", len(got.Scores), len(tc.targets))
			}
			for i, s := range got.Scores {
				if s.Posterior != 0 {
					t.Errorf("candidate %d: Posterior = %v, want 0 when the field was not read", i, s.Posterior)
				}
			}
		})
	}
}

// TestScoreField_ImpossibleTargetIsAScore: "aa" needs a frame for the blank
// between the two a's, so on a two-frame line it is genuinely impossible —
// LogLik -Inf with a scorable sibling, which is a real score, not "unread".
func TestScoreField_ImpossibleTargetIsAScore(t *testing.T) {
	f := FieldLattices{Lines: []localocr.LineLattice{spikeLine("aa", 0.9)}}
	got := ScoreField(f, testCharset(), [][]string{{"aa"}, {"a"}})
	if !got.Read {
		t.Fatal("Read = false, want true")
	}
	if !math.IsInf(got.Scores[0].LogLik, -1) {
		t.Errorf("\"aa\" on a 2-frame line: LogLik = %v, want -Inf", got.Scores[0].LogLik)
	}
	if got.Scores[0].Posterior != 0 {
		t.Errorf("-Inf LogLik must map to posterior 0, got %v", got.Scores[0].Posterior)
	}
	if math.Abs(got.Scores[1].Posterior-1) > 1e-12 {
		t.Errorf("the possible candidate takes the whole posterior; got %v", got.Scores[1].Posterior)
	}
}

// TestScoreField_UniformGarbageIsNearlyFlat is the anchor the low-score floor
// depends on: a line that says nothing spreads its posterior almost evenly over
// the candidates, which is what drives S below MinScore in match.go.
func TestScoreField_UniformGarbageIsNearlyFlat(t *testing.T) {
	f := FieldLattices{Lines: []localocr.LineLattice{uniformLine(8)}}
	targets := make([][]string, 0, 10)
	for _, c := range flatCandidates(10) {
		targets = append(targets, []string{c})
	}
	got := ScoreField(f, testCharset(), targets)
	if !got.Read {
		t.Fatal("Read = false, want true")
	}
	for i, s := range got.Scores {
		if math.Abs(s.Posterior-0.1) > 1e-12 {
			t.Errorf("candidate %d: Posterior = %.6f, want 0.1 on a line that says nothing", i, s.Posterior)
		}
	}
}

// flatCandidates returns n distinct same-length strings over the toy alphabet
// with no two ADJACENT characters equal.
//
// Both properties matter for the uniform-garbage anchor. On a lattice where
// every class is equally likely at every frame, a candidate's forward score
// depends only on its alignment structure — its length, and where CTC's
// "identical symbols need a blank between them" rule blocks a skip. Same length
// plus no repeats means every candidate has exactly the same structure, so the
// posteriors are exactly uniform rather than merely close, and the floor test
// downstream is pinned by arithmetic instead of by a tolerance.
func flatCandidates(n int) []string {
	var out []string
	for _, a := range testKeys {
		for _, b := range testKeys {
			if b == a {
				continue
			}
			for _, c := range testKeys {
				if c == b {
					continue
				}
				if len(out) == n {
					return out
				}
				out = append(out, string([]rune{a, b, c}))
			}
		}
	}
	return out
}
