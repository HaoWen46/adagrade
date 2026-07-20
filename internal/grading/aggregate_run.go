package grading

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// AggregationReport summarizes one aggregation pass for the operator.
type AggregationReport struct {
	AnswersConsidered int            `json:"answers_considered"`
	AggregatesWritten int            `json:"aggregates_written"`
	Flagged           map[string]int `json:"flagged"`
	// OfficialsSet is how many official pointers the post-run derivation
	// (store.RecomputeOfficials, 0027) moved — non-zero only when this
	// assessment's final source is the consensus.
	OfficialsSet int `json:"officials_set"`
}

// ExecuteAggregation runs the assessment's consensus policy over existing records
// (D17). Pure DB math — no provider calls — so it is cheap to re-run after every
// new grading run or policy tweak.
func ExecuteAggregation(ctx context.Context, st *store.Store, assessmentID, actorID int64) (AggregationReport, error) {
	rep := AggregationReport{Flagged: map[string]int{}}

	policy, err := st.Q.GetAggregationPolicy(ctx, assessmentID)
	if err != nil {
		return rep, fmt.Errorf("no aggregation policy configured for this assessment yet")
	}
	n := len(policy.MethodVersionIds)
	if err := ValidatePolicyShape(n, int(policy.FaultTolerance)); err != nil {
		return rep, err
	}
	triggers := map[string]bool{}
	for _, t := range policy.FlagTriggers {
		triggers[t] = true
	}

	rows, err := st.Q.PanelRecordsForAssessment(ctx, db.PanelRecordsForAssessmentParams{
		AssessmentID: assessmentID, MethodVersionIds: policy.MethodVersionIds,
	})
	if err != nil {
		return rep, err
	}

	// Group rows per answer (query is ordered by answer_id).
	type answerBatch struct {
		answerID        int64
		rubricVersionID int64
		increment       string
		inputs          []PanelInput
	}
	var batches []answerBatch
	for _, row := range rows {
		var scores []CriterionScore
		if err := json.Unmarshal(row.CriterionScores, &scores); err != nil {
			return rep, fmt.Errorf("record %d: bad criterion_scores: %w", row.RecordID, err)
		}
		in := PanelInput{
			MethodVersionID: row.MethodVersionID.Int64,
			RecordID:        row.RecordID,
			Confidence:      row.Confidence.String,
			Scores:          scores,
		}
		if len(batches) == 0 || batches[len(batches)-1].answerID != row.AnswerID {
			batches = append(batches, answerBatch{
				answerID:        row.AnswerID,
				rubricVersionID: row.RubricVersionID,
				increment:       store.NumStr(row.ScoreIncrement),
			})
		}
		batches[len(batches)-1].inputs = append(batches[len(batches)-1].inputs, in)
	}

	// Criterion maxes per rubric version, cached.
	maxCache := map[int64]map[int64]string{}
	criterionMaxes := func(rvID int64) (map[int64]string, error) {
		if m, ok := maxCache[rvID]; ok {
			return m, nil
		}
		crits, err := st.Q.ListRubricCriteria(ctx, rvID)
		if err != nil {
			return nil, err
		}
		m := make(map[int64]string, len(crits))
		for _, c := range crits {
			m[c.ID] = store.NumStr(c.Points)
		}
		maxCache[rvID] = m
		return m, nil
	}

	// aggregationChunkSize batches per-answer transactions (F11): aggregation is
	// a derived, re-runnable operation (D17 — pure DB math, no provider calls),
	// so aborting a whole chunk on one answer's error and letting the operator
	// re-run is an acceptable trade for cutting ~1800 sequential transactions
	// down to ~36. Result semantics (records/flags written) are identical to
	// the one-tx-per-answer version; only the commit granularity changes.
	const aggregationChunkSize = 50
	for start := 0; start < len(batches); start += aggregationChunkSize {
		end := min(start+aggregationChunkSize, len(batches))
		chunk := batches[start:end]

		err := st.WithTx(ctx, func(q *db.Queries) error {
			for _, b := range chunk {
				if err := aggregateOneAnswer(ctx, q, policy, n, triggers, actorID, b.answerID, b.rubricVersionID, b.increment, b.inputs, criterionMaxes, &rep); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return rep, err
		}
	}

	// Officials are derived, never set here (0027): with kind='consensus'
	// selected, the recompute picks up the aggregates this run just wrote
	// (flagged answers stay holes for the human-fallback queue).
	moved, err := st.RecomputeOfficials(ctx, assessmentID)
	if err != nil {
		return rep, err
	}
	rep.OfficialsSet = int(moved)
	return rep, nil
}

// aggregateOneAnswer runs the decision algorithm and writes flags/record/official
// for one answer, inside the caller's transaction. Split out of ExecuteAggregation
// so the per-answer body is identical whether it runs alone or batched (F11).
func aggregateOneAnswer(
	ctx context.Context, q *db.Queries, policy db.AggregationPolicy, n int, triggers map[string]bool,
	actorID, answerID, rubricVersionID int64, increment string, inputs []PanelInput,
	criterionMaxes func(int64) (map[int64]string, error), rep *AggregationReport,
) error {
	rep.AnswersConsidered++
	maxes, err := criterionMaxes(rubricVersionID)
	if err != nil {
		return err
	}
	res, err := Combine(CombineParams{
		Combiner:       policy.Combiner,
		PanelSize:      n,
		FaultTolerance: int(policy.FaultTolerance),
		Increment:      increment,
		CriterionMax:   maxes,
		Inputs:         inputs,
	})
	if err != nil {
		return fmt.Errorf("answer %d: %w", answerID, err)
	}

	// Which agg_* flags does this outcome raise (filtered by enabled triggers)?
	raise := map[string]bool{}
	if res.Missing && triggers[FlagAggMissing] {
		raise[FlagAggMissing] = true
	}
	if len(res.ContestedCriteria) > 0 && triggers[FlagAggDisagreement] {
		raise[FlagAggDisagreement] = true
	}
	if res.LowConfidence && triggers[FlagAggLowConf] {
		raise[FlagAggLowConf] = true
	}

	// Aggregation owns the agg_* flags: clear all, re-add raised (D17).
	// RemoveAnswerFlag/AddAnswerFlag are both guarded (only match a row that
	// actually changes state), so an unchanged outcome across re-runs writes no
	// flag rows at all (F11).
	for _, f := range []string{FlagAggMissing, FlagAggDisagreement, FlagAggLowConf} {
		if raise[f] {
			rep.Flagged[f]++
			if err := q.AddAnswerFlag(ctx, db.AddAnswerFlagParams{ID: answerID, Flag: f}); err != nil {
				return err
			}
		} else if err := q.RemoveAnswerFlag(ctx, db.RemoveAnswerFlagParams{ID: answerID, Flag: f}); err != nil {
			return err
		}
	}
	if res.Missing {
		return nil // nothing trustworthy to write
	}

	scoresJSON, _ := json.Marshal(res.Scores)
	total, err := store.Num(res.Total)
	if err != nil {
		return err
	}
	prov, _ := json.Marshal(map[string]any{
		"combiner":        policy.Combiner,
		"fault_tolerance": policy.FaultTolerance,
		"panel": func() []map[string]int64 {
			out := make([]map[string]int64, 0, len(inputs))
			for _, in := range inputs {
				out = append(out, map[string]int64{"method_version_id": in.MethodVersionID, "record_id": in.RecordID})
			}
			return out
		}(),
		"contested_criteria": res.ContestedCriteria,
	})
	comment := fmt.Sprintf("Consensus of %d/%d models (%s, fault tolerance %d)",
		len(inputs), n, policy.Combiner, policy.FaultTolerance)

	_, err = q.InsertAggregateRecord(ctx, db.InsertAggregateRecordParams{
		AnswerID:        answerID,
		RubricVersionID: rubricVersionID,
		CriterionScores: scoresJSON,
		Total:           total,
		Comment:         comment,
		RawOutput:       prov,
		CreatedBy:       pgtype.Int8{Int64: actorID, Valid: actorID != 0},
	})
	if err != nil {
		return err
	}
	rep.AggregatesWritten++
	return nil
}
