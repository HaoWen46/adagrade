package grading

import "testing"

// leaf is the minimal shape SelectSpotCheckSample needs: a graded record id and
// the problem it belongs to (for stratification).
func leaves(n int, problems int) []SpotCheckLeaf {
	out := make([]SpotCheckLeaf, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, SpotCheckLeaf{
			RecordID:  int64(i + 1),
			ProblemID: int64(i%problems + 1),
		})
	}
	return out
}

func TestSelectSpotCheckSample_Deterministic(t *testing.T) {
	pool := leaves(200, 4)
	a := SelectSpotCheckSample(42, pool)
	b := SelectSpotCheckSample(42, pool)
	if len(a) != len(b) {
		t.Fatalf("sample sizes differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].RecordID != b[i].RecordID {
			t.Fatalf("sample %d diverged at index %d: %d vs %d", 42, i, a[i].RecordID, b[i].RecordID)
		}
	}
}

func TestSelectSpotCheckSample_DifferentRunIDsDiffer(t *testing.T) {
	pool := leaves(200, 4)
	a := SelectSpotCheckSample(1, pool)
	b := SelectSpotCheckSample(2, pool)
	same := len(a) == len(b)
	if same {
		for i := range a {
			if a[i].RecordID != b[i].RecordID {
				same = false
				break
			}
		}
	}
	if same {
		t.Fatalf("expected different run ids to (almost certainly) produce different samples, got identical")
	}
}

func TestSelectSpotCheckSample_SizeFormula(t *testing.T) {
	cases := []struct {
		name    string
		nLeaves int
		want    int
	}{
		{"empty pool", 0, 0},
		{"tiny run below floor: all leaves", 3, 3},
		{"exactly floor", 5, 5},
		{"just above floor, 5% still below floor", 60, 5}, // 5% of 60 = 3, floored to 5
		{"5% exceeds floor", 200, 10},                     // 5% of 200 = 10
		{"huge run capped at 20", 1000, 20},               // 5% of 1000 = 50, capped to 20
		{"cap boundary exactly 20", 400, 20},              // 5% of 400 = 20
		{"just under cap boundary", 399, 19},              // 5% of 399 = 19.95 -> 19 (floor via int truncation intended below)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SpotCheckSampleSize(c.nLeaves)
			if got != c.want {
				t.Errorf("SpotCheckSampleSize(%d) = %d, want %d", c.nLeaves, got, c.want)
			}
			if c.nLeaves > 0 {
				sample := SelectSpotCheckSample(7, leaves(c.nLeaves, 3))
				if len(sample) != got {
					t.Errorf("SelectSpotCheckSample len = %d, want %d (SpotCheckSampleSize)", len(sample), got)
				}
			}
		})
	}
}

func TestSelectSpotCheckSample_NeverExceedsPool(t *testing.T) {
	for _, n := range []int{0, 1, 2, 4, 5} {
		sample := SelectSpotCheckSample(9, leaves(n, 2))
		if len(sample) != n {
			t.Errorf("pool of %d: expected all %d leaves sampled (below floor), got %d", n, n, len(sample))
		}
	}
}

func TestSelectSpotCheckSample_NoDuplicates(t *testing.T) {
	pool := leaves(200, 5)
	sample := SelectSpotCheckSample(123, pool)
	seen := map[int64]bool{}
	for _, l := range sample {
		if seen[l.RecordID] {
			t.Fatalf("duplicate record id %d in sample", l.RecordID)
		}
		seen[l.RecordID] = true
	}
}

// TestSelectSpotCheckSample_Stratification: with enough sample budget, every
// problem must be represented at least once (trust spec §4: "stratified across
// the run's problems").
func TestSelectSpotCheckSample_Stratification(t *testing.T) {
	pool := leaves(400, 4) // sample size 20 (5% of 400, capped), 4 problems
	sample := SelectSpotCheckSample(55, pool)
	seenProblems := map[int64]bool{}
	for _, l := range sample {
		seenProblems[l.ProblemID] = true
	}
	if len(seenProblems) != 4 {
		t.Fatalf("expected all 4 problems represented in a 20-record sample, got %d: %v", len(seenProblems), seenProblems)
	}
}

// TestSelectSpotCheckSample_StratificationSmallSample: sample size smaller than
// problem count still spreads as evenly as possible (no single problem hogging
// the whole tiny sample when others have candidates).
func TestSelectSpotCheckSample_StratificationSmallSample(t *testing.T) {
	// 5 problems, sample size floors at 5 (5% of 100 = 5) — exactly one per problem.
	pool := leaves(100, 5)
	sample := SelectSpotCheckSample(77, pool)
	if len(sample) != 5 {
		t.Fatalf("expected sample size 5, got %d", len(sample))
	}
	seenProblems := map[int64]bool{}
	for _, l := range sample {
		seenProblems[l.ProblemID] = true
	}
	if len(seenProblems) != 5 {
		t.Fatalf("expected each of 5 problems represented exactly once, got %d distinct: %v", len(seenProblems), seenProblems)
	}
}

func TestSelectSpotCheckSample_UnevenProblemSizes(t *testing.T) {
	// Problem 1 has 90 leaves, problems 2-4 have ~3 each — stratification should
	// still pull from every problem that HAS leaves, not just the biggest one.
	pool := make([]SpotCheckLeaf, 0, 100)
	id := int64(1)
	for i := 0; i < 90; i++ {
		pool = append(pool, SpotCheckLeaf{RecordID: id, ProblemID: 1})
		id++
	}
	for p := int64(2); p <= 4; p++ {
		for i := 0; i < 3; i++ {
			pool = append(pool, SpotCheckLeaf{RecordID: id, ProblemID: p})
			id++
		}
	}
	sample := SelectSpotCheckSample(88, pool) // 99 leaves total -> size 5
	seenProblems := map[int64]bool{}
	for _, l := range sample {
		seenProblems[l.ProblemID] = true
	}
	if len(seenProblems) < 4 {
		t.Fatalf("expected all 4 problems represented even though sample (5) < problems' leaves spread, got %d: %v", len(seenProblems), seenProblems)
	}
}
