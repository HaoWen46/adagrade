package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// directUploadExt maps a filename's extension to the ingest decode kind ("pdf" |
// "image"), mirroring scan.acceptedExt's accepted-format set. Unknown extensions
// fall back to "pdf" — StageDirectUpload's own roster-match/decode gates reject
// anything that doesn't actually parse, exactly as the previous synchronous
// PDF-only path did for a non-PDF file.
func directUploadExt(filename string) string {
	i := strings.LastIndexByte(filename, '.')
	if i < 0 || i == len(filename)-1 {
		return "pdf"
	}
	switch strings.ToLower(filename[i+1:]) {
	case "png", "jpg", "jpeg":
		return "image"
	default:
		return "pdf"
	}
}

// directUploadResultJSON is one file's outcome from handleUploadSubmissions (D27,
// F1): staging is now synchronous but ingest runs off-request, so the response
// reports "queued" (an ingest job was enqueued) or "rejected" (the sync gate — empty
// or too-large — rejected it with no row).
type directUploadResultJSON struct {
	Filename string `json:"filename"`
	UploadID int64  `json:"upload_id,omitempty"`
	Status   string `json:"status"` // "queued" | "rejected"
	Reason   string `json:"reason,omitempty"`
}

// handleUploadSubmissions accepts multipart files (field "files", filename
// "<student_id>.<ext>") and stages each one for off-request ingest (D27, F1): at
// 200×9 scale, rendering every page synchronously in one HTTP request cannot
// complete. force=1 permits replacing graded submissions (D1), threaded through to
// the ingest worker.
func (s *Server) handleUploadSubmissions(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	if _, err := s.store.Q.GetAssessment(r.Context(), aid); err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	// F5: extend the per-request body deadline before reading a potentially large
	// multipart body of submission files; the default 30 s read deadline would kill
	// a healthy bulk upload mid-body.
	_ = extendBodyDeadline(w, uploadBodyDeadline)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		apiError(w, http.StatusBadRequest, "expected multipart form with 'files'")
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		apiError(w, http.StatusBadRequest, "no files provided")
		return
	}
	force := r.FormValue("force") == "1"
	me, _ := currentUser(r.Context())

	results := make([]directUploadResultJSON, 0, len(files))
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			results = append(results, directUploadResultJSON{Filename: fh.Filename, Status: "rejected", Reason: "unreadable upload"})
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, ingest.MaxPDFBytes+1))
		f.Close()
		if err != nil {
			results = append(results, directUploadResultJSON{Filename: fh.Filename, Status: "rejected", Reason: "read failed"})
			continue
		}
		id, rejected, err := s.ingest.StageDirectUpload(r.Context(), aid, ingest.DirectUploadInput{
			Filename: fh.Filename, Data: data, Kind: directUploadExt(fh.Filename),
			Force: force, Actor: me.ID,
		})
		if err != nil {
			results = append(results, directUploadResultJSON{Filename: fh.Filename, Status: "rejected", Reason: "staging failed"})
			continue
		}
		if rejected != "" {
			results = append(results, directUploadResultJSON{Filename: fh.Filename, Status: "rejected", Reason: rejected})
			continue
		}
		results = append(results, directUploadResultJSON{Filename: fh.Filename, UploadID: id, Status: "queued"})
	}
	s.audit(r, "submissions.upload", "assessment", strconv.FormatInt(aid, 10), map[string]any{
		"files": len(files), "force": force,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"results": results})
}

// directUploadStatus derives the reconciliation-view status for one direct_uploads
// row (D27, F1): an infra error takes priority, then a finished row's recorded
// ingest status, else "pending" (staged, ingest not yet run).
func directUploadStatus(row db.DirectUpload) string {
	if row.Error.Valid && row.Error.String != "" {
		return "error"
	}
	if row.FinishedAt.Valid && row.Status.Valid && row.Status.String != "" {
		return row.Status.String
	}
	return "pending"
}

type directUploadRowJSON struct {
	ID           int64      `json:"id"`
	Filename     string     `json:"filename"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason,omitempty"`
	SubmissionID int64      `json:"submission_id,omitempty"`
	CreatedAt    *time.Time `json:"created_at,omitempty"`
}

// handleListDirectUploads is the direct-upload reconciliation view (D27, F1): every
// staged upload for the assessment (most recent first, capped at 200) with its
// derived status. No student names/OCR text — filename/reason are the only
// potentially-identifying fields, mirroring the existing scan-file surface (D14).
func (s *Server) handleListDirectUploads(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	rows, err := s.store.Q.ListDirectUploadsForAssessment(r.Context(), db.ListDirectUploadsForAssessmentParams{
		AssessmentID: aid, Limit: 200,
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	out := make([]directUploadRowJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, directUploadRowJSON{
			ID: row.ID, Filename: row.OriginalFilename,
			Status: directUploadStatus(row), Reason: row.Reason.String,
			SubmissionID: row.SubmissionID.Int64,
			CreatedAt:    tsPtr(row.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploads": out})
}

// handleIngestReport is the reconciliation view: every rostered student with their
// live submission state, plus open quarantine entries (D13).
func (s *Server) handleIngestReport(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	rows, err := s.store.Q.IngestReportRows(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "report failed")
		return
	}
	problems, err := s.store.Q.ListProblems(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "problems fetch failed")
		return
	}
	type row struct {
		StudentID     string `json:"student_id"`
		Name          string `json:"name"`
		SubmissionID  *int64 `json:"submission_id,omitempty"`
		Filename      string `json:"filename,omitempty"`
		PageCount     *int32 `json:"page_count,omitempty"`
		MappedPages   int64  `json:"mapped_pages"`
		ExpectedPages int    `json:"expected_pages"`
	}
	out := make([]row, 0, len(rows))
	for _, rr := range rows {
		x := row{
			StudentID: rr.StudentID, Name: rr.Name,
			MappedPages: rr.MappedPages, ExpectedPages: len(problems),
		}
		if rr.SubmissionID.Valid {
			x.SubmissionID = &rr.SubmissionID.Int64
			x.Filename = rr.OriginalFilename.String
			x.PageCount = &rr.PageCount.Int32
		}
		out = append(out, x)
	}

	quarantine, err := s.store.Q.ListOpenQuarantine(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "quarantine fetch failed")
		return
	}
	type qrow struct {
		ID       int64  `json:"id"`
		Filename string `json:"filename"`
		Reason   string `json:"reason"`
	}
	qout := make([]qrow, 0, len(quarantine))
	for _, q := range quarantine {
		qout = append(qout, qrow{ID: q.ID, Filename: q.OriginalFilename, Reason: q.Reason})
	}
	writeJSON(w, http.StatusOK, map[string]any{"students": out, "quarantine": qout})
}

func (s *Server) handleAssignQuarantine(w http.ResponseWriter, r *http.Request) {
	qid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid quarantine id")
		return
	}
	var body struct {
		StudentID string `json:"student_id"`
		Force     bool   `json:"force"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.StudentID == "" {
		apiError(w, http.StatusBadRequest, "student_id is required")
		return
	}
	me, _ := currentUser(r.Context())
	res, err := s.ingest.AssignQuarantine(r.Context(), qid, body.StudentID, me.ID, body.Force)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "quarantine.assign", "quarantine", strconv.FormatInt(qid, 10), map[string]any{"student_id": body.StudentID})
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDismissQuarantine(w http.ResponseWriter, r *http.Request) {
	qid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid quarantine id")
		return
	}
	if err := s.ingest.DismissQuarantine(r.Context(), qid); err != nil {
		switch {
		case errors.Is(err, ingest.ErrQuarantineNotFound):
			apiError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ingest.ErrQuarantineAlreadyResolved):
			apiError(w, http.StatusConflict, err.Error())
		default:
			apiError(w, http.StatusInternalServerError, "quarantine dismiss failed")
		}
		return
	}
	s.audit(r, "quarantine.dismiss", "quarantine", strconv.FormatInt(qid, 10), nil)
	writeJSON(w, http.StatusOK, map[string]bool{"dismissed": true})
}

// handleRetractSubmission unassigns a live submission (HCI audit fix): the retract
// control three UI surfaces already point TAs at, finally routed. It is the mirror
// of a re-upload's guards (D1) — RetractSubmission is blocked without force when the
// scope has grading records, blocked always when published, and a no-op conflict
// when the row is already retracted or superseded. Body {"force": bool} is optional
// (absent = false). Same intake-mutation role (any signed-in user, TA+) and CSRF as
// its siblings above.
func (s *Server) handleRetractSubmission(w http.ResponseWriter, r *http.Request) {
	sid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid submission id")
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	// The force flag is optional: an absent body means force=false. Only a present,
	// malformed body is a 400.
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	me, _ := currentUser(r.Context())
	err := s.ingest.RetractSubmission(r.Context(), sid, me.ID, body.Force)
	switch {
	case err == nil:
		s.audit(r, "submission.retract", "submission", strconv.FormatInt(sid, 10), map[string]any{"force": body.Force})
		writeJSON(w, http.StatusOK, map[string]bool{"retracted": true})
	case errors.Is(err, ingest.ErrRetractionNeedsForce):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "needs_force": true})
	case errors.Is(err, ingest.ErrRetractionBlocked):
		apiError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ingest.ErrAlreadyRetracted),
		strings.Contains(err.Error(), "not active (already superseded)"):
		apiError(w, http.StatusConflict, err.Error())
	case strings.Contains(err.Error(), "no such submission"):
		apiError(w, http.StatusNotFound, err.Error())
	default:
		apiError(w, http.StatusInternalServerError, "retraction failed")
	}
}

// handleMaterializeAnswers backfills the answer grid for late-added students
// (roster-lifecycle plan 2026-07-10, fix 6): MaterializeAnswers only ever runs
// as a side effect of an upload, so a student who joins after the last upload
// has no answers rows — invisible to publish coverage and ungradeable — with
// no way to fix it short of re-uploading someone's file. This action runs the
// same INSERT ... ON CONFLICT DO NOTHING standalone. The row count is computed
// as before/after counts inside ONE tx (MaterializeAnswers is :exec — its
// ingest caller ignores the count) so a concurrent insert can't skew n.
// Route (R1): POST /api/assessments/{id}/materialize-answers, lecturer+.
func (s *Server) handleMaterializeAnswers(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	if _, err := s.store.Q.GetAssessment(r.Context(), aid); err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	var created int64
	err := s.store.WithTx(r.Context(), func(q *db.Queries) error {
		before, err := q.CountAnswersForAssessment(r.Context(), aid)
		if err != nil {
			return err
		}
		if err := q.MaterializeAnswers(r.Context(), aid); err != nil {
			return err
		}
		after, err := q.CountAnswersForAssessment(r.Context(), aid)
		if err != nil {
			return err
		}
		created = after - before
		return nil
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "materialize failed")
		return
	}
	s.audit(r, "answers.materialize", "assessment", strconv.FormatInt(aid, 10), map[string]any{"created": created})
	writeJSON(w, http.StatusOK, map[string]any{"created": created})
}

// --- mask regions + apply + review ------------------------------------------------

type maskRegionJSON struct {
	PageScope string  `json:"page_scope"`
	X         float32 `json:"x"`
	Y         float32 `json:"y"`
	W         float32 `json:"w"`
	H         float32 `json:"h"`
	Color     string  `json:"color"`
	Padding   float32 `json:"padding"`
}

func (s *Server) handleGetMaskRegions(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	regions, err := s.store.Q.ListMaskRegions(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	out := make([]maskRegionJSON, 0, len(regions))
	for _, m := range regions {
		out = append(out, maskRegionJSON{PageScope: m.PageScope, X: m.X, Y: m.Y, W: m.W, H: m.H, Color: m.Color, Padding: m.Padding})
	}
	writeJSON(w, http.StatusOK, map[string]any{"regions": out})
}

// handlePutMaskRegions replaces the assessment's region set atomically and, in
// the SAME transaction, reconciles mask-review acceptances against the new set
// (stale-mask fix 2026-07-11): any accepted page whose stored mask fingerprint
// no longer matches is knocked back to pending, so the "masked + accepted"
// grading gates block until the Apply-masks flow re-masks it and a reviewer
// re-accepts — instead of runs/regrades silently sending the OLD (possibly
// identity-revealing) masked images to providers forever. Pages whose inputs
// didn't change (e.g. a 'first'-scope edit leaves later pages alone) keep
// their acceptance, mirroring MaskPage's preserve-review skip path (D27, F2).
func (s *Server) handlePutMaskRegions(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	var body struct {
		Regions []maskRegionJSON `json:"regions"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	for i, reg := range body.Regions {
		if reg.W <= 0 || reg.H <= 0 || reg.X < 0 || reg.Y < 0 || reg.X > 1 || reg.Y > 1 || reg.W > 1 || reg.H > 1 {
			apiError(w, http.StatusBadRequest, "region "+strconv.Itoa(i+1)+": x/y/w/h must be normalized 0..1")
			return
		}
		if reg.PageScope != "first" && reg.PageScope != "all" {
			apiError(w, http.StatusBadRequest, "region "+strconv.Itoa(i+1)+": page_scope must be first|all")
			return
		}
	}
	var stale int64
	err := s.store.WithTx(r.Context(), func(q *db.Queries) error {
		if err := q.DeleteMaskRegions(r.Context(), aid); err != nil {
			return err
		}
		for _, reg := range body.Regions {
			color := reg.Color
			if color == "" {
				color = "#4a4a4a"
			}
			if _, err := q.CreateMaskRegion(r.Context(), db.CreateMaskRegionParams{
				AssessmentID: aid, PageScope: reg.PageScope,
				X: reg.X, Y: reg.Y, W: reg.W, H: reg.H,
				Color: color, Padding: reg.Padding,
			}); err != nil {
				return err
			}
		}
		var err error
		stale, err = ingest.InvalidateStaleMasks(r.Context(), q, aid)
		return err
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "saving regions failed")
		return
	}
	s.audit(r, "mask.regions", "assessment", strconv.FormatInt(aid, 10), map[string]any{
		"count": len(body.Regions), "stale_masks_reset": stale,
	})
	writeJSON(w, http.StatusOK, map[string]any{"saved": len(body.Regions), "stale": stale})
}

// handleApplyMasks plans the assessment's masking work (D27, F2) and enqueues one
// mask.page job per page that needs (re-)masking, rather than running the pass
// synchronously — at 200×9 scale re-masking ~1800 pages in one HTTP request cannot
// complete. It keeps ApplyMasks' 400 when no regions are defined.
func (s *Server) handleApplyMasks(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	pageIDs, skipped, err := s.ingest.PlanMasks(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.queue != nil {
		if err := s.queue.EnqueueMaskPages(r.Context(), pageIDs); err != nil {
			apiError(w, http.StatusInternalServerError, "enqueue failed")
			return
		}
	}
	s.audit(r, "mask.apply", "assessment", strconv.FormatInt(aid, 10), map[string]any{
		"enqueued": len(pageIDs), "skipped": skipped,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"enqueued": len(pageIDs), "skipped": skipped})
}

func (s *Server) handleMaskReviewList(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	rows, err := s.store.Q.MaskReviewList(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	type row struct {
		PageID        int64  `json:"page_id"`
		AnswerID      int64  `json:"answer_id"`
		PageIndex     int32  `json:"page_index"`
		StudentID     string `json:"student_id"`
		ProblemNumber int32  `json:"problem_number"`
		Masked        bool   `json:"masked"`
		ReviewStatus  string `json:"review_status"`
		// MaskError is the static, PII-free terminal reason set when a page's mask
		// job exhausted its attempts (D27 review, F1) — empty when the page has no
		// error. The UI treats a page with mask_error as terminal so its poll stops.
		MaskError string `json:"mask_error,omitempty"`
	}
	out := make([]row, 0, len(rows))
	for _, m := range rows {
		out = append(out, row{
			PageID: m.ID, AnswerID: m.AnswerID, PageIndex: m.PageIndex,
			StudentID: m.StudentID, ProblemNumber: m.ProblemNumber,
			Masked: m.MaskedImageRef.Valid, ReviewStatus: m.MaskReviewStatus,
			MaskError: m.MaskError.String,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pages": out})
}

func (s *Server) handleMaskReview(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(w, r, &body); err != nil || (body.Status != "accepted" && body.Status != "flagged") {
		apiError(w, http.StatusBadRequest, "status must be accepted|flagged")
		return
	}
	me, _ := currentUser(r.Context())
	if _, err := s.store.Q.SetMaskReview(r.Context(), db.SetMaskReviewParams{
		ID: pid, MaskReviewStatus: body.Status,
		MaskReviewedBy: int8Of(me.ID),
	}); err != nil {
		apiError(w, http.StatusNotFound, "no such page")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAcceptPendingMasks bulk-accepts every masked, still-pending page of the
// assessment as the current user — the "spot-checked a few, accept the rest"
// path. Flagged and unmasked pages are untouched (see AcceptPendingMasks).
func (s *Server) handleAcceptPendingMasks(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	me, _ := currentUser(r.Context())
	n, err := s.store.Q.AcceptPendingMasks(r.Context(), db.AcceptPendingMasksParams{
		AssessmentID: aid, MaskReviewedBy: int8Of(me.ID),
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "accept failed")
		return
	}
	s.audit(r, "mask.accept_pending", "assessment", strconv.FormatInt(aid, 10), map[string]any{"accepted": n})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": n})
}

// --- mapping corrections -----------------------------------------------------------

// handleMovePage reassigns a page to a different problem's answer for the same
// student (the manual mapping-correction path, spec §7).
func (s *Server) handleMovePage(w http.ResponseWriter, r *http.Request) {
	pageID, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	var body struct {
		ProblemID int64 `json:"problem_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.ProblemID == 0 {
		apiError(w, http.StatusBadRequest, "problem_id is required")
		return
	}
	page, err := s.store.Q.GetAnswerPage(r.Context(), pageID)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such page")
		return
	}
	answer, err := s.store.Q.GetAnswer(r.Context(), page.AnswerID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "answer fetch failed")
		return
	}
	err = s.store.WithTx(r.Context(), func(q *db.Queries) error {
		target, err := q.EnsureAnswer(r.Context(), db.EnsureAnswerParams{
			AssessmentID: answer.AssessmentID, StudentID: answer.StudentID, ProblemID: body.ProblemID,
		})
		if err != nil {
			return err
		}
		next, err := q.NextPageIndex(r.Context(), target.ID)
		if err != nil {
			return err
		}
		_, err = q.MoveAnswerPage(r.Context(), db.MoveAnswerPageParams{ID: pageID, AnswerID: target.ID, PageIndex: next})
		return err
	})
	if err != nil {
		apiError(w, http.StatusBadRequest, "move failed (does the problem belong to this assessment?)")
		return
	}
	s.audit(r, "page.move", "answer_page", strconv.FormatInt(pageID, 10), map[string]any{"problem_id": body.ProblemID})
	w.WriteHeader(http.StatusNoContent)
}

// Manual flags a TA may toggle on an answer.
var manualFlags = map[string]bool{"blank": true, "needs_review": true, ingest.FlagImageSuperseded: true}

func (s *Server) handleAnswerFlag(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid answer id")
		return
	}
	var body struct {
		Flag string `json:"flag"`
		Add  bool   `json:"add"`
	}
	if err := decodeJSON(w, r, &body); err != nil || !manualFlags[body.Flag] {
		apiError(w, http.StatusBadRequest, "flag must be one of: blank, needs_review, image_superseded")
		return
	}
	var err error
	if body.Add {
		err = s.store.Q.AddAnswerFlag(r.Context(), db.AddAnswerFlagParams{ID: aid, Flag: body.Flag})
	} else {
		err = s.store.Q.RemoveAnswerFlag(r.Context(), db.RemoveAnswerFlagParams{ID: aid, Flag: body.Flag})
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "flag update failed")
		return
	}
	s.audit(r, "answer.flag", "answer", strconv.FormatInt(aid, 10), map[string]any{"flag": body.Flag, "add": body.Add})
	// Flags block the AI source in the officials derivation (0027): toggling
	// one can open or close a hole, so re-derive.
	if ans, err := s.store.Q.GetAnswer(r.Context(), aid); err == nil {
		if _, err := s.store.RecomputeOfficials(r.Context(), ans.AssessmentID); err != nil {
			s.log.Error("officials recompute failed", "assessment_id", ans.AssessmentID, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- blob streaming (D10: everything through the authenticated process) ------------

func (s *Server) handlePageImage(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	page, err := s.store.Q.GetAnswerPage(r.Context(), pid)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such page")
		return
	}
	key := page.ImageRef
	if r.URL.Query().Get("variant") == "masked" {
		if !page.MaskedImageRef.Valid {
			apiError(w, http.StatusNotFound, "page has no masked derivative yet")
			return
		}
		key = page.MaskedImageRef.String
	}
	s.streamBlob(w, r, key, "image/jpeg")
}

// handleSubmissionPDF streams a submission's original source. The route predates
// image submissions (D22) and is kept for compatibility, but semantically it's
// "/source" now: Content-Type follows source_kind rather than assuming PDF.
func (s *Server) handleSubmissionPDF(w http.ResponseWriter, r *http.Request) {
	sid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid submission id")
		return
	}
	sub, err := s.store.Q.GetSubmission(r.Context(), sid)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such submission")
		return
	}
	contentType := "application/pdf"
	if sub.SourceKind == "image" {
		contentType = "image/jpeg"
		if strings.HasSuffix(sub.SourceRef, ".png") {
			contentType = "image/png"
		}
	}
	s.streamBlob(w, r, sub.SourceRef, contentType)
}

func (s *Server) streamBlob(w http.ResponseWriter, r *http.Request, key, contentType string) {
	rc, err := s.blobs.Get(r.Context(), key)
	if errors.Is(err, blobstore.ErrNotFound) {
		apiError(w, http.StatusNotFound, "blob missing from storage")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "storage error")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", contentType)
	// Refs are content-addressed (sha-suffixed), so short private caching is safe.
	w.Header().Set("Cache-Control", "private, max-age=600")
	if strings.HasSuffix(contentType, "pdf") {
		w.Header().Set("Content-Disposition", "inline")
	}
	_, _ = io.Copy(w, rc)
}
