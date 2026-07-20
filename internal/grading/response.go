package grading

import (
	"encoding/json"
	"fmt"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// ModelOutput is the validated transcribe-then-grade result.
type ModelOutput struct {
	Transcription  string
	Confidence     string
	OverallComment string
	Criteria       []CriterionScore // scores as decimal strings, pre-snap
}

// MalformedError marks output that fails validation — re-askable up to the method's
// cap, never silently accepted (spec §5).
type MalformedError struct{ Msg string }

func (e MalformedError) Error() string { return "malformed model output: " + e.Msg }

// ParseModelOutput decodes and validates raw structured output against the rubric:
// every criterion scored exactly once, no unknown ids, a legal confidence value.
func ParseModelOutput(raw []byte, criteria []db.RubricCriterium) (ModelOutput, error) {
	var wire struct {
		Transcription  string `json:"transcription"`
		Confidence     string `json:"confidence"`
		OverallComment string `json:"overall_comment"`
		Criteria       []struct {
			CriterionID int64       `json:"criterion_id"`
			Score       json.Number `json:"score"`
			Rationale   string      `json:"rationale"`
		} `json:"criteria"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return ModelOutput{}, MalformedError{fmt.Sprintf("not valid JSON for the schema: %v", err)}
	}

	switch wire.Confidence {
	case "high", "medium", "low", "illegible":
	default:
		return ModelOutput{}, MalformedError{fmt.Sprintf("confidence %q not in {high,medium,low,illegible}", wire.Confidence)}
	}

	want := make(map[int64]bool, len(criteria))
	for _, c := range criteria {
		want[c.ID] = false
	}
	out := ModelOutput{
		Transcription:  wire.Transcription,
		Confidence:     wire.Confidence,
		OverallComment: wire.OverallComment,
	}
	for _, c := range wire.Criteria {
		seen, ok := want[c.CriterionID]
		if !ok {
			return ModelOutput{}, MalformedError{fmt.Sprintf("unknown criterion_id %d", c.CriterionID)}
		}
		if seen {
			return ModelOutput{}, MalformedError{fmt.Sprintf("criterion_id %d scored twice", c.CriterionID)}
		}
		want[c.CriterionID] = true
		out.Criteria = append(out.Criteria, CriterionScore{
			CriterionID: c.CriterionID,
			Score:       c.Score.String(),
			Rationale:   c.Rationale,
		})
	}
	for id, seen := range want {
		if !seen {
			return ModelOutput{}, MalformedError{fmt.Sprintf("criterion_id %d missing", id)}
		}
	}
	return out, nil
}
