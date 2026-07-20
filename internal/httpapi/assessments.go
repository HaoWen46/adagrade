package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// --- JSON shapes ----------------------------------------------------------------

type assessmentJSON struct {
	ID           int64      `json:"id"`
	Kind         string     `json:"kind"`
	Name         string     `json:"name"`
	Archived     bool       `json:"archived"`
	ProblemCount *int64     `json:"problem_count,omitempty"`
	CreatedAt    *time.Time `json:"created_at,omitempty"`
	// Round-based grading (0027): the exam-wide final grading source
	// ('method'+id, 'consensus', or absent = not chosen yet) and the regrade
	// cutoff. Officials derive from the source; see store.RecomputeOfficials.
	FinalSourceKind *string    `json:"final_source_kind,omitempty"`
	FinalMethodID   *int64     `json:"final_method_id,omitempty"`
	FinalRunID      *int64     `json:"final_run_id,omitempty"`
	RegradeDeadline *time.Time `json:"regrade_deadline,omitempty"`
}

func toAssessmentJSON(a db.Assessment) assessmentJSON {
	out := assessmentJSON{
		ID: a.ID, Kind: a.Kind, Name: a.Name,
		Archived:        a.ArchivedAt.Valid,
		CreatedAt:       tsPtr(a.CreatedAt),
		RegradeDeadline: tsPtr(a.RegradeDeadline),
	}
	if a.FinalSourceKind.Valid {
		out.FinalSourceKind = &a.FinalSourceKind.String
	}
	if a.FinalMethodID.Valid {
		out.FinalMethodID = &a.FinalMethodID.Int64
	}
	if a.FinalRunID.Valid {
		out.FinalRunID = &a.FinalRunID.Int64
	}
	return out
}

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// pathID parses the {id} path segment.
func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

// --- handlers ---------------------------------------------------------------------

func (s *Server) handleListAssessments(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Q.ListAssessments(r.Context(), r.URL.Query().Get("include_archived") == "1")
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]assessmentJSON, 0, len(rows))
	for _, row := range rows {
		a := toAssessmentJSON(db.Assessment{
			ID: row.ID, Kind: row.Kind, Name: row.Name,
			ArchivedAt: row.ArchivedAt, CreatedAt: row.CreatedAt,
		})
		n := row.ProblemCount
		a.ProblemCount = &n
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"assessments": out})
}

func (s *Server) handleCreateAssessment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if (body.Kind != "exam" && body.Kind != "assignment") || body.Name == "" {
		apiError(w, http.StatusBadRequest, "kind (exam|assignment) and name are required")
		return
	}
	me, _ := currentUser(r.Context())
	a, err := s.store.Q.CreateAssessment(r.Context(), db.CreateAssessmentParams{
		Kind: body.Kind, Name: body.Name,
		CreatedBy: pgtype.Int8{Int64: me.ID, Valid: true},
	})
	if err != nil {
		apiError(w, http.StatusConflict, "an active assessment with that name already exists")
		return
	}
	s.audit(r, "assessment.create", "assessment", strconv.FormatInt(a.ID, 10), map[string]any{"name": a.Name, "kind": a.Kind})
	writeJSON(w, http.StatusCreated, toAssessmentJSON(a))
}

// handleSetFinalSource records the exam's ONE grading source (round-based
// grading, 0027) and re-derives every unpublished official from it. kind=null
// un-chooses the source, clearing unpublished officials back to holes.
func (s *Server) handleSetFinalSource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.Q.GetAssessment(r.Context(), id); err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	var body struct {
		Kind  *string `json:"kind"` // "method" | "consensus" | null
		RunID *int64  `json:"run_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	kind := ""
	runID := int64(0)
	switch {
	case body.Kind == nil:
		// un-choose
	case *body.Kind == "consensus":
		kind = "consensus"
	case *body.Kind == "method":
		if body.RunID == nil || *body.RunID <= 0 {
			apiError(w, http.StatusBadRequest, "kind=method requires run_id")
			return
		}
		kind = "method"
		runID = *body.RunID
	default:
		apiError(w, http.StatusBadRequest, "kind must be method|consensus|null")
		return
	}
	a, moved, err := s.store.SetAssessmentFinalSource(r.Context(), id, kind, runID)
	switch {
	case errors.Is(err, store.ErrFinalSourcePublished):
		apiError(w, http.StatusConflict, "assessment is published; unpublish before changing the final source")
		return
	case errors.Is(err, store.ErrFinalRunInvalid):
		apiError(w, http.StatusBadRequest, "run does not belong to this assessment")
		return
	case errors.Is(err, store.ErrFinalRunNotCompleted):
		apiError(w, http.StatusConflict, "final source run must be completed")
		return
	case errors.Is(err, store.ErrFinalRunNotAssessmentScope):
		// A3/A4 (audit 2026-07-16): 422, not 400/409 — the request shape is
		// valid and the run genuinely exists/completed, but the run's own
		// scope makes it structurally unusable as the exam-wide source
		// (RecomputeOfficials joins run_id = final_run_id only; see
		// docs/PLAN_GAPS.md for the deferred problem-scoped-overlay feature).
		apiError422(w, "final_run_scope_not_assessment",
			fmt.Sprintf("run #%d is not scoped to the whole assessment; only assessment-wide runs can be the final source", runID))
		return
	case errors.Is(err, store.ErrFinalRunNoSucceeded):
		apiError422(w, "final_run_no_succeeded",
			fmt.Sprintf("run #%d graded nothing — pick a run that produced grades", runID))
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "final source update failed")
		return
	}
	s.audit(r, "assessment.final_source", "assessment", strconv.FormatInt(id, 10), map[string]any{
		"kind": body.Kind, "run_id": body.RunID, "officials_moved": moved,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"assessment":      toAssessmentJSON(a),
		"officials_moved": moved,
	})
}

func (s *Server) handleGetAssessment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	a, err := s.store.Q.GetAssessment(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	problems, err := s.store.Q.ListProblems(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list problems failed")
		return
	}
	pj := make([]problemJSON, 0, len(problems))
	for _, p := range problems {
		pj = append(pj, toProblemJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"assessment": toAssessmentJSON(a), "problems": pj})
}

func (s *Server) handleRenameAssessment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Name == "" {
		apiError(w, http.StatusBadRequest, "name is required")
		return
	}
	a, err := s.store.Q.RenameAssessment(r.Context(), db.RenameAssessmentParams{ID: id, Name: body.Name})
	if err != nil {
		apiError(w, http.StatusConflict, "rename failed (duplicate name?)")
		return
	}
	s.audit(r, "assessment.rename", "assessment", strconv.FormatInt(id, 10), map[string]any{"name": body.Name})
	writeJSON(w, http.StatusOK, toAssessmentJSON(a))
}

func (s *Server) handleArchiveAssessment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Archived bool `json:"archived"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	a, err := s.store.Q.SetAssessmentArchived(r.Context(), db.SetAssessmentArchivedParams{ID: id, Archived: body.Archived})
	if err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	s.audit(r, "assessment.archive", "assessment", strconv.FormatInt(id, 10), map[string]any{"archived": body.Archived})
	writeJSON(w, http.StatusOK, toAssessmentJSON(a))
}

// handleDeleteAssessment enforces the plan §10 guardrails: hard delete is blocked
// when submissions or grading records exist unless an admin forces it, and always
// requires typing the assessment name.
func (s *Server) handleDeleteAssessment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		ConfirmName string `json:"confirm_name"`
		Force       bool   `json:"force"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	a, err := s.store.Q.GetAssessment(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	if body.ConfirmName != a.Name {
		apiError(w, http.StatusBadRequest, "confirm_name does not match the assessment name")
		return
	}
	subs, err1 := s.store.Q.CountAssessmentSubmissions(r.Context(), id)
	recs, err2 := s.store.Q.CountAssessmentRecords(r.Context(), id)
	if err1 != nil || err2 != nil {
		apiError(w, http.StatusInternalServerError, "guardrail check failed")
		return
	}
	if (subs > 0 || recs > 0) && !body.Force {
		apiError(w, http.StatusConflict, "assessment has submissions or grading records; deletion requires force=true")
		return
	}
	if err := s.store.Q.DeleteAssessment(r.Context(), id); err != nil {
		apiError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	s.audit(r, "assessment.delete", "assessment", strconv.FormatInt(id, 10), map[string]any{
		"name": a.Name, "forced": body.Force, "submissions": subs, "records": recs,
	})
	w.WriteHeader(http.StatusNoContent)
}

// numFromBody parses a required decimal-string field.
func numFromBody(w http.ResponseWriter, field, val string) (pgtype.Numeric, bool) {
	n, err := store.Num(val)
	if err != nil {
		apiError(w, http.StatusBadRequest, field+" must be a decimal string like \"10\" or \"2.5\"")
		return pgtype.Numeric{}, false
	}
	return n, true
}
