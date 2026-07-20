package grading

import (
	"slices"
	"testing"
)

// pool builds count answers per problem with ids interleaved across problems so
// ascending answer id never accidentally equals the stratified order.
func calibPool(perProblem map[int64]int) []SampleAnswer {
	var out []SampleAnswer
	id := int64(100)
	for remaining := true; remaining; {
		remaining = false
		for _, pid := range []int64{1, 2, 3, 4} {
			if perProblem[pid] > 0 {
				out = append(out, SampleAnswer{AnswerID: id, ProblemID: pid})
				perProblem[pid]--
				id++
				remaining = true
			}
		}
	}
	return out
}

func TestSelectCalibrationSample_DeterministicAcrossPoolOrder(t *testing.T) {
	pool := calibPool(map[int64]int{1: 4, 2: 4, 3: 4})
	shuffled := slices.Clone(pool)
	slices.Reverse(shuffled)

	a := SelectCalibrationSample(7, pool, 6)
	b := SelectCalibrationSample(7, shuffled, 6)
	if !slices.Equal(a, b) {
		t.Fatalf("pool order changed the sample: %v vs %v", a, b)
	}
	c := SelectCalibrationSample(7, pool, 6)
	if !slices.Equal(a, c) {
		t.Fatalf("same inputs, different sample: %v vs %v", a, c)
	}
}

func TestSelectCalibrationSample_StratifiesAcrossProblems(t *testing.T) {
	pool := calibPool(map[int64]int{1: 4, 2: 4, 3: 4})
	got := SelectCalibrationSample(42, pool, 6)
	if len(got) != 6 {
		t.Fatalf("want 6, got %d", len(got))
	}
	perProblem := map[int64]int{}
	for _, s := range got {
		perProblem[s.ProblemID]++
	}
	for pid, n := range perProblem {
		if n != 2 {
			t.Fatalf("problem %d got %d picks, want 2 (even split): %v", pid, n, got)
		}
	}
}

func TestSelectCalibrationSample_SmallProblemNotStarved(t *testing.T) {
	pool := calibPool(map[int64]int{1: 1, 2: 5})
	got := SelectCalibrationSample(3, pool, 4)
	perProblem := map[int64]int{}
	for _, s := range got {
		perProblem[s.ProblemID]++
	}
	if perProblem[1] != 1 || perProblem[2] != 3 {
		t.Fatalf("want 1 from problem 1 and 3 from problem 2, got %v", perProblem)
	}
}

func TestSelectCalibrationSample_ClampsToPool(t *testing.T) {
	pool := calibPool(map[int64]int{1: 2, 2: 1})
	got := SelectCalibrationSample(9, pool, 50)
	if len(got) != 3 {
		t.Fatalf("want whole pool (3), got %d", len(got))
	}
	if !slices.IsSortedFunc(got, func(a, b SampleAnswer) int { return int(a.AnswerID - b.AnswerID) }) {
		t.Fatalf("clamped sample not in stable answer-id order: %v", got)
	}
}

func TestSelectCalibrationSample_NonPositiveN(t *testing.T) {
	pool := calibPool(map[int64]int{1: 3})
	if got := SelectCalibrationSample(1, pool, 0); got != nil {
		t.Fatalf("n=0: want nil, got %v", got)
	}
	if got := SelectCalibrationSample(1, nil, 5); got != nil {
		t.Fatalf("empty pool: want nil, got %v", got)
	}
}
