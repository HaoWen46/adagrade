package localocr

import (
	"cmp"
	"fmt"
	"math"
	"slices"
)

// This file exposes what greedy CTC decoding throws away. ReadLines answers
// "what does this line say"; a lexicon scorer needs "how well does THIS
// candidate string explain this line", which requires the per-timestep class
// distribution, not just its argmax. Keeping the full [T][C] matrix is
// wasteful (C is ~6.6k classes for the PP-OCR dictionary and every candidate
// only ever asks about a handful of them), so the distribution is compressed
// to the top K classes per timestep plus one honest floor for everything else.

const (
	// latticeTopK is the compression width used by the Engine. 64 classes per
	// timestep comfortably covers the argmax plus its realistic confusions
	// (0/O, 1/l/I, visually close CJK glyphs) while cutting the stored matrix
	// by ~100x; anything a candidate asks for beyond that scores at the
	// residual floor, which is the correct answer for a class the model gave
	// essentially no mass.
	latticeTopK = 64

	// minResidualMass floors the discarded probability mass. When the kept K
	// classes already carry all of it (a saturated timestep, or float noise
	// pushing the sum just past 1) the true residual is 0 and log(0) is -Inf,
	// which would annihilate a whole candidate's score on a single timestep.
	// A tiny positive mass keeps the penalty severe but finite.
	minResidualMass = 1e-12
)

// logProbFloor is the value substituted for any log-probability that comes out
// non-finite (a zero probability in softmaxed input, or NaN/Inf from a broken
// model). Scores are summed over a whole candidate string, so a single -Inf or
// NaN would silently destroy the comparison between candidates.
var logProbFloor = math.Log(minResidualMass)

// Charset is the model's CTC class dictionary, viewed rune-first: it answers
// "which output class would emit this character", the direction a lexicon
// scorer needs (ctc.go's classToRune answers the decoder's direction).
//
// Class 0 is the CTC blank and is never returned. Dictionary entry i is class
// i+1, and a model exported with the extra trailing space class has ' ' at
// class len(keys)+1 (docs/DECISIONS.md D24).
type Charset struct {
	keys          []rune
	index         map[rune]int
	trailingSpace bool
}

// Charset returns the engine's dictionary view. It builds a fresh index on
// every call (one map over the ~6.6k-entry PP-OCR dictionary), so callers
// should hoist it out of loops rather than calling it per candidate.
func (e *Engine) Charset() Charset { return newCharset(e.keys, e.trailingSpace) }

// newCharset builds the rune -> class index. Duplicate dictionary entries keep
// their FIRST class (the decoder would emit either, and the lower class is the
// one a well-formed PP-OCR dictionary intends). The trailing space class is
// registered only when ' ' is not already a dictionary entry, so an explicit
// dictionary space always wins.
func newCharset(keys []rune, trailingSpace bool) Charset {
	index := make(map[rune]int, len(keys)+1)
	for i, r := range keys {
		if _, dup := index[r]; !dup {
			index[r] = i + 1
		}
	}
	if trailingSpace {
		if _, dup := index[' ']; !dup {
			index[' '] = len(keys) + 1
		}
	}
	return Charset{keys: keys, index: index, trailingSpace: trailingSpace}
}

// Class returns the CTC class that emits r, or false when r is outside the
// dictionary. The returned class is never 0 (blank).
func (c Charset) Class(r rune) (int, bool) {
	cls, ok := c.index[r]
	return cls, ok
}

// NumClasses is the model's output width: the dictionary plus the blank, plus
// the trailing space class when the export has one.
func (c Charset) NumClasses() int {
	if c.trailingSpace {
		return len(c.keys) + 2
	}
	return len(c.keys) + 1
}

// Lattice is one line's per-timestep class log-probabilities, compressed to the
// top K classes per timestep.
//
// Sparse form (K > 0): Classes and LogP are T*K row-major — timestep t owns
// [t*K, (t+1)*K) — holding the K highest-probability classes and their
// log-probs, ordered by descending probability. LogRest[t] is what any other
// class scores at that timestep.
//
// Dense form (K == 0): LogP is T*C, indexed directly by class, and Classes and
// LogRest are nil. Dense exists so tests can assert against every class without
// a selection step in the way; production always uses the sparse form.
type Lattice struct {
	T, K    int       // timesteps; kept classes per timestep (K==0 => dense over all classes)
	Classes []int32   // len T*K (top-K class indices per timestep); nil when dense
	LogP    []float32 // len T*K (log-probs of those classes); when dense: len T*C, class-indexed
	LogRest []float32 // len T; log-prob floor for any class not among the kept K; when dense: unused (may be nil)
}

// LogProbAt returns log P(class at timestep t). Classes outside the kept K
// score LogRest[t] — the discarded mass spread evenly over the classes that
// did not make the cut, which is a finite, deliberately pessimistic estimate
// rather than a claim of impossibility.
//
// A timestep outside [0, T) or a negative class is a programmer error and
// panics: those cannot come from a scorer walking a real candidate, and
// returning a plausible number instead would hide the bug in a score.
func (l Lattice) LogProbAt(t, class int) float64 {
	if t < 0 || t >= l.T || class < 0 {
		panic(fmt.Sprintf("localocr: Lattice.LogProbAt(t=%d, class=%d) out of range (T=%d)", t, class, l.T))
	}
	if l.K == 0 {
		c := len(l.LogP) / l.T
		if class >= c {
			panic(fmt.Sprintf("localocr: Lattice.LogProbAt(t=%d, class=%d) out of range (dense C=%d)", t, class, c))
		}
		return float64(l.LogP[t*c+class])
	}
	// K is small (64) and lookups are for one candidate character at a time,
	// so a linear scan beats keeping a per-timestep map.
	base := t * l.K
	for i := base; i < base+l.K; i++ {
		if int(l.Classes[i]) == class {
			return float64(l.LogP[i])
		}
	}
	return float64(l.LogRest[t])
}

// NewLattice compresses a [T][C] model output matrix into a Lattice keeping the
// topK highest-probability classes per timestep. topK <= 0 or topK >= C
// produces the dense form.
//
// rows may be EITHER already-softmaxed probabilities (PP-OCRv4 exports end in a
// Softmax node) or raw logits (PP-OCRv5 does not); the same looksSoftmaxed test
// the decoder uses on rows[0] decides for the whole matrix. In the logits case
// the log-probs come from a direct log-softmax (x - max - log(Σexp(x-max))),
// NOT from softmax followed by log: the latter rounds small probabilities to
// float32 zero and hands back -Inf exactly where the scorer needs a usable
// penalty.
//
// rows is expected to be rectangular ([T][C], as outputToRows produces). A
// short row is tolerated defensively: its missing classes score the floor.
func NewLattice(rows [][]float32, topK int) Lattice {
	T := len(rows)
	if T == 0 {
		return Lattice{}
	}
	C := len(rows[0])
	softmaxed := looksSoftmaxed(rows[0])
	if topK <= 0 || topK >= C {
		return denseLattice(rows, C, softmaxed)
	}
	return sparseLattice(rows, C, topK, softmaxed)
}

// denseLattice keeps every class's log-prob, class-indexed.
func denseLattice(rows [][]float32, C int, softmaxed bool) Lattice {
	logP := make([]float32, len(rows)*C)
	for t, row := range rows {
		off := t * C
		n := min(len(row), C)
		maxV, logSum := 0.0, 0.0
		if !softmaxed {
			maxV, logSum = logSumExpRow(row, n)
		}
		for j := 0; j < n; j++ {
			logP[off+j] = float32(classLogProb(row[j], softmaxed, maxV, logSum))
		}
		for j := n; j < C; j++ {
			logP[off+j] = float32(logProbFloor)
		}
	}
	return Lattice{T: len(rows), K: 0, LogP: logP}
}

// sparseLattice keeps the K highest-probability classes per timestep plus the
// residual floor for the rest.
func sparseLattice(rows [][]float32, C, K int, softmaxed bool) Lattice {
	T := len(rows)
	out := Lattice{
		T:       T,
		K:       K,
		Classes: make([]int32, T*K),
		LogP:    make([]float32, T*K),
		LogRest: make([]float32, T),
	}
	sel := newTopKSelector(K)
	for t, row := range rows {
		n := min(len(row), C)
		maxV, logSum := 0.0, 0.0
		if !softmaxed {
			maxV, logSum = logSumExpRow(row, n)
		}
		// Selection ranks on the raw value: softmax is monotonic, so the
		// top-K logits are the top-K probabilities and no exponentials are
		// needed until the winners are known.
		sel.reset()
		for j := 0; j < n; j++ {
			sel.push(float64(row[j]), int32(j))
		}
		kept := sel.sorted()

		off := t * K
		var keptMass float64
		for i, k := range kept {
			lp := classLogProb(row[k.class], softmaxed, maxV, logSum)
			out.Classes[off+i] = k.class
			out.LogP[off+i] = float32(lp)
			keptMass += math.Exp(lp)
		}
		// Short row: pad the unused slots with a class no lookup can match.
		for i := len(kept); i < K; i++ {
			out.Classes[off+i] = -1
			out.LogP[off+i] = float32(logProbFloor)
		}

		rest := 1 - keptMass
		if !(rest > minResidualMass) { // negated so NaN clamps too
			rest = minResidualMass
		}
		out.LogRest[t] = float32(sanitizeLogProb(math.Log(rest / float64(C-K))))
	}
	return out
}

// classLogProb turns one raw row value into a log-probability: log(v) for
// already-softmaxed input, or the log-softmax term v-max-logSum for logits.
func classLogProb(v float32, softmaxed bool, maxV, logSum float64) float64 {
	if softmaxed {
		return sanitizeLogProb(math.Log(float64(v)))
	}
	return sanitizeLogProb(float64(v) - maxV - logSum)
}

// logSumExpRow returns the two terms of a numerically-stable log-softmax over
// the first n values of row: the row max and log(Σ exp(v - max)).
func logSumExpRow(row []float32, n int) (maxV, logSum float64) {
	maxV = math.Inf(-1)
	for j := 0; j < n; j++ {
		if v := float64(row[j]); v > maxV { // NaN compares false and is skipped
			maxV = v
		}
	}
	if math.IsInf(maxV, -1) {
		maxV = 0 // no usable maximum (empty or all-NaN row); sanitize handles the rest
	}
	var sum float64
	for j := 0; j < n; j++ {
		sum += math.Exp(float64(row[j]) - maxV)
	}
	return maxV, math.Log(sum)
}

// sanitizeLogProb keeps a log-probability finite and <= 0. NaN and -Inf (a
// zero-probability class, or a model emitting garbage) collapse to the floor;
// a positive value (probability > 1, only reachable from malformed input)
// clamps to log(1).
func sanitizeLogProb(x float64) float64 {
	switch {
	case math.IsNaN(x) || math.IsInf(x, -1):
		return logProbFloor
	case x > 0:
		return 0
	default:
		return x
	}
}

// keptClass is one survivor of the top-K selection.
type keptClass struct {
	score float64
	class int32
}

// topKSelector keeps the K highest-scoring classes of one timestep in a bounded
// min-heap: O(C log K) per timestep. A full sort would be O(C log C) with
// C ~= 6.6k classes at every one of the ~80 timesteps of every line, for a
// result that discards all but 64 of them.
type topKSelector struct {
	k    int
	heap []keptClass // min-heap on score; heap[0] is the weakest survivor
}

func newTopKSelector(k int) *topKSelector {
	return &topKSelector{k: k, heap: make([]keptClass, 0, k)}
}

func (s *topKSelector) reset() { s.heap = s.heap[:0] }

func (s *topKSelector) push(score float64, class int32) {
	if len(s.heap) < s.k {
		s.heap = append(s.heap, keptClass{score, class})
		s.up(len(s.heap) - 1)
		return
	}
	if !(score > s.heap[0].score) { // ties keep the incumbent; NaN never displaces
		return
	}
	s.heap[0] = keptClass{score, class}
	s.down(0)
}

// sorted returns the survivors ordered by descending score (ties by ascending
// class) so a lattice is a deterministic function of its input. It sorts in
// place, destroying the heap invariant: reset before pushing again.
func (s *topKSelector) sorted() []keptClass {
	slices.SortFunc(s.heap, func(a, b keptClass) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(a.class, b.class)
	})
	return s.heap
}

func (s *topKSelector) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if s.heap[parent].score <= s.heap[i].score {
			return
		}
		s.heap[parent], s.heap[i] = s.heap[i], s.heap[parent]
		i = parent
	}
}

func (s *topKSelector) down(i int) {
	for {
		l, r := 2*i+1, 2*i+2
		small := i
		if l < len(s.heap) && s.heap[l].score < s.heap[small].score {
			small = l
		}
		if r < len(s.heap) && s.heap[r].score < s.heap[small].score {
			small = r
		}
		if small == i {
			return
		}
		s.heap[small], s.heap[i] = s.heap[i], s.heap[small]
		i = small
	}
}

// LineLattice is one recognized line: the greedy decode ReadLines would return,
// plus the lattice the same inference pass produced. Both describe the SAME
// winning retry variant, so a scorer cannot disagree with the reported text
// about which image it is looking at.
type LineLattice struct {
	Lattice    Lattice
	Text       string
	Confidence float64
}

// bestLatticeConfidence is the maximum line confidence, 0 when nothing was
// read — the retry ladder's comparison rule for the lattice path (the mirror
// of bestLineConfidence).
func bestLatticeConfidence(lines []LineLattice) float64 {
	best := 0.0
	for _, l := range lines {
		if l.Confidence > best {
			best = l.Confidence
		}
	}
	return best
}
