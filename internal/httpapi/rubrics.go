package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

type criterionJSON struct {
	ID                 int64  `json:"id"`
	Position           int32  `json:"position"`
	Description        string `json:"description"`
	Points             string `json:"points"`
	PartialCreditNotes string `json:"partial_credit_notes"`
}

type rubricVersionJSON struct {
	ID             int64           `json:"id"`
	Version        int32           `json:"version"`
	Notes          string          `json:"notes"`
	ScoreIncrement string          `json:"score_increment"`
	CreatedAt      *time.Time      `json:"created_at,omitempty"`
	Criteria       []criterionJSON `json:"criteria,omitempty"`
}

func (s *Server) rubricVersionJSON(r *http.Request, v db.RubricVersion, withCriteria bool) (rubricVersionJSON, error) {
	out := rubricVersionJSON{
		ID: v.ID, Version: v.Version, Notes: v.Notes,
		ScoreIncrement: store.NumStr(v.ScoreIncrement),
		CreatedAt:      tsPtr(v.CreatedAt),
	}
	if withCriteria {
		crits, err := s.store.Q.ListRubricCriteria(r.Context(), v.ID)
		if err != nil {
			return out, err
		}
		for _, c := range crits {
			out.Criteria = append(out.Criteria, criterionJSON{
				ID: c.ID, Position: c.Position, Description: c.Description,
				Points: store.NumStr(c.Points), PartialCreditNotes: c.PartialCreditNotes,
			})
		}
	}
	return out, nil
}

func (s *Server) handleGetRubric(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid problem id")
		return
	}
	versions, err := s.store.Q.ListRubricVersions(r.Context(), pid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	resp := map[string]any{"current": nil, "versions": []rubricVersionJSON{}}
	metas := make([]rubricVersionJSON, 0, len(versions))
	for i, v := range versions {
		vj, err := s.rubricVersionJSON(r, v, i == 0) // newest carries criteria
		if err != nil {
			apiError(w, http.StatusInternalServerError, "criteria fetch failed")
			return
		}
		if i == 0 {
			resp["current"] = vj
		}
		metas = append(metas, rubricVersionJSON{ID: v.ID, Version: v.Version, Notes: v.Notes,
			ScoreIncrement: store.NumStr(v.ScoreIncrement), CreatedAt: tsPtr(v.CreatedAt)})
	}
	resp["versions"] = metas
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetRubricVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid rubric version id")
		return
	}
	v, err := s.store.Q.GetRubricVersion(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such rubric version")
		return
	}
	vj, err := s.rubricVersionJSON(r, v, true)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "criteria fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, vj)
}

// handleCreateRubricVersion inserts version MAX+1 with its criteria, enforcing the
// Σ(criterion points) == problem.max_points invariant (D4) transactionally.
func (s *Server) handleCreateRubricVersion(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid problem id")
		return
	}
	problem, err := s.store.Q.GetProblem(r.Context(), pid)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such problem")
		return
	}

	var body struct {
		Notes          string `json:"notes"`
		ScoreIncrement string `json:"score_increment"`
		Criteria       []struct {
			Description        string `json:"description"`
			Points             string `json:"points"`
			PartialCreditNotes string `json:"partial_credit_notes"`
		} `json:"criteria"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Criteria) == 0 {
		apiError(w, http.StatusBadRequest, "at least one criterion is required")
		return
	}
	if body.ScoreIncrement == "" {
		body.ScoreIncrement = "0.5"
	}
	increment, ok := numFromBody(w, "score_increment", body.ScoreIncrement)
	if !ok {
		return
	}

	points := make([]pgtype.Numeric, 0, len(body.Criteria))
	for i, c := range body.Criteria {
		if c.Description == "" {
			apiError(w, http.StatusBadRequest, "criterion "+strconv.Itoa(i+1)+": description is required")
			return
		}
		n, err := store.Num(c.Points)
		if err != nil {
			apiError(w, http.StatusBadRequest, "criterion "+strconv.Itoa(i+1)+": points must be a decimal string")
			return
		}
		points = append(points, n)
	}
	if !store.NumSumEqual(points, problem.MaxPoints) {
		apiError(w, http.StatusBadRequest,
			"criterion points must sum to the problem max ("+store.NumStr(problem.MaxPoints)+")")
		return
	}

	me, _ := currentUser(r.Context())
	var created db.RubricVersion
	err = s.store.WithTx(r.Context(), func(q *db.Queries) error {
		v, err := q.CreateRubricVersion(r.Context(), db.CreateRubricVersionParams{
			ProblemID: pid, Notes: body.Notes, ScoreIncrement: increment,
			CreatedBy: pgtype.Int8{Int64: me.ID, Valid: true},
		})
		if err != nil {
			return err
		}
		for i, c := range body.Criteria {
			if _, err := q.CreateRubricCriterion(r.Context(), db.CreateRubricCriterionParams{
				RubricVersionID: v.ID, Position: int32(i),
				Description: c.Description, Points: points[i],
				PartialCreditNotes: c.PartialCreditNotes,
			}); err != nil {
				return err
			}
		}
		created = v
		return nil
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "rubric version create failed")
		return
	}
	s.audit(r, "rubric.version", "problem", strconv.FormatInt(pid, 10), map[string]any{"version": created.Version})
	// A new rubric version reopens holes (0027 freshness rule: only
	// current-rubric records derive officials) — re-derive so stale grades
	// surface as unresolved instead of silently publishing.
	if _, err := s.store.RecomputeOfficials(r.Context(), problem.AssessmentID); err != nil {
		s.log.Error("officials recompute failed", "assessment_id", problem.AssessmentID, "err", err)
	}
	vj, err := s.rubricVersionJSON(r, created, true)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "criteria fetch failed")
		return
	}
	writeJSON(w, http.StatusCreated, vj)
}

// --- reference solutions -----------------------------------------------------------

type solutionVersionJSON struct {
	ID        int64      `json:"id"`
	Version   int32      `json:"version"`
	Content   string     `json:"content,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

func (s *Server) handleGetSolutions(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid problem id")
		return
	}
	versions, err := s.store.Q.ListSolutionVersions(r.Context(), pid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	resp := map[string]any{"current": nil, "versions": []solutionVersionJSON{}}
	metas := make([]solutionVersionJSON, 0, len(versions))
	for i, v := range versions {
		if i == 0 {
			resp["current"] = solutionVersionJSON{ID: v.ID, Version: v.Version, Content: v.Content, CreatedAt: tsPtr(v.CreatedAt)}
		}
		metas = append(metas, solutionVersionJSON{ID: v.ID, Version: v.Version, CreatedAt: tsPtr(v.CreatedAt)})
	}
	resp["versions"] = metas
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetSolutionVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid solution version id")
		return
	}
	v, err := s.store.Q.GetSolutionVersion(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such solution version")
		return
	}
	writeJSON(w, http.StatusOK, solutionVersionJSON{ID: v.ID, Version: v.Version, Content: v.Content, CreatedAt: tsPtr(v.CreatedAt)})
}

func (s *Server) handleCreateSolutionVersion(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid problem id")
		return
	}
	if _, err := s.store.Q.GetProblem(r.Context(), pid); err != nil {
		apiError(w, http.StatusNotFound, "no such problem")
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Content == "" {
		apiError(w, http.StatusBadRequest, "content is required")
		return
	}
	me, _ := currentUser(r.Context())
	v, err := s.store.Q.CreateSolutionVersion(r.Context(), db.CreateSolutionVersionParams{
		ProblemID: pid, Content: body.Content,
		CreatedBy: pgtype.Int8{Int64: me.ID, Valid: true},
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "create failed")
		return
	}
	s.audit(r, "solution.version", "problem", strconv.FormatInt(pid, 10), map[string]any{"version": v.Version})
	writeJSON(w, http.StatusCreated, solutionVersionJSON{ID: v.ID, Version: v.Version, Content: v.Content, CreatedAt: tsPtr(v.CreatedAt)})
}
