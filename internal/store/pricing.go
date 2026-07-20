package store

import (
	"context"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// ModelPricing aliases the generated row type.
type ModelPricing = db.ModelPricing

// UpsertModelPricing creates or edits a (provider, model) pricing row (trust spec
// §2). input/output are decimal-string $/Mtok (NUMERIC handling per
// internal/store/numeric.go — never float64).
func (s *Store) UpsertModelPricing(ctx context.Context, providerID int64, model, inputUSDPerMtok, outputUSDPerMtok string) (ModelPricing, error) {
	in, err := Num(inputUSDPerMtok)
	if err != nil {
		return ModelPricing{}, fmt.Errorf("input_usd_per_mtok: %w", err)
	}
	out, err := Num(outputUSDPerMtok)
	if err != nil {
		return ModelPricing{}, fmt.Errorf("output_usd_per_mtok: %w", err)
	}
	return s.Q.UpsertModelPricing(ctx, db.UpsertModelPricingParams{
		ProviderID:       providerID,
		Model:            model,
		InputUsdPerMtok:  in,
		OutputUsdPerMtok: out,
	})
}

// ListModelPricing returns every pricing row for one provider (Providers page, D35).
func (s *Store) ListModelPricing(ctx context.Context, providerID int64) ([]ModelPricing, error) {
	return s.Q.ListModelPricing(ctx, providerID)
}

// MonthToDateCost sums cost_usd across all model grading_records created since the
// start of the current UTC calendar month — the left side of the monthly global cap
// comparison (trust spec §3, D36). Returned as a decimal string per the NUMERIC
// convention; never float64.
func (s *Store) MonthToDateCost(ctx context.Context) (string, error) {
	n, err := s.Q.MonthToDateCost(ctx)
	if err != nil {
		return "", err
	}
	return NumStr(n), nil
}

// RunCostRow is RunCost's result: total spend + token counts for one run.
type RunCostRow struct {
	TotalUSD     string
	InputTokens  int64
	OutputTokens int64
}

// RunCost sums cost_usd/tokens for one run's grading_records (Runs list "cost per
// run", trust spec §7; also the per-run cap check's accumulated-spend read side).
func (s *Store) RunCost(ctx context.Context, runID int64) (RunCostRow, error) {
	row, err := s.Q.RunCost(ctx, pgtype.Int8{Int64: runID, Valid: true})
	if err != nil {
		return RunCostRow{}, err
	}
	return RunCostRow{
		TotalUSD:     NumStr(row.Total),
		InputTokens:  row.InputTokens,
		OutputTokens: row.OutputTokens,
	}, nil
}

// costScale is grading_records.cost_usd's NUMERIC(10,6) fractional scale — the
// computed cost is rounded to 6 decimal places before being stored.
const costScale = 6

// bigTenPow6 is 10^6, used to convert token counts to fractional Mtok and to
// quantize the final rational cost to costScale decimal places.
var bigTenPow6 = new(big.Int).Exp(big.NewInt(10), big.NewInt(6), nil)

// CostUSD computes a grading_record's cost from its token counts and a pricing
// row's $/Mtok rates (trust spec §2), doing every step as exact big.Rat math —
// never float64. When either price is not a valid finite numeric (no pricing row
// resolved for this provider/model), the result is an invalid (NULL) Numeric:
// absence must stay visible, never collapse into a fake $0 (D35).
func CostUSD(inputTokens, outputTokens int64, inputUSDPerMtok, outputUSDPerMtok pgtype.Numeric) pgtype.Numeric {
	if !validFiniteNumeric(inputUSDPerMtok) || !validFiniteNumeric(outputUSDPerMtok) {
		return pgtype.Numeric{} // Valid: false — NULL, not zero
	}

	inCost := new(big.Rat).Mul(
		new(big.Rat).SetFrac(big.NewInt(inputTokens), bigTenPow6),
		numRat(inputUSDPerMtok),
	)
	outCost := new(big.Rat).Mul(
		new(big.Rat).SetFrac(big.NewInt(outputTokens), bigTenPow6),
		numRat(outputUSDPerMtok),
	)
	total := new(big.Rat).Add(inCost, outCost)

	return ratToNumeric(total, costScale)
}

// EstimateHeuristicInputTokens and EstimateHeuristicOutputTokens are MODELS.md's
// own per-answer token heuristic (trust spec §3): 1500 input + 400 output tokens,
// the basis for a run's pre-flight cost estimate before any leaf actually runs.
const (
	EstimateHeuristicInputTokens  = 1500
	EstimateHeuristicOutputTokens = 400
)

// EstimateCostUSD is the pre-flight estimate shown on the run-creation dialog
// (trust spec §3): answers × the MODELS.md heuristic tokens × pricing. ok is false
// when pricing isn't valid/finite for this (provider, model) — the caller must
// show "unknown", never a fake $0 (D35 applied to estimates too).
func EstimateCostUSD(answers int64, inputUSDPerMtok, outputUSDPerMtok pgtype.Numeric) (estimate pgtype.Numeric, ok bool) {
	if !validFiniteNumeric(inputUSDPerMtok) || !validFiniteNumeric(outputUSDPerMtok) {
		return pgtype.Numeric{}, false
	}
	if answers < 0 {
		answers = 0
	}
	cost := CostUSD(answers*EstimateHeuristicInputTokens, answers*EstimateHeuristicOutputTokens, inputUSDPerMtok, outputUSDPerMtok)
	return cost, true
}

// validFiniteNumeric reports whether n holds a real, finite value (not SQL NULL,
// not NaN/Infinity) — the precondition for numRat.
func validFiniteNumeric(n pgtype.Numeric) bool {
	return n.Valid && !n.NaN && n.InfinityModifier == pgtype.Finite && n.Int != nil
}

// ratToNumeric rounds r to `scale` decimal places (half-away-from-zero, matching
// SQL NUMERIC rounding) and encodes it as a pgtype.Numeric with that fixed scale —
// mirroring how Postgres stores a NUMERIC(p,scale) column.
func ratToNumeric(r *big.Rat, scale int) pgtype.Numeric {
	scaleFactor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(scaleFactor))

	num := new(big.Int).Set(scaled.Num())
	den := scaled.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	// Half-away-from-zero: if the remainder is at least half the denominator,
	// bump the quotient's magnitude by one in the sign direction of r.
	remAbs := new(big.Int).Abs(rem)
	doubled := new(big.Int).Lsh(remAbs, 1) // 2*|rem|
	if doubled.CmpAbs(den) >= 0 {
		if scaled.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}

	return pgtype.Numeric{Int: q, Exp: int32(-scale), Valid: true}
}
