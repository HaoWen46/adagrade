package httpapi

import (
	"net/http"
	"strconv"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

type problemJSON struct {
	ID           int64  `json:"id"`
	AssessmentID int64  `json:"assessment_id"`
	Number       int32  `json:"number"`
	Title        string `json:"title"`
	Statement    string `json:"statement"`
	MaxPoints    string `json:"max_points"`
	Position     int32  `json:"position"`
}

func toProblemJSON(p db.Problem) problemJSON {
	return problemJSON{
		ID: p.ID, AssessmentID: p.AssessmentID, Number: p.Number,
		Title: p.Title, Statement: p.Statement,
		MaxPoints: store.NumStr(p.MaxPoints), Position: p.Position,
	}
}

type problemBody struct {
	Number    *int32  `json:"number"`
	Title     *string `json:"title"`
	Statement *string `json:"statement"`
	MaxPoints *string `json:"max_points"`
	Position  *int32  `json:"position"`
}

func (s *Server) handleCreateProblem(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	var body problemBody
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Number == nil || body.MaxPoints == nil {
		apiError(w, http.StatusBadRequest, "number and max_points are required")
		return
	}
	maxPts, ok := numFromBody(w, "max_points", *body.MaxPoints)
	if !ok {
		return
	}
	params := db.CreateProblemParams{
		AssessmentID: aid,
		Number:       *body.Number,
		MaxPoints:    maxPts,
		Position:     *body.Number, // default: page order follows problem number
	}
	if body.Title != nil {
		params.Title = *body.Title
	}
	if body.Statement != nil {
		params.Statement = *body.Statement
	}
	if body.Position != nil {
		params.Position = *body.Position
	}
	p, err := s.store.Q.CreateProblem(r.Context(), params)
	if err != nil {
		apiError(w, http.StatusConflict, "problem number already exists in this assessment")
		return
	}
	s.audit(r, "problem.create", "problem", strconv.FormatInt(p.ID, 10), map[string]any{"number": p.Number})
	writeJSON(w, http.StatusCreated, toProblemJSON(p))
}

func (s *Server) handleUpdateProblem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid problem id")
		return
	}
	existing, err := s.store.Q.GetProblem(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such problem")
		return
	}
	var body problemBody
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	params := db.UpdateProblemParams{
		ID:        id,
		Number:    existing.Number,
		Title:     existing.Title,
		Statement: existing.Statement,
		MaxPoints: existing.MaxPoints,
		Position:  existing.Position,
	}
	if body.Number != nil {
		params.Number = *body.Number
	}
	if body.Title != nil {
		params.Title = *body.Title
	}
	if body.Statement != nil {
		params.Statement = *body.Statement
	}
	if body.Position != nil {
		params.Position = *body.Position
	}
	if body.MaxPoints != nil {
		n, ok := numFromBody(w, "max_points", *body.MaxPoints)
		if !ok {
			return
		}
		// Changing max_points after a rubric exists would break the Σ==max invariant
		// silently; require a new rubric version instead (D4).
		if _, err := s.store.Q.LatestRubricVersion(r.Context(), id); err == nil && store.NumCmp(n, existing.MaxPoints) != 0 {
			apiError(w, http.StatusConflict, "cannot change max_points while a rubric exists; create a new rubric version first")
			return
		}
		params.MaxPoints = n
	}
	p, err := s.store.Q.UpdateProblem(r.Context(), params)
	if err != nil {
		apiError(w, http.StatusConflict, "update failed (duplicate number?)")
		return
	}
	s.audit(r, "problem.update", "problem", strconv.FormatInt(id, 10), nil)
	writeJSON(w, http.StatusOK, toProblemJSON(p))
}

func (s *Server) handleDeleteProblem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid problem id")
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	pages, err1 := s.store.Q.CountProblemPages(r.Context(), id)
	recs, err2 := s.store.Q.CountProblemRecords(r.Context(), id)
	if err1 != nil || err2 != nil {
		apiError(w, http.StatusInternalServerError, "guardrail check failed")
		return
	}
	if (pages > 0 || recs > 0) && !body.Force {
		apiError(w, http.StatusConflict, "problem has ingested pages or grading records; deletion requires force=true")
		return
	}
	if err := s.store.Q.DeleteProblem(r.Context(), id); err != nil {
		apiError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	s.audit(r, "problem.delete", "problem", strconv.FormatInt(id, 10), map[string]any{"forced": body.Force})
	w.WriteHeader(http.StatusNoContent)
}
