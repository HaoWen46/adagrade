package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// resolveScope returns the gradable answer ids for a (kind, id) scope.
func (s *Server) resolveScope(r *http.Request, assessmentID int64, kind string, scopeID int64) ([]int64, string) {
	switch kind {
	case "assessment":
		ids, err := s.store.Q.AnswerIDsForAssessment(r.Context(), assessmentID)
		if err != nil {
			return nil, "scope resolve failed"
		}
		return ids, ""
	case "problem":
		p, err := s.store.Q.GetProblem(r.Context(), scopeID)
		if err != nil || p.AssessmentID != assessmentID {
			return nil, "problem does not belong to this assessment"
		}
		ids, err := s.store.Q.AnswerIDsForProblem(r.Context(), scopeID)
		if err != nil {
			return nil, "scope resolve failed"
		}
		return ids, ""
	case "answer":
		a, err := s.store.Q.GetAnswer(r.Context(), scopeID)
		if err != nil || a.AssessmentID != assessmentID {
			return nil, "answer does not belong to this assessment"
		}
		return []int64{scopeID}, ""
	case "sample":
		// Calibration run: scope_id is the sample size N, drawn at plan time.
		// Gates (masks, rubrics, reference solutions) run over the WHOLE
		// assessment pool since the sample may touch any problem — same
		// conservative footing as an assessment-scope launch.
		if scopeID < 1 {
			return nil, "sample size must be at least 1"
		}
		ids, err := s.store.Q.AnswerIDsForAssessment(r.Context(), assessmentID)
		if err != nil {
			return nil, "scope resolve failed"
		}
		return ids, ""
	default:
		return nil, "scope_kind must be assessment|problem|answer|sample"
	}
}

// sampleUnitCount is the estimated leaf count for a scope: min(N, pool) for a
// sample scope, the full pool otherwise. Keeps the preview's unit count and the
// pre-flight budget estimate honest for calibration runs.
func sampleUnitCount(kind string, scopeID int64, poolSize int) int {
	if kind == "sample" && scopeID < int64(poolSize) {
		return int(scopeID)
	}
	return poolSize
}

// handleRunPreview is the pre-flight check / cost-estimate endpoint: leaf count
// + mask-gate blockers (D10) + launch-scoped workflow warnings (hazard audit
// 2026-07-10) + guaranteed-launch-failure RunBlockers (B9, audit 2026-07-16),
// so the launch dialog can show what will happen — and what will refuse to
// even start — before any tokens are spent.
func (s *Server) handleRunPreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	assessmentID, _ := strconv.ParseInt(q.Get("assessment_id"), 10, 64)
	scopeID, _ := strconv.ParseInt(q.Get("scope_id"), 10, 64)
	methodID, _ := strconv.ParseInt(q.Get("method_id"), 10, 64)
	kind := q.Get("scope_kind")
	if assessmentID == 0 {
		apiError(w, http.StatusBadRequest, "assessment_id is required")
		return
	}
	if kind == "assessment" {
		scopeID = assessmentID
	}
	ids, msg := s.resolveScope(r, assessmentID, kind, scopeID)
	if msg != "" {
		apiError(w, http.StatusBadRequest, msg)
		return
	}
	maskBlockers := int64(0)
	if len(ids) > 0 {
		var err error
		maskBlockers, err = s.store.Q.CountMaskBlockersForAnswers(r.Context(), ids)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "mask check failed")
			return
		}
	}
	warnings, blockers, msg, err := s.launchWarnings(r.Context(), assessmentID, kind, scopeID, methodID, ids)
	if msg != "" {
		apiError(w, http.StatusBadRequest, msg)
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "warnings check failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answers":       sampleUnitCount(kind, scopeID, len(ids)),
		"mask_blockers": maskBlockers, "warnings": warnings, "blockers": blockers,
	})
}

// RunBlocker is a machine-readable reason THIS launch is guaranteed to fail —
// as opposed to WorkflowWarning's advisory hazards, which a TA may knowingly
// launch through. The run cost-estimate endpoint (GET /api/runs/preview) runs
// the same per-problem validations Runner.Plan enforces at launch time (B9,
// audit 2026-07-16: a run used to reach the queue and die instantly with
// "method includes a reference solution but problem N has none", visible only
// after expanding the failed run row). Launch (handleCreateRun) keeps its own
// independent check — this is the estimate-time mirror, not a replacement.
type RunBlocker struct {
	Code      string `json:"code"`
	ProblemID *int64 `json:"problem_id,omitempty"`
	Message   string `json:"message"`
}

// launchWarnings computes the launch-scoped preflight warnings (workflow-guards
// plan 2026-07-10, Task B2): the assessment-wide intake hazards a run would bake
// into grades (stranded/unpromoted pages and quarantined uploads — the same
// derivations as workflowWarnings), no_rubric_problems narrowed to the problems
// the run would actually grade, active_run_overlap, and — when a method is
// chosen (method_id) — provider_disabled and missing_reference_solutions.
// answerIDs is the already-resolved run scope (resolveScope), reused for the
// method-requirement check. msg reports a client error (a method_id the launch
// dialog could never legitimately send); err a server one.
//
// The second return is B9-backend's machine-readable RunBlocker list (audit
// 2026-07-16): a strict subset of the warnings above, currently just
// missing_reference_solutions exploded to one entry per affected problem —
// the same planner check Runner.Plan enforces at launch, mirrored here so the
// estimate can report it before a token is spent.
func (s *Server) launchWarnings(ctx context.Context, assessmentID int64, kind string, scopeID, methodID int64, answerIDs []int64) ([]WorkflowWarning, []RunBlocker, string, error) {
	out := make([]WorkflowWarning, 0, 4)
	blockers := make([]RunBlocker, 0, 1)

	// Intake: answers a run grades incompletely (or skips) look identical to
	// graded ones afterwards, so warn while the launch can still wait. Stranded
	// pages are classified by cell coverage (false-alarm fix 2026-07-11, same
	// helper as the standing warnings) so a dead superseded batch never claims
	// answers grade incomplete.
	pc, err := s.store.Q.ScanPageStateCounts(ctx, assessmentID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("scan page state counts: %w", err)
	}
	strandedWs, err := s.strandedScanPageWarnings(ctx, assessmentID)
	if err != nil {
		return nil, nil, "", err
	}
	out = append(out, strandedWs...)
	if pc.AssignedUnpromoted > 0 {
		out = append(out, WorkflowWarning{Code: "assigned_unpromoted_pages", Severity: "warning", Count: pc.AssignedUnpromoted})
	}
	quarantined, err := s.store.Q.CountOpenQuarantine(ctx, assessmentID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("quarantine count: %w", err)
	}
	if quarantined > 0 {
		out = append(out, WorkflowWarning{Code: "quarantined_uploads", Severity: "warning", Count: quarantined})
	}

	// Masking: accepted pages whose masked artifact no longer matches the
	// current regions pass the mask_blockers gate while sending outdated —
	// possibly identity-revealing — images to the provider (stale-mask fix
	// 2026-07-11). Assessment-wide, like the other intake hazards.
	staleMasks, err := s.staleMaskWarning(ctx, assessmentID)
	if err != nil {
		return nil, nil, "", err
	}
	out = append(out, staleMasks...)

	// Rubrics, scoped to the run: an assessment-wide launch cares about every
	// problem, a problem/answer-scoped one only about its own.
	noRubric, err := s.scopedNoRubricCount(ctx, assessmentID, kind, scopeID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("no-rubric problems count: %w", err)
	}
	if noRubric > 0 {
		out = append(out, WorkflowWarning{Code: "no_rubric_problems", Severity: "warning", Count: noRubric})
	}
	// Rubric blockers follow Runner.Plan's semantics, which are NARROWER than
	// the count-based warning above: Plan only ever touches problems with
	// in-scope gradable answers, so only those hard-fail a launch — a
	// rubric-less problem nothing would grade warns (a TA should still know)
	// but must not block. Rubrics are required regardless of the method, so
	// this runs even with no method_id chosen.
	rubricBlockers, err := s.missingRubricBlockers(ctx, answerIDs)
	if err != nil {
		return nil, nil, "", fmt.Errorf("rubric check: %w", err)
	}
	blockers = append(blockers, rubricBlockers...)

	activeRuns, err := s.store.Q.CountActiveRunsForAssessment(ctx, assessmentID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("active runs count: %w", err)
	}
	if activeRuns > 0 {
		out = append(out, WorkflowWarning{Code: "active_run_overlap", Severity: "warning", Count: activeRuns})
	}

	if methodID != 0 {
		mv, err := s.store.Q.LatestMethodVersion(ctx, methodID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, "method has no versions", nil
		}
		if err != nil {
			return nil, nil, "no such method", nil
		}
		// An unparseable config is handleCreateRun's 400 to report — the provider
		// name is unknowable here, so there is nothing sound to warn about.
		if cfg, err := grading.ParseMethodConfig(mv.Config); err == nil {
			missing, disabled, err := s.providerState(ctx, cfg.Provider)
			if err != nil {
				return nil, nil, "", fmt.Errorf("provider check: %w", err)
			}
			switch {
			case missing:
				out = append(out, WorkflowWarning{
					Code: "provider_disabled", Severity: "danger",
					Detail: fmt.Sprintf("provider %q is not configured", cfg.Provider),
				})
			case disabled:
				out = append(out, WorkflowWarning{
					Code: "provider_disabled", Severity: "danger",
					Detail: fmt.Sprintf("provider %q is disabled", cfg.Provider),
				})
			}
			// Method-requirement preview (adversarial audit 2026-07-11): the same
			// check that 400s handleCreateRun, surfaced before submit. Danger —
			// this launch is a guaranteed refusal, not a judgment call. B9-backend
			// (2026-07-16) additionally explodes it into per-problem RunBlockers;
			// the warning's Detail reuses the first blocker's message so the two
			// signals can never drift apart.
			if cfg.RefSolutions > 0 {
				bs, err := s.missingRefSolutionBlockers(ctx, answerIDs)
				if err != nil {
					return nil, nil, "", fmt.Errorf("reference solution check: %w", err)
				}
				blockers = append(blockers, bs...)
				if len(bs) > 0 {
					out = append(out, WorkflowWarning{
						Code: "missing_reference_solutions", Severity: "danger", Detail: bs[0].Message,
					})
				}
			}
		}
	}

	return out, blockers, "", nil
}

// missingRefSolutionBlockers is missingRefSolutionMessage's estimate-time
// sibling (B9-backend, audit 2026-07-16). The launch-time check stops at the
// FIRST affected problem — one 400 is enough to refuse a launch — but the
// estimate should show a TA everything wrong at once, so this enumerates
// EVERY in-scope problem missing a reference solution.
//
// The reference-solution requirement lives in THREE places that must agree
// (same query, same message text): Runner.Plan (internal/grading/runner.go,
// the plan-time authority), missingRefSolutionMessage above (launch-time
// 400), and this blocker enumeration (estimate-time). Edit all three
// together.
func (s *Server) missingRefSolutionBlockers(ctx context.Context, answerIDs []int64) ([]RunBlocker, error) {
	answers, err := s.store.Q.AnswersWithProblems(ctx, answerIDs)
	if err != nil {
		return nil, fmt.Errorf("answers fetch: %w", err)
	}
	checked := map[int64]bool{}
	var out []RunBlocker
	for _, a := range answers {
		if checked[a.ProblemID] {
			continue
		}
		checked[a.ProblemID] = true
		if _, err := s.store.Q.LatestSolutionVersion(ctx, a.ProblemID); errors.Is(err, pgx.ErrNoRows) {
			pid := a.ProblemID
			out = append(out, RunBlocker{
				Code:      "missing_reference_solutions",
				ProblemID: &pid,
				Message:   fmt.Sprintf("method includes a reference solution but problem %d has none", a.ProblemID),
			})
		} else if err != nil {
			return nil, fmt.Errorf("solution version fetch: %w", err)
		}
	}
	return out, nil
}

// missingRubricBlockers enumerates every in-scope problem with no rubric
// version — Runner.Plan's other unconditional hard-fail ("problem N has no
// rubric — grading needs one", internal/grading/runner.go), mirrored at
// estimate time exactly like missingRefSolutionBlockers (B9-backend, audit
// 2026-07-16 review follow-up). Same query (LatestRubricVersion), same message
// text as Plan; edit both together. Plan's semantics, not the count-warning's:
// only problems that in-scope gradable answers point at can fail a launch.
func (s *Server) missingRubricBlockers(ctx context.Context, answerIDs []int64) ([]RunBlocker, error) {
	answers, err := s.store.Q.AnswersWithProblems(ctx, answerIDs)
	if err != nil {
		return nil, fmt.Errorf("answers fetch: %w", err)
	}
	checked := map[int64]bool{}
	var out []RunBlocker
	for _, a := range answers {
		if checked[a.ProblemID] {
			continue
		}
		checked[a.ProblemID] = true
		if _, err := s.store.Q.LatestRubricVersion(ctx, a.ProblemID); errors.Is(err, pgx.ErrNoRows) {
			pid := a.ProblemID
			out = append(out, RunBlocker{
				Code:      "no_rubric_problems",
				ProblemID: &pid,
				Message:   fmt.Sprintf("problem %d has no rubric — grading needs one", a.ProblemID),
			})
		} else if err != nil {
			return nil, fmt.Errorf("rubric version fetch: %w", err)
		}
	}
	return out, nil
}

// scopedNoRubricCount counts rubric-less problems among the ones a (kind,
// scopeID) run scope would grade. The caller has already validated the scope
// via resolveScope, so ownership checks aren't repeated here.
func (s *Server) scopedNoRubricCount(ctx context.Context, assessmentID int64, kind string, scopeID int64) (int64, error) {
	switch kind {
	case "problem":
		return s.store.Q.CountProblemsWithoutRubricByIDs(ctx, []int64{scopeID})
	case "answer":
		a, err := s.store.Q.GetAnswer(ctx, scopeID)
		if err != nil {
			return 0, fmt.Errorf("answer fetch: %w", err)
		}
		return s.store.Q.CountProblemsWithoutRubricByIDs(ctx, []int64{a.ProblemID})
	default: // assessment
		return s.store.Q.CountProblemsWithoutRubric(ctx, assessmentID)
	}
}

// missingRefSolutionMessage runs Runner.Plan's reference-solution requirement
// over an already-resolved run scope: the first in-scope problem with no
// solution version yields Plan's exact failure message (so the pre-launch 400
// and the plan-time dead run read identically); "" when every problem has one.
//
// One of THREE sites sharing this requirement (same query, same message):
// Runner.Plan (internal/grading/runner.go), this launch-time check, and
// missingRefSolutionBlockers (estimate-time). Edit all three together.
func (s *Server) missingRefSolutionMessage(ctx context.Context, answerIDs []int64) (string, error) {
	answers, err := s.store.Q.AnswersWithProblems(ctx, answerIDs)
	if err != nil {
		return "", fmt.Errorf("answers fetch: %w", err)
	}
	checked := map[int64]bool{}
	for _, a := range answers {
		if checked[a.ProblemID] {
			continue
		}
		checked[a.ProblemID] = true
		if _, err := s.store.Q.LatestSolutionVersion(ctx, a.ProblemID); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Sprintf("method includes a reference solution but problem %d has none", a.ProblemID), nil
		} else if err != nil {
			return "", fmt.Errorf("solution version fetch: %w", err)
		}
	}
	return "", nil
}

// providerState reports whether the named provider's llm_providers row is
// missing or disabled (an enabled row returns false, false). The two are kept
// distinct on purpose: a DISABLED row is an operator's explicit stop and hard-
// blocks handleCreateRun, while a MISSING row only warns in the preview — the
// provider source is injectable (llm.StaticSource in tests/dev harnesses runs
// methods with no llm_providers row at all), so absence isn't proof of failure.
func (s *Server) providerState(ctx context.Context, name string) (missing, disabled bool, err error) {
	p, err := s.store.Q.GetProviderByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return false, !p.Enabled, nil
}

type runJSON struct {
	ID            int64            `json:"id"`
	AssessmentID  int64            `json:"assessment_id"`
	ScopeKind     string           `json:"scope_kind"`
	ScopeID       int64            `json:"scope_id"`
	MethodVersion int64            `json:"method_version_id"`
	Status        string           `json:"status"`
	Error         *string          `json:"error,omitempty"`
	Counts        map[string]int64 `json:"counts"`
	CreatedAt     *time.Time       `json:"created_at,omitempty"`
	StartedAt     *time.Time       `json:"started_at,omitempty"`
	FinishedAt    *time.Time       `json:"finished_at,omitempty"`
	CostCapUSD    *string          `json:"cost_cap_usd,omitempty"`
	CostUSD       string           `json:"cost_usd"` // decimal string; "0" (not null) when the run has no priced records yet (trust spec §7)
	InputTokens   int64            `json:"input_tokens"`
	OutputTokens  int64            `json:"output_tokens"`
}

func (s *Server) runJSON(r *http.Request, run db.GradingRun) runJSON {
	out := runJSON{
		ID: run.ID, AssessmentID: run.AssessmentID,
		ScopeKind: run.ScopeKind, ScopeID: run.ScopeID,
		MethodVersion: run.MethodVersionID,
		Status:        run.Status,
		Counts:        map[string]int64{},
		CreatedAt:     tsPtr(run.CreatedAt), StartedAt: tsPtr(run.StartedAt), FinishedAt: tsPtr(run.FinishedAt),
		CostUSD: "0",
	}
	if run.Error.Valid {
		out.Error = &run.Error.String
	}
	if run.CostCapUsd.Valid {
		v := store.NumStr(run.CostCapUsd)
		out.CostCapUSD = &v
	}
	if counts, err := s.store.Q.RunItemStateCounts(r.Context(), run.ID); err == nil {
		for _, c := range counts {
			out.Counts[c.State] = c.N
		}
	}
	// Cost/tokens per run (trust spec §7, D40): sums the run's own grading_records,
	// so a run's "cost per run" report figure is exact even mid-flight.
	if cost, err := s.store.RunCost(r.Context(), run.ID); err == nil {
		out.CostUSD = cost.TotalUSD
		out.InputTokens = cost.InputTokens
		out.OutputTokens = cost.OutputTokens
	}
	return out
}

// handleCreateRun launches a run: run row + PlanRun job in ONE transaction (spec §6.1).
//
// Two independent budget brakes apply here (trust spec §3, D36): an optional
// per-run cost_cap_usd is stored on the row for the leaf executor to enforce, and
// — when ADAMARKER_MONTHLY_BUDGET_USD is configured — month-to-date spend plus
// this run's pre-flight estimate must not exceed it, or the launch is refused
// with a 409 carrying the numbers so the UI can show exactly why.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AssessmentID int64   `json:"assessment_id"`
		ScopeKind    string  `json:"scope_kind"`
		ScopeID      int64   `json:"scope_id"`
		MethodID     int64   `json:"method_id"`
		CostCapUSD   *string `json:"cost_cap_usd"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ScopeKind == "assessment" {
		body.ScopeID = body.AssessmentID
	}
	ids, msg := s.resolveScope(r, body.AssessmentID, body.ScopeKind, body.ScopeID)
	if msg != "" {
		apiError(w, http.StatusBadRequest, msg)
		return
	}
	mv, err := s.store.Q.LatestMethodVersion(r.Context(), body.MethodID)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusBadRequest, "method has no versions")
		return
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, "no such method")
		return
	}
	cfg, msg := s.validateMethodConfig(r, mv.Config)
	if msg != "" {
		apiError(w, http.StatusBadRequest, "method config invalid: "+msg)
		return
	}

	// Provider gate (workflow-guards B2): a DISABLED provider row is an explicit
	// operator stop, so launching against it is a guaranteed failure — refuse
	// with a 409, mirroring the mask-gate precedent. A missing row does NOT
	// block (see providerState); the preview already warns about it.
	if _, disabled, err := s.providerState(r.Context(), cfg.Provider); err != nil {
		apiError(w, http.StatusInternalServerError, "provider check failed")
		return
	} else if disabled {
		apiError(w, http.StatusConflict, "method's provider is disabled")
		return
	}

	// Method-requirement gate (adversarial audit 2026-07-11): a method that
	// includes a reference solution used to fail only at PLAN time, leaving a
	// dead run row as the TA's first hint. Same per-problem check as Runner.Plan
	// (same query, same message), hoisted here so the launch dialog gets a 400.
	if cfg.RefSolutions > 0 {
		msg, err := s.missingRefSolutionMessage(r.Context(), ids)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "reference solution check failed")
			return
		}
		if msg != "" {
			apiError(w, http.StatusBadRequest, msg)
			return
		}
	}

	var costCap pgtype.Numeric
	if body.CostCapUSD != nil && strings.TrimSpace(*body.CostCapUSD) != "" {
		costCap, err = store.Num(*body.CostCapUSD)
		zero, _ := store.Num("0")
		if err != nil || store.NumCmp(costCap, zero) < 0 {
			apiError(w, http.StatusBadRequest, "cost_cap_usd must be a non-negative decimal amount")
			return
		}
	}

	// Monthly global cap (D36): month-to-date spend + this run's pre-flight
	// estimate must not exceed ADAMARKER_MONTHLY_BUDGET_USD when configured. An
	// unconfigured budget (empty string) never blocks — behaves exactly as today.
	if strings.TrimSpace(s.cfg.MonthlyBudgetUSD) != "" {
		budget, err := store.Num(s.cfg.MonthlyBudgetUSD)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "invalid configured monthly budget")
			return
		}
		monthToDate, err := s.store.MonthToDateCost(r.Context())
		if err != nil {
			apiError(w, http.StatusInternalServerError, "month-to-date spend check failed")
			return
		}
		mtdNum, err := store.Num(monthToDate)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "month-to-date spend check failed")
			return
		}
		estimate, estKnown := s.estimateRunCost(r.Context(), cfg.Provider, cfg.Model,
			int64(sampleUnitCount(body.ScopeKind, body.ScopeID, len(ids))))
		if estKnown {
			projected := new(big.Rat).Add(store.NumRat(mtdNum), store.NumRat(estimate))
			if projected.Cmp(store.NumRat(budget)) > 0 {
				apiError409Budget(w, monthToDate, store.NumStr(estimate), s.cfg.MonthlyBudgetUSD)
				return
			}
		}
		// estKnown=false (no pricing row): the estimate is genuinely unknown, so
		// there is nothing sound to compare against the budget — per spec §3 an
		// unknown estimate is shown as "unknown", never treated as $0 that would
		// let an uncapped run through the money gate unexamined, but it also must
		// not silently block every run before any pricing is ever entered. The
		// escape hatch is the same as an exceeded budget: raise the env var, or
		// (preferred) enter pricing so the estimate becomes known.
	}

	me, _ := currentUser(r.Context())
	var run db.GradingRun
	err = s.store.WithTxPgx(r.Context(), func(tx pgx.Tx, q *db.Queries) error {
		var err error
		run, err = q.CreateRun(r.Context(), db.CreateRunParams{
			AssessmentID: body.AssessmentID, ScopeKind: body.ScopeKind, ScopeID: body.ScopeID,
			MethodVersionID: mv.ID, ExecutionMode: "sync",
			CreatedBy: int8Of(me.ID),
		})
		if err != nil {
			return err
		}
		if costCap.Valid {
			run, err = q.SetRunCostCap(r.Context(), db.SetRunCostCapParams{ID: run.ID, CostCapUsd: costCap})
			if err != nil {
				return err
			}
		}
		return s.queue.EnqueuePlanTx(r.Context(), tx, run.ID)
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "run launch failed")
		return
	}
	s.audit(r, "run.launch", "run", strconv.FormatInt(run.ID, 10), map[string]any{
		"scope_kind": body.ScopeKind, "scope_id": body.ScopeID, "method_version_id": mv.ID,
	})
	writeJSON(w, http.StatusCreated, s.runJSON(r, run))
}

// estimateRunCost resolves pricing for (provider, model) and applies the
// MODELS.md heuristic (trust spec §3): answers x (1500 in + 400 out tokens) x
// pricing. ok is false when no pricing row exists — callers must show "unknown",
// never treat a zero-valued estimate as real money.
func (s *Server) estimateRunCost(ctx context.Context, providerName, model string, answers int64) (pgtype.Numeric, bool) {
	provider, err := s.store.Q.GetProviderByName(ctx, providerName)
	if err != nil {
		return pgtype.Numeric{}, false
	}
	pricing, err := s.store.Q.GetModelPricing(ctx, db.GetModelPricingParams{ProviderID: provider.ID, Model: model})
	if err != nil {
		return pgtype.Numeric{}, false
	}
	return store.EstimateCostUSD(answers, pricing.InputUsdPerMtok, pricing.OutputUsdPerMtok)
}

// apiError409Budget writes the monthly-budget-exceeded response (trust spec §3):
// {month_to_date, estimate, budget} as decimal strings, never float64.
func apiError409Budget(w http.ResponseWriter, monthToDate, estimate, budget string) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":         "launching this run would exceed the configured monthly budget",
		"month_to_date": monthToDate,
		"estimate":      estimate,
		"budget":        budget,
	})
}

// runStatuses mirrors the grading_runs.status CHECK constraint (migration 0004).
var runStatuses = map[string]bool{
	"pending": true, "running": true, "paused": true,
	"cancelled": true, "completed": true, "failed": true,
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.ParseInt(q.Get("limit"), 10, 32)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// Optional filters (the runs-page dropdowns). Bad values are a 400, not a
	// silently empty list.
	params := db.ListRunsParams{RowLimit: int32(limit)}
	if raw := q.Get("assessment_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			apiError(w, http.StatusBadRequest, "assessment_id must be an integer")
			return
		}
		params.AssessmentID = pgtype.Int8{Int64: id, Valid: true}
	}
	if raw := q.Get("status"); raw != "" {
		if !runStatuses[raw] {
			apiError(w, http.StatusBadRequest, "status must be one of pending|running|paused|cancelled|completed|failed")
			return
		}
		params.Status = pgtype.Text{String: raw, Valid: true}
	}
	rows, err := s.store.Q.ListRuns(r.Context(), params)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	type listed struct {
		runJSON
		MethodName     string `json:"method_name"`
		MethodVer      int32  `json:"method_version"`
		AssessmentName string `json:"assessment_name"`
	}
	out := make([]listed, 0, len(rows))
	for _, row := range rows {
		rj := s.runJSON(r, db.GradingRun{
			ID: row.ID, AssessmentID: row.AssessmentID, ScopeKind: row.ScopeKind, ScopeID: row.ScopeID,
			MethodVersionID: row.MethodVersionID, Status: row.Status, Error: row.Error,
			CreatedAt: row.CreatedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt,
			CostCapUsd: row.CostCapUsd,
		})
		out = append(out, listed{runJSON: rj, MethodName: row.MethodName, MethodVer: row.MethodVersion, AssessmentName: row.AssessmentName})
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// runItemsLimit is the LIMIT high-water mark for both run-items queries (F20):
// a single run tops out around ~1800 leaves in current usage, so this cap is
// generous headroom while still bounding worst-case response size.
const runItemsLimit = 2000

type itemJSON struct {
	ID            int64   `json:"id"`
	AnswerID      int64   `json:"answer_id"`
	StudentID     string  `json:"student_id"`
	ProblemNumber int32   `json:"problem_number"`
	Model         string  `json:"model"`
	State         string  `json:"state"`
	Attempts      int32   `json:"attempts"`
	Error         *string `json:"error,omitempty"`
	RecordID      *int64  `json:"record_id,omitempty"`
}

// handleGetRun returns the run plus its per-leaf items. By default (F20) only
// "interesting" items — failed (needs retry) or running (the live edge) — are
// returned; the GROUP-BY counts in runJSON already summarize the rest. Pass
// `?all=1` for the full item list, capped at runItemsLimit with `truncated`
// echoed back when the cap was hit so the UI can say so rather than silently
// dropping rows.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	run, err := s.store.Q.GetRun(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such run")
		return
	}

	all := r.URL.Query().Get("all") == "1"
	var ij []itemJSON
	var truncated bool
	if all {
		items, err := s.store.Q.ListRunItems(r.Context(), db.ListRunItemsParams{RunID: id, ItemLimit: runItemsLimit})
		if err != nil {
			apiError(w, http.StatusInternalServerError, "items fetch failed")
			return
		}
		ij = make([]itemJSON, 0, len(items))
		for _, it := range items {
			ij = append(ij, itemJSONFrom(it.ID, it.AnswerID, it.StudentID, it.ProblemNumber, it.ModelID, it.State, it.Attempts, it.Error, it.RecordID))
		}
		truncated = len(items) >= runItemsLimit
	} else {
		items, err := s.store.Q.ListRunItemsInteresting(r.Context(), db.ListRunItemsInterestingParams{RunID: id, ItemLimit: runItemsLimit})
		if err != nil {
			apiError(w, http.StatusInternalServerError, "items fetch failed")
			return
		}
		ij = make([]itemJSON, 0, len(items))
		for _, it := range items {
			ij = append(ij, itemJSONFrom(it.ID, it.AnswerID, it.StudentID, it.ProblemNumber, it.ModelID, it.State, it.Attempts, it.Error, it.RecordID))
		}
		truncated = len(items) >= runItemsLimit
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"run": s.runJSON(r, run), "items": ij, "truncated": truncated, "all": all,
	})
}

func itemJSONFrom(id, answerID int64, studentID string, problemNumber int32, model, state string, attempts int32, errText pgtype.Text, recordID pgtype.Int8) itemJSON {
	x := itemJSON{
		ID: id, AnswerID: answerID, StudentID: studentID,
		ProblemNumber: problemNumber, Model: model,
		State: state, Attempts: attempts,
	}
	if errText.Valid {
		x.Error = &errText.String
	}
	if recordID.Valid {
		x.RecordID = &recordID.Int64
	}
	return x
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	run, err := s.store.Q.GetRun(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such run")
		return
	}
	if run.Status != "pending" && run.Status != "running" {
		apiError(w, http.StatusConflict, "run is already "+run.Status)
		return
	}
	if _, err := s.store.Q.SetRunStatus(r.Context(), db.SetRunStatusParams{ID: id, Status: "cancelled"}); err != nil {
		apiError(w, http.StatusInternalServerError, "cancel failed")
		return
	}
	s.audit(r, "run.cancel", "run", strconv.FormatInt(id, 10), nil)
	w.WriteHeader(http.StatusNoContent)
}

// errNoFailedItems distinguishes "nothing to retry" (a 400) from a real
// transaction failure inside handleRetryFailed's WithTxPgx closure.
var errNoFailedItems = errors.New("run has no failed items")
var errFinalRunSelected = errors.New("run is selected as an assessment's final source")

// handleRetryFailed re-enqueues only failed leaves (B-H4). Reset + status flip +
// leaf enqueue run in ONE transaction, the same shape as the planning path
// (runner.go Plan, spec §6.1): a failure anywhere rolls everything back, so the
// items stay 'failed' and retrying again still works — never pending items with
// no jobs and a run wedged 'running' forever.
func (s *Server) handleRetryFailed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	run, err := s.store.Q.GetRun(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such run")
		return
	}
	mv, err := s.store.Q.GetMethodVersion(r.Context(), run.MethodVersionID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "method fetch failed")
		return
	}
	cfg, err := grading.ParseMethodConfig(mv.Config)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "method config invalid")
		return
	}
	var itemIDs []int64
	err = s.store.WithTxPgx(r.Context(), func(tx pgx.Tx, q *db.Queries) error {
		// Final-source selection and retry share the same lock order. Once a
		// completed run is pinned it is immutable; unselect it before retrying.
		if _, err := q.GetAssessmentForUpdate(r.Context(), run.AssessmentID); err != nil {
			return err
		}
		lockedRun, err := q.GetRunForUpdate(r.Context(), id)
		if err != nil {
			return err
		}
		selected, err := q.IsFinalRunSelected(r.Context(), pgtype.Int8{Int64: lockedRun.ID, Valid: true})
		if err != nil {
			return err
		}
		if selected {
			return errFinalRunSelected
		}
		itemIDs, err = q.ResetFailedItems(r.Context(), id)
		if err != nil {
			return err
		}
		if len(itemIDs) == 0 {
			return errNoFailedItems
		}
		if _, err := q.SetRunStatus(r.Context(), db.SetRunStatusParams{ID: id, Status: "running"}); err != nil {
			return err
		}
		return s.queue.EnqueueLeavesTx(r.Context(), tx, cfg.Provider, itemIDs)
	})
	if errors.Is(err, errNoFailedItems) {
		apiError(w, http.StatusBadRequest, errNoFailedItems.Error())
		return
	}
	if errors.Is(err, errFinalRunSelected) {
		apiError(w, http.StatusConflict, "run is selected as the final source; choose another source before retrying")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "retry failed")
		return
	}
	s.audit(r, "run.retry_failed", "run", strconv.FormatInt(id, 10), map[string]any{"items": len(itemIDs)})
	writeJSON(w, http.StatusOK, map[string]any{"retried": len(itemIDs)})
}

// (handleAcceptOfficial removed in 0027: officials derive from the
// assessment's final source — the spot-check gate now blocks PUBLISHING with
// an AI method source instead of a per-run bulk accept.)

// spotCheckGate reports the run's spot-check gate state (trust spec §4).
type spotCheckGate struct {
	Total  int  `json:"total"`
	Done   int  `json:"done"`
	Waived bool `json:"waived"`
}

func (g spotCheckGate) open() bool {
	return g.Waived || (g.Total > 0 && g.Done == g.Total)
}

// spotCheckGateBlocked loads the gate state and reports whether AI grades from
// this run may take effect (trust spec §4). Since 0027 the gate's enforcement
// point is publishing with an AI method source (internal/httpapi/publish.go);
// blocked=true when the gate isn't open yet.
func (s *Server) spotCheckGateBlocked(r *http.Request, runID int64) (state spotCheckGate, blocked bool) {
	total, done, waived, err := s.store.SpotCheckState(r.Context(), runID)
	if err != nil {
		// Fail closed: an unreadable gate state must not silently let a bulk
		// accept through.
		return spotCheckGate{}, true
	}
	g := spotCheckGate{Total: total, Done: done, Waived: waived}
	return g, !g.open()
}

// apiError409SpotCheck writes the spot-check-pending response (trust spec §4):
// {total, done} so the UI can show "N of M spot-checked".
func apiError409SpotCheck(w http.ResponseWriter, g spotCheckGate) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": "this run's spot-check sample must be reviewed (or waived) before bulk-accepting official grades",
		"total": g.Total, "done": g.Done,
	})
}

// spotCheckJSON is one sampled record: the AI grade plus its verdict, shaped
// for the Runs detail "Spot-check" strip (image + grade side-by-side, trust
// spec §4).
type spotCheckJSON struct {
	ID              int64   `json:"id"`
	GradingRecordID int64   `json:"grading_record_id"`
	AnswerID        int64   `json:"answer_id"`
	ProblemNumber   int32   `json:"problem_number"`
	Total           *string `json:"total,omitempty"`
	Confidence      *string `json:"confidence,omitempty"`
	Verdict         *string `json:"verdict,omitempty"`
	Note            string  `json:"note"`
	CheckerID       *int64  `json:"checker_id,omitempty"`
}

func toSpotCheckJSON(row db.ListSpotChecksRow) spotCheckJSON {
	sc := spotCheckJSON{
		ID: row.ID, GradingRecordID: row.GradingRecordID, AnswerID: row.AnswerID,
		ProblemNumber: row.ProblemNumber, Note: row.Note,
	}
	if row.Total.Valid {
		s := store.NumStr(row.Total)
		sc.Total = &s
	}
	if row.Confidence.Valid {
		sc.Confidence = &row.Confidence.String
	}
	if row.Verdict.Valid {
		sc.Verdict = &row.Verdict.String
	}
	if row.CheckerID.Valid {
		sc.CheckerID = &row.CheckerID.Int64
	}
	return sc
}

// handleGetSpotCheck returns a run's sample, each record's verdict (if any), and
// the gate's overall state — everything the Runs detail "Spot-check" strip and
// the accept-official confirm dialog need (trust spec §4).
func (s *Server) handleGetSpotCheck(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	if _, err := s.store.Q.GetRun(r.Context(), id); err != nil {
		apiError(w, http.StatusNotFound, "no such run")
		return
	}
	rows, err := s.store.ListSpotChecks(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "spot-check list failed")
		return
	}
	out := make([]spotCheckJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, toSpotCheckJSON(row))
	}
	total, done, waived, err := s.store.SpotCheckState(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "spot-check state failed")
		return
	}
	agreed, agreedTotal, err := s.store.SpotCheckAgreementRate(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "spot-check agreement rate failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"samples":   out,
		"state":     spotCheckGate{Total: total, Done: done, Waived: waived},
		"agreement": map[string]any{"agreed": agreed, "total": agreedTotal},
	})
}

// handleSetSpotCheckVerdict records one checker's agree/adjusted call on a
// sampled record (trust spec §4). Per-answer manual acceptance (AnswerView)
// stays ungated — this endpoint only records the spot-check verdict itself;
// "adjusted" deep-links to the manual-grade form on the frontend, it doesn't
// perform the grade edit here.
func (s *Server) handleSetSpotCheckVerdict(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	recordID, ok := pathID(r, "recordID")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid record id")
		return
	}
	var body struct {
		Verdict string `json:"verdict"`
		Note    string `json:"note"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Verdict != "agree" && body.Verdict != "adjusted" {
		apiError(w, http.StatusBadRequest, "verdict must be agree or adjusted")
		return
	}
	sc, err := s.store.Q.GetSpotCheck(r.Context(), recordID)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such spot-check record")
		return
	}
	if sc.RunID != runID {
		apiError(w, http.StatusNotFound, "no such spot-check record")
		return
	}
	me, _ := currentUser(r.Context())
	updated, err := s.store.SetSpotCheckVerdict(r.Context(), recordID, body.Verdict, body.Note, me.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "verdict save failed")
		return
	}
	s.audit(r, "run.spotcheck.verdict", "run", strconv.FormatInt(runID, 10), map[string]any{
		"spot_check_id": recordID, "grading_record_id": updated.GradingRecordID, "verdict": body.Verdict,
	})
	writeJSON(w, http.StatusOK, map[string]any{"id": updated.ID, "verdict": body.Verdict})
}

// handleWaiveSpotCheck is the admin-only override (trust spec §4): opens the
// gate for a run without requiring a sample to be checked, with an audited
// reason (run.spotcheck.waive).
func (s *Server) handleWaiveSpotCheck(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	if _, err := s.store.Q.GetRun(r.Context(), id); err != nil {
		apiError(w, http.StatusNotFound, "no such run")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		apiError(w, http.StatusBadRequest, "reason is required")
		return
	}
	me, _ := currentUser(r.Context())
	if err := s.store.WaiveSpotCheck(r.Context(), id, me.ID, body.Reason); err != nil {
		apiError(w, http.StatusInternalServerError, "waive failed")
		return
	}
	s.audit(r, "run.spotcheck.waive", "run", strconv.FormatInt(id, 10), map[string]any{"reason": body.Reason})
	writeJSON(w, http.StatusOK, map[string]any{"waived": true})
}
