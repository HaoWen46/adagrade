package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// deleteReq sends a DELETE with the CSRF header (like the SPA does).
func deleteReq(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// deleteStudentFixture bundles the minimal assessment -> problem chain the
// roster-delete tests need to seed submissions/answers against, without
// pulling in the full store_test fixture helpers (different package).
type deleteStudentFixture struct {
	AssessmentID int64
	ProblemID    int64
}

func mustDeleteStudentFixture(t *testing.T, st *store.Store) deleteStudentFixture {
	t.Helper()
	ctx := t.Context()
	assessment, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: t.Name() + "-assessment"})
	if err != nil {
		t.Fatalf("CreateAssessment: %v", err)
	}
	maxPoints, err := store.Num("10")
	if err != nil {
		t.Fatalf("Num: %v", err)
	}
	problem, err := st.Q.CreateProblem(ctx, db.CreateProblemParams{
		AssessmentID: assessment.ID, Number: 1, Title: "Problem 1", MaxPoints: maxPoints, Position: 1,
	})
	if err != nil {
		t.Fatalf("CreateProblem: %v", err)
	}
	return deleteStudentFixture{AssessmentID: assessment.ID, ProblemID: problem.ID}
}

// TestDeleteStudent_RequiresAdmin: TA and lecturer are forbidden (B15 is
// admin-only, one rung above the existing lecturer-gated roster actions).
func TestDeleteStudent_RequiresAdmin(t *testing.T) {
	ts, _, st := harness(t)
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")
	ta := loginAs(t, ts, st, "ta@ntu.edu.tw", "ta")
	admin := loginAs(t, ts, st, "admin@ntu.edu.tw", "admin")
	seedStudent(t, st, "b01", "Alice", "a@x.edu")
	student, err := st.Q.GetStudentByExternalID(t.Context(), "b01")
	if err != nil {
		t.Fatalf("GetStudentByExternalID: %v", err)
	}

	for _, c := range []*http.Client{ta, lecturer} {
		resp := deleteReq(t, c, fmt.Sprintf("%s/api/students/%d", ts.URL, student.ID))
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("non-admin delete: got %d want 403", resp.StatusCode)
		}
	}

	// The row must still exist — a rejected request never touches state.
	students := getJSON[map[string][]map[string]any](t, admin, ts.URL+"/api/students", http.StatusOK)
	if len(students["students"]) != 1 {
		t.Fatalf("student should be untouched by forbidden requests: %v", students)
	}

	resp := deleteReq(t, admin, fmt.Sprintf("%s/api/students/%d", ts.URL, student.ID))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin delete: got %d", resp.StatusCode)
	}
}

// TestDeleteStudent_NotFound: an unknown id is a 404, not a silent success.
func TestDeleteStudent_NotFound(t *testing.T) {
	ts, _, st := harness(t)
	admin := loginAs(t, ts, st, "admin@ntu.edu.tw", "admin")

	resp := deleteReq(t, admin, ts.URL+"/api/students/999999")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown student: got %d want 404", resp.StatusCode)
	}
}

// TestDeleteStudent_UnreferencedSucceeds: the B15 happy path — a student with
// no artifacts at all deletes cleanly, disappears from the roster, and is
// audit-logged as roster.delete.
func TestDeleteStudent_UnreferencedSucceeds(t *testing.T) {
	ts, _, st := harness(t)
	admin := loginAs(t, ts, st, "admin@ntu.edu.tw", "admin")
	seedStudent(t, st, "b-typo", "Typo Student", "typo@x.edu")
	student, err := st.Q.GetStudentByExternalID(t.Context(), "b-typo")
	if err != nil {
		t.Fatalf("GetStudentByExternalID: %v", err)
	}

	resp := deleteReq(t, admin, fmt.Sprintf("%s/api/students/%d", ts.URL, student.ID))
	var body map[string]any
	_ = decodeJSONResp(t, resp, &body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: got %d want 200 — %v", resp.StatusCode, body)
	}
	if body["deleted"] != true {
		t.Errorf("response should carry deleted=true: %v", body)
	}

	students := getJSON[map[string][]map[string]any](t, admin, ts.URL+"/api/students", http.StatusOK)
	if len(students["students"]) != 0 {
		t.Fatalf("student should be gone from the roster: %v", students)
	}

	audit := getJSON[map[string]any](t, admin, ts.URL+"/api/audit?action=roster.delete", http.StatusOK)
	entries, _ := audit["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("expected 1 roster.delete audit entry: %v", audit)
	}
}

// TestDeleteStudent_BareAnswerDeletedWithStudent: MaterializeAnswers scaffolding
// (an answer row with no pages, no grading records) must not block deletion,
// and must be removed along with the student in the same transaction.
func TestDeleteStudent_BareAnswerDeletedWithStudent(t *testing.T) {
	ts, _, st := harness(t)
	admin := loginAs(t, ts, st, "admin@ntu.edu.tw", "admin")
	f := mustDeleteStudentFixture(t, st)
	seedStudent(t, st, "b-bare", "Bare Student", "bare@x.edu")
	student, err := st.Q.GetStudentByExternalID(t.Context(), "b-bare")
	if err != nil {
		t.Fatalf("GetStudentByExternalID: %v", err)
	}
	answer, err := st.Q.EnsureAnswer(t.Context(), db.EnsureAnswerParams{
		AssessmentID: f.AssessmentID, StudentID: student.ID, ProblemID: f.ProblemID,
	})
	if err != nil {
		t.Fatalf("EnsureAnswer: %v", err)
	}

	resp := deleteReq(t, admin, fmt.Sprintf("%s/api/students/%d", ts.URL, student.ID))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: got %d want 200", resp.StatusCode)
	}

	if _, err := st.Q.GetStudent(t.Context(), student.ID); err == nil {
		t.Fatalf("student row should be gone")
	}
	if _, err := st.Q.GetAnswer(t.Context(), answer.ID); err == nil {
		t.Fatalf("bare answer row should be gone alongside the student")
	}
}

// TestDeleteStudent_BlockedBySubmission: a real artifact (a submission) blocks
// with 409, names the blocking kind, and points at Withdraw as the alternative
// — and leaves the row untouched.
func TestDeleteStudent_BlockedBySubmission(t *testing.T) {
	ts, _, st := harness(t)
	admin := loginAs(t, ts, st, "admin@ntu.edu.tw", "admin")
	f := mustDeleteStudentFixture(t, st)
	seedStudent(t, st, "b-sub", "Sub Student", "sub@x.edu")
	student, err := st.Q.GetStudentByExternalID(t.Context(), "b-sub")
	if err != nil {
		t.Fatalf("GetStudentByExternalID: %v", err)
	}
	mustExec(t, st, `INSERT INTO submissions (assessment_id, student_id, original_filename, source_ref, source_sha256, page_count)
		VALUES ($1, $2, 'f.pdf', 'ref', 'sha', 1)`, f.AssessmentID, student.ID)

	resp := deleteReq(t, admin, fmt.Sprintf("%s/api/students/%d", ts.URL, student.ID))
	var body struct {
		Error    string   `json:"error"`
		Blocking []string `json:"blocking"`
	}
	_ = decodeJSONResp(t, resp, &body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete referenced student: got %d want 409", resp.StatusCode)
	}
	if len(body.Blocking) != 1 || body.Blocking[0] != "submissions" {
		t.Fatalf("blocking: got %v want [submissions]", body.Blocking)
	}
	if !strings.Contains(strings.ToLower(body.Error), "withdraw") {
		t.Errorf("409 message should point at Withdraw: %q", body.Error)
	}

	// The row must still exist.
	if _, err := st.Q.GetStudent(t.Context(), student.ID); err != nil {
		t.Fatalf("blocked delete must not remove the row: %v", err)
	}
}

// TestDeleteStudent_BlockedByMultipleKinds proves the blocking list names every
// referencing kind at once, not just the first one found.
func TestDeleteStudent_BlockedByMultipleKinds(t *testing.T) {
	ts, _, st := harness(t)
	admin := loginAs(t, ts, st, "admin@ntu.edu.tw", "admin")
	f := mustDeleteStudentFixture(t, st)
	seedStudent(t, st, "b-multi", "Multi Student", "multi@x.edu")
	student, err := st.Q.GetStudentByExternalID(t.Context(), "b-multi")
	if err != nil {
		t.Fatalf("GetStudentByExternalID: %v", err)
	}
	mustExec(t, st, `INSERT INTO submissions (assessment_id, student_id, original_filename, source_ref, source_sha256, page_count)
		VALUES ($1, $2, 'f.pdf', 'ref', 'sha', 1)`, f.AssessmentID, student.ID)
	mustExec(t, st, `INSERT INTO regrade_requests (student_id, assessment_id, from_email, kind)
		VALUES ($1, $2, 'someone@example.test', 'unparsed')`, student.ID, f.AssessmentID)

	resp := deleteReq(t, admin, fmt.Sprintf("%s/api/students/%d", ts.URL, student.ID))
	var body struct {
		Error    string   `json:"error"`
		Blocking []string `json:"blocking"`
	}
	_ = decodeJSONResp(t, resp, &body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete referenced student: got %d want 409", resp.StatusCode)
	}
	got := map[string]bool{}
	for _, k := range body.Blocking {
		got[k] = true
	}
	if !got["submissions"] || !got["regrade_requests"] {
		t.Fatalf("blocking should list both kinds: %v", body.Blocking)
	}
}

func TestPatchStudent_WithdrawnToggleRequiresLecturer(t *testing.T) {
	ts, _, st := harness(t)
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")
	ta := loginAs(t, ts, st, "ta@ntu.edu.tw", "ta")
	seedStudent(t, st, "b01", "Alice", "a@x.edu")

	students := getJSON[map[string][]map[string]any](t, lecturer, ts.URL+"/api/students", http.StatusOK)
	var sid int64
	for _, s := range students["students"] {
		if s["student_id"] == "b01" {
			sid = int64(s["id"].(float64))
		}
	}
	if sid == 0 {
		t.Fatalf("seeded student not found: %v", students)
	}
	if withdrawn, ok := students["students"][0]["withdrawn"]; !ok || withdrawn != false {
		t.Errorf("student JSON should carry withdrawn=false by default: %v", students["students"][0])
	}

	// TA cannot withdraw.
	resp := patchJSON(t, ta, fmt.Sprintf("%s/api/students/%d", ts.URL, sid), map[string]bool{"withdrawn": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ta withdraw: got %d want 403", resp.StatusCode)
	}

	// Lecturer can withdraw; the flag round-trips through the list endpoint.
	resp = patchJSON(t, lecturer, fmt.Sprintf("%s/api/students/%d", ts.URL, sid), map[string]bool{"withdrawn": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lecturer withdraw: got %d", resp.StatusCode)
	}
	students = getJSON[map[string][]map[string]any](t, lecturer, ts.URL+"/api/students", http.StatusOK)
	if students["students"][0]["withdrawn"] != true {
		t.Errorf("withdrawn should now be true: %v", students["students"][0])
	}

	// Lecturer can clear it again.
	resp = patchJSON(t, lecturer, fmt.Sprintf("%s/api/students/%d", ts.URL, sid), map[string]bool{"withdrawn": false})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lecturer un-withdraw: got %d", resp.StatusCode)
	}
	students = getJSON[map[string][]map[string]any](t, lecturer, ts.URL+"/api/students", http.StatusOK)
	if students["students"][0]["withdrawn"] != false {
		t.Errorf("withdrawn should be cleared: %v", students["students"][0])
	}

	// Unknown student id → 404.
	resp = patchJSON(t, lecturer, ts.URL+"/api/students/999999", map[string]bool{"withdrawn": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown student: got %d want 404", resp.StatusCode)
	}
}

// Roster-lifecycle plan 2026-07-10: bulk withdraw/reinstate back the import-diff
// sync buttons. Lecturer+, strict unknown-id validation (400 listing them),
// {"updated": n}, audit-logged with counts + external ids only.
func TestBulkWithdrawReinstate(t *testing.T) {
	ts, _, st := harness(t)
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")
	ta := loginAs(t, ts, st, "ta@ntu.edu.tw", "ta")
	admin := loginAs(t, ts, st, "admin@ntu.edu.tw", "admin")
	seedStudent(t, st, "b01", "Alice", "a@x.edu")
	seedStudent(t, st, "b02", "Bob", "b@x.edu")
	seedStudent(t, st, "b03", "Carol", "c@x.edu")

	// TA is forbidden on both endpoints.
	for _, path := range []string{"/api/students/bulk-withdraw", "/api/students/bulk-reinstate"} {
		resp := postJSON(t, ta, ts.URL+path, map[string]any{"student_ids": []string{"b01"}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("ta %s: got %d want 403", path, resp.StatusCode)
		}
	}

	// Missing/empty student_ids → 400.
	resp := postJSON(t, lecturer, ts.URL+"/api/students/bulk-withdraw", map[string]any{"student_ids": []string{}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty ids: got %d want 400", resp.StatusCode)
	}

	// Unknown ids → 400 listing them, and nothing is updated.
	resp = postJSON(t, lecturer, ts.URL+"/api/students/bulk-withdraw", map[string]any{"student_ids": []string{"b01", "b99"}})
	var errBody struct {
		Error string `json:"error"`
	}
	_ = decodeJSONResp(t, resp, &errBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(errBody.Error, "b99") {
		t.Fatalf("unknown id: got %d %q, want 400 naming b99", resp.StatusCode, errBody.Error)
	}
	students := getJSON[map[string][]map[string]any](t, lecturer, ts.URL+"/api/students", http.StatusOK)
	for _, s := range students["students"] {
		if s["withdrawn"] == true {
			t.Fatalf("strict validation must not partially withdraw: %v", s)
		}
	}

	// Withdraw two; the flag round-trips through the list endpoint.
	got := postExpect(t, lecturer, ts.URL+"/api/students/bulk-withdraw", map[string]any{"student_ids": []string{"b01", "b02"}}, http.StatusOK)
	if got["updated"].(float64) != 2 {
		t.Fatalf("bulk-withdraw updated: %v", got)
	}
	students = getJSON[map[string][]map[string]any](t, lecturer, ts.URL+"/api/students", http.StatusOK)
	withdrawnCount := 0
	for _, s := range students["students"] {
		if s["withdrawn"] == true {
			withdrawnCount++
			if id := s["student_id"]; id != "b01" && id != "b02" {
				t.Errorf("unexpected withdrawn student: %v", id)
			}
		}
	}
	if withdrawnCount != 2 {
		t.Errorf("want 2 withdrawn students, got %d", withdrawnCount)
	}

	// Reinstate accepts ids in any state (idempotent target state).
	got = postExpect(t, lecturer, ts.URL+"/api/students/bulk-reinstate", map[string]any{"student_ids": []string{"b01", "b02"}}, http.StatusOK)
	if got["updated"].(float64) != 2 {
		t.Fatalf("bulk-reinstate updated: %v", got)
	}
	students = getJSON[map[string][]map[string]any](t, lecturer, ts.URL+"/api/students", http.StatusOK)
	for _, s := range students["students"] {
		if s["withdrawn"] == true {
			t.Errorf("student should be reinstated: %v", s)
		}
	}

	// Both actions are audit-logged (admin read path).
	for _, action := range []string{"students.bulk_withdraw", "students.bulk_reinstate"} {
		audit := getJSON[map[string]any](t, admin, ts.URL+"/api/audit?action="+action, http.StatusOK)
		entries, _ := audit["entries"].([]any)
		if len(entries) != 1 {
			t.Errorf("audit %s: want 1 entry, got %v", action, audit)
		}
	}
}

// Import response carries the roster diff (fix 1): actives missing from the CSV,
// withdrawn students present in it, changed-field counts. Reporting only — the
// import itself never flips withdrawn state.
func TestRosterImport_Diff(t *testing.T) {
	ts, _, st := harness(t)
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	// The diff flattens additively into the report (frontend types.ts contract).
	type report struct {
		Added            int      `json:"added"`
		Total            int      `json:"total"`
		MissingActive    []string `json:"missing_active"`
		WithdrawnPresent []string `json:"withdrawn_present"`
		EmailChanged     int64    `json:"email_changed"`
		NameChanged      int64    `json:"name_changed"`
	}

	// v1: three students, empty diff.
	resp := uploadRoster(t, lecturer, ts.URL, "student_id,name,email\nb01,Alice,a@x.edu\nb02,Bob,b@x.edu\nb03,Carol,c@x.edu\n")
	var rep report
	_ = decodeJSONResp(t, resp, &rep)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || rep.Added != 3 {
		t.Fatalf("v1 import: %d %+v", resp.StatusCode, rep)
	}
	if len(rep.MissingActive) != 0 || len(rep.WithdrawnPresent) != 0 {
		t.Fatalf("v1 diff should be empty: %+v", rep)
	}

	// Withdraw b03, then re-import a CSV missing b02 but containing b03 with a
	// changed email.
	postExpect(t, lecturer, ts.URL+"/api/students/bulk-withdraw", map[string]any{"student_ids": []string{"b03"}}, http.StatusOK)
	resp = uploadRoster(t, lecturer, ts.URL, "student_id,name,email\nb01,Alice,a@x.edu\nb03,Carol,c-new@x.edu\n")
	rep = report{}
	_ = decodeJSONResp(t, resp, &rep)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("v2 import: %d", resp.StatusCode)
	}
	if len(rep.MissingActive) != 1 || rep.MissingActive[0] != "b02" {
		t.Errorf("missing_active: %+v", rep)
	}
	if len(rep.WithdrawnPresent) != 1 || rep.WithdrawnPresent[0] != "b03" {
		t.Errorf("withdrawn_present: %+v", rep)
	}
	if rep.EmailChanged != 1 || rep.NameChanged != 0 {
		t.Errorf("changed counts: %+v", rep)
	}

	// The diff reported, but never mutated: b02 still active, b03 still withdrawn.
	students := getJSON[map[string][]map[string]any](t, lecturer, ts.URL+"/api/students", http.StatusOK)
	for _, s := range students["students"] {
		if s["student_id"] == "b02" && s["withdrawn"] != false {
			t.Errorf("b02 must stay active: %v", s)
		}
		if s["student_id"] == "b03" && s["withdrawn"] != true {
			t.Errorf("b03 must stay withdrawn: %v", s)
		}
	}
}

// Fixes 2+3 at the HTTP surface: non-UTF-8 files and duplicate emails reject the
// whole import through the existing per-line error envelope.
func TestRosterImport_RejectsNonUTF8AndDuplicateEmails(t *testing.T) {
	ts, _, st := harness(t)
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")
	seedStudent(t, st, "b01", "Alice", "a@x.edu")

	type errBody struct {
		Error  string `json:"error"`
		Errors []struct {
			Line int    `json:"line"`
			Msg  string `json:"msg"`
		} `json:"errors"`
	}

	// Big5 bytes → single actionable error.
	resp := uploadRoster(t, lecturer, ts.URL, "student_id,name,email\nb02,\xa4\xa4\xa4\xe5,b@x.edu\n")
	var eb errBody
	_ = decodeJSONResp(t, resp, &eb)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || len(eb.Errors) != 1 {
		t.Fatalf("non-UTF-8: %d %+v", resp.StatusCode, eb)
	}
	if want := "file is not valid UTF-8 — in Excel, use Save As → 'CSV UTF-8 (Comma delimited)'"; eb.Errors[0].Msg != want {
		t.Errorf("msg: got %q want %q", eb.Errors[0].Msg, want)
	}

	// Cross-DB duplicate email → row error naming the owning student, all-or-nothing.
	resp = uploadRoster(t, lecturer, ts.URL, "student_id,name,email\nb05,Eve,A@X.edu\n")
	eb = errBody{}
	_ = decodeJSONResp(t, resp, &eb)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || len(eb.Errors) != 1 {
		t.Fatalf("cross-DB dup email: %d %+v", resp.StatusCode, eb)
	}
	if want := "email already belongs to student b01"; eb.Errors[0].Msg != want || eb.Errors[0].Line != 2 {
		t.Errorf("row error: %+v", eb.Errors[0])
	}
	students := getJSON[map[string][]map[string]any](t, lecturer, ts.URL+"/api/students", http.StatusOK)
	if len(students["students"]) != 1 {
		t.Errorf("all-or-nothing: b05 must not exist: %v", students["students"])
	}

	// In-file duplicate email → rejected at parse with both ids, never the address.
	resp = uploadRoster(t, lecturer, ts.URL, "student_id,name,email\nb06,Frank,f@x.edu\nb07,Grace,F@X.edu\n")
	eb = errBody{}
	_ = decodeJSONResp(t, resp, &eb)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || len(eb.Errors) != 1 {
		t.Fatalf("in-file dup email: %d %+v", resp.StatusCode, eb)
	}
	if msg := eb.Errors[0].Msg; !strings.Contains(msg, "b06") || !strings.Contains(msg, "b07") || strings.Contains(msg, "@") {
		t.Errorf("in-file dup email msg: %q", msg)
	}
}

// uploadRoster posts a multipart roster CSV like the SPA's import form.
func uploadRoster(t *testing.T, c *http.Client, baseURL, csv string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "roster.csv")
	_, _ = fw.Write([]byte(csv))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/students/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// patchJSON sends a JSON PATCH with the CSRF header (like the SPA does).
func patchJSON(t *testing.T, c *http.Client, url string, body any) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, url, strings.NewReader(mustJSON(t, body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
