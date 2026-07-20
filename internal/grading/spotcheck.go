package grading

import (
	"math/rand"
	"sort"
)

// SpotCheckLeaf is the minimal shape the sampler needs: a graded record and a
// stratification key for the problem it belongs to.
//
// ProblemID is a MISNOMER kept for source stability: it is a stratification key, NOT
// the problems.id FK. The only caller (the runner's completion hook) fills it with the
// problem NUMBER, which is unique within an assessment and cheaper to fetch than the FK.
// The sampler only requires that the value be stable and unique per problem within the
// run's scope — it never dereferences it as an id.
type SpotCheckLeaf struct {
	RecordID  int64
	ProblemID int64 // stratification key = problem NUMBER (see doc above), not problems.id
}

// spotCheckFloor and spotCheckCap bound the sample size (trust spec §4): never
// fewer than 5 records (unless the pool itself is smaller), never more than 20.
const (
	spotCheckFloor = 5
	spotCheckCap   = 20
)

// SpotCheckSampleSize implements the trust-spec §4 formula:
// min(max(5, 5% of graded leaves), 20), capped again at the pool size itself
// (a run with fewer than 5 graded leaves samples all of them — there's nothing
// else to check).
func SpotCheckSampleSize(nLeaves int) int {
	if nLeaves <= 0 {
		return 0
	}
	fivePct := nLeaves * 5 / 100
	size := max(spotCheckFloor, fivePct)
	size = min(size, spotCheckCap)
	return min(size, nLeaves)
}

// SelectSpotCheckSample picks a deterministic, problem-stratified sample of a
// run's graded leaves (trust spec §4). Determinism: the same runID and the same
// input pool (in any order — the pool is sorted internally before sampling)
// always produce the same sample, because the PRNG is seeded from runID alone
// and every random draw operates on a stable, sorted ordering.
//
// Stratification: leaves are grouped by problem, then filled round-robin across
// problems (smallest remaining group first, so problems with few leaves aren't
// starved by a big one) until the target size is reached or the pool is
// exhausted. Within each problem, candidates are shuffled with the seeded PRNG
// before being drawn, so which specific records are picked is still randomized,
// not just "the first N per problem".
func SelectSpotCheckSample(runID int64, pool []SpotCheckLeaf) []SpotCheckLeaf {
	target := SpotCheckSampleSize(len(pool))
	if target == 0 {
		return nil
	}
	if target >= len(pool) {
		out := make([]SpotCheckLeaf, len(pool))
		copy(out, pool)
		sortLeaves(out)
		return out
	}

	// Stable base ordering so the same pool (regardless of caller's slice order)
	// always seeds the same shuffle.
	sorted := make([]SpotCheckLeaf, len(pool))
	copy(sorted, pool)
	sortLeaves(sorted)

	// Group by problem, preserving the sorted (by record id) order within each.
	byProblem := map[int64][]SpotCheckLeaf{}
	var problemIDs []int64
	for _, l := range sorted {
		if _, ok := byProblem[l.ProblemID]; !ok {
			problemIDs = append(problemIDs, l.ProblemID)
		}
		byProblem[l.ProblemID] = append(byProblem[l.ProblemID], l)
	}
	sort.Slice(problemIDs, func(i, j int) bool { return problemIDs[i] < problemIDs[j] })

	rng := rand.New(rand.NewSource(runID))
	// Shuffle within each problem group so the draw order isn't just ascending
	// record id.
	for _, pid := range problemIDs {
		group := byProblem[pid]
		rng.Shuffle(len(group), func(i, j int) { group[i], group[j] = group[j], group[i] })
		byProblem[pid] = group
	}

	// Round-robin: repeatedly take one from the non-empty group with the FEWEST
	// records TAKEN so far (ties broken by problem id) so every problem gets an
	// even share before any problem gets a second pick — a big problem never
	// exhausts the whole budget before a small problem contributes at least one
	// record.
	taken := make(map[int64]int, len(problemIDs))
	selected := make([]SpotCheckLeaf, 0, target)
	for len(selected) < target {
		bestPID := int64(-1)
		bestTaken := -1
		for _, pid := range problemIDs {
			if len(byProblem[pid]) == 0 {
				continue
			}
			t := taken[pid]
			if bestTaken == -1 || t < bestTaken {
				bestTaken = t
				bestPID = pid
			}
		}
		if bestPID == -1 {
			break // pool exhausted (shouldn't happen: target <= len(pool))
		}
		group := byProblem[bestPID]
		selected = append(selected, group[0])
		byProblem[bestPID] = group[1:]
		taken[bestPID]++
	}

	sortLeaves(selected)
	return selected
}

// sortLeaves gives a stable, deterministic display/storage order (by record id)
// independent of the sampling process itself.
func sortLeaves(leaves []SpotCheckLeaf) {
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].RecordID < leaves[j].RecordID })
}
