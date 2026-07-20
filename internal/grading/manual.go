package grading

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// CriterionScore is one criterion's result as stored in grading_records JSONB.
// Scores are decimal strings (D4). Rationale carries the human note or (Phase 4)
// the model's justification.
type CriterionScore struct {
	CriterionID int64  `json:"criterion_id"`
	Score       string `json:"score"`
	Rationale   string `json:"rationale,omitempty"`
}

// Adjustment documents a snap/clamp applied by the app (D4: auditable, never silent).
type Adjustment struct {
	CriterionID int64  `json:"criterion_id"`
	From        string `json:"from"`
	To          string `json:"to"`
}

// ManualGradeInput is a TA's per-criterion grade for one answer.
type ManualGradeInput struct {
	AnswerID        int64
	RubricVersionID int64
	Comment         string
	Scores          []CriterionScore
	CreatedBy       int64
}

// ValidationError distinguishes caller mistakes (→ 400) from system failures.
type ValidationError struct{ Msg string }

func (e ValidationError) Error() string { return e.Msg }

// InsertManualRecord validates a manual grade against the pinned rubric version,
// snaps/clamps every score, computes the total app-side, and appends an immutable
// human record. It never touches the official pointer (that is a separate,
// deliberate action — D6).
func InsertManualRecord(ctx context.Context, st *store.Store, in ManualGradeInput) (db.GradingRecord, error) {
	answer, err := st.Q.GetAnswer(ctx, in.AnswerID)
	if err != nil {
		return db.GradingRecord{}, ValidationError{"no such answer"}
	}
	rv, err := st.Q.GetRubricVersion(ctx, in.RubricVersionID)
	if err != nil || rv.ProblemID != answer.ProblemID {
		return db.GradingRecord{}, ValidationError{"rubric version does not belong to this answer's problem"}
	}
	criteria, err := st.Q.ListRubricCriteria(ctx, rv.ID)
	if err != nil {
		return db.GradingRecord{}, err
	}
	if len(criteria) == 0 {
		return db.GradingRecord{}, ValidationError{"rubric version has no criteria"}
	}

	byID := make(map[int64]db.RubricCriterium, len(criteria))
	for _, c := range criteria {
		byID[c.ID] = c
	}
	seen := make(map[int64]bool, len(in.Scores))
	increment := store.NumStr(rv.ScoreIncrement)

	finalScores := make([]CriterionScore, 0, len(criteria))
	var adjustments []Adjustment
	var totals []string
	for _, sc := range in.Scores {
		crit, ok := byID[sc.CriterionID]
		if !ok {
			return db.GradingRecord{}, ValidationError{fmt.Sprintf("criterion %d is not part of rubric version %d", sc.CriterionID, rv.Version)}
		}
		if seen[sc.CriterionID] {
			return db.GradingRecord{}, ValidationError{fmt.Sprintf("criterion %d scored twice", sc.CriterionID)}
		}
		seen[sc.CriterionID] = true

		snapped, adjusted, err := SnapClamp(sc.Score, store.NumStr(crit.Points), increment)
		if err != nil {
			return db.GradingRecord{}, ValidationError{fmt.Sprintf("criterion %d: %v", sc.CriterionID, err)}
		}
		if adjusted {
			adjustments = append(adjustments, Adjustment{CriterionID: sc.CriterionID, From: sc.Score, To: snapped})
		}
		finalScores = append(finalScores, CriterionScore{CriterionID: sc.CriterionID, Score: snapped, Rationale: sc.Rationale})
		totals = append(totals, snapped)
	}
	if len(seen) != len(criteria) {
		return db.GradingRecord{}, ValidationError{fmt.Sprintf("all %d criteria must be scored (got %d)", len(criteria), len(seen))}
	}

	totalStr, err := SumDecimals(totals)
	if err != nil {
		return db.GradingRecord{}, err
	}
	total, err := store.Num(totalStr)
	if err != nil {
		return db.GradingRecord{}, err
	}

	pages, err := st.Q.ListAnswerPages(ctx, in.AnswerID)
	if err != nil {
		return db.GradingRecord{}, err
	}
	shas := make([]string, 0, len(pages))
	for _, p := range pages {
		shas = append(shas, p.ImageSha256)
	}

	scoresJSON, err := json.Marshal(finalScores)
	if err != nil {
		return db.GradingRecord{}, err
	}
	adjJSON, err := json.Marshal(adjustments)
	if err != nil {
		return db.GradingRecord{}, err
	}
	if adjustments == nil {
		adjJSON = []byte("[]")
	}

	return st.Q.InsertHumanRecord(ctx, db.InsertHumanRecordParams{
		AnswerID:        in.AnswerID,
		RubricVersionID: rv.ID,
		GradedImageShas: shas,
		CriterionScores: scoresJSON,
		Total:           total,
		Comment:         in.Comment,
		Adjustments:     adjJSON,
		CreatedBy:       pgtype.Int8{Int64: in.CreatedBy, Valid: in.CreatedBy != 0},
	})
}
