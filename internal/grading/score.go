// Package grading owns grade computation and (Phase 4) run execution. Everything
// score-shaped is a decimal string end-to-end; all arithmetic is exact big.Rat —
// float64 never touches a grade (docs/DECISIONS.md D4).
package grading

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// SnapClamp normalizes one criterion score: snap to the nearest multiple of
// increment (ties round away from zero), then clamp to [0, max]. It returns the
// canonical decimal string and whether the value changed — every change is recorded
// on the grading record (D4: the app, never the model or the UI, owns legal scores).
func SnapClamp(score, max, increment string) (string, bool, error) {
	s, err := ratOf(score)
	if err != nil {
		return "", false, fmt.Errorf("score: %w", err)
	}
	m, err := ratOf(max)
	if err != nil {
		return "", false, fmt.Errorf("max: %w", err)
	}
	inc, err := ratOf(increment)
	if err != nil {
		return "", false, fmt.Errorf("increment: %w", err)
	}
	if inc.Sign() <= 0 {
		return "", false, errors.New("increment must be positive")
	}

	// snapped = round(score/inc) * inc, ties away from zero.
	q := new(big.Rat).Quo(s, inc)
	k := roundRat(q)
	snapped := new(big.Rat).Mul(new(big.Rat).SetInt(k), inc)

	// clamp to [0, max]
	zero := new(big.Rat)
	if snapped.Cmp(zero) < 0 {
		snapped = zero
	}
	if snapped.Cmp(m) > 0 {
		snapped = m
	}

	out := ratStr(snapped)
	return out, snapped.Cmp(s) != 0, nil
}

// SumDecimals adds decimal strings exactly, returning a canonical decimal string.
func SumDecimals(scores []string) (string, error) {
	sum := new(big.Rat)
	for _, s := range scores {
		r, err := ratOf(s)
		if err != nil {
			return "", err
		}
		sum.Add(sum, r)
	}
	return ratStr(sum), nil
}

// roundRat rounds to the nearest integer, ties away from zero.
func roundRat(q *big.Rat) *big.Int {
	num := new(big.Int).Abs(q.Num())
	den := q.Denom() // always positive
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	// 2*rem >= den → round up
	if new(big.Int).Lsh(rem, 1).Cmp(den) >= 0 {
		quo.Add(quo, big.NewInt(1))
	}
	if q.Sign() < 0 {
		quo.Neg(quo)
	}
	return quo
}

func ratOf(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty decimal")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("invalid decimal %q", s)
	}
	return r, nil
}

// ratStr renders a rat whose denominator divides a power of 10 (all legal scores:
// integer × decimal increment) as a canonical decimal string.
func ratStr(r *big.Rat) string {
	// 6 fractional digits is far beyond NUMERIC(6,2); trim to canonical form.
	s := r.FloatString(6)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
