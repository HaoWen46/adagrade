package localocr

import (
	"math"
	"testing"
)

// scoreKeys is the dictionary every test below scores against: three letters,
// so class 0 is the CTC blank, 'a'=1, 'b'=2, 'c'=3 and C=4 — a dense lattice
// row is four numbers a reader can add up by hand. 'z' is deliberately absent
// so the unmappable-rune path has something to reject.
var scoreKeys = []rune{'a', 'b', 'c'}

func scoreCharset() Charset { return newCharset(scoreKeys, false) }

// pathLogProb is the log-probability of ONE CTC alignment, written as a frame
// string: '_' is the blank class, '*' is a wildcard flank (which emits any
// class, so log-prob 0), and any other rune is that character's class.
//
// It reads the log-probs back out of the lattice rather than recomputing them
// from the decimal probabilities the row was written with. Lattice stores
// log-probs as float32 (~6e-8 relative), and a four-frame path would carry
// several 1e-7 of that quantization into a decimal expectation; sourcing both
// sides from the same stored values keeps the assertions on the forward
// algorithm itself, at 1e-9, instead of on float32 rounding.
func pathLogProb(t *testing.T, l Lattice, cs Charset, path string) float64 {
	t.Helper()
	frames := []rune(path)
	if len(frames) != l.T {
		t.Fatalf("alignment %q covers %d frames, lattice has %d", path, len(frames), l.T)
	}
	var sum float64
	for i, r := range frames {
		switch r {
		case '*':
			// Wildcard flank: "any class here", log-prob 0.
		case '_':
			sum += l.LogProbAt(i, 0)
		default:
			cls, ok := cs.Class(r)
			if !ok {
				t.Fatalf("alignment %q: %q is not in the charset", path, r)
			}
			sum += l.LogProbAt(i, cls)
		}
	}
	return sum
}

// sumPaths is the log of the total probability of a set of alignments — the
// value the forward algorithm must produce for a target whose complete
// alignment set is exactly these paths. It adds in probability space with
// plain Exp/Log so the expectation does not go through the implementation's
// own logAdd.
func sumPaths(t *testing.T, l Lattice, cs Charset, paths ...string) float64 {
	t.Helper()
	var total float64
	for _, p := range paths {
		total += math.Exp(pathLogProb(t, l, cs, p))
	}
	return math.Log(total)
}

// TestCTCScore_ExactSingleChar is the smallest possible forward pass: one
// frame, one character. ext = [_, a, _], only states 0 and 1 may start, and
// the only alignment that reaches a legal end state in one frame is "emit a",
// so the whole forward sum collapses to p(a).
func TestCTCScore_ExactSingleChar(t *testing.T) {
	cs := scoreCharset()
	// blank .10  a .70  b .10  c .10
	l := NewLattice(probRows([][]float64{{0.10, 0.70, 0.10, 0.10}}), 0)

	got, ok := ScoreTarget(l, cs, "a", ScoreOptions{})
	if !ok {
		t.Fatalf("ScoreTarget(a): ok=false, want true")
	}
	// LogLik = log 0.70 = -0.356674943938...; one character, so Norm == LogLik
	// and Prob == 0.70.
	if want := l.LogProbAt(0, 1); !closeTo(got.LogLik, want, 1e-12) {
		t.Errorf("LogLik: got %v, want %v (log p(a))", got.LogLik, want)
	}
	if !closeTo(got.LogLik, math.Log(0.70), 1e-6) {
		t.Errorf("LogLik: got %v, want %v (log 0.70)", got.LogLik, math.Log(0.70))
	}
	if got.RuneLen != 1 {
		t.Errorf("RuneLen: got %d, want 1", got.RuneLen)
	}
	if !closeTo(got.Norm, got.LogLik, 1e-15) {
		t.Errorf("Norm: got %v, want %v (== LogLik for a 1-rune target)", got.Norm, got.LogLik)
	}
	if !closeTo(got.Prob, 0.70, 1e-6) {
		t.Errorf("Prob: got %v, want 0.70", got.Prob)
	}
}

// abProbs greedy-decodes to "ab" and is the workhorse of the exact-sum,
// ranking and top-K tests. Rows sum to 1 so NewLattice treats them as an
// already-softmaxed export.
//
//	t   blank    a      b      c
//	0    .20    .60    .10    .10
//	1    .50    .20    .20    .10
//	2    .20    .10    .60    .10
//	3    .60    .10    .20    .10
var abProbs = [][]float64{
	{0.20, 0.60, 0.10, 0.10},
	{0.50, 0.20, 0.20, 0.10},
	{0.20, 0.10, 0.60, 0.10},
	{0.60, 0.10, 0.20, 0.10},
}

// abAlignments is every alignment of "ab" into abProbs' four frames. An
// alignment is _^i a^j _^k b^m _^n with j,m >= 1 and i+j+k+m+n = 4, which
// leaves i+k+n <= 2 and gives 6+3+3+1+1+1 = 15 of them. Their probabilities:
//
//	__ab .20*.50*.10*.20 = .00200    aa_b .60*.20*.20*.20 = .00480
//	a__b .60*.50*.20*.20 = .01200    aab_ .60*.20*.60*.60 = .04320
//	ab__ .60*.20*.20*.60 = .01440    _abb .20*.20*.60*.20 = .00480
//	_a_b .20*.20*.20*.20 = .00160    a_bb .60*.50*.60*.20 = .03600
//	_ab_ .20*.20*.60*.60 = .01440    abb_ .60*.20*.60*.60 = .04320
//	a_b_ .60*.50*.60*.60 = .10800    aaab .60*.20*.10*.20 = .00240
//	_aab .20*.20*.10*.20 = .00080    abbb .60*.20*.60*.20 = .01440
//	                                 aabb .60*.20*.60*.20 = .01440
//
// Total = 0.3164, so LogLik = log 0.3164 = -1.150553... and, over two
// characters, Prob = sqrt(0.3164) = 0.562494...
var abAlignments = []string{
	"__ab", "a__b", "ab__", "_a_b", "_ab_", "a_b_", "_aab", "aa_b",
	"aab_", "_abb", "a_bb", "abb_", "aaab", "abbb", "aabb",
}

// TestCTCScore_ExactTwoCharsOverFourFrames pins the full forward sum against
// an independent enumeration of all 15 alignments. The forward algorithm
// shares alignment prefixes; the enumeration does not, so agreement is real
// evidence the recursion visits exactly the right alignment set.
func TestCTCScore_ExactTwoCharsOverFourFrames(t *testing.T) {
	cs := scoreCharset()
	l := NewLattice(probRows(abProbs), 0)

	got, ok := ScoreTarget(l, cs, "ab", ScoreOptions{})
	if !ok {
		t.Fatalf("ScoreTarget(ab): ok=false, want true")
	}
	if want := sumPaths(t, l, cs, abAlignments...); !closeTo(got.LogLik, want, 1e-9) {
		t.Errorf("LogLik: got %v, want %v (sum over 15 alignments)", got.LogLik, want)
	}
	if !closeTo(got.LogLik, math.Log(0.3164), 1e-6) {
		t.Errorf("LogLik: got %v, want %v (log 0.3164)", got.LogLik, math.Log(0.3164))
	}
	if got.RuneLen != 2 {
		t.Errorf("RuneLen: got %d, want 2", got.RuneLen)
	}
	if !closeTo(got.Norm, got.LogLik/2, 1e-15) {
		t.Errorf("Norm: got %v, want %v (LogLik/2)", got.Norm, got.LogLik/2)
	}
	if !closeTo(got.Prob, math.Sqrt(0.3164), 1e-6) {
		t.Errorf("Prob: got %v, want %v (sqrt 0.3164)", got.Prob, math.Sqrt(0.3164))
	}
}

// aaProbs is a line whose first and last frames peak on 'a' with a
// blank-dominated frame between them — the only shape in which CTC can express
// a doubled character.
//
//	t   blank    a      b      c
//	0    .10    .70    .10    .10
//	1    .60    .20    .10    .10
//	2    .10    .80    .05    .05
var aaProbs = [][]float64{
	{0.10, 0.70, 0.10, 0.10},
	{0.60, 0.20, 0.10, 0.10},
	{0.10, 0.80, 0.05, 0.05},
}

// TestCTCScore_RepeatedCharNeedsSeparatingBlank exercises the rule that makes
// CTC CTC: the skip transition is forbidden between two identical symbols, so
// the second 'a' can only be reached through the blank state between them.
// Roster candidates with doubled characters (林林, "Lee") depend on it, and a
// scorer that allowed the skip would happily read a single long 'a' spike as
// "aa" and mis-rank every such candidate.
func TestCTCScore_RepeatedCharNeedsSeparatingBlank(t *testing.T) {
	cs := scoreCharset()
	l := NewLattice(probRows(aaProbs), 0)

	got, ok := ScoreTarget(l, cs, "aa", ScoreOptions{})
	if !ok {
		t.Fatalf("ScoreTarget(aa): ok=false, want true")
	}
	// Three frames leave exactly one alignment: a _ a. "aaa" collapses to "a",
	// and every other 3-frame string either does too or is not "aa" at all.
	// LogLik = log(.70*.60*.80) = log 0.336 = -1.090597...; over two
	// characters Prob = sqrt(0.336) = 0.579655...
	if want := sumPaths(t, l, cs, "a_a"); !closeTo(got.LogLik, want, 1e-9) {
		t.Errorf("LogLik: got %v, want %v (the single alignment a_a)", got.LogLik, want)
	}
	if !closeTo(got.LogLik, math.Log(0.336), 1e-6) {
		t.Errorf("LogLik: got %v, want %v (log 0.336)", got.LogLik, math.Log(0.336))
	}
	if !closeTo(got.Prob, math.Sqrt(0.336), 1e-6) {
		t.Errorf("Prob: got %v, want %v (sqrt 0.336)", got.Prob, math.Sqrt(0.336))
	}

	// Two frames cannot hold "aa" at all: the separating blank needs a frame of
	// its own. That is a score, not an error — "this candidate cannot be this
	// line" is exactly what the matcher wants to hear.
	short := NewLattice(probRows(aaProbs[:2]), 0)
	got, ok = ScoreTarget(short, cs, "aa", ScoreOptions{})
	if !ok {
		t.Fatalf("ScoreTarget(aa) on T=2: ok=false, want true")
	}
	if !math.IsInf(got.LogLik, -1) {
		t.Errorf("LogLik on T=2: got %v, want -Inf (aa needs >= 3 frames)", got.LogLik)
	}
	if !math.IsInf(got.Norm, -1) {
		t.Errorf("Norm on T=2: got %v, want -Inf", got.Norm)
	}
	if got.Prob != 0 {
		t.Errorf("Prob on T=2: got %v, want exactly 0 (not NaN)", got.Prob)
	}
	if got.RuneLen != 2 {
		t.Errorf("RuneLen on T=2: got %d, want 2", got.RuneLen)
	}

	// Control: the same two frames score "a" finitely, so the -Inf above is the
	// repeat rule talking and not a truncated lattice breaking the scorer.
	single, ok := ScoreTarget(short, cs, "a", ScoreOptions{})
	if !ok || math.IsInf(single.LogLik, -1) {
		t.Errorf("ScoreTarget(a) on T=2: got LogLik=%v ok=%v, want a finite score", single.LogLik, ok)
	}
}

// TestCTCScore_RanksTheGreedyDecodeHighest is the property the matcher
// actually consumes: on a line the model reads as "ab", the candidate "ab"
// must outrank candidates that differ in either character.
func TestCTCScore_RanksTheGreedyDecodeHighest(t *testing.T) {
	cs := scoreCharset()
	rows := probRows(abProbs)
	if text, _ := ctcGreedyDecode(rows, scoreKeys, false); text != "ab" {
		t.Fatalf("premise: greedy decode is %q, want %q", text, "ab")
	}
	l := NewLattice(rows, 0)

	score := func(target string) ScoreResult {
		t.Helper()
		got, ok := ScoreTarget(l, cs, target, ScoreOptions{})
		if !ok {
			t.Fatalf("ScoreTarget(%q): ok=false, want true", target)
		}
		return got
	}
	ab, ac, ba := score("ab"), score("ac"), score("ba")

	if !(ab.LogLik > ac.LogLik) {
		t.Errorf("LogLik: ab=%v, ac=%v, want ab strictly higher", ab.LogLik, ac.LogLik)
	}
	if !(ab.LogLik > ba.LogLik) {
		t.Errorf("LogLik: ab=%v, ba=%v, want ab strictly higher", ab.LogLik, ba.LogLik)
	}
	// Same rune length, so Prob is a monotone image of LogLik — asserted anyway
	// because the matcher ranks on Prob, not on LogLik.
	if !(ab.Prob > ac.Prob) {
		t.Errorf("Prob: ab=%v, ac=%v, want ab strictly higher", ab.Prob, ac.Prob)
	}
}

// spikeProbs is a "bac" line in the shape a real CTC head emits it: character
// spikes separated by blank-dominated frames. 'b' and 'c' play the surrounding
// text, 'a' is the target.
//
//	t   blank    a      b      c
//	0    .05    .03    .90    .02   <- b
//	1    .90    .04    .03    .03   <- blank
//	2    .05    .90    .03    .02   <- a
//	3    .90    .04    .03    .03   <- blank
//	4    .05    .02    .03    .90   <- c
var spikeProbs = [][]float64{
	{0.05, 0.03, 0.90, 0.02},
	{0.90, 0.04, 0.03, 0.03},
	{0.05, 0.90, 0.03, 0.02},
	{0.90, 0.04, 0.03, 0.03},
	{0.05, 0.02, 0.03, 0.90},
}

// tightProbs is the same "bac" line with NO blank frame anywhere: three
// adjacent character spikes. It exists to pin what the wildcard flanks do when
// the model never emits a blank between the surrounding text and the target.
//
//	t   blank    a      b      c
//	0    .04    .03    .90    .03   <- b
//	1    .04    .90    .03    .03   <- a
//	2    .04    .03    .03    .90   <- c
var tightProbs = [][]float64{
	{0.04, 0.03, 0.90, 0.03},
	{0.04, 0.90, 0.03, 0.03},
	{0.04, 0.03, 0.03, 0.90},
}

// TestCTCScore_WildcardFlanks covers substring scoring, which is what a real
// page needs: the line reads "座號 05 姓名 王小明" and the candidate is just the
// name. Without the flanks the candidate has to explain every frame of the
// line and scores near zero however well it matches its own region.
func TestCTCScore_WildcardFlanks(t *testing.T) {
	cs := scoreCharset()
	l := NewLattice(probRows(spikeProbs), 0)
	if text, _ := ctcGreedyDecode(probRows(spikeProbs), scoreKeys, false); text != "bac" {
		t.Fatalf("premise: greedy decode is %q, want %q", text, "bac")
	}

	with, ok := ScoreTarget(l, cs, "a", ScoreOptions{AllowSurroundingText: true})
	if !ok {
		t.Fatalf("ScoreTarget(a, flanks): ok=false, want true")
	}
	without, ok := ScoreTarget(l, cs, "a", ScoreOptions{})
	if !ok {
		t.Fatalf("ScoreTarget(a): ok=false, want true")
	}

	// Without flanks every alignment is _^i a^j _^k over all five frames, so it
	// pays .05 on both the 'b' frame and the 'c' frame. Summing the 15 such
	// alignments (dominant: __a__ = .05*.90*.90*.90*.05 = 1.8225e-3) gives
	// 2.188314e-3 — tiny, and correctly so: "a" is not this whole line.
	if !closeTo(without.LogLik, math.Log(2.188314e-3), 1e-6) {
		t.Errorf("LogLik without flanks: got %v, want %v (log 2.188314e-3)",
			without.LogLik, math.Log(2.188314e-3))
	}
	// With flanks the alignment *_a_* costs nothing outside the target region:
	// 1*.90*.90*.90*1 = 0.729, and the forward sum is over a superset of that,
	// so Prob clears 0.7 — two and a half orders of magnitude above the
	// whole-line reading.
	if !(with.Prob > 0.70) {
		t.Errorf("Prob with flanks: got %v, want > 0.70 (alignment *_a_* alone is 0.729)", with.Prob)
	}
	if !(with.LogLik > without.LogLik+math.Log(100)) {
		t.Errorf("flanks: with=%v without=%v, want with > without + log 100",
			with.LogLik, without.LogLik)
	}

	// Adjacency of the flanks, pinned exactly on the no-blank line. A wildcard
	// state self-loops and advances, but may NOT skip: the skip transition is
	// reserved for two distinct symbols, and a wildcard has no identity to
	// compare, so entering the target from the leading wildcard goes through
	// the leading blank state (and symmetrically at the tail). On tightProbs
	// that costs a .04 blank frame on each side, which is why the flanks buy
	// much less here than on a line with real blanks.
	//
	// The alignment sets are literally the without-flanks set plus two paths,
	// which is the "flanks only ever ADD alignments" invariant made concrete:
	//
	//	without: __a .04*.04*.03 = 4.8e-5   aaa .03*.90*.03 = 8.10e-4
	//	         _aa .04*.90*.03 = 1.08e-3  aa_ .03*.90*.04 = 1.08e-3
	//	         _a_ .04*.90*.04 = 1.44e-3  a__ .03*.04*.04 = 4.8e-5
	//	         total 4.506e-3
	//	with:    the same six, plus
	//	         *_a 1*.04*.03   = 1.20e-3  a_* .03*.04*1   = 1.20e-3
	//	         total 6.906e-3
	tight := NewLattice(probRows(tightProbs), 0)
	tightWithout := []string{"__a", "_aa", "_a_", "aaa", "aa_", "a__"}
	tightWith := append([]string{"*_a", "a_*"}, tightWithout...)

	gotTightWithout, ok := ScoreTarget(tight, cs, "a", ScoreOptions{})
	if !ok {
		t.Fatalf("ScoreTarget(a) on tight line: ok=false, want true")
	}
	if want := sumPaths(t, tight, cs, tightWithout...); !closeTo(gotTightWithout.LogLik, want, 1e-9) {
		t.Errorf("tight, no flanks: got %v, want %v (6 alignments, total 4.506e-3)",
			gotTightWithout.LogLik, want)
	}
	if !closeTo(gotTightWithout.LogLik, math.Log(4.506e-3), 1e-6) {
		t.Errorf("tight, no flanks: got %v, want %v (log 4.506e-3)",
			gotTightWithout.LogLik, math.Log(4.506e-3))
	}

	gotTightWith, ok := ScoreTarget(tight, cs, "a", ScoreOptions{AllowSurroundingText: true})
	if !ok {
		t.Fatalf("ScoreTarget(a, flanks) on tight line: ok=false, want true")
	}
	if want := sumPaths(t, tight, cs, tightWith...); !closeTo(gotTightWith.LogLik, want, 1e-9) {
		t.Errorf("tight, flanks: got %v, want %v (8 alignments, total 6.906e-3)",
			gotTightWith.LogLik, want)
	}
	if !closeTo(gotTightWith.LogLik, math.Log(6.906e-3), 1e-6) {
		t.Errorf("tight, flanks: got %v, want %v (log 6.906e-3)",
			gotTightWith.LogLik, math.Log(6.906e-3))
	}

	// A one-frame line peaked on the target: the flank states can be skipped
	// entirely (state 2 is a legal start and a legal end when the flanks are
	// present), so the wildcards add no alignment and cannot inflate an exact
	// match.
	peak := NewLattice(probRows([][]float64{{0.10, 0.70, 0.10, 0.10}}), 0)
	peakWith, _ := ScoreTarget(peak, cs, "a", ScoreOptions{AllowSurroundingText: true})
	peakWithout, _ := ScoreTarget(peak, cs, "a", ScoreOptions{})
	if !closeTo(peakWith.LogLik, peakWithout.LogLik, 1e-9) {
		t.Errorf("T=1 peaked: with flanks %v, without %v, want equal",
			peakWith.LogLik, peakWithout.LogLik)
	}

	// The invariant behind all of the above: flanks only ever add alignments,
	// so they can never lower a score. Checked across every lattice and target
	// in this file, including the ones where the flanks are useless.
	for _, tc := range []struct {
		name   string
		probs  [][]float64
		target string
	}{
		{"spike/a", spikeProbs, "a"},
		{"spike/bac", spikeProbs, "bac"},
		{"tight/a", tightProbs, "a"},
		{"tight/bac", tightProbs, "bac"},
		{"ab/ab", abProbs, "ab"},
		{"ab/ca", abProbs, "ca"},
		{"aa/aa", aaProbs, "aa"},
		{"peak/a", [][]float64{{0.10, 0.70, 0.10, 0.10}}, "a"},
	} {
		lt := NewLattice(probRows(tc.probs), 0)
		w, okw := ScoreTarget(lt, cs, tc.target, ScoreOptions{AllowSurroundingText: true})
		n, okn := ScoreTarget(lt, cs, tc.target, ScoreOptions{})
		if !okw || !okn {
			t.Fatalf("%s: ok=false, want true for both modes", tc.name)
		}
		if w.LogLik < n.LogLik {
			t.Errorf("%s: with flanks %v < without %v, want with >= without", tc.name, w.LogLik, n.LogLik)
		}
		// A plain match sums disjoint alignments, so its total probability
		// cannot exceed 1. (The wildcard modes are exempt: two wildcard
		// alignments can cover the same class sequence and double-count.)
		if n.Prob > 1 {
			t.Errorf("%s: Prob without flanks is %v, want <= 1", tc.name, n.Prob)
		}
	}
}

// TestCTCScore_WildcardBonusScalesWithTheLineNotTheTarget pins the caveat
// ScoreTarget's doc comment gives its caller, so the guidance cannot go stale
// silently. Under AllowSurroundingText the flanks absorb the rest of the line
// for free, and that bonus depends on how much line there is to absorb — not
// on how long the candidate is. Dividing it by RuneLen therefore hands short
// candidates an advantage they have not earned, and Prob inverts a ranking
// LogLik gets right. A matcher that ranks roster candidates of differing
// lengths on Prob with the flanks on will prefer the shortest name on the
// roster.
func TestCTCScore_WildcardBonusScalesWithTheLineNotTheTarget(t *testing.T) {
	cs := scoreCharset()
	// 24 blank-dominated frames with an 'a' spike at 10 and a 'b' spike at 12:
	// a short line reading "ab" with a lot of surrounding nothing.
	rows := make([][]float64, 24)
	for i := range rows {
		rows[i] = []float64{0.90, 0.04, 0.03, 0.03}
	}
	rows[10] = []float64{0.05, 0.90, 0.03, 0.02}
	rows[12] = []float64{0.05, 0.02, 0.90, 0.03}
	if text, _ := ctcGreedyDecode(probRows(rows), scoreKeys, false); text != "ab" {
		t.Fatalf("premise: greedy decode is %q, want %q", text, "ab")
	}
	l := NewLattice(probRows(rows), 0)

	flanked := func(target string) ScoreResult {
		t.Helper()
		got, ok := ScoreTarget(l, cs, target, ScoreOptions{AllowSurroundingText: true})
		if !ok {
			t.Fatalf("ScoreTarget(%q, flanks): ok=false, want true", target)
		}
		return got
	}
	one, two := flanked("a"), flanked("ab")

	// The line really does read "ab", and the raw forward sum says so.
	if !(two.LogLik > one.LogLik) {
		t.Errorf("LogLik: ab=%v, a=%v, want ab higher (the line reads ab)", two.LogLik, one.LogLik)
	}
	// Per-character normalization inverts it: the same line-sized wildcard
	// bonus is spread over one character instead of two.
	if !(one.Prob > two.Prob) {
		t.Errorf("Prob: a=%v, ab=%v, want the short candidate to win — if this "+
			"now passes, the normalization changed and ScoreTarget's ranking "+
			"guidance needs updating", one.Prob, two.Prob)
	}
	// And the flanked score is not a probability at all.
	if !(one.Prob > 1) {
		t.Errorf("Prob with flanks: got %v, want > 1 (wildcard alignments double-count)", one.Prob)
	}
	// Exact mode on the same line stays a probability, whatever the length.
	for _, target := range []string{"a", "ab", "abc"} {
		got, ok := ScoreTarget(l, cs, target, ScoreOptions{})
		if !ok {
			t.Fatalf("ScoreTarget(%q): ok=false, want true", target)
		}
		if got.Prob > 1 {
			t.Errorf("Prob without flanks for %q: got %v, want <= 1", target, got.Prob)
		}
	}
}

// TestCTCScore_UnscorableTargets covers the three ways a candidate is not a
// score at all. None of them is an error: a roster full of names the model's
// dictionary cannot spell is a configuration fact the caller handles once, not
// an exception per candidate.
func TestCTCScore_UnscorableTargets(t *testing.T) {
	cs := scoreCharset()
	l := NewLattice(probRows(abProbs), 0)

	for _, tc := range []struct {
		name    string
		lattice Lattice
		target  string
	}{
		{"empty target", l, ""},
		{"unmappable rune alone", l, "z"},
		{"unmappable rune inside", l, "abz"},
		{"unmappable rune first", l, "za"},
		{"empty lattice", NewLattice(nil, 0), "ab"},
		{"empty lattice and target", Lattice{}, ""},
	} {
		got, ok := ScoreTarget(tc.lattice, cs, tc.target, ScoreOptions{})
		if ok {
			t.Errorf("%s: ok=true, want false", tc.name)
		}
		if got != (ScoreResult{}) {
			t.Errorf("%s: got %+v, want the zero ScoreResult", tc.name, got)
		}
		// The wildcard path must reject the same inputs, not walk a lattice
		// with no frames in it.
		if _, ok := ScoreTarget(tc.lattice, cs, tc.target, ScoreOptions{AllowSurroundingText: true}); ok {
			t.Errorf("%s with flanks: ok=true, want false", tc.name)
		}
	}
}

// TestCTCScore_NormalizationAcrossLengths is why ScoreResult carries Norm and
// Prob at all: a longer target accumulates more log-probability than a short
// one at the same per-character quality, so raw LogLik cannot be compared
// across lines of different lengths — which is exactly what a fixed "is this
// match good enough" threshold has to do. Dividing by rune count turns it into
// a per-character quality that means the same thing on a 1-frame line and a
// 3-frame one, as the cases below show at a constant 0.80 per character.
//
// Note this is about comparing scores from DIFFERENT lattices. Candidates
// scored against ONE line all sum over the same T frames, so there is no
// mechanical length bias to remove there; see ScoreTarget's doc comment for
// which of LogLik and Prob to rank on.
func TestCTCScore_NormalizationAcrossLengths(t *testing.T) {
	cs := scoreCharset()

	// One frame at 0.80 quality, one character.
	one := NewLattice(probRows([][]float64{{0.10, 0.80, 0.05, 0.05}}), 0)
	// Two frames at 0.80 quality, two characters. T == RuneLen leaves exactly
	// one alignment (ab), so LogLik = log(.80*.80) and Prob = sqrt(0.64) is
	// EXACTLY 0.80: no alignment combinatorics to blur it.
	two := NewLattice(probRows([][]float64{
		{0.10, 0.80, 0.05, 0.05},
		{0.10, 0.05, 0.80, 0.05},
	}), 0)

	gotOne, ok1 := ScoreTarget(one, cs, "a", ScoreOptions{})
	gotTwo, ok2 := ScoreTarget(two, cs, "ab", ScoreOptions{})
	if !ok1 || !ok2 {
		t.Fatalf("ok: got %v and %v, want true for both", ok1, ok2)
	}
	if !closeTo(gotOne.Prob, 0.80, 1e-6) {
		t.Errorf("Prob for 1 char at 0.80: got %v, want 0.80", gotOne.Prob)
	}
	if !closeTo(gotTwo.Prob, 0.80, 1e-6) {
		t.Errorf("Prob for 2 chars at 0.80: got %v, want 0.80", gotTwo.Prob)
	}
	if !closeTo(gotTwo.LogLik, 2*gotOne.LogLik, 1e-6) {
		t.Errorf("LogLik: 2 chars %v, 2x 1 char %v — want the raw sums to differ by length",
			gotTwo.LogLik, 2*gotOne.LogLik)
	}

	// Add a blank frame between the two spikes and the same 0.80 per-character
	// quality no longer lands exactly on 0.80: three frames admit five
	// alignments instead of one, and their extra mass raises the total from
	// .80*.80*.80 = 0.512 to
	//   _ab .10*.10*.80 = .008   aab .80*.10*.80 = .064
	//   a_b .80*.80*.80 = .512   abb .80*.05*.80 = .032
	//   ab_ .80*.05*.10 = .004
	//   total 0.620 -> Prob = sqrt(0.620) = 0.787400...
	// which is why this comparison is an order-of-magnitude claim (tolerance
	// 0.1) rather than an equality: normalization removes the LENGTH bias, not
	// the alignment-count bias.
	three := NewLattice(probRows([][]float64{
		{0.10, 0.80, 0.05, 0.05},
		{0.80, 0.10, 0.05, 0.05},
		{0.10, 0.05, 0.80, 0.05},
	}), 0)
	gotThree, ok := ScoreTarget(three, cs, "ab", ScoreOptions{})
	if !ok {
		t.Fatalf("ScoreTarget(ab) on 3 frames: ok=false, want true")
	}
	if want := sumPaths(t, three, cs, "_ab", "a_b", "ab_", "aab", "abb"); !closeTo(gotThree.LogLik, want, 1e-9) {
		t.Errorf("LogLik on 3 frames: got %v, want %v (5 alignments)", gotThree.LogLik, want)
	}
	if !closeTo(gotThree.Prob, math.Sqrt(0.620), 1e-6) {
		t.Errorf("Prob on 3 frames: got %v, want %v (sqrt 0.620)", gotThree.Prob, math.Sqrt(0.620))
	}
	if !closeTo(gotThree.Prob, 0.80, 0.1) {
		t.Errorf("Prob on 3 frames: got %v, want within 0.1 of 0.80", gotThree.Prob)
	}
}

// TestCTCScore_TopKLatticeMatchesDense checks the scorer against the
// compression production actually runs on. Keeping every class a target asks
// about must not change its score at all; keeping only the argmax must degrade
// it to the residual floor rather than to -Inf, because one dropped class on
// one frame would otherwise annihilate a whole candidate.
func TestCTCScore_TopKLatticeMatchesDense(t *testing.T) {
	cs := scoreCharset()
	rows := probRows(abProbs)

	dense, ok := ScoreTarget(NewLattice(rows, 0), cs, "ab", ScoreOptions{})
	if !ok {
		t.Fatalf("ScoreTarget(ab) on dense: ok=false, want true")
	}
	// K=3 of 4 classes drops only 'c' on every frame (ties break to the lower
	// class, so 'a' survives frames 2 and 3), and "ab" never asks about 'c'.
	keepAll, ok := ScoreTarget(NewLattice(rows, 3), cs, "ab", ScoreOptions{})
	if !ok {
		t.Fatalf("ScoreTarget(ab) on K=3: ok=false, want true")
	}
	if !closeTo(keepAll.LogLik, dense.LogLik, 1e-9) {
		t.Errorf("K=3 LogLik: got %v, want %v (dense)", keepAll.LogLik, dense.LogLik)
	}
	if !closeTo(keepAll.Prob, dense.Prob, 1e-9) {
		t.Errorf("K=3 Prob: got %v, want %v (dense)", keepAll.Prob, dense.Prob)
	}

	// K=1 keeps only the argmax, so "ab" reads the residual floor on most
	// frames: the score must move, and must stay a usable number.
	argmaxOnly, ok := ScoreTarget(NewLattice(rows, 1), cs, "ab", ScoreOptions{})
	if !ok {
		t.Fatalf("ScoreTarget(ab) on K=1: ok=false, want true")
	}
	if math.IsInf(argmaxOnly.LogLik, 0) || math.IsNaN(argmaxOnly.LogLik) {
		t.Errorf("K=1 LogLik: got %v, want finite (residual floor, not -Inf)", argmaxOnly.LogLik)
	}
	if !(argmaxOnly.Prob > 0) {
		t.Errorf("K=1 Prob: got %v, want > 0", argmaxOnly.Prob)
	}
	if closeTo(argmaxOnly.LogLik, dense.LogLik, 1e-6) {
		t.Errorf("K=1 LogLik: got %v, want it to differ from dense %v", argmaxOnly.LogLik, dense.LogLik)
	}
}

// TestCTCScore_LogAddIdentities pins the one primitive the whole forward pass
// is built from. Every impossible state carries -Inf and every real line mixes
// log-probs of wildly different magnitudes, so a logAdd that returns NaN on
// -Inf + -Inf, or overflows on a large gap, would corrupt scores silently
// rather than fail.
func TestCTCScore_LogAddIdentities(t *testing.T) {
	ninf := math.Inf(-1)

	if got := logAdd(ninf, ninf); !math.IsInf(got, -1) {
		t.Errorf("logAdd(-Inf, -Inf): got %v, want -Inf", got)
	}
	if got := logAdd(ninf, -0.5); got != -0.5 {
		t.Errorf("logAdd(-Inf, -0.5): got %v, want -0.5", got)
	}
	if got := logAdd(-0.5, ninf); got != -0.5 {
		t.Errorf("logAdd(-0.5, -Inf): got %v, want -0.5", got)
	}
	// log(1+1) = log 2.
	if got := logAdd(0, 0); !closeTo(got, math.Ln2, 1e-15) {
		t.Errorf("logAdd(0, 0): got %v, want %v", got, math.Ln2)
	}
	// log(0.3 + 0.7) = log 1 = 0.
	if got := logAdd(math.Log(0.3), math.Log(0.7)); !closeTo(got, 0, 1e-15) {
		t.Errorf("logAdd(log .3, log .7): got %v, want 0", got)
	}
	// Symmetric, and dominated by the larger term when the gap is huge: the
	// naive log(exp(a)+exp(b)) would overflow at 1e300 and underflow at -745.
	for _, tc := range []struct{ a, b, want float64 }{
		{1e300, -1e300, 1e300},
		{-1e300, 1e300, 1e300},
		{-745, -1e6, -745},
		{-1e6, -745, -745},
		{1000, 1000 - 100, 1000},
	} {
		if got := logAdd(tc.a, tc.b); !closeTo(got, tc.want, 1e-9*math.Abs(tc.want)+1e-9) {
			t.Errorf("logAdd(%v, %v): got %v, want ~%v", tc.a, tc.b, got, tc.want)
		}
	}
	// Equal extreme magnitudes still pick up the log 2.
	if got := logAdd(-745, -745); !closeTo(got, -745+math.Ln2, 1e-12) {
		t.Errorf("logAdd(-745, -745): got %v, want %v", got, -745+math.Ln2)
	}
	// Nothing above may produce a NaN.
	for _, pair := range [][2]float64{{ninf, ninf}, {ninf, 0}, {0, ninf}, {1e300, -1e300}, {-745, -745}} {
		if got := logAdd(pair[0], pair[1]); math.IsNaN(got) {
			t.Errorf("logAdd(%v, %v): got NaN", pair[0], pair[1])
		}
	}
}
