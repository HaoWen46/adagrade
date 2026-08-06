package localocr

import "math"

// This file answers the closed-set question greedy decoding cannot: "how well
// does THIS candidate explain this line". ReadLines commits to one decode, so
// matching a page to a roster becomes a string comparison against a decode that
// ugly handwriting may have gotten wrong in exactly the character that
// separates two students. Scoring each candidate directly against the
// per-timestep distribution (the Lattice from lattice.go) never commits: the
// candidate whose alignments best explain the observed frames wins, even when
// no single greedy decode would have produced it.
//
// The scorer is the standard CTC forward algorithm — the same dynamic program
// CTC training uses for P(labels | frames), summed over EVERY alignment of the
// labels to the frames — run in log space over one candidate at a time.

const (
	// blankClass is the CTC blank. PP-OCR puts it at class 0 (ctc.go,
	// docs/DECISIONS.md D24) and Charset.Class never returns it.
	blankClass = 0

	// wildState marks an extended-state slot that matches ANY class at any
	// timestep — the flanks of a substring match. It is a STATE marker, never
	// a class index: Charset.Class only ever returns >= 1, so a negative value
	// cannot collide with a real class, and it never reaches
	// Lattice.LogProbAt, which rejects negative classes.
	wildState = -1
)

// ScoreOptions controls how a target is matched against a lattice.
type ScoreOptions struct {
	// AllowSurroundingText scores the target as a SUBSTRING of the line:
	// arbitrary other text may precede and follow it. Off, the target must
	// account for the entire line. A page line usually reads
	// "座號 05 姓名 王小明" while the roster candidate is just the name, so the
	// matcher wants this on; an exact-field crop does not.
	AllowSurroundingText bool
}

// ScoreResult is the CTC forward log-likelihood of a target string.
type ScoreResult struct {
	LogLik  float64 // total forward log-likelihood over all alignments (log space)
	RuneLen int     // rune length of the target as scored
	Norm    float64 // LogLik / max(1, RuneLen) — per-character normalization
	Prob    float64 // exp(Norm) — geometric-mean per-character probability
}

// ScoreTarget scores target against the lattice, summing over every alignment
// of target to the lattice's frames.
//
// ok=false when the target is empty, when it contains a rune the charset cannot
// represent, or when the lattice has no frames. None of those is an error: a
// candidate the model's dictionary cannot even spell simply cannot be this
// line, and a roster full of them is one configuration fact for the caller to
// notice, not an exception per candidate.
//
// A target that IS spellable but cannot fit — "aa" needs a separating frame for
// its blank, so it needs at least three — scores LogLik = -Inf, Prob = 0 with
// ok=true. That is a real score meaning "impossible", and the matcher ranks on
// it like any other.
//
// Norm and Prob are per-character quality: they divide out the fact that a
// long target accumulates more log-probability than a short one, which is what
// makes scores from DIFFERENT lines comparable — a fixed "is this match good
// enough" threshold has to mean the same thing on a 3-glyph line and a 12-glyph
// one. Prob is not a calibrated probability even so, because more frames admit
// more alignments and every extra alignment only adds mass.
//
// For ranking candidates against ONE line the two are equivalent whenever the
// candidates have the same rune length, and differ when they do not:
//
//   - Without AllowSurroundingText, every candidate's score is a sum over the
//     same T frames, so either ordering is reasonable.
//   - With AllowSurroundingText, rank on LogLik. The wildcard flanks contribute
//     a bonus that scales with the LINE, not with the target, so dividing it by
//     RuneLen systematically favours short candidates. On a 24-frame line
//     reading "ab", Prob("a") = 14.2 beats Prob("ab") = 5.6 while
//     LogLik("ab") = 3.45 correctly beats LogLik("a") = 2.66.
//
// Those numbers also show Prob leaving (0,1] under AllowSurroundingText: a
// wildcard stands for "any class", so two wildcard alignments can cover the
// same class sequence and their probabilities double-count. A plain match has
// no such overlap — its alignments are disjoint events, so its Prob is in
// (0,1]. Nothing is clamped: a clamp would hide a broken forward pass, and
// ranking needs the ordering, not the range.
//
// ScoreTarget reads l and cs and allocates everything else per call, so a
// roster's worth of candidates may be scored against one lattice concurrently.
func ScoreTarget(l Lattice, cs Charset, target string, opt ScoreOptions) (ScoreResult, bool) {
	runes := []rune(target)
	if len(runes) == 0 || l.T == 0 {
		return ScoreResult{}, false
	}
	classes := make([]int, len(runes))
	for i, r := range runes {
		cls, ok := cs.Class(r)
		if !ok {
			return ScoreResult{}, false
		}
		classes[i] = cls
	}

	logLik := ctcForward(l, extendedStates(classes, opt.AllowSurroundingText))
	res := ScoreResult{LogLik: logLik, RuneLen: len(runes)}
	// max is defensive only — an empty target was rejected above — but it keeps
	// the division safe if that guard is ever loosened.
	res.Norm = logLik / float64(max(1, res.RuneLen))
	// -Inf / n is -Inf and exp(-Inf) is exactly 0, so an impossible target
	// reports Prob = 0 rather than NaN.
	res.Prob = math.Exp(res.Norm)
	return res, true
}

// extendedStates builds the blank-extended state sequence the forward pass
// walks: [_, c1, _, c2, ..., cL, _], which is how CTC represents "a blank may
// appear before, between and after any characters". Its length is 2L+1.
//
// wildFlanks wraps that in a wildcard state at each end — [W, _, c1, ..., _, W]
// — turning a whole-line match into a substring match: the wildcards absorb
// whatever text surrounds the target at no cost.
func extendedStates(classes []int, wildFlanks bool) []int {
	size := 2*len(classes) + 1
	off := 0
	if wildFlanks {
		size += 2
		off = 1
	}
	ext := make([]int, size)
	if wildFlanks {
		ext[0] = wildState
		ext[size-1] = wildState
	}
	for i, cls := range classes {
		ext[off+2*i] = blankClass
		ext[off+2*i+1] = cls
	}
	ext[off+2*len(classes)] = blankClass
	return ext
}

// ctcForward is the forward recursion over the extended state sequence:
// alpha[t][s] is the total probability of every alignment prefix that has the
// lattice's first t+1 frames sitting in states 0..s and ends in state s. Two
// rolling rows give O(len(ext)) space and O(T*len(ext)) time — a candidate is
// scored against a whole line for the cost of a couple of thousand adds.
//
// len(ext) >= 3 always (the caller rejects an empty target, so there is at
// least one character and its two surrounding blanks), which is why the start
// and end state windows below need no bounds guard.
func ctcForward(l Lattice, ext []int) float64 {
	S := len(ext)
	prev := make([]float64, S)
	cur := make([]float64, S)
	for i := range prev {
		prev[i] = math.Inf(-1)
	}

	// An alignment may start in the leading blank or on the first character
	// (the leading blank is optional). With a leading wildcard it may also
	// start on the wildcard itself, so all three of W, blank and c1 are legal
	// — which is exactly what lets the flanks be skipped entirely when the
	// target IS the whole line.
	starts := 2
	if ext[0] == wildState {
		starts = 3
	}
	for s := 0; s < starts; s++ {
		prev[s] = stateLogProb(l, 0, ext[s])
	}

	for t := 1; t < l.T; t++ {
		for s := 0; s < S; s++ {
			// Stay in this state, or advance from the one before it.
			a := prev[s]
			if s > 0 {
				a = logAdd(a, prev[s-1])
			}
			if s > 1 && canSkip(ext, s) {
				a = logAdd(a, prev[s-2])
			}
			// Emissions are always finite (Lattice sanitizes its log-probs and
			// a wildcard emits 0), so an unreachable state stays -Inf here
			// instead of turning into a NaN.
			cur[s] = a + stateLogProb(l, t, ext[s])
		}
		prev, cur = cur, prev
	}

	// An alignment may end on the last character or in the trailing blank; with
	// a trailing wildcard, on the wildcard too.
	ends := 2
	if ext[S-1] == wildState {
		ends = 3
	}
	total := math.Inf(-1)
	for i := 0; i < ends; i++ {
		total = logAdd(total, prev[S-1-i])
	}
	return total
}

// canSkip reports whether an alignment may jump from state s-2 straight to
// state s, skipping the blank between them.
//
// This is the rule that makes CTC CTC. Skipping is only ever about reaching a
// SYMBOL without spending a frame on the optional blank before it, so it is
// never allowed into a blank state. And it is forbidden between two IDENTICAL
// symbols: without the blank, "aa" and "a" would have the same alignments, and
// the doubled character would be unrecoverable. Roster candidates like 林林 and
// "Lee" depend on this — allowing the skip would let one long 'a' spike score
// as "aa" and mis-rank every such name.
//
// A wildcard endpoint blocks the skip as well. The identity test above is
// undecidable there (a wildcard stands for any class, so it may or may not be
// the same symbol as its neighbour), and the conservative answer keeps the
// score a lower bound: entering the target from the leading wildcard, or
// leaving it into the trailing one, goes through the blank state between them
// and pays that frame's blank log-prob. On a real line that is cheap — a CTC
// head separates character spikes with blank-dominated frames — and it never
// over-credits a candidate whose neighbouring text happens to end in the
// candidate's own first character.
func canSkip(ext []int, s int) bool {
	if ext[s] == blankClass || ext[s] == wildState || ext[s-2] == wildState {
		return false
	}
	return ext[s] != ext[s-2]
}

// stateLogProb is one extended state's emission log-probability at frame t. A
// wildcard scores exactly 0: lattice rows are log-softmax normalized, so
// logsumexp over all classes is 0, which is precisely "any class here".
func stateLogProb(l Lattice, t, state int) float64 {
	if state == wildState {
		return 0
	}
	return l.LogProbAt(t, state)
}

// logAdd returns log(exp(a) + exp(b)) without leaving log space. A whole line's
// alignments span many orders of magnitude, so the naive exp-sum-log would
// underflow the small terms to zero and overflow the large ones; factoring out
// the larger operand keeps every intermediate in range.
//
// -Inf is the identity, and -Inf + -Inf is -Inf — impossible states are the
// normal case in a CTC trellis, and the arithmetic must carry them rather than
// produce NaN from an Inf-minus-Inf.
func logAdd(a, b float64) float64 {
	if math.IsInf(a, -1) {
		return b
	}
	if math.IsInf(b, -1) {
		return a
	}
	if a < b {
		a, b = b, a
	}
	// b-a <= 0, so Exp cannot overflow; Log1p keeps precision when it underflows.
	return a + math.Log1p(math.Exp(b-a))
}
