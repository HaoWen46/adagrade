// Package assign solves the rectangular min-cost assignment (linear sum
// assignment) problem with the Jonker-Volgenant algorithm.
package assign

import (
	"fmt"
	"math"
)

// Solve returns, for each row of cost, the column assigned to it, or -1 when
// the row is left unassigned. cost is an n×m matrix (n rows, m cols); the
// returned slice has length n. The assignment minimizes total cost over all
// matchings that assign min(n,m) rows — so with more rows than columns, the
// solver also chooses WHICH rows to drop, and those get -1.
//
// Every cost must be finite. The algorithm carries row/column potentials and
// repeatedly subtracts a running minimum from them; a NaN or ±Inf anywhere
// poisons those reduced costs and turns every later comparison into a silent
// arbitrary choice, so non-finite input is rejected instead of returning a
// plausible-looking wrong answer. A ragged matrix, or rows with no columns, is
// likewise an error. An empty matrix (n==0) returns an empty assignment.
func Solve(cost [][]float64) ([]int, error) {
	n := len(cost)
	if n == 0 {
		return []int{}, nil
	}
	m := len(cost[0])
	if m == 0 {
		return nil, fmt.Errorf("assign: %d rows with 0 columns", n)
	}
	for i, row := range cost {
		if len(row) != m {
			return nil, fmt.Errorf("assign: ragged matrix: row %d has %d columns, want %d", i, len(row), m)
		}
		for j, c := range row {
			if math.IsNaN(c) || math.IsInf(c, 0) {
				return nil, fmt.Errorf("assign: cost[%d][%d] is not finite: %v", i, j, c)
			}
		}
	}

	if n <= m {
		return matchRows(cost), nil
	}

	// More rows than columns: transpose so the matched side is the smaller one,
	// then invert the result. Only m rows come back with a column; the rest keep
	// the -1 fill.
	rowFor := matchRows(transpose(cost))
	out := make([]int, n)
	for i := range out {
		out[i] = -1
	}
	for col, row := range rowFor {
		out[row] = col
	}
	return out, nil
}

// matchRows assigns every one of the n rows a distinct column, minimizing the
// total. It requires a non-ragged n×m matrix with n <= m and finite costs; Solve
// enforces all three, and n <= m is also what makes the loop below terminate —
// there is always a free column left to augment into.
//
// This is the successive-shortest-path (Jonker-Volgenant) form: rows are added
// one at a time and matched along a shortest augmenting path in the reduced-cost
// graph. The row and column potentials u and v keep every reduced cost
// non-negative, which is what makes the dense Dijkstra scan below valid, and are
// tightened by delta after each path step. Cost is O(n·m·min(n,m)) overall.
//
// Arrays are 1-based internally — index 0 is the virtual source column the
// augmenting path starts from, and p[j]==0 means column j is free — matching the
// standard formulation. Callers pass and receive 0-based indices.
func matchRows(cost [][]float64) []int {
	n, m := len(cost), len(cost[0])
	inf := math.Inf(1)
	u := make([]float64, n+1)
	v := make([]float64, m+1)
	p := make([]int, m+1)   // p[j]: row matched to column j
	way := make([]int, m+1) // way[j]: column preceding j on the augmenting path

	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]float64, m+1)
		used := make([]bool, m+1)
		for j := range minv {
			minv[j] = inf
		}

		// Grow the shortest-path tree one column at a time until it reaches a
		// free column. Only i-1 < m columns are matched at this point, so an
		// unused one always remains: j1 gets set and delta stays finite.
		for {
			used[j0] = true
			i0, j1 := p[j0], 0
			delta := inf
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				if cur := cost[i0-1][j-1] - u[i0] - v[j]; cur < minv[j] {
					minv[j], way[j] = cur, j0
				}
				if minv[j] < delta {
					delta, j1 = minv[j], j
				}
			}
			for j := 0; j <= m; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}

		// Walk the path back to the source, shifting each column's row onto it.
		for j0 != 0 {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
		}
	}

	out := make([]int, n)
	for j := 1; j <= m; j++ {
		if p[j] != 0 {
			out[p[j]-1] = j - 1
		}
	}
	return out
}

func transpose(cost [][]float64) [][]float64 {
	n, m := len(cost), len(cost[0])
	out := make([][]float64, m)
	for j := range out {
		out[j] = make([]float64, n)
		for i := range cost {
			out[j][i] = cost[i][j]
		}
	}
	return out
}
