package grading

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// ErrNoContestedRecord means an AI re-grade was requested for a sub-item whose student
// has no officially-graded answer for that problem (nothing to re-examine). The worker
// treats this as terminal — retrying can't conjure an official grade.
var ErrNoContestedRecord = errors.New("regrade: no officially-graded answer for this problem to re-examine")

// RegradeAssistForSubItem runs the stricter AI re-grade for ONE regrade sub-item — the
// (request, problem) pair (spec §8, per-sub-item re-scope) — end to end: the single entry
// point the River `regrade.ai` worker calls with a SUB-ITEM id only (D14). It resolves
// the ONE contested official answer for that problem, redacts THAT problem's complaint
// text (D51) using the roster identity — never logging the text, only redaction counts —
// pins the method/rubric/refsol from the contested record (D53) and the latest regrade_v1
// prompt template, and appends a source='regrade_ai' record, linking the SUB-ITEM to it
// via ai_record_id.
//
// CONTEXT ISOLATION (spec §10, normative): the LLM sees ONLY this problem's masked pages,
// rubric, reference, original grades, and THIS problem's complaint text. Problem 1's
// prompt can never contain problem 4's complaint because the job is scoped to a single
// sub-item's complaint_text and a single answer — the "no problem tag ⇒ fan out to all
// answers" v1 behavior is deleted.
//
// Redaction happens HERE, at execution time, not at enqueue time — so the raw complaint
// text is never serialized into River's durable job args. Identity strings come from the
// students row (resolvable from the sub-item's request), never from the text itself.
//
// Finding 3 (spec §8): every TERMINAL failure path persists a short constant reason to
// regrade_request_problems.ai_error via persistSubItemAIError before returning. A
// non-terminal outcome (a mid-flight shutdown cancellation, F17) does NOT persist
// anything — the job simply re-runs on the next start. Success clears any prior ai_error
// via SetProblemAIRecord (inside RegradeAssist).
func (r *Runner) RegradeAssistForSubItem(ctx context.Context, subItemID int64) error {
	sub, err := r.Store.Q.GetRequestProblem(ctx, subItemID)
	if err != nil {
		return fmt.Errorf("regrade: load sub-item %d: %w", subItemID, err)
	}
	rr, err := r.Store.Q.GetRegradeRequest(ctx, sub.RequestID)
	if err != nil {
		return fmt.Errorf("regrade: load request %d: %w", sub.RequestID, err)
	}

	// Finding 4: re-check eligibility HERE, at execution time — a request can resolve in
	// the window between enqueue and this job running. Skip quietly (no record, no
	// ai_error, no error) rather than grading a sub-item whose request is no longer open.
	// Deliberately does NOT re-check "already has an AI record" — a legitimate queued
	// re-run already carries one.
	if !regradeStillOpenForAI(rr) {
		r.log().Info("regrade: skipping AI re-grade — request no longer open at execution time",
			"sub_item_id", subItemID, "status", rr.Status, "kind", rr.Kind)
		return nil
	}

	if !rr.StudentID.Valid || !rr.AssessmentID.Valid {
		r.persistSubItemAIError(ctx, subItemID, AIErrorNoContestedRecord)
		return ErrNoContestedRecord
	}

	// Brief the re-grade against the CURRENT EFFECTIVE grade (round 0 overlaid by the
	// latest adopted record from a PRIOR turn), excluding THIS sub-item so a re-run never
	// briefs against its own prior adoption (regrade-round correctness fix).
	contested, err := r.Store.Q.ContestedAnswerForSubItem(ctx, db.ContestedAnswerForSubItemParams{
		AssessmentID:     rr.AssessmentID.Int64,
		StudentID:        rr.StudentID.Int64,
		ProblemID:        sub.ProblemID,
		ExcludeSubItemID: subItemID,
	})
	if err != nil {
		return fmt.Errorf("regrade: resolve contested answer: %w", err)
	}
	if len(contested) == 0 {
		r.persistSubItemAIError(ctx, subItemID, AIErrorNoContestedRecord)
		return ErrNoContestedRecord
	}
	ca := contested[0] // at most one answer per (student, problem)

	// Redact THIS sub-item's complaint text (D51). Identity comes from the roster row.
	student, err := r.Store.Q.GetStudent(ctx, rr.StudentID.Int64)
	if err != nil {
		return fmt.Errorf("regrade: load student for redaction: %w", err)
	}
	identity := regrade.Identity{
		Name:      student.Name,
		StudentID: student.StudentID,
		Email:     student.Email,
	}
	redactedText, counts := regrade.Redact(sub.ComplaintText, identity)
	// COUNTS ONLY in the log (CLAUDE.md PII rule); never the text or the identities.
	r.log().Info("regrade: redacted complaint text",
		"sub_item_id", subItemID,
		"redactions", counts.Total(),
		"name", counts.Name, "student_id", counts.StudentID, "email", counts.Email, "token", counts.Token)

	// The AI re-grade prompt template (latest regrade_v1); pinned per record so a later
	// firmware bump doesn't change already-produced records.
	tmpl, err := r.Store.Q.LatestPromptTemplate(ctx, RegradeTemplateName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("regrade: prompt template %q not seeded", RegradeTemplateName)
		}
		return fmt.Errorf("regrade: load prompt template: %w", err)
	}

	// The problem NUMBER for the hint line.
	var hintNumber int32
	if p, err := r.Store.Q.GetProblem(ctx, sub.ProblemID); err == nil {
		hintNumber = p.Number
	}

	// Round-method resolution (rounds design, supersedes D53's same-model
	// pinning): each regrade turn grades with ITS OWN configured method —
	// usually a single strict model, per the round config on the Regrade tab.
	// Falls back to the contested record's pin when no round method is set.
	var methodVersionID int64
	if rr.Turn.Valid {
		if round, err := r.Store.Q.GetRegradeRoundMethod(ctx, db.GetRegradeRoundMethodParams{
			AssessmentID: rr.AssessmentID.Int64, Turn: rr.Turn.Int32,
		}); err == nil {
			if mv, err := r.Store.Q.LatestMethodVersion(ctx, round.MethodID); err == nil {
				methodVersionID = mv.ID
			}
		}
	}
	if methodVersionID == 0 {
		if !ca.MethodVersionID.Valid {
			// No round method AND no pinned method on the contested record (e.g. an
			// old human official) — nothing to re-run with; persist an ai_error so
			// the amber note surfaces WHY nothing happened (M-drift4).
			r.log().Warn("regrade: no round method and no pinned method version; skipping",
				"sub_item_id", subItemID, "answer_id", ca.AnswerID, "record_id", ca.RecordID)
			r.persistSubItemAIError(ctx, subItemID, AIErrorNoMethodPinned)
			return nil
		}
		methodVersionID = ca.MethodVersionID.Int64
	}

	// D51 (I3): the CONTESTED record's own text reaches the provider too. A HUMAN (TA
	// edit) comment or rationale can name the student, so the SAME identity redaction the
	// complaint gets must cover OriginalComment and every per-criterion rationale before
	// they enter the prompt's original-grade block. Scores/criterion ids are numeric
	// grading signal and pass through unchanged; only the free-text is scrubbed.
	originalComment, cComment := regrade.Redact(ca.OriginalComment, identity)
	origScores := parseRecordScores(ca.CriterionScores)
	rationaleRedactions := 0
	for i := range origScores {
		redacted, rc := regrade.Redact(origScores[i].Rationale, identity)
		origScores[i].Rationale = redacted
		rationaleRedactions += rc.Total()
	}
	if cComment.Total()+rationaleRedactions > 0 {
		r.log().Info("regrade: redacted original-grade text",
			"sub_item_id", subItemID, "answer_id", ca.AnswerID,
			"comment_redactions", cComment.Total(), "rationale_redactions", rationaleRedactions)
	}

	in := RegradeAssistInput{
		SubItemID:                  subItemID,
		AnswerID:                   ca.AnswerID,
		MethodVersionID:            methodVersionID,
		RubricVersionID:            ca.RubricVersionID,
		ReferenceSolutionVersionID: ca.ReferenceSolutionVersionID.Int64,
		PromptTemplateVersionID:    tmpl.ID,
		OriginalScores:             origScores,
		OriginalComment:            originalComment,
		RequestText:                redactedText,
		ProblemNumber:              hintNumber,
	}
	if _, err := r.RegradeAssist(ctx, in); err != nil {
		r.log().Error("regrade: assist failed for sub-item",
			"sub_item_id", subItemID, "answer_id", ca.AnswerID, "err", err)
		if reason, ok := classifyAIError(err); ok {
			r.persistSubItemAIError(ctx, subItemID, reason)
		}
		return err
	}
	return nil
}

// classifyAIError maps a RegradeAssist error to the short constant ai_error reason it
// should surface (Finding 3), or ok=false when the error is NOT terminal (a mid-flight
// shutdown cancellation, or a plain retryable provider/transport blip River will retry —
// persisting a reason for those would show a stale/wrong "failure" while the job is only
// queued to try again).
func classifyAIError(err error) (reason string, ok bool) {
	if errors.Is(err, context.Canceled) {
		return "", false // F17 drain — not terminal, the job re-runs.
	}
	var unavailable *llm.ProviderUnavailableError
	if errors.As(err, &unavailable) {
		return AIErrorProviderRemoved, true
	}
	var mal MalformedError
	if errors.As(err, &mal) {
		return AIErrorOutputInvalid, true
	}
	// Any other error from RegradeAssist not explicitly wrapped as retryableError is
	// itself a plain (terminal-by-default) failure — but without a recognized constant
	// there's nothing safe to persist (never the raw error text — CLAUDE.md PII rule).
	return "", false
}

// persistSubItemAIError writes a terminal AI re-grade failure reason on ONE sub-item
// (Finding 3). Best-effort: a failure to persist is logged, not surfaced.
func (r *Runner) persistSubItemAIError(ctx context.Context, subItemID int64, reason string) {
	if _, err := r.Store.SetProblemAIError(ctx, subItemID, reason); err != nil {
		r.log().Error("regrade: persist sub-item ai_error failed", "sub_item_id", subItemID, "err", err)
	}
}

// regradeStillOpenForAI reports whether a regrade request is still open for an AI
// re-grade at EXECUTION time (Finding 4): a FILED request whose status is
// received/under_review. A resolved request (a result email already sent) or a
// non-filed row (addendum/unparsed/handed_off) is not AI-regradable — mirrors the
// httpapi enqueue gate (aiSubItemEligible), re-checked here because the request can
// resolve between enqueue and this job running.
func regradeStillOpenForAI(rr db.RegradeRequest) bool {
	if rr.Kind != "filed" {
		return false
	}
	return rr.Status == "received" || rr.Status == "under_review"
}

// parseRecordScores decodes a grading_records.criterion_scores JSONB into the shared
// CriterionScore shape for the prompt's "original grade" block. A decode failure yields
// an empty slice — the template simply renders no original-score lines.
func parseRecordScores(raw []byte) []CriterionScore {
	var scores []CriterionScore
	_ = json.Unmarshal(raw, &scores)
	return scores
}
