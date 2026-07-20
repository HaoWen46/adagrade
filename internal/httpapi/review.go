package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// --- drill-down reads (D2: status derived in SQL, rollups are count summaries) -----

func (s *Server) handleProblemSummaries(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	rows, err := s.store.Q.ProblemSummaries(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "summary failed")
		return
	}
	type summary struct {
		ProblemID   int64  `json:"problem_id"`
		Number      int32  `json:"number"`
		Title       string `json:"title"`
		MaxPoints   string `json:"max_points"`
		Answers     int64  `json:"answers"`
		WithPages   int64  `json:"with_pages"`
		OfficialSet int64  `json:"official_set"`
		AIGraded    int64  `json:"ai_graded"`
		HumanGraded int64  `json:"human_graded"`
		Flagged     int64  `json:"flagged"`
		Published   int64  `json:"published"`
	}
	out := make([]summary, 0, len(rows))
	for _, p := range rows {
		out = append(out, summary{
			ProblemID: p.ID, Number: p.Number, Title: p.Title,
			MaxPoints: store.NumStr(p.MaxPoints),
			Answers:   p.AnswerCount, WithPages: p.WithPages,
			OfficialSet: p.OfficialSet, AIGraded: p.AiGraded, HumanGraded: p.HumanGraded,
			Flagged: p.Flagged, Published: p.Published,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"problems": out})
}

func (s *Server) handleProblemStudents(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid problem id")
		return
	}
	rows, err := s.store.Q.ProblemStudentRows(r.Context(), pid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	type row struct {
		AnswerID       int64      `json:"answer_id"`
		StudentID      string     `json:"student_id"`
		Name           string     `json:"name"`
		Email          string     `json:"email"`
		Flags          []string   `json:"flags"`
		PageCount      int64      `json:"page_count"`
		RecordCount    int64      `json:"record_count"`
		OfficialTotal  *string    `json:"official_total,omitempty"`
		OfficialSource *string    `json:"official_source,omitempty"`
		PublishedAt    *time.Time `json:"published_at,omitempty"`
		Status         string     `json:"status"`
	}
	out := make([]row, 0, len(rows))
	for _, a := range rows {
		x := row{
			AnswerID: a.AnswerID, StudentID: a.StudentID, Name: a.Name, Email: a.Email,
			Flags: a.Flags, PageCount: a.PageCount, RecordCount: a.RecordCount,
			PublishedAt: tsPtr(a.PublishedAt),
		}
		if a.OfficialSource.Valid {
			t := store.NumStr(a.OfficialTotal)
			x.OfficialTotal = &t
			x.OfficialSource = &a.OfficialSource.String
		}
		// Derived display status (D2).
		switch {
		case a.PublishedAt.Valid:
			x.Status = "published"
		case a.OfficialRecordID.Valid:
			x.Status = "official_set"
		case a.RecordCount > 0:
			x.Status = "graded"
		case a.PageCount > 0:
			x.Status = "ungraded"
		default:
			x.Status = "no_submission"
		}
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, map[string]any{"students": out})
}

type recordJSON struct {
	ID                      int64           `json:"id"`
	Source                  string          `json:"source"`
	RunID                   *int64          `json:"run_id,omitempty"`
	Provider                *string         `json:"provider,omitempty"`
	ModelID                 *string         `json:"model_id,omitempty"`
	MethodVersionID         *int64          `json:"method_version_id,omitempty"`
	MethodVersion           *int32          `json:"method_version,omitempty"` // human version integer behind method_version_id
	PromptTemplateVersionID *int64          `json:"prompt_template_version_id,omitempty"`
	PromptVersion           *int32          `json:"prompt_version,omitempty"` // human version integer behind prompt_template_version_id
	Policy                  *string         `json:"policy,omitempty"`
	RubricVersionID         int64           `json:"rubric_version_id"`
	CriterionScores         json.RawMessage `json:"criterion_scores"`
	Total                   *string         `json:"total"`
	Comment                 string          `json:"comment"`
	Transcription           *string         `json:"transcription,omitempty"`
	Confidence              *string         `json:"confidence,omitempty"`
	Adjustments             json.RawMessage `json:"adjustments"`
	GradedImageShas         []string        `json:"graded_image_shas"`
	CreatedBy               *int64          `json:"created_by,omitempty"`
	CreatedAt               *time.Time      `json:"created_at,omitempty"`
	InputTokens             *int64          `json:"input_tokens,omitempty"`
	OutputTokens            *int64          `json:"output_tokens,omitempty"`
	CostUSD                 *string         `json:"cost_usd,omitempty"` // decimal string, NULL means no pricing row at insert time (D35)
}

func toRecordJSON(rec db.GradingRecord) recordJSON {
	out := recordJSON{
		ID: rec.ID, Source: rec.Source,
		RubricVersionID: rec.RubricVersionID,
		CriterionScores: json.RawMessage(rec.CriterionScores),
		Comment:         rec.Comment,
		Adjustments:     json.RawMessage(rec.Adjustments),
		GradedImageShas: rec.GradedImageShas,
		CreatedAt:       tsPtr(rec.CreatedAt),
	}
	if rec.RunID.Valid {
		out.RunID = &rec.RunID.Int64
	}
	if rec.Provider.Valid {
		out.Provider = &rec.Provider.String
	}
	if rec.ModelID.Valid {
		out.ModelID = &rec.ModelID.String
	}
	if rec.MethodVersionID.Valid {
		out.MethodVersionID = &rec.MethodVersionID.Int64
	}
	if rec.PromptTemplateVersionID.Valid {
		out.PromptTemplateVersionID = &rec.PromptTemplateVersionID.Int64
	}
	if rec.Policy.Valid {
		out.Policy = &rec.Policy.String
	}
	if rec.Total.Valid {
		t := store.NumStr(rec.Total)
		out.Total = &t
	}
	if rec.Transcription.Valid {
		out.Transcription = &rec.Transcription.String
	}
	if rec.Confidence.Valid {
		out.Confidence = &rec.Confidence.String
	}
	if rec.CreatedBy.Valid {
		out.CreatedBy = &rec.CreatedBy.Int64
	}
	if rec.InputTokens.Valid {
		v := int64(rec.InputTokens.Int32)
		out.InputTokens = &v
	}
	if rec.OutputTokens.Valid {
		v := int64(rec.OutputTokens.Int32)
		out.OutputTokens = &v
	}
	if rec.CostUsd.Valid {
		c := store.NumStr(rec.CostUsd)
		out.CostUSD = &c
	}
	return out
}

func (s *Server) handleGetAnswer(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid answer id")
		return
	}
	ctxRow, err := s.store.Q.GetAnswerContext(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such answer")
		return
	}
	pages, err := s.store.Q.ListAnswerPages(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "pages fetch failed")
		return
	}
	records, err := s.store.Q.ListRecordsForAnswer(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "records fetch failed")
		return
	}

	type pageJSON struct {
		ID           int64  `json:"id"`
		PageIndex    int32  `json:"page_index"`
		SubmissionID int64  `json:"submission_id"`
		PdfPageIndex int32  `json:"pdf_page_index"`
		Width        int32  `json:"width"`
		Height       int32  `json:"height"`
		Masked       bool   `json:"masked"`
		MaskReview   string `json:"mask_review"`
	}
	pj := make([]pageJSON, 0, len(pages))
	for _, p := range pages {
		pj = append(pj, pageJSON{
			ID: p.ID, PageIndex: p.PageIndex, SubmissionID: p.SubmissionID,
			PdfPageIndex: p.PdfPageIndex, Width: p.ImageWidth, Height: p.ImageHeight,
			Masked: p.MaskedImageRef.Valid, MaskReview: p.MaskReviewStatus,
		})
	}
	// Resolve the version-id FKs to their human version integers so the UI can
	// say "method v2 · prompt v3" instead of raw DB ids. Memoized per request —
	// one answer's records typically share a single method/prompt version.
	methodVer := map[int64]int32{}
	promptVer := map[int64]int32{}
	rj := make([]recordJSON, 0, len(records))
	for _, rec := range records {
		out := toRecordJSON(rec)
		if rec.MethodVersionID.Valid {
			id := rec.MethodVersionID.Int64
			if _, ok := methodVer[id]; !ok {
				mv, err := s.store.Q.GetMethodVersion(r.Context(), id)
				if err != nil {
					apiError(w, http.StatusInternalServerError, "version lookup failed")
					return
				}
				methodVer[id] = mv.Version
			}
			v := methodVer[id]
			out.MethodVersion = &v
		}
		if rec.PromptTemplateVersionID.Valid {
			id := rec.PromptTemplateVersionID.Int64
			if _, ok := promptVer[id]; !ok {
				tv, err := s.store.Q.GetPromptTemplateVersion(r.Context(), id)
				if err != nil {
					apiError(w, http.StatusInternalServerError, "version lookup failed")
					return
				}
				promptVer[id] = tv.Version
			}
			v := promptVer[id]
			out.PromptVersion = &v
		}
		rj = append(rj, out)
	}

	resp := map[string]any{
		"answer": map[string]any{
			"id":            ctxRow.ID,
			"assessment_id": ctxRow.AssessmentID,
			"problem_id":    ctxRow.ProblemID,
			"flags":         ctxRow.Flags,
			"published_at":  tsPtr(ctxRow.PublishedAt),
			"official_record_id": func() *int64 {
				if ctxRow.OfficialRecordID.Valid {
					return &ctxRow.OfficialRecordID.Int64
				}
				return nil
			}(),
		},
		"student": map[string]any{
			"student_id": ctxRow.StudentExternalID,
			"name":       ctxRow.StudentName,
			"email":      ctxRow.StudentEmail,
		},
		"problem": map[string]any{
			"id":         ctxRow.ProblemID,
			"number":     ctxRow.ProblemNumber,
			"title":      ctxRow.ProblemTitle,
			"max_points": store.NumStr(ctxRow.MaxPoints),
			"statement":  ctxRow.Statement,
		},
		"assessment_name": ctxRow.AssessmentName,
		"pages":           pj,
		"records":         rj,
	}

	// Regrade layers (rounds design, 0028): the adjudicated overlays stacked on
	// round 0 — the answer page renders them as a layer pager. Oldest turn first.
	layers, err := s.store.Q.RegradeLayersForAnswer(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "regrade layers lookup failed")
		return
	}
	lj := make([]map[string]any, 0, len(layers))
	for _, l := range layers {
		entry := map[string]any{
			"turn":        l.Turn.Int32,
			"request_id":  l.RequestID,
			"sub_item_id": l.SubItemID,
			"verdict":     l.Verdict.String,
			"note":        l.VerdictNote,
			"verdict_at":  tsPtr(l.VerdictAt),
		}
		if l.AdoptedRecordID.Valid {
			entry["adopted_record_id"] = l.AdoptedRecordID.Int64
		}
		if l.AdoptedTotal.Valid {
			entry["adopted_total"] = store.NumStr(l.AdoptedTotal)
		}
		lj = append(lj, entry)
	}
	resp["regrade_layers"] = lj
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAssessmentTotals(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	rows, err := s.store.Q.AssessmentStudentTotals(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "totals failed")
		return
	}
	type row struct {
		StudentID string  `json:"student_id"`
		Name      string  `json:"name"`
		Answers   int64   `json:"answers"`
		Graded    int64   `json:"graded"`
		Total     *string `json:"total,omitempty"` // NULL until anything official (D3)
		// Withdrawn students stay visible with an explicit marker — never silently
		// dropped (roster-lifecycle plan 2026-07-10, locked semantics e).
		Withdrawn bool `json:"withdrawn"`
	}
	out := make([]row, 0, len(rows))
	for _, t := range rows {
		x := row{StudentID: t.StudentID, Name: t.Name, Answers: t.Answers, Graded: t.Graded, Withdrawn: t.Withdrawn}
		if t.Total.Valid {
			v := store.NumStr(t.Total)
			x.Total = &v
		}
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, map[string]any{"students": out})
}

// handleStudentSubmission is the student-centric view: one student's uploaded
// submission + every answer (with pages) for an assessment, regardless of grading
// state — browsing is never gated on grades (plan §5: full browsing).
func (s *Server) handleStudentSubmission(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	externalID := r.PathValue("sid")
	student, err := s.store.Q.GetStudentByExternalID(r.Context(), externalID)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such student on the roster")
		return
	}

	resp := map[string]any{
		"student":    map[string]any{"student_id": student.StudentID, "name": student.Name, "email": student.Email},
		"submission": nil,
	}
	if sub, err := s.store.Q.GetActiveSubmission(r.Context(), db.GetActiveSubmissionParams{
		AssessmentID: aid, StudentID: student.ID,
	}); err == nil {
		resp["submission"] = map[string]any{
			"id": sub.ID, "filename": sub.OriginalFilename,
			"page_count": sub.PageCount, "uploaded_at": tsPtr(sub.UploadedAt),
		}
	}

	rows, err := s.store.Q.StudentSubmissionRows(r.Context(), db.StudentSubmissionRowsParams{
		AssessmentID: aid, StudentID: externalID,
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "answers fetch failed")
		return
	}
	type page struct {
		PageID    int64 `json:"page_id"`
		PageIndex int32 `json:"page_index"`
		Masked    bool  `json:"masked"`
	}
	type answer struct {
		AnswerID      int64  `json:"answer_id"`
		ProblemNumber int32  `json:"problem_number"`
		ProblemTitle  string `json:"problem_title"`
		RecordCount   int64  `json:"record_count"`
		HasOfficial   bool   `json:"has_official"`
		Pages         []page `json:"pages"`
	}
	var answers []answer
	for _, row := range rows {
		if len(answers) == 0 || answers[len(answers)-1].AnswerID != row.AnswerID {
			answers = append(answers, answer{
				AnswerID: row.AnswerID, ProblemNumber: row.ProblemNumber, ProblemTitle: row.ProblemTitle,
				RecordCount: row.RecordCount, HasOfficial: row.OfficialRecordID.Valid,
				Pages: []page{},
			})
		}
		if row.PageID.Valid {
			answers[len(answers)-1].Pages = append(answers[len(answers)-1].Pages, page{
				PageID: row.PageID.Int64, PageIndex: row.PageIndex.Int32, Masked: row.Masked,
			})
		}
	}
	if answers == nil {
		answers = []answer{}
	}
	resp["answers"] = answers
	writeJSON(w, http.StatusOK, resp)
}

// --- manual grading + official pointer (D4/D6) --------------------------------------

// handleCreateManualRecord files a human grade. Under round-based grading
// (0027) a human record is a FALLBACK, never a choice: the post-insert
// recompute makes it official exactly when the assessment's final source left
// this answer undecided, and ignores it otherwise. There is no set_official
// path anymore — nothing user-driven writes the official pointer directly.
func (s *Server) handleCreateManualRecord(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid answer id")
		return
	}
	var body struct {
		RubricVersionID int64                    `json:"rubric_version_id"`
		Comment         string                   `json:"comment"`
		Scores          []grading.CriterionScore `json:"scores"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	me, _ := currentUser(r.Context())
	rec, err := grading.InsertManualRecord(r.Context(), s.store, grading.ManualGradeInput{
		AnswerID:        aid,
		RubricVersionID: body.RubricVersionID,
		Comment:         body.Comment,
		Scores:          body.Scores,
		CreatedBy:       me.ID,
	})
	var verr grading.ValidationError
	if errors.As(err, &verr) {
		apiError(w, http.StatusBadRequest, verr.Msg)
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "record insert failed")
		return
	}
	s.audit(r, "record.manual", "answer", strconv.FormatInt(aid, 10), map[string]any{"record_id": rec.ID})

	if ans, err := s.store.Q.GetAnswer(r.Context(), aid); err == nil {
		if _, err := s.store.RecomputeOfficials(r.Context(), ans.AssessmentID); err != nil {
			s.log.Error("officials recompute failed", "assessment_id", ans.AssessmentID, "err", err)
		}
	}
	writeJSON(w, http.StatusCreated, toRecordJSON(rec))
}

func int8OfPtr(p *int64) (out pgtype.Int8) {
	if p != nil {
		out.Int64 = *p
		out.Valid = true
	}
	return out
}
