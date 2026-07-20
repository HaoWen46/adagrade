package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/HaoWen46/adagrade/internal/roster"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

type studentJSON struct {
	ID        int64  `json:"id"`
	StudentID string `json:"student_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Withdrawn bool   `json:"withdrawn"`
}

func (s *Server) handleListStudents(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Q.ListStudents(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]studentJSON, 0, len(rows))
	for _, st := range rows {
		out = append(out, studentJSON{ID: st.ID, StudentID: st.StudentID, Name: st.Name, Email: st.Email, Withdrawn: st.WithdrawnAt.Valid})
	}
	writeJSON(w, http.StatusOK, map[string]any{"students": out})
}

// handleUpdateStudent toggles withdrawn status (D23): excluded from future
// MaterializeAnswers/ingest-report/matching once withdrawn, existing history kept.
// Lecturer+ only; audit-logged (no student PII in the audit detail, D14 — only the
// numeric id and the boolean).
func (s *Server) handleUpdateStudent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid student id")
		return
	}
	var body struct {
		Withdrawn *bool `json:"withdrawn"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Withdrawn == nil {
		apiError(w, http.StatusBadRequest, "withdrawn is required")
		return
	}
	if _, err := s.store.Q.GetStudent(r.Context(), id); err != nil {
		apiError(w, http.StatusNotFound, "no such student")
		return
	}
	st, err := s.store.Q.SetStudentWithdrawn(r.Context(), db.SetStudentWithdrawnParams{ID: id, Withdrawn: *body.Withdrawn})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "update failed")
		return
	}
	s.audit(r, "student.withdrawn", "student", strconv.FormatInt(id, 10), map[string]any{"withdrawn": *body.Withdrawn})
	writeJSON(w, http.StatusOK, studentJSON{ID: st.ID, StudentID: st.StudentID, Name: st.Name, Email: st.Email, Withdrawn: st.WithdrawnAt.Valid})
}

// handleImportRoster accepts a multipart "file" field with the roster CSV. Any
// parse or row error (non-UTF-8, duplicate ids/emails, an email owned by a
// different existing student) rejects the entire import (D13). The 200 response
// carries the roster diff (roster-lifecycle plan 2026-07-10 fix 1) — reporting
// only; the Students page proposes bulk withdraw/reinstate from it.
func (s *Server) handleImportRoster(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		apiError(w, http.StatusBadRequest, "expected multipart form with a 'file' field")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		apiError(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer f.Close()

	rows, parseErrs := roster.Parse(f)
	if len(parseErrs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "roster rejected", "errors": parseErrs})
		return
	}

	// Sentinel so row-level rejections roll the transaction back (Import
	// validates before writing, but the rollback keeps that non-load-bearing).
	errRowsRejected := errors.New("roster rows rejected")
	var report roster.Report
	var rowErrs []roster.ParseError
	err = s.store.WithTx(r.Context(), func(q *db.Queries) error {
		var err error
		report, rowErrs, err = roster.Import(r.Context(), q, rows)
		if err != nil {
			return err
		}
		if len(rowErrs) > 0 {
			return errRowsRejected
		}
		return nil
	})
	if len(rowErrs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "roster rejected", "errors": rowErrs})
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "import failed")
		return
	}
	s.audit(r, "roster.import", "roster", "", map[string]any{
		"added": report.Added, "updated": report.Updated, "unchanged": report.Unchanged,
		"missing_active": len(report.MissingActive), "withdrawn_present": len(report.WithdrawnPresent),
		"email_changed": report.EmailChanged, "name_changed": report.NameChanged,
	})
	writeJSON(w, http.StatusOK, report)
}

// handleBulkWithdrawStudents / handleBulkReinstateStudents back the import-diff
// sync buttons ("Withdraw all N" / "Reinstate all N"). Lecturer+ (route-level);
// ids are EXTERNAL student_ids. Validation is strict: any unknown id fails the
// whole request with a 400 listing them — never a partial update.
func (s *Server) handleBulkWithdrawStudents(w http.ResponseWriter, r *http.Request) {
	s.bulkSetWithdrawn(w, r, true)
}

func (s *Server) handleBulkReinstateStudents(w http.ResponseWriter, r *http.Request) {
	s.bulkSetWithdrawn(w, r, false)
}

// handleDeleteStudent hard-deletes a roster row (audit finding B15): Withdraw
// can never be undone into "gone" — a typo'd import or smoke-test row haunts
// every assessment's publish coverage gate forever with no way to remove it.
// Admin-only (route-level, one rung above the lecturer-gated actions above).
// Succeeds ONLY when nothing but bare materialized answers (MaterializeAnswers
// scaffolding — no ingested page, no grading record) reference the row; those
// are deleted alongside the student in the same transaction. Anything else
// (submissions, paged/graded answers, scan pages, publish items, regrade
// requests, resolved quarantine rows) blocks with a 409 naming which kinds,
// pointing at Withdraw as the reversible alternative.
func (s *Server) handleDeleteStudent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid student id")
		return
	}
	if _, err := s.store.Q.GetStudent(r.Context(), id); err != nil {
		apiError(w, http.StatusNotFound, "no such student")
		return
	}

	blockers, err := s.store.Q.GetStudentBlockingArtifacts(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "guardrail check failed")
		return
	}
	var blocking []string
	if blockers.HasSubmissions {
		blocking = append(blocking, "submissions")
	}
	if blockers.HasGradedAnswers {
		blocking = append(blocking, "answers")
	}
	if blockers.HasScanPages {
		blocking = append(blocking, "scan_pages")
	}
	if blockers.HasPublishItems {
		blocking = append(blocking, "publish_items")
	}
	if blockers.HasRegradeRequests {
		blocking = append(blocking, "regrade_requests")
	}
	if blockers.HasQuarantineResolutions {
		blocking = append(blocking, "quarantine")
	}
	if len(blocking) > 0 {
		apiError409Blocking(w, blocking)
		return
	}

	// Sentinel: a race between the existence check above and this tx (another
	// admin's concurrent delete) surfaces as 0 rows affected, not an error —
	// report it as 404 rather than a false 200.
	errStudentGone := errors.New("student gone")
	err = s.store.WithTx(r.Context(), func(q *db.Queries) error {
		if _, err := q.DeleteBareAnswersForStudent(r.Context(), id); err != nil {
			return err
		}
		n, err := q.DeleteStudent(r.Context(), id)
		if err != nil {
			return err
		}
		if n == 0 {
			return errStudentGone
		}
		return nil
	})
	if errors.Is(err, errStudentGone) {
		apiError(w, http.StatusNotFound, "no such student")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "delete failed")
		return
	}

	// No student PII in the audit detail (D14) — the numeric id is already the
	// target_id; the boolean-less detail just marks the action took place.
	s.audit(r, "roster.delete", "student", strconv.FormatInt(id, 10), map[string]any{})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// apiError409Blocking is the roster hard-delete guard's structured 409 envelope
// (B15): {"error": ..., "blocking": [kind, ...]} naming which artifact kinds
// prevented the delete, mirroring apiError409Unverdicted (regrade.go) and
// apiError409Budget/apiError409SpotCheck (runs.go) elsewhere in this package.
// The message always points at Withdraw as the reversible alternative.
func apiError409Blocking(w http.ResponseWriter, blocking []string) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":    "student has graded or submitted artifacts and cannot be deleted; withdraw the student instead",
		"blocking": blocking,
	})
}

func (s *Server) bulkSetWithdrawn(w http.ResponseWriter, r *http.Request, withdrawn bool) {
	var body struct {
		StudentIDs []string `json:"student_ids"`
	}
	if err := decodeJSON(w, r, &body); err != nil || len(body.StudentIDs) == 0 {
		apiError(w, http.StatusBadRequest, "student_ids is required")
		return
	}

	// Strict unknown-id validation (the bulk UPDATE silently skips misses, and
	// a typo'd sync list must not half-apply). ID-only queries — no PII pulled.
	active, err := s.store.Q.ListActiveStudentIDs(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	withdrawnIDs, err := s.store.Q.ListWithdrawnStudentIDs(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	known := make(map[string]bool, len(active)+len(withdrawnIDs))
	for _, id := range active {
		known[id] = true
	}
	for _, id := range withdrawnIDs {
		known[id] = true
	}
	var unknown []string
	seen := map[string]bool{}
	for _, id := range body.StudentIDs {
		if !known[id] && !seen[id] {
			unknown = append(unknown, id)
			seen[id] = true
		}
	}
	if len(unknown) > 0 {
		apiError(w, http.StatusBadRequest, "unknown student ids: "+strings.Join(unknown, ", "))
		return
	}

	n, err := s.store.Q.SetStudentsWithdrawnBulk(r.Context(), db.SetStudentsWithdrawnBulkParams{
		Withdrawn: withdrawn, StudentIds: body.StudentIDs,
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "update failed")
		return
	}
	action := "students.bulk_reinstate"
	if withdrawn {
		action = "students.bulk_withdraw"
	}
	// Counts + external ids only in the audit detail (D14 — no names/emails).
	s.audit(r, action, "student", "", map[string]any{"count": n, "student_ids": body.StudentIDs})
	writeJSON(w, http.StatusOK, map[string]int64{"updated": n})
}
