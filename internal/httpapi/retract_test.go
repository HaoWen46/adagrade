package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// gradeStudentAnswer inserts one human grading_records row on any of the given
// student's answers, so the retraction records-guard fires (mirrors the raw
// insert in internal/ingest's TestRetractSubmission_* scope setup).
func gradeStudentAnswer(t *testing.T, e *testEnv, aid int64, studentExternalID, graderEmail string) {
	t.Helper()
	ctx := context.Background()
	student, err := e.st.Q.GetStudentByExternalID(ctx, studentExternalID)
	if err != nil {
		t.Fatalf("get student %s: %v", studentExternalID, err)
	}
	answers, err := e.st.Q.ListAnswersForAssessment(ctx, aid)
	if err != nil {
		t.Fatalf("list answers: %v", err)
	}
	var target db.Answer
	for _, a := range answers {
		if a.StudentID == student.ID {
			target = a
			break
		}
	}
	if target.ID == 0 {
		t.Fatalf("no materialized answer for student %s", studentExternalID)
	}
	inc, err := store.Num("0.5")
	if err != nil {
		t.Fatal(err)
	}
	rv, err := e.st.Q.CreateRubricVersion(ctx, db.CreateRubricVersionParams{ProblemID: target.ProblemID, ScoreIncrement: inc})
	if err != nil {
		t.Fatalf("create rubric version: %v", err)
	}
	u, err := e.st.Q.CreateUser(ctx, db.CreateUserParams{Email: graderEmail, Role: "ta", Active: true})
	if err != nil {
		t.Fatalf("create grader: %v", err)
	}
	total, err := store.Num("7")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.st, `INSERT INTO grading_records (answer_id, source, rubric_version_id, criterion_scores, total, created_by)
		VALUES ($1, 'human', $2, '[]', $3, $4)`, target.ID, rv.ID, total, u.ID)
}

// TestRetractSubmissionRoute drives the HCI-audit retract route
// (POST /api/submissions/{id}/retract) through its whole error-mapping contract:
// an ungraded live submission retracts 200 {"retracted":true}; a graded one is a
// 409 needs_force until force:true lets it through (200); a published one is a 409
// that force can NOT override (and carries no needs_force); an unknown id is 404.
func TestRetractSubmissionRoute(t *testing.T) {
	e := harnessEnv(t)
	// Unlike the one-page default HTTP fixture, this assessment intentionally
	// defines two positional problems.
	e.ing.Renderer = render.NewFake(2)
	ctx := context.Background()
	ta := loginAs(t, e.ts, e.st, "ta@ntu.edu.tw", "ta")
	lect := loginAs(t, e.ts, e.st, "lect@ntu.edu.tw", "lecturer")

	a := postExpect(t, lect, e.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Midterm"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	for n := 1; n <= 2; n++ {
		postExpect(t, lect, fmt.Sprintf("%s/api/assessments/%d/problems", e.ts.URL, aid),
			map[string]any{"number": n, "title": fmt.Sprintf("Q%d", n), "max_points": "10"}, http.StatusCreated)
	}
	for _, sid := range []string{"b01", "b02", "b03"} {
		seedStudent(t, e.st, sid, "Student "+sid, sid+"@x.edu")
	}
	retractURL := func(sid int64) string { return fmt.Sprintf("%s/api/submissions/%d/retract", e.ts.URL, sid) }

	// --- Scenario 1: no grading records → 200 {"retracted": true}, actually unassigns.
	sub1 := e.ing.IngestFile(ctx, aid, "b01.pdf", []byte("%PDF-b01"), 0, false)
	if sub1.Status != "ingested" {
		t.Fatalf("ingest b01: %+v", sub1)
	}
	got1 := postExpect(t, ta, retractURL(sub1.SubmissionID), map[string]any{"force": false}, http.StatusOK)
	if got1["retracted"] != true {
		t.Fatalf("ungraded retract: got %v want {retracted:true}", got1)
	}
	if s, err := e.st.Q.GetSubmission(ctx, sub1.SubmissionID); err != nil || !s.RetractedAt.Valid {
		t.Fatalf("retracted_at should be set after retract: %+v %v", s, err)
	}

	// --- Scenario 2: with grading records → 409 needs_force:true, then 200 with force:true.
	sub2 := e.ing.IngestFile(ctx, aid, "b02.pdf", []byte("%PDF-b02"), 0, false)
	if sub2.Status != "ingested" {
		t.Fatalf("ingest b02: %+v", sub2)
	}
	gradeStudentAnswer(t, e, aid, "b02", "grader-b02@x.edu")

	resp := postJSON(t, ta, retractURL(sub2.SubmissionID), map[string]any{"force": false})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("graded retract without force: got %d want 409", resp.StatusCode)
	}
	var needsForce struct {
		Error      string `json:"error"`
		NeedsForce bool   `json:"needs_force"`
	}
	_ = decodeJSONResp(t, resp, &needsForce)
	resp.Body.Close()
	if !needsForce.NeedsForce || needsForce.Error == "" {
		t.Fatalf("graded retract: got %+v want needs_force:true with a message", needsForce)
	}

	got2 := postExpect(t, ta, retractURL(sub2.SubmissionID), map[string]any{"force": true}, http.StatusOK)
	if got2["retracted"] != true {
		t.Fatalf("graded retract with force: got %v want {retracted:true}", got2)
	}

	// --- Scenario 3: published → 409 that force can NOT override, no needs_force flag.
	sub3 := e.ing.IngestFile(ctx, aid, "b03.pdf", []byte("%PDF-b03"), 0, false)
	if sub3.Status != "ingested" {
		t.Fatalf("ingest b03: %+v", sub3)
	}
	b03, err := e.st.Q.GetStudentByExternalID(ctx, "b03")
	if err != nil {
		t.Fatal(err)
	}
	// The retract guard only reads answers.published_at; set it directly.
	mustExec(t, e.st, `UPDATE answers SET published_at = now() WHERE assessment_id = $1 AND student_id = $2`, aid, b03.ID)

	resp = postJSON(t, ta, retractURL(sub3.SubmissionID), map[string]any{"force": true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("published retract with force: got %d want 409", resp.StatusCode)
	}
	var pub struct {
		Error      string `json:"error"`
		NeedsForce bool   `json:"needs_force"`
	}
	_ = decodeJSONResp(t, resp, &pub)
	resp.Body.Close()
	if pub.NeedsForce || pub.Error == "" {
		t.Fatalf("published retract: got %+v want an error and NO needs_force (force can't override published)", pub)
	}
	// Still live: the blocked retract left the submission untouched.
	if s, err := e.st.Q.GetSubmission(ctx, sub3.SubmissionID); err != nil || s.RetractedAt.Valid {
		t.Fatalf("published submission must stay live after a blocked retract: %+v %v", s, err)
	}

	// --- Scenario 4: unknown submission id → 404.
	resp = postJSON(t, ta, retractURL(999999), map[string]any{"force": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown submission: got %d want 404", resp.StatusCode)
	}
}
