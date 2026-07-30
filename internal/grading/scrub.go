package grading

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// Pre-persistence identity scrub (PLAN_GAPS B-C10).
//
// grading_records is append-only: whatever text lands in `transcription`,
// `comment`, `criterion_scores[].rationale` and `raw_output` is there forever,
// and per spec §5/§8 the comment can flow outbound in a published result email.
// Image masking is REGION-based, so a name scrawled in a margin the mask never
// covered is read by the model and — because the transcribe-then-grade template
// explicitly ASKS for a verbatim transcription (a reasoning scaffold we must not
// remove) — copied faithfully into that immutable column.
//
// So the scrub happens HERE, between "the model answered" and "we write the
// row": the answer's own roster identity is mechanically excised from every
// model-authored free-text field. This is a persistence-layer pass only —
// prompts, the output schema, and scoring are untouched, and numeric grading
// signal (criterion ids, scores, confidence) never passes through the redactor.
//
// The redactor itself is REUSED from internal/regrade (D51), which already
// handles case-insensitive exact matching, the regrade+<token>@… reply address,
// UTF-8 offset translation, and the empty-needle-is-not-a-wildcard rule.

// ScrubModelOutput removes the answer's roster identity from every free-text
// field of a parsed model output and reports the combined per-kind counts. It is
// pure: the returned ModelOutput carries a fresh criteria slice, so the caller's
// value (and its backing array) is never mutated.
func ScrubModelOutput(out ModelOutput, id regrade.Identity) (ModelOutput, regrade.RedactionCounts) {
	var total regrade.RedactionCounts

	var c regrade.RedactionCounts
	out.Transcription, c = regrade.Redact(out.Transcription, id)
	total = addCounts(total, c)
	out.OverallComment, c = regrade.Redact(out.OverallComment, id)
	total = addCounts(total, c)

	scrubbed := make([]CriterionScore, len(out.Criteria))
	copy(scrubbed, out.Criteria)
	for i := range scrubbed {
		// Only the rationale is free text; CriterionID and Score are grading
		// signal and must reach the snap/clamp step byte-identical.
		scrubbed[i].Rationale, c = regrade.Redact(scrubbed[i].Rationale, id)
		total = addCounts(total, c)
	}
	out.Criteria = scrubbed

	return out, total
}

// addCounts sums two RedactionCounts kind-wise.
func addCounts(a, b regrade.RedactionCounts) regrade.RedactionCounts {
	return regrade.RedactionCounts{
		Name:      a.Name + b.Name,
		StudentID: a.StudentID + b.StudentID,
		Email:     a.Email + b.Email,
		Token:     a.Token + b.Token,
	}
}

// BuildScrubbedRawOutput builds the grading_records.raw_output JSONB.
//
// B-C10 DECISION — raw_output stores a VALIDATED, SCRUBBED SUBSET, not the
// provider's verbatim bytes. Two reasons, in order of weight:
//
//  1. Scrubbing the verbatim bytes is not sound. The identity appears in the raw
//     payload in JSON-ESCAPED form, so an exact-substring redactor silently
//     misses `O\"Brien`, a `張`-escaped CJK name, or any provider that
//     escapes non-ASCII — the leak survives while the counts read zero. Redaction
//     is only reliable on DECODED strings, which is exactly what ModelOutput
//     holds after ParseModelOutput. Re-serializing from the scrubbed ModelOutput
//     therefore gives a raw_output that is scrubbed by construction.
//
//  2. The subset loses nothing that is actually retained for audit. Everything
//     ParseModelOutput validated is here — the resolved model version string, the
//     transcription, confidence, overall comment, and the PRE-SNAP per-criterion
//     scores and rationales (the snapped values and the from→to deltas live in
//     criterion_scores/adjustments). Only unschema'd extra keys a provider might
//     have appended are dropped, and nothing in the codebase reads them.
//
// The envelope also carries the RedactionCounts (numbers only — the one
// derivative of a redaction that is safe to persist, D51) so an operator can
// query "which records had identity survive the mask?" without re-deriving it,
// and `"scrubbed": true` so no future reader mistakes this for verbatim bytes.
func BuildScrubbedRawOutput(resolvedModel string, out ModelOutput, counts regrade.RedactionCounts) []byte {
	type criterionOut struct {
		CriterionID int64       `json:"criterion_id"`
		Score       json.Number `json:"score"`
		Rationale   string      `json:"rationale"`
	}
	criteria := make([]criterionOut, 0, len(out.Criteria))
	for _, c := range out.Criteria {
		// Score came out of a json.Number in ParseModelOutput, so it is already a
		// legal JSON numeric literal and re-encodes unquoted.
		criteria = append(criteria, criterionOut{
			CriterionID: c.CriterionID, Score: json.Number(c.Score), Rationale: c.Rationale,
		})
	}

	raw, err := json.Marshal(map[string]any{
		"resolved_model": resolvedModel,
		"scrubbed":       true,
		"redaction":      counts,
		"output": map[string]any{
			"transcription":   out.Transcription,
			"confidence":      out.Confidence,
			"overall_comment": out.OverallComment,
			"criteria":        criteria,
		},
	})
	if err != nil {
		// Only reachable if a score somehow isn't a legal JSON number. Keep the
		// audit envelope rather than writing NULL, and never fall back to the
		// unscrubbed provider bytes.
		raw, _ = json.Marshal(map[string]any{
			"resolved_model": resolvedModel,
			"scrubbed":       true,
			"redaction":      counts,
			"output_error":   "raw output subset could not be encoded",
		})
	}
	return raw
}

// answerIdentity resolves the roster identity of the answer's OWN student — the
// only identity that may be scrubbed out of that answer's transcription. It
// fails closed: a caller that cannot resolve the identity must not persist the
// record, because an unscrubbed transcription is exactly the B-C10 leak.
func (r *Runner) answerIdentity(ctx context.Context, answer db.Answer) (regrade.Identity, error) {
	student, err := r.Store.Q.GetStudent(ctx, answer.StudentID)
	if err != nil {
		return regrade.Identity{}, fmt.Errorf("resolve roster identity for transcription scrub: %w", err)
	}
	return regrade.Identity{
		Name:      student.Name,
		StudentID: student.StudentID,
		Email:     student.Email,
	}, nil
}

// logRedactions surfaces a non-zero scrub as an operator-visible WARNING: a
// non-zero count means identity text survived the region mask and reached the
// provider, which is a mask-quality defect worth investigating on the CORE
// grading path — not a routine event. COUNTS ONLY ever reach the log; never the
// redacted text, never the identity itself (CLAUDE.md PII rule). A zero-count
// scrub logs nothing, so the signal is not buried under one line per leaf.
func (r *Runner) logRedactions(msg string, counts regrade.RedactionCounts, kv ...any) {
	if counts.Total() == 0 {
		return
	}
	args := append([]any{}, kv...)
	args = append(args,
		"redactions", counts.Total(),
		"name", counts.Name, "student_id", counts.StudentID,
		"email", counts.Email, "token", counts.Token)
	r.log().Warn(msg, args...)
}
