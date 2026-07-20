package grading

import (
	"math/rand"
	"sort"
)

// SampleAnswer is the calibration sampler's pool item: one gradeable answer and
// the problem it belongs to.
type SampleAnswer struct {
	AnswerID  int64
	ProblemID int64
}

// SelectCalibrationSample picks a deterministic, problem-stratified sample of n
// answers for a calibration ("sample"-scope) run — the guide's 校準批 without
// hand-launching one run per answer. It mirrors SelectSpotCheckSample's
// algorithm (trust spec §4) with the answer id as the stable key; the spot-check
// sampler is deliberately left untouched rather than factored, so this addition
// cannot perturb the canonical-sample determinism D37 depends on.
//
// Determinism: the PRNG is seeded from runID alone and every draw operates on a
// stable ordering, so the same runID and pool (in any order) always produce the
// same sample. Stratification: round-robin across problems, fewest-taken-first
// with ties broken by problem id, so a big problem never exhausts the budget
// before a small one contributes.
func SelectCalibrationSample(runID int64, pool []SampleAnswer, n int) []SampleAnswer {
	if n <= 0 || len(pool) == 0 {
		return nil
	}
	if n >= len(pool) {
		out := make([]SampleAnswer, len(pool))
		copy(out, pool)
		sortSampleAnswers(out)
		return out
	}

	sorted := make([]SampleAnswer, len(pool))
	copy(sorted, pool)
	sortSampleAnswers(sorted)

	byProblem := map[int64][]SampleAnswer{}
	var problemIDs []int64
	for _, a := range sorted {
		if _, ok := byProblem[a.ProblemID]; !ok {
			problemIDs = append(problemIDs, a.ProblemID)
		}
		byProblem[a.ProblemID] = append(byProblem[a.ProblemID], a)
	}
	sort.Slice(problemIDs, func(i, j int) bool { return problemIDs[i] < problemIDs[j] })

	rng := rand.New(rand.NewSource(runID))
	for _, pid := range problemIDs {
		group := byProblem[pid]
		rng.Shuffle(len(group), func(i, j int) { group[i], group[j] = group[j], group[i] })
		byProblem[pid] = group
	}

	taken := make(map[int64]int, len(problemIDs))
	selected := make([]SampleAnswer, 0, n)
	for len(selected) < n {
		bestPID := int64(-1)
		bestTaken := -1
		for _, pid := range problemIDs {
			if len(byProblem[pid]) == 0 {
				continue
			}
			if t := taken[pid]; bestTaken == -1 || t < bestTaken {
				bestTaken = t
				bestPID = pid
			}
		}
		if bestPID == -1 {
			break
		}
		group := byProblem[bestPID]
		selected = append(selected, group[0])
		byProblem[bestPID] = group[1:]
		taken[bestPID]++
	}

	sortSampleAnswers(selected)
	return selected
}

func sortSampleAnswers(s []SampleAnswer) {
	sort.Slice(s, func(i, j int) bool { return s[i].AnswerID < s[j].AnswerID })
}
