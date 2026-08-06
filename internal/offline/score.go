package offline

import (
	"math"

	"github.com/HaoWen46/adagrade/internal/localocr"
)

// CandidateScore is one candidate's standing on one field.
//
// LogLik is the raw CTC forward log-likelihood — the best any of the
// candidate's spellings achieved against any line of the field's crop.
// Posterior is that log-likelihood softmaxed against the OTHER candidates,
// which is what makes it comparable across fields: a log-likelihood is on an
// arbitrary per-line scale, while "how much of this field's probability mass
// does this candidate hold" means the same thing on the ID crop and the name
// crop, and can therefore be combined with weights.
type CandidateScore struct {
	LogLik    float64
	Posterior float64
}

// FieldResult is one field's verdict over the whole candidate list.
//
// Read is the honest "did this field produce evidence at all" bit. False means
// the crop had no lines, or nothing in the candidate list could be scored
// against them — NOT that every candidate scored badly. match.go gives an
// unread field a contribution of ZERO for every candidate rather than
// redistributing its weight, which is the difference between "the ID is
// unreadable, so this page is weak" and "the ID is unreadable, so trust the
// name completely".
type FieldResult struct {
	Scores []CandidateScore // parallel to the candidate list passed in
	Read   bool
}

// ScoreField scores every candidate against one field's lattices.
//
// targets[i] is candidate i's list of acceptable spellings — a roster name has
// a case-folded and a case-preserving form, a problem number is written "Q3" or
// "3" — and the candidate's score is the MAX over its spellings crossed with
// every line the crop produced. Max, not sum: the spellings are alternative
// renderings of the same identity, not independent hypotheses, and the lines
// are alternative places it may have been written.
//
// Two decisions here are load-bearing and come from the scorer's measured
// behaviour (see localocr.ScoreTarget's doc comment):
//
//   - AllowSurroundingText is ON. A crop of a real page reads
//     "座號 05 姓名 王小明" while the candidate is just the name, so demanding the
//     candidate account for the whole line would score every true match as a
//     mismatch.
//
//   - Ranking is on LogLik, never on Prob or Norm. With surrounding text
//     allowed, the wildcard flanks contribute a bonus that scales with the LINE
//     rather than with the candidate, so dividing by the candidate's length
//     systematically favours SHORT candidates — "a" beating "ab" on a line that
//     plainly reads "ab". LogLik carries that bonus without dividing it out, and
//     the softmax below is monotone in it.
//
// A candidate no spelling of which could be scored (an ID the model's
// dictionary cannot spell, an empty roster name) keeps LogLik -Inf and
// posterior 0. That is deliberately not an error: a roster full of unspellable
// IDs is one configuration fact for the operator to notice, not one exception
// per student.
func ScoreField(f FieldLattices, cs localocr.Charset, targets [][]string) FieldResult {
	res := FieldResult{Scores: make([]CandidateScore, len(targets))}
	for i := range res.Scores {
		res.Scores[i].LogLik = math.Inf(-1)
	}
	if len(targets) == 0 || len(f.Lines) == 0 {
		return res
	}

	opt := localocr.ScoreOptions{AllowSurroundingText: true}
	for i, variants := range targets {
		best := math.Inf(-1)
		for _, variant := range variants {
			for _, line := range f.Lines {
				sr, ok := localocr.ScoreTarget(line.Lattice, cs, variant, opt)
				if !ok {
					// Unspellable, empty, or a line with no frames: this
					// pairing has no opinion, so it does not get to lower the
					// candidate's score either.
					continue
				}
				if sr.LogLik > best {
					best = sr.LogLik
				}
			}
		}
		res.Scores[i].LogLik = best
	}

	res.Read = softmaxPosteriors(res.Scores)
	return res
}

// softmaxPosteriors fills Posterior from LogLik with a max-shifted softmax and
// reports whether ANY candidate was scorable.
//
// The shift by the maximum is what keeps the exponentials in range: a field's
// log-likelihoods run to several hundred negative nats on a long line, and a
// naive exp would underflow every one of them to zero and produce 0/0.
// Candidates at -Inf (unspellable, or impossible to align) contribute exactly
// zero mass and come out at posterior 0. When EVERY candidate is at -Inf there
// is no mass to divide, and the field reports as unread rather than as a flat
// distribution — an unreadable field must not look like an undecided one.
func softmaxPosteriors(scores []CandidateScore) bool {
	maxLL := math.Inf(-1)
	for _, s := range scores {
		if s.LogLik > maxLL {
			maxLL = s.LogLik
		}
	}
	if math.IsInf(maxLL, -1) {
		return false
	}
	var denom float64
	for _, s := range scores {
		denom += math.Exp(s.LogLik - maxLL)
	}
	for i := range scores {
		scores[i].Posterior = math.Exp(scores[i].LogLik-maxLL) / denom
	}
	return true
}
