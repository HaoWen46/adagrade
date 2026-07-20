package store

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// Points travel as decimal strings end-to-end (docs/DECISIONS.md D4): Postgres
// NUMERIC ↔ pgtype.Numeric ↔ string. float64 never appears on the path.

// Num parses a decimal string into a pgtype.Numeric, rejecting NaN/Inf.
func Num(s string) (pgtype.Numeric, error) {
	var n pgtype.Numeric
	if err := n.Scan(strings.TrimSpace(s)); err != nil {
		return pgtype.Numeric{}, fmt.Errorf("invalid decimal %q: %w", s, err)
	}
	if n.NaN || n.InfinityModifier != pgtype.Finite {
		return pgtype.Numeric{}, fmt.Errorf("invalid decimal %q: not a finite number", s)
	}
	return n, nil
}

// NumStr formats a numeric as a canonical decimal string ("12.5", "100"): trailing
// fractional zeros are trimmed so the output is independent of the column's scale
// (NUMERIC(6,2) returns 5.50 for 5.5). Invalid (SQL NULL) numerics format as "".
func NumStr(n pgtype.Numeric) string {
	if !n.Valid || n.NaN || n.InfinityModifier != pgtype.Finite || n.Int == nil {
		return ""
	}
	digits := n.Int.String()
	neg := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")

	exp := int(n.Exp)
	var out string
	switch {
	case exp >= 0:
		out = digits + strings.Repeat("0", exp)
	default:
		frac := -exp
		if len(digits) <= frac {
			digits = strings.Repeat("0", frac-len(digits)+1) + digits
		}
		out = digits[:len(digits)-frac] + "." + digits[len(digits)-frac:]
		out = strings.TrimRight(out, "0")
		out = strings.TrimSuffix(out, ".")
	}
	if neg {
		out = "-" + out
	}
	return out
}

// NumSumEqual reports whether Σ nums == want — the rubric-save invariant (D4).
func NumSumEqual(nums []pgtype.Numeric, want pgtype.Numeric) bool {
	sum := new(big.Rat)
	for _, n := range nums {
		sum.Add(sum, numRat(n))
	}
	return sum.Cmp(numRat(want)) == 0
}

// NumCmp compares two valid numerics: -1, 0, or 1.
func NumCmp(a, b pgtype.Numeric) int {
	ar := numRat(a)
	br := numRat(b)
	return ar.Cmp(br)
}

// NumRat exposes the exact rational value (for snap/clamp math in grading, D4).
func NumRat(n pgtype.Numeric) *big.Rat { return numRat(n) }

func numRat(n pgtype.Numeric) *big.Rat {
	r := new(big.Rat).SetInt(n.Int)
	exp := int64(n.Exp)
	if exp > 0 {
		r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(exp), nil)))
	} else if exp < 0 {
		r.Quo(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(-exp), nil)))
	}
	return r
}

func zeroNumeric() pgtype.Numeric { return pgtype.Numeric{} }
