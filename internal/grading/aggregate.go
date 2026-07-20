package grading

import (
	"fmt"
	"math/big"
	"sort"
)

// Post-hoc multi-model consensus (DECISIONS D17): pure decision math over records
// that already exist — no provider calls. Methods stay single-model; a per-
// assessment policy picks a panel of method versions, a combiner, and a fault
// tolerance f (2f < n).

// Aggregation flag names — owned exclusively by the aggregation step, which both
// adds and clears them on every re-run.
const (
	FlagAggDisagreement = "agg_disagreement"
	FlagAggMissing      = "agg_missing"
	FlagAggLowConf      = "agg_low_confidence"
)

// PanelInput is one usable model record for an answer.
type PanelInput struct {
	MethodVersionID int64
	RecordID        int64
	Confidence      string
	Scores          []CriterionScore
}

// CombineParams describes one answer's aggregation.
type CombineParams struct {
	Combiner       string // "majority" | "mean"
	PanelSize      int    // n — selected methods, not just usable ones
	FaultTolerance int    // f
	Increment      string // rubric score_increment
	CriterionMax   map[int64]string
	Inputs         []PanelInput
}

// CombineResult is the decision for one answer.
type CombineResult struct {
	Missing           bool // usable < n - f: nothing trustworthy to write
	LowConfidence     bool // > f usable records with low/illegible confidence
	ContestedCriteria []int64
	Scores            []CriterionScore
	Total             string
}

// ValidatePolicyShape enforces the panel/fault-tolerance relation (D17): at least
// one method, and strictly fewer than half the panel may be at fault.
func ValidatePolicyShape(n, f int) error {
	if n < 1 {
		return fmt.Errorf("select at least one method for the panel")
	}
	if f < 0 || 2*f >= n {
		return fmt.Errorf("fault tolerance must satisfy 2×f < panel size (n=%d allows f ≤ %d)", n, (n-1)/2)
	}
	return nil
}

// Combine runs the decision algorithm for one answer.
func Combine(p CombineParams) (CombineResult, error) {
	var res CombineResult
	if err := ValidatePolicyShape(p.PanelSize, p.FaultTolerance); err != nil {
		return res, err
	}
	need := p.PanelSize - p.FaultTolerance
	if len(p.Inputs) < need {
		res.Missing = true
		return res, nil
	}

	lowConf := 0
	for _, in := range p.Inputs {
		if in.Confidence == "low" || in.Confidence == "illegible" {
			lowConf++
		}
	}
	res.LowConfidence = lowConf > p.FaultTolerance

	// criterion id → per-model scores (as rats for math, strings for identity).
	type vote struct {
		str string
		rat *big.Rat
	}
	votes := map[int64][]vote{}
	for _, in := range p.Inputs {
		for _, sc := range in.Scores {
			r, err := ratOf(sc.Score)
			if err != nil {
				return res, fmt.Errorf("record %d criterion %d: %w", in.RecordID, sc.CriterionID, err)
			}
			votes[sc.CriterionID] = append(votes[sc.CriterionID], vote{str: sc.Score, rat: r})
		}
	}

	critIDs := make([]int64, 0, len(votes))
	for id := range votes {
		critIDs = append(critIDs, id)
	}
	sort.Slice(critIDs, func(i, j int) bool { return critIDs[i] < critIDs[j] })

	inc, err := ratOf(p.Increment)
	if err != nil || inc.Sign() <= 0 {
		return res, fmt.Errorf("invalid increment %q", p.Increment)
	}

	var totals []string
	for _, id := range critIDs {
		vs := votes[id]
		maxStr, ok := p.CriterionMax[id]
		if !ok {
			return res, fmt.Errorf("criterion %d has no max (rubric mismatch)", id)
		}

		snappedMean := func() (string, error) {
			sum := new(big.Rat)
			for _, v := range vs {
				sum.Add(sum, v.rat)
			}
			mean := new(big.Rat).Quo(sum, big.NewRat(int64(len(vs)), 1))
			out, _, err := SnapClamp(ratStr(mean), maxStr, p.Increment)
			return out, err
		}

		contested := false
		var value string
		switch p.Combiner {
		case "majority":
			// consensus value = any exact value with ≥ (n - f) votes.
			counts := map[string]int{}
			for _, v := range vs {
				counts[canonical(v.rat)]++
			}
			best, bestN := "", 0
			for s, c := range counts {
				if c > bestN {
					best, bestN = s, c
				}
			}
			if bestN >= need {
				value = best
			} else {
				contested = true
				if value, err = snappedMean(); err != nil {
					return res, err
				}
			}
		case "mean":
			if value, err = snappedMean(); err != nil {
				return res, err
			}
			// outlier = further than one increment from the aggregate value.
			valRat, _ := ratOf(value)
			outliers := 0
			for _, v := range vs {
				diff := new(big.Rat).Sub(v.rat, valRat)
				if diff.Sign() < 0 {
					diff.Neg(diff)
				}
				if diff.Cmp(inc) > 0 {
					outliers++
				}
			}
			contested = outliers > p.FaultTolerance
		default:
			return res, fmt.Errorf("unknown combiner %q", p.Combiner)
		}

		// Clamp/snap once more for safety (majority values are already legal).
		value, _, err = SnapClamp(value, maxStr, p.Increment)
		if err != nil {
			return res, err
		}
		if contested {
			res.ContestedCriteria = append(res.ContestedCriteria, id)
		}
		res.Scores = append(res.Scores, CriterionScore{CriterionID: id, Score: value})
		totals = append(totals, value)
	}

	res.Total, err = SumDecimals(totals)
	return res, err
}

// canonical renders a rat so 3, 3.0 and 3.00 vote identically.
func canonical(r *big.Rat) string { return ratStr(r) }
