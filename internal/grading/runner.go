package grading

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// Answer flags the runner may add (D12).
const (
	FlagIllegible     = "illegible"
	FlagLowConfidence = "low_confidence"
)

// BudgetExceededReason is the terminal grading_run_items.error value written when
// the per-run cost cap stops a leaf before it calls the provider (trust spec §3,
// D36). It is a plain string, matching how every other terminal failure reason
// (malformed-output messages, provider errors) is stored — retry-failed-only
// (ResetFailedItems) clears it like any other failed leaf once the cap is raised.
const BudgetExceededReason = "budget_exceeded"

// Runner executes grading runs. It is queue-agnostic: the River workers call Plan
// and ExecuteLeaf; tests call them directly with a fake provider source.
type Runner struct {
	Store     *store.Store
	Blobs     blobstore.Store
	Providers llm.ProviderSource // DB-backed in production (app-managed, D11 v1)

	// EnqueueLeaves inserts one leaf job per item INSIDE the planning transaction
	// (spec §6.1: no "items written, jobs lost"). nil in tests.
	EnqueueLeaves func(ctx context.Context, tx pgx.Tx, provider string, itemIDs []int64) error

	// Stopping reports whether a graceful shutdown drain is in progress (F17).
	// Wired to the queue client's stopping flag; nil means "not stopping".
	// ExecuteLeaf consults it on a final-attempt interruption: during a shutdown
	// the worker snoozes the job (the attempt is not consumed, the leaf reworks
	// on next start) so the item may safely stay 'running'; outside a shutdown
	// River is about to DISCARD the job, so the item must be failed terminally
	// or nothing — not River, not retry-failed — can ever reach it again.
	Stopping func() bool

	Log *slog.Logger
}

// stopping is the nil-safe read of the Stopping hook.
func (r *Runner) stopping() bool { return r.Stopping != nil && r.Stopping() }

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// failRun marks the run failed with a reason (best-effort).
func (r *Runner) failRun(ctx context.Context, runID int64, reason string) error {
	_, err := r.Store.Q.SetRunStatus(ctx, db.SetRunStatusParams{
		ID: runID, Status: "failed", Error: pgtype.Text{String: reason, Valid: true},
	})
	if err == nil {
		r.log().Warn("run failed", "run_id", runID, "reason", reason)
	}
	return err
}

// Plan resolves the run's scope into per-(answer, model) items with pinned rubric +
// reference-solution versions, enforcing the mask gate (D10), then enqueues leaves
// transactionally and flips the run to running.
func (r *Runner) Plan(ctx context.Context, runID int64) error {
	run, err := r.Store.Q.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("plan: load run %d: %w", runID, err)
	}
	if run.Status != "pending" {
		return nil // idempotent re-delivery
	}

	mv, err := r.Store.Q.GetMethodVersion(ctx, run.MethodVersionID)
	if err != nil {
		return r.failRun(ctx, runID, "method version missing")
	}
	cfg, err := ParseMethodConfig(mv.Config)
	if err != nil {
		return r.failRun(ctx, runID, err.Error())
	}
	if _, _, err := r.Providers.Provider(ctx, cfg.Provider); err != nil {
		return r.failRun(ctx, runID, err.Error())
	}

	// Resolve scope → answers that actually have pages.
	var answerIDs []int64
	switch run.ScopeKind {
	case "assessment":
		answerIDs, err = r.Store.Q.AnswerIDsForAssessment(ctx, run.AssessmentID)
	case "problem":
		answerIDs, err = r.Store.Q.AnswerIDsForProblem(ctx, run.ScopeID)
	case "answer":
		answerIDs = []int64{run.ScopeID}
	case "sample":
		// Calibration run (spec 2026-07-20 §1): scope_id carries the sample size
		// N; the concrete answer set is drawn deterministically here (seeded by
		// run id, problem-stratified) and persisted as this run's items, so the
		// sample is recorded and re-planning is idempotent.
		if run.ScopeID < 1 {
			return r.failRun(ctx, runID, "sample size must be at least 1")
		}
		answerIDs, err = r.resolveSampleScope(ctx, run.AssessmentID, runID, run.ScopeID)
	default:
		return r.failRun(ctx, runID, "unknown scope kind")
	}
	if err != nil {
		return fmt.Errorf("plan: scope resolve: %w", err)
	}
	if len(answerIDs) == 0 {
		return r.failRun(ctx, runID, "no answers with pages in scope — ingest submissions first")
	}

	// Mask gate (D10): every in-scope page must be masked AND review-accepted.
	blockers, err := r.Store.Q.CountMaskBlockersForAnswers(ctx, answerIDs)
	if err != nil {
		return fmt.Errorf("plan: mask gate: %w", err)
	}
	if blockers > 0 {
		return r.failRun(ctx, runID, fmt.Sprintf("%d page(s) in scope lack an accepted masked copy — apply masks and finish the mask review first", blockers))
	}

	// Pin rubric + reference-solution versions per problem (D5/B-H18).
	answers, err := r.Store.Q.AnswersWithProblems(ctx, answerIDs)
	if err != nil {
		return fmt.Errorf("plan: answers: %w", err)
	}
	type pins struct {
		rubric pgtype.Int8
		refsol pgtype.Int8
	}
	pinned := map[int64]pins{}
	for _, a := range answers {
		if _, done := pinned[a.ProblemID]; done {
			continue
		}
		// Both per-problem requirements below are MIRRORED in internal/httpapi/
		// runs.go (same query, same message text): the rubric check by
		// missingRubricBlockers (estimate-time), the reference-solution check by
		// missingRefSolutionMessage (launch-time 400) and
		// missingRefSolutionBlockers (estimate-time). Edit every site together.
		rv, err := r.Store.Q.LatestRubricVersion(ctx, a.ProblemID)
		if err != nil {
			return r.failRun(ctx, runID, fmt.Sprintf("problem %d has no rubric — grading needs one", a.ProblemID))
		}
		p := pins{rubric: pgtype.Int8{Int64: rv.ID, Valid: true}}
		if cfg.RefSolutions > 0 {
			sv, err := r.Store.Q.LatestSolutionVersion(ctx, a.ProblemID)
			if err != nil {
				return r.failRun(ctx, runID, fmt.Sprintf("method includes a reference solution but problem %d has none", a.ProblemID))
			}
			p.refsol = pgtype.Int8{Int64: sv.ID, Valid: true}
		}
		pinned[a.ProblemID] = p
	}

	// Transaction: items + leaf jobs + status flip.
	return r.Store.WithTxPgx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		itemIDs := make([]int64, 0, len(answers))
		for _, a := range answers {
			p := pinned[a.ProblemID]
			item, err := q.CreateRunItem(ctx, db.CreateRunItemParams{
				RunID: runID, AnswerID: a.ID,
				ModelID: cfg.Model, Provider: cfg.Provider,
				RubricVersionID: p.rubric, ReferenceSolutionVersionID: p.refsol,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				continue // re-planned duplicate; job already exists
			}
			if err != nil {
				return err
			}
			itemIDs = append(itemIDs, item.ID)
		}
		if r.EnqueueLeaves != nil && len(itemIDs) > 0 {
			if err := r.EnqueueLeaves(ctx, tx, cfg.Provider, itemIDs); err != nil {
				return err
			}
		}
		_, err := q.SetRunStatus(ctx, db.SetRunStatusParams{ID: runID, Status: "running"})
		return err
	})
}

// isInterruption reports whether a failed leaf/identify call was a shutdown or
// timeout interruption rather than a grading verdict (F17). It is true when the
// returned error is (or wraps) context.Canceled/DeadlineExceeded, OR the context
// itself is already done — the latter catches a provider that swallowed the cause
// into a plain error but was really aborted by ctx cancellation. Callers must treat
// this as "record a plain attempt, write no terminal state".
func isInterruption(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return ctx.Err() != nil
}

// retryableError wraps provider/transient failures so the queue retries the leaf.
// resolveSampleScope draws a sample-scope run's answer set: the assessment's
// gradeable answers (same pool as assessment scope), reduced to a
// deterministic problem-stratified sample of n = the run's scope_id.
func (r *Runner) resolveSampleScope(ctx context.Context, assessmentID, runID, n int64) ([]int64, error) {
	ids, err := r.Store.Q.AnswerIDsForAssessment(ctx, assessmentID)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	rows, err := r.Store.Q.AnswersWithProblems(ctx, ids)
	if err != nil {
		return nil, err
	}
	pool := make([]SampleAnswer, 0, len(rows))
	for _, row := range rows {
		pool = append(pool, SampleAnswer{AnswerID: row.ID, ProblemID: row.ProblemID})
	}
	sample := SelectCalibrationSample(runID, pool, int(n))
	out := make([]int64, 0, len(sample))
	for _, s := range sample {
		out = append(out, s.AnswerID)
	}
	return out, nil
}

type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

// ExecuteLeaf grades one (answer, model) pair. finalAttempt tells it to convert a
// retryable failure into a terminal failed item instead of retrying forever (D12).
func (r *Runner) ExecuteLeaf(ctx context.Context, itemID int64, finalAttempt bool) error {
	item, err := r.Store.Q.GetRunItem(ctx, itemID)
	if err != nil {
		return fmt.Errorf("leaf: load item %d: %w", itemID, err)
	}
	if item.State == "succeeded" || item.State == "skipped" {
		// Redelivered already-terminal leaf: the original attempt may have
		// crashed after writing the item but BEFORE its completion check, leaving
		// the run 'running' with no live items. Re-run the same check the success
		// path runs — MaybeCompleteRun's WHERE guard makes it idempotent.
		return r.finishLeaf(ctx, item.RunID, nil)
	}
	run, err := r.Store.Q.GetRun(ctx, item.RunID)
	if err != nil {
		return err
	}
	if run.Status == "cancelled" || run.Status == "failed" {
		_, err := r.Store.Q.MarkItemTerminal(ctx, db.MarkItemTerminalParams{
			ID: itemID, State: "skipped", Error: pgtype.Text{String: "run " + run.Status, Valid: true},
		})
		return err
	}

	// Per-run budget cap (trust spec §3, D36): checked before every grade call,
	// not just at plan time, so a cap crossed mid-run by earlier leaves' spend
	// still stops the rest. At/over cap is a terminal failure — never a retry —
	// so the queue doesn't spin on it; raising the cap and retry-failed reruns
	// these leaves exactly like any other terminal failure. This check runs
	// BEFORE MarkItemRunning and before any provider call, so it can never race
	// with the F17 shutdown-cancel path above: if the RunCost read itself errors
	// (e.g. context cancelled mid-query during a shutdown drain), this returns
	// the plain error without calling terminalFail — no terminal state is
	// written, the item stays whatever it was, and the queue's normal
	// retry/redelivery picks it back up. Only a SUCCESSFUL read that finds
	// spend >= cap is a real terminal budget_exceeded verdict.
	if run.CostCapUsd.Valid {
		spend, err := r.Store.RunCost(ctx, run.ID)
		if err != nil {
			return fmt.Errorf("leaf: run cost check: %w", err)
		}
		spendNum, err := store.Num(spend.TotalUSD)
		if err != nil {
			return fmt.Errorf("leaf: parse run cost: %w", err)
		}
		if store.NumCmp(spendNum, run.CostCapUsd) >= 0 {
			return r.terminalFail(ctx, item, BudgetExceededReason)
		}
	}

	if _, err := r.Store.Q.MarkItemRunning(ctx, itemID); err != nil {
		return err
	}

	err = r.gradeLeaf(ctx, item, run)
	// F17: a shutdown/timeout that cancels the provider call mid-flight surfaces as
	// context.Canceled/DeadlineExceeded. This is an INTERRUPTION, not a grading
	// verdict — never burn it into a terminal failed item while another delivery is
	// still coming. Skip every terminal-state write and return the error so River
	// records a plain errored attempt (the leafWorker turns it into a JobSnooze on
	// the shutdown path so the attempt isn't consumed). The item stays 'running' and
	// the rescuer / next start reworks it. Checked on BOTH the returned error and ctx
	// so a wrapped-away cancellation (an error that swallowed the cause) is still
	// caught.
	//
	// EXCEPT on a final attempt outside a shutdown drain: there is no next delivery —
	// River discards the job, and an item left 'running' is unrecoverable (retry-
	// failed only resets state='failed'). Write an honest terminal failure instead so
	// retry-failed can pick it up. The write runs on a detached context because the
	// interruption usually killed ctx itself (per-job timeout). During a shutdown
	// (Stopping hook) the worker snoozes the job — the attempt is NOT consumed, so
	// the non-terminal path stays correct even on the final attempt.
	if isInterruption(ctx, err) {
		if finalAttempt && !r.stopping() {
			r.log().Warn("leaf interrupted on final attempt; failing terminally so retry-failed can recover it", "item_id", itemID, "run_id", item.RunID)
			return r.terminalFail(context.WithoutCancel(ctx), item, "interrupted on final attempt (timeout/shutdown)")
		}
		r.log().Warn("leaf interrupted (shutdown/timeout); not terminal", "item_id", itemID, "run_id", item.RunID)
		return err
	}
	var mal MalformedError
	var retry retryableError
	switch {
	case err == nil:
		return r.finishLeaf(ctx, item.RunID, nil)
	case errors.As(err, &mal):
		// Malformed past the re-ask cap: terminal, NO record, never a silent zero (D12).
		return r.terminalFail(ctx, item, mal.Error())
	case errors.As(err, &retry) && !finalAttempt:
		r.log().Warn("leaf retryable failure", "item_id", itemID, "run_id", item.RunID, "err", err)
		return err // queue backoff retries
	default:
		return r.terminalFail(ctx, item, err.Error())
	}
}

func (r *Runner) terminalFail(ctx context.Context, item db.GradingRunItem, msg string) error {
	if _, err := r.Store.Q.MarkItemTerminal(ctx, db.MarkItemTerminalParams{
		ID: item.ID, State: "failed", Error: pgtype.Text{String: msg, Valid: true},
	}); err != nil {
		return err
	}
	r.log().Warn("leaf failed terminally", "item_id", item.ID, "run_id", item.RunID)
	return r.finishLeaf(ctx, item.RunID, errors.New(msg))
}

// finishLeaf completes the run when no live items remain. The leaf's own error (if
// any) is swallowed here — it lives on the item row.
func (r *Runner) finishLeaf(ctx context.Context, runID int64, _ error) error {
	rowsAffected, err := r.Store.Q.MaybeCompleteRun(ctx, runID)
	if err != nil {
		return err
	}
	if rowsAffected > 0 {
		// This call just flipped the run pending->completed (MaybeCompleteRun's
		// WHERE guards against re-triggering on redelivery). Create the spot-check
		// sample now (trust spec §4, D37): best-effort — a failure here must not
		// fail the leaf/run completion itself, since InsertSpotChecks is
		// idempotent (ON CONFLICT DO NOTHING) and a later poll of the run detail
		// page can be extended to retry if this ever needs to be more than
		// best-effort. Logged, not propagated.
		// createSpotCheckSample itself no-ops if a sample already exists (e.g. a
		// retry-failed run re-completing) — see its doc comment for why that's not
		// just an optimization but a correctness requirement (the first sample is
		// canonical, trust spec §4).
		if err := r.createSpotCheckSample(ctx, runID); err != nil {
			r.log().Error("spot-check sample creation failed", "run_id", runID, "err", err)
		}
		// Officials are derived (0027): a finished run may have produced the
		// chosen source's newest records, so re-derive. Cheap no-op when this
		// run's method isn't the assessment's final source (or none is chosen).
		if run, err := r.Store.Q.GetRun(ctx, runID); err == nil {
			if _, err := r.Store.RecomputeOfficials(ctx, run.AssessmentID); err != nil {
				r.log().Error("officials recompute failed", "run_id", runID, "err", err)
			}
		}
	}
	return nil
}

// createSpotCheckSample builds the run's graded-leaf pool from its succeeded
// items and inserts the deterministic, problem-stratified sample (trust spec §4).
//
// The FIRST sample taken for a run is canonical: a retry-failed cycle can bring
// previously-failed leaves into the succeeded pool and re-complete the run (via
// MaybeCompleteRun), which re-invokes this method. Drawing a fresh sample against
// that larger pool would pick a different/larger set, and InsertSpotChecks (row-
// idempotent only via ON CONFLICT DO NOTHING) would APPEND the new rows rather
// than replace anything — re-blocking a spot-check gate a human already cleared.
// So once a run already has a sample (or has been waived), this is a no-op.
func (r *Runner) createSpotCheckSample(ctx context.Context, runID int64) error {
	total, _, waived, err := r.Store.SpotCheckState(ctx, runID)
	if err != nil {
		return fmt.Errorf("spot-check sample: state: %w", err)
	}
	if total > 0 || waived {
		return nil // sample already canonical for this run — never re-drawn
	}

	// runItemsSampleLimit mirrors httpapi's runItemsLimit headroom (F20): a
	// single run tops out around ~1800 leaves in current usage.
	const runItemsSampleLimit = 2000
	items, err := r.Store.Q.ListRunItems(ctx, db.ListRunItemsParams{RunID: runID, ItemLimit: runItemsSampleLimit})
	if err != nil {
		return fmt.Errorf("spot-check sample: list run items: %w", err)
	}
	pool := make([]SpotCheckLeaf, 0, len(items))
	for _, it := range items {
		if it.State != "succeeded" || !it.RecordID.Valid {
			continue
		}
		// NOTE: SpotCheckLeaf.ProblemID is a stratification KEY, not the problems.id FK —
		// here it carries the problem NUMBER (unique within the assessment, cheaper to
		// fetch). See the SpotCheckLeaf doc comment in spotcheck.go.
		pool = append(pool, SpotCheckLeaf{RecordID: it.RecordID.Int64, ProblemID: int64(it.ProblemNumber)})
	}
	if len(pool) == 0 {
		return nil // nothing graded (e.g. a run that completed with only skipped/failed leaves)
	}
	sample := SelectSpotCheckSample(runID, pool)
	recordIDs := make([]int64, 0, len(sample))
	for _, l := range sample {
		recordIDs = append(recordIDs, l.RecordID)
	}
	return r.Store.InsertSpotChecks(ctx, runID, recordIDs)
}

// gradeLeaf does the actual provider call + validation + record write.
func (r *Runner) gradeLeaf(ctx context.Context, item db.GradingRunItem, run db.GradingRun) error {
	// Idempotence: a record may already exist from a prior delivery.
	if rec, err := r.Store.Q.GetRecordForLeaf(ctx, db.GetRecordForLeafParams{
		RunID:    pgtype.Int8{Int64: item.RunID, Valid: true},
		AnswerID: item.AnswerID,
		ModelID:  pgtype.Text{String: item.ModelID, Valid: true},
	}); err == nil {
		_, err := r.Store.Q.MarkItemTerminal(ctx, db.MarkItemTerminalParams{
			ID: item.ID, State: "succeeded", RecordID: pgtype.Int8{Int64: rec.ID, Valid: true},
		})
		return err
	}

	mv, err := r.Store.Q.GetMethodVersion(ctx, run.MethodVersionID)
	if err != nil {
		return err
	}
	cfg, err := ParseMethodConfig(mv.Config)
	if err != nil {
		return err
	}
	provider, limiter, err := r.Providers.Provider(ctx, cfg.Provider)
	if err != nil {
		var unavailable *llm.ProviderUnavailableError
		if errors.As(err, &unavailable) {
			return err // terminal: a missing/disabled provider won't heal by retrying
		}
		return retryableError{err}
	}

	// Load masked page images — the ONLY thing that may reach the provider (D10).
	pages, err := r.Store.Q.ListAnswerPages(ctx, item.AnswerID)
	if err != nil {
		return err
	}
	if len(pages) == 0 {
		return errors.New("answer has no pages")
	}
	images := make([]imaging.ProviderImage, 0, len(pages))
	shas := make([]string, 0, len(pages))
	for _, pg := range pages {
		if !pg.MaskedImageRef.Valid || pg.MaskReviewStatus != "accepted" {
			return errors.New("page lacks an accepted masked copy (mask gate)")
		}
		rc, err := r.Blobs.Get(ctx, pg.MaskedImageRef.String)
		if err != nil {
			return fmt.Errorf("masked blob missing: %w", err)
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		m, err := imaging.LoadMasked(pg.MaskedImageRef.String, raw)
		if err != nil {
			return err
		}
		images = append(images, m)
		shas = append(shas, pg.ImageSha256)
	}

	// Assemble the frozen prompt inputs (all pinned versions).
	answer, err := r.Store.Q.GetAnswer(ctx, item.AnswerID)
	if err != nil {
		return err
	}
	// B-C10: resolved BEFORE the provider call so a leaf can never spend tokens
	// and then discover it has no identity to scrub the transcription against.
	identity, err := r.answerIdentity(ctx, answer)
	if err != nil {
		return err
	}
	problem, err := r.Store.Q.GetProblem(ctx, answer.ProblemID)
	if err != nil {
		return err
	}
	if !item.RubricVersionID.Valid {
		return errors.New("item has no pinned rubric version")
	}
	rv, err := r.Store.Q.GetRubricVersion(ctx, item.RubricVersionID.Int64)
	if err != nil {
		return err
	}
	criteria, err := r.Store.Q.ListRubricCriteria(ctx, rv.ID)
	if err != nil {
		return err
	}
	refSolution := ""
	if item.ReferenceSolutionVersionID.Valid {
		sv, err := r.Store.Q.GetSolutionVersion(ctx, item.ReferenceSolutionVersionID.Int64)
		if err != nil {
			return err
		}
		refSolution = sv.Content
	}
	tmpl, err := r.Store.Q.GetPromptTemplateVersion(ctx, cfg.PromptTemplateVersionID)
	if err != nil {
		return fmt.Errorf("pinned prompt template missing: %w", err)
	}

	data := BuildPromptData(problem, rv, criteria, refSolution, cfg.Policy)
	systemPrompt, err := RenderPrompt(tmpl.SystemTemplate, data)
	if err != nil {
		return err
	}
	userPrompt, err := RenderPrompt(tmpl.UserTemplate, data)
	if err != nil {
		return err
	}
	schema := BuildOutputSchema(criteria)

	// Call with bounded re-asks on malformed output (spec §5).
	var output ModelOutput
	var result llm.Result
	prompt := userPrompt
	for attempt := 0; ; attempt++ {
		if limiter != nil {
			if err := limiter.Wait(ctx); err != nil {
				return retryableError{err}
			}
		}
		result, err = provider.Grade(ctx, cfg.Model, llm.Request{
			System:         systemPrompt,
			Prompt:         prompt,
			Images:         images,
			Schema:         schema,
			Temperature:    cfg.Temperature,
			MaxTokens:      cfg.MaxTokens,
			ReasoningLevel: cfg.ReasoningLevel,
		})
		if err != nil {
			return retryableError{err} // provider/transport errors → queue backoff
		}
		output, err = ParseModelOutput(result.JSON, criteria)
		if err == nil {
			break
		}
		var mal MalformedError
		if !errors.As(err, &mal) {
			return err
		}
		if attempt >= cfg.ReaskCap {
			return mal // terminal (D12)
		}
		r.log().Warn("re-asking after malformed output", "item_id", item.ID, "attempt", attempt+1)
		prompt = userPrompt + "\n\nYour previous response was rejected: " + mal.Msg +
			". Respond again following the schema exactly — every rubric criterion exactly once."
	}

	// Identity scrub BEFORE anything is persisted (B-C10). Runs on the parsed
	// output, so every downstream artifact — the transcription and comment
	// columns, criterion_scores' rationales, and raw_output — inherits the
	// scrubbed text from this one point. Scores/criterion ids pass through
	// untouched, so the snap/clamp below is unaffected.
	output, redactions := ScrubModelOutput(output, identity)
	r.logRedactions("identity survived the mask and was scrubbed from the model output before persistence",
		redactions, "item_id", item.ID, "run_id", item.RunID, "answer_id", item.AnswerID)

	// Snap/clamp in Go; the model's arithmetic is never trusted (D4).
	byID := make(map[int64]db.RubricCriterium, len(criteria))
	for _, c := range criteria {
		byID[c.ID] = c
	}
	increment := store.NumStr(rv.ScoreIncrement)
	final := make([]CriterionScore, 0, len(output.Criteria))
	var adjustments []Adjustment
	var totals []string
	for _, sc := range output.Criteria {
		crit := byID[sc.CriterionID]
		snapped, adjusted, err := SnapClamp(sc.Score, store.NumStr(crit.Points), increment)
		if err != nil {
			return MalformedError{fmt.Sprintf("criterion %d score %q: %v", sc.CriterionID, sc.Score, err)}
		}
		if adjusted {
			adjustments = append(adjustments, Adjustment{CriterionID: sc.CriterionID, From: sc.Score, To: snapped})
		}
		final = append(final, CriterionScore{CriterionID: sc.CriterionID, Score: snapped, Rationale: sc.Rationale})
		totals = append(totals, snapped)
	}

	illegible := output.Confidence == "illegible"
	var total pgtype.Numeric // NULL for the refusal path (D12)
	if !illegible {
		totalStr, err := SumDecimals(totals)
		if err != nil {
			return err
		}
		if total, err = store.Num(totalStr); err != nil {
			return err
		}
	}

	scoresJSON, _ := json.Marshal(final)
	adjJSON := []byte("[]")
	if adjustments != nil {
		adjJSON, _ = json.Marshal(adjustments)
	}
	// model_id keeps the method's requested id (leaf identity/idempotence); the
	// provider-resolved concrete version string is preserved for audit (B-H2).
	// raw_output is the SCRUBBED VALIDATED SUBSET, never the verbatim provider
	// bytes — see BuildScrubbedRawOutput for why (B-C10).
	rawOutput := BuildScrubbedRawOutput(result.Model, output, redactions)

	// cost_usd computed HERE, at insert time, from this leaf's own token counts ×
	// today's pricing row (trust spec §2, D35). No historical backfill: a pricing
	// edit only changes what future InsertModelRecord calls compute, never rewrites
	// past records. Missing provider/pricing row ⇒ NULL cost, never a fake $0.
	costUSD := r.lookupCost(ctx, cfg.Provider, item.ModelID, int64(result.InputTokens), int64(result.OutputTokens))

	// Persist record + item + flags + run completion check.
	return r.Store.WithTx(ctx, func(q *db.Queries) error {
		rec, err := q.InsertModelRecord(ctx, db.InsertModelRecordParams{
			AnswerID:                   item.AnswerID,
			RunID:                      pgtype.Int8{Int64: item.RunID, Valid: true},
			Provider:                   pgtype.Text{String: cfg.Provider, Valid: true},
			ModelID:                    pgtype.Text{String: item.ModelID, Valid: true},
			MethodVersionID:            pgtype.Int8{Int64: mv.ID, Valid: true},
			RubricVersionID:            rv.ID,
			ReferenceSolutionVersionID: item.ReferenceSolutionVersionID,
			PromptTemplateVersionID:    pgtype.Int8{Int64: tmpl.ID, Valid: true},
			GradedImageShas:            shas,
			CriterionScores:            scoresJSON,
			Total:                      total,
			Comment:                    output.OverallComment,
			Transcription:              pgtype.Text{String: output.Transcription, Valid: output.Transcription != ""},
			Confidence:                 pgtype.Text{String: output.Confidence, Valid: true},
			Adjustments:                adjJSON,
			RawOutput:                  rawOutput,
			InputTokens:                pgtype.Int4{Int32: int32(result.InputTokens), Valid: true},
			OutputTokens:               pgtype.Int4{Int32: int32(result.OutputTokens), Valid: true},
			CostUsd:                    costUSD,
			Temperature:                pgtype.Float4{Float32: float32(cfg.Temperature), Valid: true},
			// Pin the grading stance alongside the template version (D25); ParseMethodConfig
			// always defaults this, so model records are never NULL.
			Policy: pgtype.Text{String: cfg.Policy, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Conflict with a concurrent duplicate — fetch the winner.
			rec, err = q.GetRecordForLeaf(ctx, db.GetRecordForLeafParams{
				RunID:    pgtype.Int8{Int64: item.RunID, Valid: true},
				AnswerID: item.AnswerID,
				ModelID:  pgtype.Text{String: item.ModelID, Valid: true},
			})
		}
		if err != nil {
			return err
		}
		if _, err := q.MarkItemTerminal(ctx, db.MarkItemTerminalParams{
			ID: item.ID, State: "succeeded", RecordID: pgtype.Int8{Int64: rec.ID, Valid: true},
		}); err != nil {
			return err
		}
		switch output.Confidence {
		case "illegible":
			if err := q.AddAnswerFlag(ctx, db.AddAnswerFlagParams{ID: item.AnswerID, Flag: FlagIllegible}); err != nil {
				return err
			}
		case "low":
			if err := q.AddAnswerFlag(ctx, db.AddAnswerFlagParams{ID: item.AnswerID, Flag: FlagLowConfidence}); err != nil {
				return err
			}
		}
		return nil
	})
}

// lookupCost resolves today's pricing for (provider, model) and computes this
// leaf's cost_usd (trust spec §2, D35). It returns an invalid (NULL) Numeric —
// never an error, never a fake $0 — whenever the provider or the (provider,
// model) pricing row can't be found: a StaticSource test provider has no
// llm_providers row at all, and an operator simply may not have entered a price
// yet. Both are "we don't know the cost", not failures that should block grading.
func (r *Runner) lookupCost(ctx context.Context, providerName, modelID string, inputTokens, outputTokens int64) pgtype.Numeric {
	provider, err := r.Store.Q.GetProviderByName(ctx, providerName)
	if err != nil {
		return pgtype.Numeric{}
	}
	pricing, err := r.Store.Q.GetModelPricing(ctx, db.GetModelPricingParams{
		ProviderID: provider.ID, Model: modelID,
	})
	if err != nil {
		return pgtype.Numeric{}
	}
	return store.CostUSD(inputTokens, outputTokens, pricing.InputUsdPerMtok, pricing.OutputUsdPerMtok)
}
