// Scan-intake HTTP surface (design spec 2026-07-04): the id-regions editor plus
// the full page-level pipeline surface (batches, pages, matrix, finalize, and
// the per-page mutation/streaming endpoints). All routes are TA+ (mutations
// also CSRF-headered by the existing middleware).
package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/scan"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// --- id-regions (typed: one region per kind, applied to EVERY page) --------------

type idRegionJSON struct {
	Kind    string  `json:"kind"`
	X       float32 `json:"x"`
	Y       float32 `json:"y"`
	W       float32 `json:"w"`
	H       float32 `json:"h"`
	Color   string  `json:"color"`
	Padding float32 `json:"padding"`
}

func (s *Server) handleGetIDRegions(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	regions, err := s.store.Q.ListIDRegions(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	out := make([]idRegionJSON, 0, len(regions))
	for _, reg := range regions {
		out = append(out, idRegionJSON{
			Kind: reg.Kind, X: reg.X, Y: reg.Y, W: reg.W, H: reg.H,
			Color: reg.Color, Padding: reg.Padding,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"regions": out})
}

// handlePutIDRegions replaces the assessment's id-region set atomically (mirrors
// handlePutMaskRegions) with a kind rule instead of the old shared-page rule:
// every region must carry a distinct kind of student_id/name/problem_id — the
// three identity boxes may live anywhere on the page and are each cropped
// independently on every page (spec 2026-07-04).
func (s *Server) handlePutIDRegions(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	var body struct {
		Regions []idRegionJSON `json:"regions"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	seenKinds := make(map[string]bool, len(body.Regions))
	for i, reg := range body.Regions {
		if reg.W <= 0 || reg.H <= 0 || reg.X < 0 || reg.Y < 0 || reg.X > 1 || reg.Y > 1 || reg.W > 1 || reg.H > 1 {
			apiError(w, http.StatusBadRequest, "region "+strconv.Itoa(i+1)+": x/y/w/h must be normalized 0..1")
			return
		}
		switch reg.Kind {
		case "student_id", "name", "problem_id":
		default:
			apiError(w, http.StatusBadRequest, "region "+strconv.Itoa(i+1)+": kind must be student_id, name, or problem_id")
			return
		}
		if seenKinds[reg.Kind] {
			apiError(w, http.StatusBadRequest, "region "+strconv.Itoa(i+1)+": duplicate kind")
			return
		}
		seenKinds[reg.Kind] = true
	}
	err := s.store.WithTx(r.Context(), func(q *db.Queries) error {
		if err := q.DeleteIDRegions(r.Context(), aid); err != nil {
			return err
		}
		for _, reg := range body.Regions {
			color := reg.Color
			if color == "" {
				color = "#4a4a4a"
			}
			if _, err := q.CreateIDRegion(r.Context(), db.CreateIDRegionParams{
				AssessmentID: aid, Kind: reg.Kind,
				X: reg.X, Y: reg.Y, W: reg.W, H: reg.H,
				Color: color, Padding: reg.Padding,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "saving id-regions failed")
		return
	}
	s.audit(r, "id_regions.replace", "assessment", strconv.FormatInt(aid, 10), map[string]any{"count": len(body.Regions)})
	writeJSON(w, http.StatusOK, map[string]any{"saved": len(body.Regions)})
}

// --- page-level scan intake (design spec 2026-07-04) ------------------------

// deriveScanPageState derives one page's staff-facing state from its row,
// precedence exactly: error > discarded > promoted > parked > assigned >
// orphan (identified_at set) > processing (D2).
func deriveScanPageState(r db.ScanPageRowsRow) string {
	switch {
	case r.Error.Valid && r.Error.String != "":
		return "error"
	case r.DiscardedAt.Valid:
		return "discarded"
	case r.SubmissionID.Valid:
		return "promoted"
	case r.ParkedReason.Valid:
		return "parked"
	case r.AssignedStudentID.Valid:
		return "assigned"
	case r.IdentifiedAt.Valid:
		return "orphan"
	default:
		return "processing"
	}
}

type scanPageJSON struct {
	ID                 int64  `json:"id"`
	BatchID            int64  `json:"batch_id"`
	PageIndex          int32  `json:"page_index"`
	State              string `json:"state"`
	Error              string `json:"error,omitempty"`
	OCRStudentID       string `json:"ocr_student_id,omitempty"`
	OCRName            string `json:"ocr_name,omitempty"`
	OCRProblem         string `json:"ocr_problem,omitempty"`
	OCREngine          string `json:"ocr_engine,omitempty"`
	ProposalSource     string `json:"proposal_source,omitempty"`
	ProposedExternalID string `json:"proposed_student_id,omitempty"`
	ProposedName       string `json:"proposed_name,omitempty"`
	ProposedProblemID  int64  `json:"proposed_problem_id,omitempty"`
	AssignedExternalID string `json:"assigned_student_id,omitempty"`
	AssignedName       string `json:"assigned_name,omitempty"`
	AssignedProblemID  int64  `json:"assigned_problem_id,omitempty"`
	AssignedByUser     bool   `json:"assigned_by_user"`
	ParkedReason       string `json:"parked_reason,omitempty"`
	ParkedAgainst      int64  `json:"parked_against,omitempty"`
	DiscardReason      string `json:"discard_reason,omitempty"`
	HasImage           bool   `json:"has_image"`
}

func toScanPageJSON(r db.ScanPageRowsRow) scanPageJSON {
	out := scanPageJSON{
		ID:        r.ID,
		BatchID:   r.BatchID,
		PageIndex: r.PageIndex,
		State:     deriveScanPageState(r),
		HasImage:  r.ImageRef.Valid && r.ImageRef.String != "",
	}
	if r.Error.Valid {
		out.Error = r.Error.String
	}
	if r.OcrStudentID.Valid {
		out.OCRStudentID = r.OcrStudentID.String
	}
	if r.OcrName.Valid {
		out.OCRName = r.OcrName.String
	}
	if r.OcrProblem.Valid {
		out.OCRProblem = r.OcrProblem.String
	}
	if r.OcrEngine.Valid {
		out.OCREngine = r.OcrEngine.String
	}
	if r.ProposalSource.Valid {
		out.ProposalSource = r.ProposalSource.String
	}
	if r.ProposedExternalID.Valid {
		out.ProposedExternalID = r.ProposedExternalID.String
	}
	if r.ProposedNameRoster.Valid {
		out.ProposedName = r.ProposedNameRoster.String
	}
	if r.ProposedProblemID.Valid {
		out.ProposedProblemID = r.ProposedProblemID.Int64
	}
	if r.AssignedExternalID.Valid {
		out.AssignedExternalID = r.AssignedExternalID.String
	}
	if r.AssignedName.Valid {
		out.AssignedName = r.AssignedName.String
	}
	if r.AssignedProblemID.Valid {
		out.AssignedProblemID = r.AssignedProblemID.Int64
	}
	// assigned_by_user: false when auto-assigned (assigned_by NULL), true when a
	// human assigned/resolved it.
	out.AssignedByUser = r.AssignedBy.Valid
	if r.ParkedReason.Valid {
		out.ParkedReason = r.ParkedReason.String
	}
	if r.ParkedAgainst.Valid {
		out.ParkedAgainst = r.ParkedAgainst.Int64
	}
	if r.DiscardReason.Valid {
		out.DiscardReason = r.DiscardReason.String
	}
	return out
}

type scanBatchJSON struct {
	ID           int64  `json:"id"`
	AssessmentID int64  `json:"assessment_id"`
	OCREnabled   bool   `json:"ocr_enabled"`
	OCRProvider  string `json:"ocr_provider,omitempty"`
	OCRModel     string `json:"ocr_model,omitempty"`
}

func toScanBatchJSON(b db.ScanBatch) scanBatchJSON {
	out := scanBatchJSON{ID: b.ID, AssessmentID: b.AssessmentID, OCREnabled: b.OcrEnabled}
	if b.OcrProvider.Valid {
		out.OCRProvider = b.OcrProvider.String
	}
	if b.OcrModel.Valid {
		out.OCRModel = b.OcrModel.String
	}
	return out
}

type scanBatchListRowJSON struct {
	scanBatchJSON
	TotalPages      int `json:"total_pages"`
	ProcessingPages int `json:"processing_pages"`
	OrphanPages     int `json:"orphan_pages"`
	AssignedPages   int `json:"assigned_pages"`
	ParkedPages     int `json:"parked_pages"`
	DiscardedPages  int `json:"discarded_pages"`
	ErroredPages    int `json:"errored_pages"`
}

type matrixCellJSON struct {
	ProblemID int64  `json:"problem_id"`
	State     string `json:"state"`
	PageID    int64  `json:"page_id,omitempty"`
}

type matrixRowJSON struct {
	StudentID string           `json:"student_id"`
	Name      string           `json:"name"`
	Cells     []matrixCellJSON `json:"cells"`
}

type matrixProblemJSON struct {
	ID     int64 `json:"id"`
	Number int32 `json:"number"`
}

type matrixJSON struct {
	Problems []matrixProblemJSON `json:"problems"`
	Rows     []matrixRowJSON     `json:"rows"`
}

// parseOCREnabled resolves the upload form's ocr_enabled field. Only an
// explicit "1" (what the SPA sends) or "true" (curl-friendly) turns the cloud
// identify step on; absent or anything else means off — cloud identification
// is opt-in (privacy audit 2026-07-12).
func parseOCREnabled(v string) bool {
	return v == "1" || v == "true"
}

// handleCreateScanBatch stages a new scan batch: multipart form with either
// loose page files ("files", many) or a single archive ("zip"), plus the
// batch's OCR options. Every source file is streamed straight into the
// service without ever buffering a whole upload in memory (F5/F4).
func (s *Server) handleCreateScanBatch(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	_ = extendBodyDeadline(w, uploadBodyDeadline)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		apiError(w, http.StatusBadRequest, "could not parse upload")
		return
	}

	files := r.MultipartForm.File["files"]
	zips := r.MultipartForm.File["zip"]
	if len(files) == 0 && len(zips) == 0 {
		apiError(w, http.StatusBadRequest, "provide loose scan files or a zip archive")
		return
	}
	if len(files) > 0 && len(zips) > 0 {
		apiError(w, http.StatusBadRequest, "provide loose scan files or a zip, not both")
		return
	}
	if len(zips) > 1 {
		apiError(w, http.StatusBadRequest, "only one zip archive may be uploaded at a time")
		return
	}

	nb := scan.NewBatch{
		// Cloud opt-in (privacy audit 2026-07-12): the cloud identify rung is
		// OFF unless explicitly requested — an absent ocr_enabled field means
		// no identity crop leaves this machine (the local rung, when
		// installed, still runs). This flips the old `!= "0"` parse, where an
		// absent field meant ON and — after the empty-provider guard — also
		// made a bare upload 400 with ErrOCRProviderRequired.
		OCREnabled:  parseOCREnabled(r.FormValue("ocr_enabled")),
		OCRProvider: r.FormValue("ocr_provider"),
		OCRModel:    r.FormValue("ocr_model"),
	}

	var sources []scan.SourceUpload
	var closers []io.Closer
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			continue // surfaced as skip by size-0 read in the service
		}
		closers = append(closers, f)
		sources = append(sources, scan.SourceUpload{Filename: fh.Filename, R: f})
	}
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()

	var zr io.Reader
	if len(zips) == 1 {
		zf, err := zips[0].Open()
		if err != nil {
			apiError(w, http.StatusBadRequest, "unreadable zip")
			return
		}
		defer zf.Close()
		zr = zf
	}

	me, _ := currentUser(r.Context())
	view, err := s.scans.CreateBatch(r.Context(), aid, nb, sources, zr, me.ID)
	if errors.Is(err, scan.ErrOCRProviderRequired) {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, scan.ErrRegionsIncomplete) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "regions_incomplete": true})
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "batch creation failed")
		return
	}

	s.audit(r, "scan.batch.create", "scan_batch", strconv.FormatInt(view.Batch.ID, 10), map[string]any{
		"assessment_id": aid, "created": view.Created,
	})
	skipped := view.Skipped
	if skipped == nil {
		skipped = []scan.SkipInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch": toScanBatchJSON(view.Batch), "created": view.Created, "skipped": skipped,
	})
}

// handleListScanBatches lists an assessment's batches with page-progress
// counters, in one grouped query (no N+1, no OCR/PII columns, F6).
func (s *Server) handleListScanBatches(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	batches, err := s.store.Q.ListScanBatches(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	batchIDs := make([]int64, 0, len(batches))
	for _, b := range batches {
		batchIDs = append(batchIDs, b.ID)
	}
	progress := map[int64]db.ScanBatchPageProgressRow{}
	if len(batchIDs) > 0 {
		rows, err := s.store.Q.ScanBatchPageProgress(r.Context(), batchIDs)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "progress lookup failed")
			return
		}
		for _, row := range rows {
			progress[row.BatchID] = row
		}
	}
	out := make([]scanBatchListRowJSON, 0, len(batches))
	for _, b := range batches {
		p := progress[b.ID]
		out = append(out, scanBatchListRowJSON{
			scanBatchJSON:   toScanBatchJSON(b),
			TotalPages:      int(p.Total),
			ProcessingPages: int(p.Processing),
			OrphanPages:     int(p.Orphaned),
			AssignedPages:   int(p.Assigned + p.Promoted),
			ParkedPages:     int(p.Parked),
			DiscardedPages:  int(p.Discarded),
			ErroredPages:    int(p.Errored),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": out})
}

// handleListScanPages lists an assessment's pages (staff-facing: OCR text and
// proposed/assigned names are included, PII to an authenticated session only),
// with an optional ?state= server-side filter.
func (s *Server) handleListScanPages(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	rows, err := s.store.Q.ScanPageRows(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	stateFilter := r.URL.Query().Get("state")
	out := make([]scanPageJSON, 0, len(rows))
	for _, row := range rows {
		pj := toScanPageJSON(row)
		if stateFilter != "" && pj.State != stateFilter {
			continue
		}
		out = append(out, pj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"pages": out})
}

// handleScanMatrix composes the student x problem grading matrix from active
// students, problems, live assigned pages, and live submissions.
func (s *Server) handleScanMatrix(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	ctx := r.Context()
	students, err := s.store.Q.ListActiveStudents(ctx)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "roster lookup failed")
		return
	}
	problems, err := s.store.Q.ListProblems(ctx, aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "problem lookup failed")
		return
	}
	pages, err := s.store.Q.ListLiveAssignedPagesForAssessment(ctx, aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "page lookup failed")
		return
	}
	subs, err := s.store.Q.ListLiveSubmissionsForAssessment(ctx, aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "submission lookup failed")
		return
	}

	matrixProblems := make([]matrixProblemJSON, 0, len(problems))
	for _, p := range problems {
		matrixProblems = append(matrixProblems, matrixProblemJSON{ID: p.ID, Number: p.Number})
	}

	// Index pages by (student, problem) cell.
	type cellKey struct{ studentID, problemID int64 }
	pageByCell := make(map[cellKey]db.ListLiveAssignedPagesForAssessmentRow, len(pages))
	for _, p := range pages {
		if !p.AssignedStudentID.Valid || !p.AssignedProblemID.Valid {
			continue
		}
		pageByCell[cellKey{p.AssignedStudentID.Int64, p.AssignedProblemID.Int64}] = p
	}
	// Whole-assessment submissions (problem_id NULL) cover every problem for
	// that student; per-problem submissions cover just their one cell.
	subByStudentWhole := make(map[int64]bool, len(subs))
	subByCell := make(map[cellKey]bool, len(subs))
	for _, sub := range subs {
		if sub.ProblemID.Valid {
			subByCell[cellKey{sub.StudentID, sub.ProblemID.Int64}] = true
		} else {
			subByStudentWhole[sub.StudentID] = true
		}
	}

	rows := make([]matrixRowJSON, 0, len(students))
	for _, st := range students {
		cells := make([]matrixCellJSON, 0, len(matrixProblems))
		for _, mp := range matrixProblems {
			pid := mp.ID
			key := cellKey{st.ID, pid}
			state := "empty"
			var cellPageID int64
			if pg, ok := pageByCell[key]; ok {
				cellPageID = pg.ID
				switch {
				case pg.SubmissionID.Valid:
					state = "promoted"
				case !pg.AssignedBy.Valid:
					state = "auto"
				default:
					state = "manual"
				}
			} else if subByCell[key] || subByStudentWhole[st.ID] {
				state = "submitted"
			}
			cells = append(cells, matrixCellJSON{ProblemID: pid, State: state, PageID: cellPageID})
		}
		rows = append(rows, matrixRowJSON{StudentID: st.StudentID, Name: st.Name, Cells: cells})
	}

	writeJSON(w, http.StatusOK, matrixJSON{Problems: matrixProblems, Rows: rows})
}

// handleScanFinalize gates and drives assessment-wide finalize (D27, F1): the
// response is 202 with the FinalizeReport since promotion itself happens
// off-request via enqueued promote jobs.
func (s *Server) handleScanFinalize(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	var body struct {
		AckMissing bool `json:"ack_missing"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	me, _ := currentUser(r.Context())
	report, err := s.scans.Finalize(r.Context(), aid, body.AckMissing, me.ID)
	var missing *scan.ErrMissingUnacknowledged
	if errors.As(err, &missing) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "missing_cells": missing.Count})
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "finalize failed")
		return
	}
	s.audit(r, "scan.finalize", "assessment", strconv.FormatInt(aid, 10), map[string]any{
		"enqueued": report.Enqueued, "missing_cells": report.MissingCells,
	})
	writeJSON(w, http.StatusAccepted, report)
}

// handleScanMissing lists the (student, problem) cells with neither a live
// assigned page nor a live submission — staff-facing only (PII).
func (s *Server) handleScanMissing(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	rows, err := s.store.Q.ListMissingCells(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "missing lookup failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"student_id": row.StudentID, "name": row.Name, "problem_number": row.ProblemNumber,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"missing": out})
}

// handleAssignScanPage manually assigns a page to a (student, problem) cell.
func (s *Server) handleAssignScanPage(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	var body struct {
		StudentID string `json:"student_id"`
		ProblemID int64  `json:"problem_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.StudentID == "" || body.ProblemID == 0 {
		apiError(w, http.StatusBadRequest, "student_id and problem_id are required")
		return
	}
	// Exact-then-normalized lookup (roster-lifecycle plan 2026-07-10, fix 4):
	// a case/width-variant of one active roster id resolves; a collision is the
	// operator's to disambiguate. scan.AssignPage keeps its own withdraw guard.
	student, err := ingest.ResolveStudentByExternalID(r.Context(), s.store.Q, body.StudentID)
	if errors.Is(err, ingest.ErrAmbiguousStudentID) {
		apiError(w, http.StatusBadRequest, "ambiguous student id")
		return
	}
	if err != nil {
		apiError(w, http.StatusNotFound, "no such student")
		return
	}
	me, _ := currentUser(r.Context())
	err = s.scans.AssignPage(r.Context(), pid, student.ID, body.ProblemID, me.ID)
	var occupied *scan.ErrCellOccupied
	var promoted *scan.ErrPagePromoted
	switch {
	case errors.As(err, &occupied):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(), "incumbent_page_id": occupied.IncumbentPageID,
			"incumbent_submission_id": occupied.IncumbentSubmissionID, "duplicate": occupied.Duplicate,
		})
		return
	case errors.As(err, &promoted):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "promoted": true})
		return
	case errors.Is(err, scan.ErrInvalidInput):
		apiError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "assign failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assigned": true})
}

// handleUnassignScanPage clears a page's assignment.
func (s *Server) handleUnassignScanPage(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	me, _ := currentUser(r.Context())
	err := s.scans.UnassignPage(r.Context(), pid, me.ID)
	if !handlePageMutationError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDiscardScanPage marks a page as discarded.
func (s *Server) handleDiscardScanPage(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	me, _ := currentUser(r.Context())
	err := s.scans.DiscardPage(r.Context(), pid, body.Reason, me.ID)
	if !handlePageMutationError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"discarded": true})
}

// handleUndiscardScanPage reverses a discard.
func (s *Server) handleUndiscardScanPage(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	me, _ := currentUser(r.Context())
	err := s.scans.UndiscardPage(r.Context(), pid, me.ID)
	if !handlePageMutationError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRetryScanPage clears a page's error and re-enqueues the right stage.
func (s *Server) handleRetryScanPage(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	me, _ := currentUser(r.Context())
	err := s.scans.RetryPage(r.Context(), pid, me.ID)
	if !handlePageMutationError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRetryErroredScanBatch bulk-retries every errored page of a batch,
// optionally repointing the batch to a different OCR provider/model first —
// the recovery path for a batch created against a broken provider (the
// per-page retry re-runs with the batch's stored provider and can only
// re-fail).
func (s *Server) handleRetryErroredScanBatch(w http.ResponseWriter, r *http.Request) {
	bid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid batch id")
		return
	}
	var body struct {
		OCRProvider string `json:"ocr_provider"`
		OCRModel    string `json:"ocr_model"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	me, _ := currentUser(r.Context())
	n, err := s.scans.RetryErroredPages(r.Context(), bid, body.OCRProvider, body.OCRModel, me.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		apiError(w, http.StatusNotFound, "no such batch")
		return
	case errors.Is(err, scan.ErrInvalidInput):
		apiError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "retry failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retried": n})
}

// handleDiscardErroredScanBatch bulk-discards every errored page of a batch.
func (s *Server) handleDiscardErroredScanBatch(w http.ResponseWriter, r *http.Request) {
	bid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid batch id")
		return
	}
	me, _ := currentUser(r.Context())
	n, err := s.scans.DiscardErroredPages(r.Context(), bid, me.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		apiError(w, http.StatusNotFound, "no such batch")
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "discard failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"discarded": n})
}

// handleResolveScanPageConflict resolves a parked page's conflict.
func (s *Server) handleResolveScanPageConflict(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	var body struct {
		Action string `json:"action"`
		Force  bool   `json:"force"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch body.Action {
	case "keep", "replace":
	default:
		apiError(w, http.StatusBadRequest, "action must be keep or replace")
		return
	}
	me, _ := currentUser(r.Context())
	err := s.scans.ResolveConflict(r.Context(), pid, body.Action, body.Force, me.ID)
	if !handlePageMutationError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolved": true})
}

// handlePageMutationError maps the common page-mutation error taxonomy to an
// HTTP response, returning false (already written) when err was handled, true
// when the caller should continue with its own success response.
func handlePageMutationError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	var promoted *scan.ErrPagePromoted
	if errors.As(err, &promoted) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "promoted": true})
		return false
	}
	var stale *scan.ErrConflictStale
	if errors.As(err, &stale) {
		// ResolveConflict replace: the incumbent no longer occupies the
		// contested cell and the cell is not recoverable — the TA
		// re-adjudicates (or assigns the parked page manually).
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "conflict_stale": true})
		return false
	}
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no such page")
		return false
	}
	if errors.Is(err, scan.ErrInvalidInput) {
		apiError(w, http.StatusBadRequest, err.Error())
		return false
	}
	apiError(w, http.StatusInternalServerError, "operation failed")
	return false
}

// handleScanPageImage streams a page's full rendered image.
func (s *Server) handleScanPageImage(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	page, err := s.store.Q.GetScanPage(r.Context(), pid)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such page")
		return
	}
	if !page.ImageRef.Valid || page.ImageRef.String == "" {
		apiError(w, http.StatusNotFound, "page has not been rendered yet")
		return
	}
	s.streamBlob(w, r, page.ImageRef.String, "image/jpeg")
}

// handleScanPageCrop streams one of a page's three identity crops, selected by
// ?kind=student_id|name|problem_id.
func (s *Server) handleScanPageCrop(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid page id")
		return
	}
	kind := r.URL.Query().Get("kind")
	var ref func(db.ScanPage) (string, bool)
	switch kind {
	case "student_id":
		ref = func(p db.ScanPage) (string, bool) { return p.StudentIDCropRef.String, p.StudentIDCropRef.Valid }
	case "name":
		ref = func(p db.ScanPage) (string, bool) { return p.NameCropRef.String, p.NameCropRef.Valid }
	case "problem_id":
		ref = func(p db.ScanPage) (string, bool) { return p.ProblemCropRef.String, p.ProblemCropRef.Valid }
	default:
		apiError(w, http.StatusBadRequest, "kind must be student_id, name, or problem_id")
		return
	}
	page, err := s.store.Q.GetScanPage(r.Context(), pid)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such page")
		return
	}
	key, valid := ref(page)
	if !valid || key == "" {
		apiError(w, http.StatusNotFound, "crop not available yet")
		return
	}
	s.streamBlob(w, r, key, "image/jpeg")
}
