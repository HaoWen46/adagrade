package localocr

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/imaging"
)

// latticeProbs is the hand-built 3-timestep, 6-class probability table shared
// by the softmaxed and the raw-logit cases. Every row sums to 1 and no two
// entries in a row are equal, so "the top K classes" has exactly one right
// answer and the expectations below can be written out by hand.
var latticeProbs = [][]float64{
	{0.50, 0.20, 0.12, 0.08, 0.06, 0.04},
	{0.05, 0.04, 0.60, 0.20, 0.08, 0.03},
	{0.02, 0.03, 0.05, 0.10, 0.30, 0.50},
}

// probRows renders the table as already-softmaxed model output (what a
// PP-OCRv4 export whose graph ends in Softmax emits).
func probRows(p [][]float64) [][]float32 {
	out := make([][]float32, len(p))
	for i, row := range p {
		out[i] = make([]float32, len(row))
		for j, v := range row {
			out[i][j] = float32(v)
		}
	}
	return out
}

// logitRows renders the same distribution as RAW logits (what PP-OCRv5 emits):
// log(p) plus an arbitrary per-matrix shift, which softmax is invariant to.
func logitRows(p [][]float64, shift float64) [][]float32 {
	out := make([][]float32, len(p))
	for i, row := range p {
		out[i] = make([]float32, len(row))
		for j, v := range row {
			out[i][j] = float32(math.Log(v) + shift)
		}
	}
	return out
}

func closeTo(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

// TestNewLattice_SoftmaxedTopK pins the whole compressed representation for
// already-softmaxed input: which classes survive, their log-probs, and the
// residual floor for everything discarded.
func TestNewLattice_SoftmaxedTopK(t *testing.T) {
	l := NewLattice(probRows(latticeProbs), 2)

	if l.T != 3 || l.K != 2 {
		t.Fatalf("shape: got T=%d K=%d, want T=3 K=2", l.T, l.K)
	}
	// Kept classes are ordered by descending probability within a timestep.
	wantClasses := []int32{0, 1, 2, 3, 5, 4}
	if !slices.Equal(l.Classes, wantClasses) {
		t.Fatalf("kept classes: got %v, want %v", l.Classes, wantClasses)
	}
	wantLogP := []float64{
		math.Log(0.50), math.Log(0.20),
		math.Log(0.60), math.Log(0.20),
		math.Log(0.50), math.Log(0.30),
	}
	if len(l.LogP) != len(wantLogP) {
		t.Fatalf("LogP length: got %d, want %d", len(l.LogP), len(wantLogP))
	}
	for i, want := range wantLogP {
		if got := float64(l.LogP[i]); !closeTo(got, want, 1e-6) {
			t.Errorf("LogP[%d]: got %v, want %v", i, got, want)
		}
	}
	// LogRest is the discarded mass smeared uniformly over the C-K classes
	// that did not make the cut.
	wantRest := []float64{
		math.Log((1 - 0.70) / 4),
		math.Log((1 - 0.80) / 4),
		math.Log((1 - 0.80) / 4),
	}
	if len(l.LogRest) != len(wantRest) {
		t.Fatalf("LogRest length: got %d, want %d", len(l.LogRest), len(wantRest))
	}
	for i, want := range wantRest {
		if got := float64(l.LogRest[i]); !closeTo(got, want, 1e-6) {
			t.Errorf("LogRest[%d]: got %v, want %v", i, got, want)
		}
	}
	// LogProbAt agrees with the raw arrays for kept classes.
	if got := l.LogProbAt(1, 2); !closeTo(got, math.Log(0.60), 1e-6) {
		t.Errorf("LogProbAt(1,2): got %v, want %v", got, math.Log(0.60))
	}
	if got := l.LogProbAt(2, 4); !closeTo(got, math.Log(0.30), 1e-6) {
		t.Errorf("LogProbAt(2,4): got %v, want %v", got, math.Log(0.30))
	}
}

// TestNewLattice_RawLogitsMatchSoftmaxedInput is the log-softmax correctness
// case: the same distribution handed in as raw logits (any shift) must produce
// the same lattice as handing in the probabilities directly.
func TestNewLattice_RawLogitsMatchSoftmaxedInput(t *testing.T) {
	want := NewLattice(probRows(latticeProbs), 2)
	for _, shift := range []float64{0, 3.5, -7.25} {
		rows := logitRows(latticeProbs, shift)
		if looksSoftmaxed(rows[0]) {
			t.Fatalf("shift %v: fixture precondition — logits must not look softmaxed", shift)
		}
		got := NewLattice(rows, 2)
		if got.T != want.T || got.K != want.K {
			t.Fatalf("shift %v: shape got T=%d K=%d, want T=%d K=%d", shift, got.T, got.K, want.T, want.K)
		}
		if !slices.Equal(got.Classes, want.Classes) {
			t.Fatalf("shift %v: kept classes got %v, want %v", shift, got.Classes, want.Classes)
		}
		for i := range want.LogP {
			if !closeTo(float64(got.LogP[i]), float64(want.LogP[i]), 1e-5) {
				t.Errorf("shift %v: LogP[%d] got %v, want %v", shift, i, got.LogP[i], want.LogP[i])
			}
		}
		for i := range want.LogRest {
			if !closeTo(float64(got.LogRest[i]), float64(want.LogRest[i]), 1e-5) {
				t.Errorf("shift %v: LogRest[%d] got %v, want %v", shift, i, got.LogRest[i], want.LogRest[i])
			}
		}
	}
}

// TestLattice_ResidualFloor covers the out-of-top-K lookup and the restMass
// clamp: a class the compression dropped must still score, finitely, and never
// as -Inf/NaN — the scorer multiplies these together over a whole name.
func TestLattice_ResidualFloor(t *testing.T) {
	l := NewLattice(probRows(latticeProbs), 2)

	// Class 2 is outside the top 2 at t=0 (p=0.12), class 0 is inside.
	if got, want := l.LogProbAt(0, 2), float64(l.LogRest[0]); got != want {
		t.Errorf("dropped class: LogProbAt(0,2) = %v, want LogRest[0] = %v", got, want)
	}
	if got := l.LogProbAt(0, 0); !closeTo(got, math.Log(0.50), 1e-6) {
		t.Errorf("kept class: LogProbAt(0,0) = %v, want %v", got, math.Log(0.50))
	}
	// A class index beyond the class axis is simply "not among the kept K".
	if got, want := l.LogProbAt(0, 5000), float64(l.LogRest[0]); got != want {
		t.Errorf("unknown class: LogProbAt(0,5000) = %v, want LogRest[0] = %v", got, want)
	}

	t.Run("kept mass of 1 still yields a finite floor", func(t *testing.T) {
		// The top-K carries the entire mass: 1-Σ is 0 (or negative from float
		// noise), which would make log(restMass) -Inf without the clamp.
		one := NewLattice([][]float32{{1, 0, 0, 0}}, 2)
		got := float64(one.LogRest[0])
		if math.IsInf(got, 0) || math.IsNaN(got) {
			t.Fatalf("LogRest must stay finite, got %v", got)
		}
		// Tolerance is float32 storage resolution at this magnitude (ulp ~2e-6).
		if want := math.Log(1e-12 / 2); !closeTo(got, want, 1e-5) {
			t.Errorf("clamped LogRest: got %v, want %v", got, want)
		}
		for i, v := range one.LogP {
			if math.IsInf(float64(v), 0) || math.IsNaN(float64(v)) {
				t.Errorf("LogP[%d] = %v: zero-probability kept class must be floored, not -Inf", i, v)
			}
		}
	})

	t.Run("non-finite input never escapes", func(t *testing.T) {
		// Defensive: a broken model could emit NaN/Inf. Garbage in is fine;
		// NaN out is not, because it would silently poison every candidate
		// score downstream.
		nan := float32(math.NaN())
		inf := float32(math.Inf(1))
		rows := [][]float32{
			{nan, 1, 2, 3, 4, 5},
			{inf, -inf, 0, 0, 1, 2},
			{nan, nan, nan, nan, nan, nan},
		}
		for _, k := range []int{2, 0} {
			l := NewLattice(rows, k)
			for i, v := range l.LogP {
				if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
					t.Errorf("topK=%d: LogP[%d] = %v, want finite", k, i, v)
				}
			}
			for i, v := range l.LogRest {
				if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
					t.Errorf("topK=%d: LogRest[%d] = %v, want finite", k, i, v)
				}
			}
		}
	})
}

// TestNewLattice_DenseMode covers the uncompressed representation used by
// tests: every class keeps its own log-prob, indexed directly.
func TestNewLattice_DenseMode(t *testing.T) {
	const C = 6
	for _, topK := range []int{0, -1, C, C + 4} {
		l := NewLattice(probRows(latticeProbs), topK)
		if l.K != 0 {
			t.Fatalf("topK=%d: want dense (K==0), got K=%d", topK, l.K)
		}
		if l.Classes != nil || l.LogRest != nil {
			t.Fatalf("topK=%d: dense lattice must leave Classes/LogRest nil, got %v / %v", topK, l.Classes, l.LogRest)
		}
		if len(l.LogP) != l.T*C {
			t.Fatalf("topK=%d: dense LogP length %d, want %d", topK, len(l.LogP), l.T*C)
		}
		for tt, row := range latticeProbs {
			for class, p := range row {
				want := math.Log(p)
				if got := l.LogProbAt(tt, class); !closeTo(got, want, 1e-6) {
					t.Errorf("topK=%d: LogProbAt(%d,%d) = %v, want %v", topK, tt, class, got, want)
				}
			}
		}
	}
}

// TestNewLattice_TopKMatchesBruteForce guards the partial-selection code
// (a bounded heap, not a full sort) against a plain sort of the same row.
func TestNewLattice_TopKMatchesBruteForce(t *testing.T) {
	const C, K = 200, 8
	// Deterministic pseudo-random row (small LCG, no math/rand dependency so
	// the fixture cannot drift with a Go release).
	x := uint32(12345)
	row := make([]float32, C)
	for i := range row {
		x = x*1664525 + 1013904223
		row[i] = float32(x>>8) / float32(1<<24)
	}
	idx := make([]int, C)
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		if c := cmp.Compare(row[b], row[a]); c != 0 {
			return c // descending probability
		}
		return cmp.Compare(a, b) // ties: lower class index first
	})
	want := make([]int32, K)
	for i := 0; i < K; i++ {
		want[i] = int32(idx[i])
	}

	l := NewLattice([][]float32{row}, K)
	if !slices.Equal(l.Classes, want) {
		t.Fatalf("top-%d selection: got %v, want %v", K, l.Classes, want)
	}
}

func TestNewLattice_EmptyRowsIsZeroLattice(t *testing.T) {
	l := NewLattice(nil, 64)
	if l.T != 0 || l.K != 0 || l.Classes != nil || l.LogP != nil || l.LogRest != nil {
		t.Fatalf("empty rows must yield the zero lattice, got %+v", l)
	}
}

// TestLattice_LogProbAtRejectsOutOfRange pins the documented programmer-error
// behavior: a bad timestep or a negative class panics rather than returning a
// plausible number.
func TestLattice_LogProbAtRejectsOutOfRange(t *testing.T) {
	sparse := NewLattice(probRows(latticeProbs), 2)
	dense := NewLattice(probRows(latticeProbs), 0)
	cases := []struct {
		name string
		call func()
	}{
		{"sparse t<0", func() { sparse.LogProbAt(-1, 0) }},
		{"sparse t==T", func() { sparse.LogProbAt(3, 0) }},
		{"sparse class<0", func() { sparse.LogProbAt(0, -1) }},
		{"dense t==T", func() { dense.LogProbAt(3, 0) }},
		{"dense class<0", func() { dense.LogProbAt(0, -1) }},
		{"dense class>=C", func() { dense.LogProbAt(0, 6) }},
		{"zero lattice", func() { Lattice{}.LogProbAt(0, 0) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: want panic, got none", tc.name)
				}
			}()
			tc.call()
		})
	}
}

// --- Charset ---------------------------------------------------------------

func TestCharset_ClassMapping(t *testing.T) {
	keys := []rune{'a', 'b', 'c'}

	cs := newCharset(keys, false)
	if got := cs.NumClasses(); got != 4 {
		t.Errorf("NumClasses (blank only): got %d, want 4", got)
	}
	for r, want := range map[rune]int{'a': 1, 'b': 2, 'c': 3} {
		got, ok := cs.Class(r)
		if !ok || got != want {
			t.Errorf("Class(%q): got (%d,%v), want (%d,true)", r, got, ok, want)
		}
	}
	if got, ok := cs.Class('z'); ok || got != 0 {
		t.Errorf("unknown rune: Class('z') = (%d,%v), want (0,false)", got, ok)
	}
	if got, ok := cs.Class(' '); ok || got != 0 {
		t.Errorf("no trailing-space class: Class(' ') = (%d,%v), want (0,false)", got, ok)
	}

	ts := newCharset(keys, true)
	if got := ts.NumClasses(); got != 5 {
		t.Errorf("NumClasses (trailing space): got %d, want 5", got)
	}
	if got, ok := ts.Class(' '); !ok || got != 4 {
		t.Errorf("trailing-space class: Class(' ') = (%d,%v), want (4,true)", got, ok)
	}
	if got, ok := ts.Class('a'); !ok || got != 1 {
		t.Errorf("trailing space must not disturb key classes: Class('a') = (%d,%v), want (1,true)", got, ok)
	}

	// The zero Charset (an engine that never loaded a dictionary) answers
	// rather than panicking.
	var zero Charset
	if got, ok := zero.Class('a'); ok || got != 0 {
		t.Errorf("zero Charset: Class('a') = (%d,%v), want (0,false)", got, ok)
	}
	if got := zero.NumClasses(); got != 1 {
		t.Errorf("zero Charset: NumClasses = %d, want 1 (blank only)", got)
	}
}

// TestCharset_AmbiguousDictionaryEntries pins which class wins when the same
// rune is reachable twice: a duplicated dictionary entry keeps the lower class,
// and a dictionary that already carries ' ' beats the appended trailing-space
// class (some PP-OCR dictionaries ship a space glyph).
func TestCharset_AmbiguousDictionaryEntries(t *testing.T) {
	dup := newCharset([]rune{'a', 'b', 'a'}, false)
	if got, ok := dup.Class('a'); !ok || got != 1 {
		t.Errorf("duplicate entry: Class('a') = (%d,%v), want (1,true)", got, ok)
	}
	sp := newCharset([]rune{'a', ' ', 'c'}, true)
	if got, ok := sp.Class(' '); !ok || got != 2 {
		t.Errorf("dictionary space must win over the trailing class: Class(' ') = (%d,%v), want (2,true)", got, ok)
	}
	if got := sp.NumClasses(); got != 5 {
		t.Errorf("NumClasses still counts the trailing class: got %d, want 5", got)
	}
}

func TestEngineCharset_MirrorsDictionary(t *testing.T) {
	e := &Engine{keys: []rune{'a', 'b', 'c'}, trailingSpace: true}
	cs := e.Charset()
	if got := cs.NumClasses(); got != 5 {
		t.Errorf("NumClasses: got %d, want 5", got)
	}
	if got, ok := cs.Class('c'); !ok || got != 3 {
		t.Errorf("Class('c') = (%d,%v), want (3,true)", got, ok)
	}
	if got, ok := cs.Class(' '); !ok || got != 4 {
		t.Errorf("Class(' ') = (%d,%v), want (4,true)", got, ok)
	}
}

// --- ReadLattices plumbing (no ONNX runtime) --------------------------------

func TestReadLattices_ClosedEngineReturnsCleanError(t *testing.T) {
	e := &Engine{} // closed / never-opened: session == nil
	_, err := e.ReadLattices(context.Background(), oneLineCrop(t))
	if err == nil {
		t.Fatal("want an error from a closed engine, got nil")
	}
	if !errors.Is(err, errEngineClosed) {
		t.Fatalf("want errEngineClosed, got: %v", err)
	}
}

func TestReadLattices_EmptyCropRejected(t *testing.T) {
	e := &Engine{}
	_, err := e.ReadLattices(context.Background(), imaging.IDCrop{})
	if err == nil {
		t.Fatal("want an error for a zero IDCrop, got nil")
	}
	if !strings.Contains(err.Error(), "localocr:") {
		t.Errorf("error should carry the localocr: prefix; got %v", err)
	}
}

func TestBestLatticeConfidence(t *testing.T) {
	if got := bestLatticeConfidence(nil); got != 0 {
		t.Fatalf("empty: got %v", got)
	}
	got := bestLatticeConfidence([]LineLattice{
		{Text: "a", Confidence: 0.3},
		{Text: "b", Confidence: 0.8},
		{Text: "c", Confidence: 0.5},
	})
	if got != 0.8 {
		t.Fatalf("want max 0.8, got %v", got)
	}
}

// TestNewCharset_ExportedMirrorsUnexported pins the production constructor: it
// must build exactly what Engine.Charset builds, without an ONNX session in the
// way, so an offline matcher (or a fixture-driven tool) can score against a
// dictionary it loaded itself.
func TestNewCharset_ExportedMirrorsUnexported(t *testing.T) {
	keys := []rune{'a', 'b', 'c'}
	for _, trailingSpace := range []bool{false, true} {
		want := newCharset(keys, trailingSpace)
		got := NewCharset(keys, trailingSpace)
		if got.NumClasses() != want.NumClasses() {
			t.Errorf("trailingSpace=%v: NumClasses = %d, want %d", trailingSpace, got.NumClasses(), want.NumClasses())
		}
		for _, r := range []rune{'a', 'b', 'c', ' ', 'z'} {
			gotCls, gotOK := got.Class(r)
			wantCls, wantOK := want.Class(r)
			if gotCls != wantCls || gotOK != wantOK {
				t.Errorf("trailingSpace=%v: Class(%q) = (%d,%v), want (%d,%v)", trailingSpace, r, gotCls, gotOK, wantCls, wantOK)
			}
		}
	}

	// The exported constructor is enough on its own to score a target: no
	// Engine, no model, no dictionary file.
	cs := NewCharset(keys, false)
	l := NewLattice([][]float32{{0.1, 0.7, 0.1, 0.1}}, 0)
	if _, ok := ScoreTarget(l, cs, "a", ScoreOptions{}); !ok {
		t.Error("ScoreTarget against a NewCharset-built charset should be scorable")
	}
}

// TestNewCharset_ClonesTheKeys — keys and index are built together, so a
// Charset that ALIASED the caller's slice could be desynced from the outside:
// the map still says 'a' is class 1 while keys[0] has become something else,
// and every consumer that reads the dictionary sees a different alphabet than
// the one Class answers for.
func TestNewCharset_ClonesTheKeys(t *testing.T) {
	keys := []rune{'a', 'b', 'c'}
	cs := NewCharset(keys, false)

	keys[0] = 'z'

	if cs.keys[0] != 'a' {
		t.Errorf("Charset aliases the caller's slice: keys[0] = %q, want 'a'", cs.keys[0])
	}
}
