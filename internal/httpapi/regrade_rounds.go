package httpapi

// Regrade rounds (rounds design, 0027/0028): each email turn is a ROUND —
// a sparse grading pass over that turn's contested problems, using the round's
// OWN method (usually one strict model). This file is the assessment-scoped
// cockpit API the Regrade tab drives:
//
//   GET  /api/assessments/{id}/regrade-rounds            — deadline + per-round config/counts
//   PUT  /api/assessments/{id}/regrade-rounds/{turn}     — set a round's method (locked once used)
//   PUT  /api/assessments/{id}/regrade-deadline          — set/clear the reply cutoff
//   POST /api/assessments/{id}/regrade-rounds/{turn}/grade — batch-grade pending requests
//
// The inbox (the global Regrades page) stays the per-request adjudication
// surface; rounds only orchestrate the grading passes over it.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

type regradeRoundJSON struct {
	Turn       int32  `json:"turn"`
	MethodID   *int64 `json:"method_id,omitempty"`
	MethodName string `json:"method_name,omitempty"`
	// Locked: this round has graded at least one sub-item — its method is frozen
	// so already-produced overlay grades stay traceable to one config.
	Locked bool `json:"locked"`
	// Work counts for the tab: pending (filed, open, ungraded), graded (AI record
	// waiting on a verdict), adjudicated (verdict recorded).
	Pending     int64 `json:"pending"`
	Graded      int64 `json:"graded"`
	Adjudicated int64 `json:"adjudicated"`
}

// handleGetRegradeRounds is GET /api/assessments/{id}/regrade-rounds (TA+).
func (s *Server) handleGetRegradeRounds(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	ctx := r.Context()
	a, err := s.store.Q.GetAssessment(ctx, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}

	configured, err := s.store.Q.ListRegradeRoundMethods(ctx, id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "round config lookup failed")
		return
	}
	methodByTurn := map[int32]int64{}
	for _, rm := range configured {
		methodByTurn[rm.Turn] = rm.MethodID
	}
	counts, err := s.store.Q.RegradeRoundCounts(ctx, pgtype.Int8{Int64: id, Valid: true})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "round counts lookup failed")
		return
	}
	countByTurn := map[int32]db.RegradeRoundCountsRow{}
	for _, c := range counts {
		countByTurn[c.Turn.Int32] = c
	}

	maxTurns := int32(s.regradeMaxTurns())
	rounds := make([]regradeRoundJSON, 0, maxTurns)
	for turn := int32(1); turn <= maxTurns; turn++ {
		rj := regradeRoundJSON{Turn: turn}
		if mid, ok := methodByTurn[turn]; ok {
			rj.MethodID = &mid
			if m, err := s.store.Q.GetGradingMethod(ctx, mid); err == nil {
				rj.MethodName = m.Name
			}
		}
		if c, ok := countByTurn[turn]; ok {
			rj.Pending, rj.Graded, rj.Adjudicated = c.Pending, c.Graded, c.Adjudicated
		}
		if locked, err := s.store.Q.RoundHasGradedSubItems(ctx, db.RoundHasGradedSubItemsParams{
			AssessmentID: pgtype.Int8{Int64: id, Valid: true},
			Turn:         pgtype.Int4{Int32: turn, Valid: true},
		}); err == nil {
			rj.Locked = locked
		}
		rounds = append(rounds, rj)
	}

	out := map[string]any{"rounds": rounds, "regrade_max": maxTurns}
	if a.RegradeDeadline.Valid {
		out["regrade_deadline"] = a.RegradeDeadline.Time
		out["deadline_passed"] = time.Now().After(a.RegradeDeadline.Time)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSetRegradeRoundMethod is PUT /api/assessments/{id}/regrade-rounds/{turn}
// {method_id} (lecturer+). 409 once the round has graded anything.
func (s *Server) handleSetRegradeRoundMethod(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	turn, err := strconv.ParseInt(r.PathValue("turn"), 10, 32)
	if err != nil || turn < 1 || int(turn) > s.regradeMaxTurns() {
		apiError(w, http.StatusBadRequest, "turn must be between 1 and the configured regrade max")
		return
	}
	var body struct {
		MethodID int64 `json:"method_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.MethodID <= 0 {
		apiError(w, http.StatusBadRequest, "method_id is required")
		return
	}
	ctx := r.Context()
	if _, err := s.store.Q.GetAssessment(ctx, id); err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	if _, err := s.store.Q.LatestMethodVersion(ctx, body.MethodID); err != nil {
		apiError(w, http.StatusBadRequest, "no such method (or it has no versions)")
		return
	}
	locked, err := s.store.Q.RoundHasGradedSubItems(ctx, db.RoundHasGradedSubItemsParams{
		AssessmentID: pgtype.Int8{Int64: id, Valid: true},
		Turn:         pgtype.Int4{Int32: int32(turn), Valid: true},
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "round state lookup failed")
		return
	}
	if locked {
		apiError(w, http.StatusConflict, "this round already graded requests — its method is frozen")
		return
	}
	rm, err := s.store.Q.UpsertRegradeRoundMethod(ctx, db.UpsertRegradeRoundMethodParams{
		AssessmentID: id, Turn: int32(turn), MethodID: body.MethodID,
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "round method update failed")
		return
	}
	s.audit(r, "regrade.round_method", "assessment", strconv.FormatInt(id, 10), map[string]any{
		"turn": turn, "method_id": body.MethodID,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"assessment_id": rm.AssessmentID, "turn": rm.Turn, "method_id": rm.MethodID,
	})
}

// handleSetRegradeDeadline is PUT /api/assessments/{id}/regrade-deadline
// {deadline: RFC3339 | null} (lecturer+). Past-deadline replies are recorded
// but rejected by the inbound webhook (rung 3.5).
func (s *Server) handleSetRegradeDeadline(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Deadline *time.Time `json:"deadline"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, "deadline must be RFC3339 or null")
		return
	}
	var deadline pgtype.Timestamptz
	if body.Deadline != nil {
		deadline = pgtype.Timestamptz{Time: *body.Deadline, Valid: true}
	}
	a, err := s.store.Q.SetAssessmentRegradeDeadline(r.Context(), db.SetAssessmentRegradeDeadlineParams{
		ID: id, Deadline: deadline,
	})
	if err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	s.audit(r, "regrade.deadline", "assessment", strconv.FormatInt(id, 10), map[string]any{
		"deadline": body.Deadline,
	})
	writeJSON(w, http.StatusOK, toAssessmentJSON(a))
}

// handleGradeRegradeRound is POST /api/assessments/{id}/regrade-rounds/{turn}/grade
// {dry_run} (TA+): the manual batch button — enqueue the round's method over every
// pending filed sub-item of this turn. Monthly budget gate applies (D36); the
// round method must be configured first (that's what makes the batch one round).
func (s *Server) handleGradeRegradeRound(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		apiError(w, http.StatusServiceUnavailable, "queue unavailable")
		return
	}
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	turn, err := strconv.ParseInt(r.PathValue("turn"), 10, 32)
	if err != nil || turn < 1 {
		apiError(w, http.StatusBadRequest, "invalid turn")
		return
	}
	var body struct {
		DryRun bool `json:"dry_run"`
	}
	_ = decodeJSON(w, r, &body) // empty body allowed
	ctx := r.Context()

	round, err := s.store.Q.GetRegradeRoundMethod(ctx, db.GetRegradeRoundMethodParams{
		AssessmentID: id, Turn: int32(turn),
	})
	if err != nil {
		apiError(w, http.StatusConflict, "configure this round's method before grading it")
		return
	}
	mv, err := s.store.Q.LatestMethodVersion(ctx, round.MethodID)
	if err != nil {
		apiError(w, http.StatusConflict, "the round's method has no versions")
		return
	}
	cfg, msg := s.validateMethodConfig(r, mv.Config)
	if msg != "" {
		apiError(w, http.StatusConflict, "round method config invalid: "+msg)
		return
	}

	ids, err := s.store.Q.PendingRoundSubItems(ctx, db.PendingRoundSubItemsParams{
		AssessmentID: pgtype.Int8{Int64: id, Valid: true},
		Turn:         pgtype.Int4{Int32: int32(turn), Valid: true},
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "pending lookup failed")
		return
	}

	// Budget gate (D36): every pending item grades with the SAME round method.
	pricing := make([]contestedAnswerPricing, len(ids))
	for i := range pricing {
		pricing[i] = contestedAnswerPricing{
			Provider: pgtype.Text{String: cfg.Provider, Valid: true},
			ModelID:  pgtype.Text{String: cfg.Model, Valid: true},
		}
	}
	estimate, estKnown := s.estimateAIRegradeCost(ctx, pricing)
	if s.enforceAIRegradeBudget(w, ctx, estimate, estKnown) {
		return
	}
	estStr := store.NumStr(estimate)
	if !estKnown {
		estStr = "" // unknown — the UI shows "unknown", never $0 (D35)
	}

	if body.DryRun {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"enqueued": len(ids), "estimated_cost": estStr, "dry_run": true,
		})
		return
	}
	enqueued, err := s.queue.EnqueueRegradeAI(ctx, ids)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "enqueue failed")
		return
	}
	s.audit(r, "regrade.grade_round", "assessment", strconv.FormatInt(id, 10), map[string]any{
		"turn": turn, "enqueued": enqueued, "method_id": round.MethodID,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"enqueued": enqueued, "estimated_cost": estStr,
	})
}
