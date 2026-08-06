package assign

import (
	"math"
	"math/rand"
	"slices"
	"testing"
)

// totalCost sums the cost of an assignment and fails the test unless the
// assignment is well formed: length n, columns in range, no column reused, and
// exactly min(n,m) rows assigned (the rest -1).
func totalCost(t *testing.T, cost [][]float64, got []int) float64 {
	t.Helper()
	n := len(cost)
	if len(got) != n {
		t.Fatalf("assignment length: got %d, want %d", len(got), n)
	}
	if n == 0 {
		return 0
	}
	m := len(cost[0])
	seen := make(map[int]int, m)
	var sum float64
	assigned := 0
	for i, c := range got {
		if c == -1 {
			continue
		}
		if c < 0 || c >= m {
			t.Fatalf("row %d assigned out-of-range column %d (m=%d)", i, c, m)
		}
		if prev, dup := seen[c]; dup {
			t.Fatalf("column %d assigned to both row %d and row %d", c, prev, i)
		}
		seen[c] = i
		sum += cost[i][c]
		assigned++
	}
	if want := min(n, m); assigned != want {
		t.Fatalf("assigned %d rows, want %d", assigned, want)
	}
	return sum
}

// bruteForceCost is the reference optimum: the cheapest way to assign exactly
// min(n,m) rows to distinct columns, found by exhaustive search. Only usable on
// tiny matrices.
func bruteForceCost(cost [][]float64) float64 {
	n := len(cost)
	if n == 0 {
		return 0
	}
	m := len(cost[0])
	need := min(n, m)
	used := make([]bool, m)
	best := math.Inf(1)

	var rec func(row, assigned int, sum float64)
	rec = func(row, assigned int, sum float64) {
		if assigned == need {
			best = math.Min(best, sum)
			return
		}
		if n-row < need-assigned {
			return
		}
		rec(row+1, assigned, sum) // leave this row unassigned
		for c := 0; c < m; c++ {
			if used[c] {
				continue
			}
			used[c] = true
			rec(row+1, assigned+1, sum+cost[row][c])
			used[c] = false
		}
	}
	rec(0, 0, 0)
	return best
}

func TestSolve_SingleCell(t *testing.T) {
	got, err := Solve([][]float64{{4.2}})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if !slices.Equal(got, []int{0}) {
		t.Fatalf("got %v, want [0]", got)
	}
}

func TestSolve_TwoByTwo(t *testing.T) {
	tests := []struct {
		name  string
		cost  [][]float64
		want  []int
		total float64
	}{
		{"diagonal optimum", [][]float64{{1, 5}, {5, 1}}, []int{0, 1}, 2},
		{"anti-diagonal optimum", [][]float64{{5, 1}, {1, 5}}, []int{1, 0}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Solve(tc.cost)
			if err != nil {
				t.Fatalf("Solve: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if total := totalCost(t, tc.cost, got); math.Abs(total-tc.total) > 1e-9 {
				t.Fatalf("total cost: got %v, want %v", total, tc.total)
			}
		})
	}
}

func TestSolve_GreedyRowMinimumIsSuboptimal(t *testing.T) {
	// Taking each row's cheapest free column in order gives 1+4+9 = 14; the
	// unique optimum is the anti-diagonal, 3+4+3 = 10.
	cost := [][]float64{
		{1, 2, 3},
		{2, 4, 6},
		{3, 6, 9},
	}
	got, err := Solve(cost)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if want := []int{2, 1, 0}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if total := totalCost(t, cost, got); math.Abs(total-10) > 1e-9 {
		t.Fatalf("total cost: got %v, want 10", total)
	}
}

func TestSolve_MoreColumnsThanRows(t *testing.T) {
	// Both rows are assigned; their individual minima (col 1 and col 2) do not
	// collide, so the optimum is 2+1 = 3 and columns 0 and 3 stay unused.
	cost := [][]float64{
		{7, 2, 9, 4},
		{5, 3, 1, 8},
	}
	got, err := Solve(cost)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if want := []int{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if total := totalCost(t, cost, got); math.Abs(total-3) > 1e-9 {
		t.Fatalf("total cost: got %v, want 3", total)
	}
}

func TestSolve_MoreRowsThanColumns(t *testing.T) {
	// Only two of the four rows can be assigned: rows 1 and 2 hold the column
	// minima (1 and 2) in different rows, so the optimum is 3 and rows 0 and 3
	// are left out.
	cost := [][]float64{
		{8, 9},
		{1, 7},
		{6, 2},
		{9, 9},
	}
	got, err := Solve(cost)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if want := []int{-1, 0, 1, -1}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	total := totalCost(t, cost, got)
	if want := bruteForceCost(cost); math.Abs(total-want) > 1e-9 {
		t.Fatalf("total cost: got %v, want %v (brute force)", total, want)
	}
}

func TestSolve_TiedOptima(t *testing.T) {
	// Both permutations cost 3, so only the total is defined; either answer is
	// correct and the solver must not be pinned to one.
	cost := [][]float64{
		{1, 2},
		{1, 2},
	}
	got, err := Solve(cost)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if total := totalCost(t, cost, got); math.Abs(total-3) > 1e-9 {
		t.Fatalf("total cost: got %v, want 3", total)
	}
}

func TestSolve_InvalidInput(t *testing.T) {
	tests := []struct {
		name string
		cost [][]float64
	}{
		{"NaN", [][]float64{{1, math.NaN()}, {3, 4}}},
		{"positive infinity", [][]float64{{1, 2}, {math.Inf(1), 4}}},
		{"negative infinity", [][]float64{{math.Inf(-1), 2}, {3, 4}}},
		{"ragged rows", [][]float64{{1, 2}, {3}}},
		{"rows without columns", [][]float64{{}, {}, {}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Solve(tc.cost)
			if err == nil {
				t.Fatalf("Solve: got %v, want an error", got)
			}
		})
	}
}

func TestSolve_EmptyMatrix(t *testing.T) {
	got, err := Solve([][]float64{})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want an empty assignment", got)
	}
}

func TestSolve_MatchesBruteForce(t *testing.T) {
	// Fixed seed so a failure reproduces exactly; these draws cover all three
	// shapes (20 with n<m, 9 square, 21 with n>m).
	rng := rand.New(rand.NewSource(2))
	for trial := 0; trial < 50; trial++ {
		n := 1 + rng.Intn(6)
		m := 1 + rng.Intn(6)
		cost := make([][]float64, n)
		for i := range cost {
			cost[i] = make([]float64, m)
			for j := range cost[i] {
				cost[i][j] = rng.Float64()*20 - 10 // negatives included
			}
		}
		got, err := Solve(cost)
		if err != nil {
			t.Fatalf("trial %d (%dx%d): Solve: %v", trial, n, m, err)
		}
		total := totalCost(t, cost, got)
		if want := bruteForceCost(cost); math.Abs(total-want) > 1e-9 {
			t.Fatalf("trial %d (%dx%d): total cost %v, brute force %v", trial, n, m, total, want)
		}
	}
}
