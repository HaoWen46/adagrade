package httpapi

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// --- wire shapes -------------------------------------------------------------------
//
// Decoded into explicit structs rather than map[string]any so the field NAMES are
// pinned by the compiler: the per-student page's frontend is written against this
// exact contract (design doc 2026-07-28-student-page-design.md §API), and a rename
// here is a silent break there. Nullable fields are pointers so "absent/ungraded"
// (JSON null, D3 — never a fake 0) is distinguishable from a real zero.

type spSummaryProblem struct {
	Number   int32   `json:"number"`
	Title    string  `json:"title"`
	AnswerID *int64  `json:"answer_id"`
	Score    *string `json:"score"`
	Max      string  `json:"max"`
}

type spSummaryAssessment struct {
	AssessmentID int64              `json:"assessment_id"`
	Name         string             `json:"name"`
	Kind         string             `json:"kind"`
	Answers      int64              `json:"answers"`
	Graded       int64              `json:"graded"`
	Total        *string            `json:"total"`
	Max          string             `json:"max"`
	Published    bool               `json:"published"`
	Problems     []spSummaryProblem `json:"problems"`
}

type spSummary struct {
	Student struct {
		StudentID string `json:"student_id"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		Withdrawn bool   `json:"withdrawn"`
	} `json:"student"`
	Assessments []spSummaryAssessment `json:"assessments"`
}

type spPublish struct {
	BatchCreatedAt      *time.Time `json:"batch_created_at"`
	EmailStatus         string     `json:"email_status"`
	SentAt              *time.Time `json:"sent_at"`
	RecipientEmail      string     `json:"recipient_email"`
	SnapshotTotal       *string    `json:"snapshot_total"`
	ChangedSincePublish bool       `json:"changed_since_publish"`
}

type spDetailProblem struct {
	Number         int32    `json:"number"`
	AnswerID       *int64   `json:"answer_id"`
	Source         *string  `json:"source"`
	ModelID        *string  `json:"model_id"`
	Confidence     *string  `json:"confidence"`
	Flags          []string `json:"flags"`
	PublishedScore *string  `json:"published_score"`
	CurrentScore   *string  `json:"current_score"`
	Changed        bool     `json:"changed"`
}

type spRegradeProblem struct {
	Number  int32   `json:"number"`
	Verdict *string `json:"verdict"`
}

type spRegrade struct {
	RequestID  int64              `json:"request_id"`
	ReceivedAt time.Time          `json:"received_at"`
	Status     string             `json:"status"`
	Problems   []spRegradeProblem `json:"problems"`
}

type spDetail struct {
	Publish  *spPublish        `json:"publish"`
	Problems []spDetailProblem `json:"problems"`
	Regrades []spRegrade       `json:"regrades"`
}

// --- fixture -----------------------------------------------------------------------

// spExternalID is the school id the page is keyed by (students.student_id) — the
// same vocabulary as the export filenames. Invented, like all fixture data
// (CLAUDE.md: never real student PII).
const spExternalID = "B11902003"

// studentPageFixture is one assessment with TWO problems where the student has an
// answers row for P1 only: P2 is created after ingest, so no MaterializeAnswers pass
// ever created its answer — exactly the "absent" problem the page must still render
// (with a null answer_id) instead of dropping.
type studentPageFixture struct {
	env       *testEnv
	ts        *httptest.Server
	c         *http.Client
	st        *store.Store
	aid       int64
	p1ID      int64
	p2ID      int64
	rubricID  int64
	critIDs   []int64
	answerID  int64
	studentID int64
}

func studentPageSetup(t *testing.T) studentPageFixture {
	t.Helper()
	env := harnessEnv(t)
	ts, st := env.ts, env.st
	c := loginAs(t, ts, st, "lect-sp@ntu.edu.tw", "lecturer")

	a := postExpect(t, c, ts.URL+"/api/assessments",
		map[string]string{"kind": "exam", "name": "Student Page Exam"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	p1 := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/problems", ts.URL, aid),
		map[string]any{"number": 1, "title": "Binary search complexity", "max_points": "10"}, http.StatusCreated)
	p1ID := int64(p1["id"].(float64))
	rv := postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/rubric", ts.URL, p1ID), map[string]any{
		"score_increment": "0.5",
		"criteria": []map[string]string{
			{"description": "Correctness", "points": "6"},
			{"description": "Clarity", "points": "4"},
		},
	}, http.StatusCreated)
	rubricID := int64(rv["id"].(float64))
	var critIDs []int64
	for _, cr := range rv["criteria"].([]any) {
		critIDs = append(critIDs, int64(cr.(map[string]any)["id"].(float64)))
	}
	// Round-based grading (0027): officials are derived from the chosen final
	// source. Consensus + no aggregates anywhere ⇒ each human record posted below
	// becomes that answer's official via the manual-record recompute hook.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)

	seedStudent(t, st, spExternalID, "Ada Fake", "ada@example.edu")
	uploadFakePDF(t, c, ts, aid, spExternalID+".pdf")
	driveDirectUploads(t, env, aid)

	students := getJSON[map[string][]map[string]any](t, c,
		fmt.Sprintf("%s/api/problems/%d/students", ts.URL, p1ID), http.StatusOK)
	var answerID int64
	for _, s := range students["students"] {
		if s["student_id"].(string) == spExternalID {
			answerID = int64(s["answer_id"].(float64))
		}
	}
	if answerID == 0 {
		t.Fatalf("fixture: no answer materialized for %s on P1", spExternalID)
	}

	p2 := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/problems", ts.URL, aid),
		map[string]any{"number": 2, "title": "Amortized analysis", "max_points": "10"}, http.StatusCreated)
	p2ID := int64(p2["id"].(float64))

	stu, err := st.Q.GetStudentByExternalID(t.Context(), spExternalID)
	if err != nil {
		t.Fatalf("fixture: load student: %v", err)
	}

	return studentPageFixture{
		env: env, ts: ts, c: c, st: st,
		aid: aid, p1ID: p1ID, p2ID: p2ID, rubricID: rubricID, critIDs: critIDs,
		answerID: answerID, studentID: stu.ID,
	}
}

// grade files a human record on the fixture's P1 answer and returns its id. Whether
// it lands as the official is the recompute hook's business (it does not, once the
// answer is published — that immutability is what the changed-since-publish test
// leans on).
func (f studentPageFixture) grade(t *testing.T, s1, s2 string) int64 {
	t.Helper()
	rec := postExpect(t, f.c, fmt.Sprintf("%s/api/answers/%d/records", f.ts.URL, f.answerID), map[string]any{
		"rubric_version_id": f.rubricID,
		"comment":           "graded",
		"scores": []map[string]any{
			{"criterion_id": f.critIDs[0], "score": s1},
			{"criterion_id": f.critIDs[1], "score": s2},
		},
	}, http.StatusCreated)
	return int64(rec["id"].(float64))
}

func (f studentPageFixture) summaryURL() string {
	return fmt.Sprintf("%s/api/students/%s", f.ts.URL, spExternalID)
}

func (f studentPageFixture) detailURL() string {
	return fmt.Sprintf("%s/api/students/%s/assessments/%d", f.ts.URL, spExternalID, f.aid)
}

// publish runs the real publish endpoint and returns the student's publish item.
func (f studentPageFixture) publish(t *testing.T) db.ListPublishItemsRow {
	t.Helper()
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid),
		map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))
	items, err := f.st.ListPublishItems(t.Context(), batchID)
	if err != nil {
		t.Fatalf("list publish items: %v", err)
	}
	for _, it := range items {
		if it.StudentID == f.studentID {
			return it
		}
	}
	t.Fatalf("publish batch %d has no item for student %d", batchID, f.studentID)
	return db.ListPublishItemsRow{}
}

// seedRegrade files one regrade request against the live publish item with a single
// contested sub-item. complaintText is student content — the point of seeding it is
// to prove it never reaches the wire (PII rule, CLAUDE.md / D14).
func (f studentPageFixture) seedRegrade(t *testing.T, itemID int64, status, complaintText, body string) (requestID, subItemID int64) {
	t.Helper()
	rr, err := f.st.InsertRegradeRequestV2(t.Context(), store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.studentID, AssessmentID: f.aid,
		FromEmail: "ada@example.edu", Subject: "re: grade", Body: body,
		Status: status, Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}
	sub, err := f.st.Q.InsertRequestProblem(t.Context(), db.InsertRequestProblemParams{
		RequestID: rr.ID, ProblemID: f.p1ID, ComplaintText: complaintText,
	})
	if err != nil {
		t.Fatalf("InsertRequestProblem: %v", err)
	}
	return rr.ID, sub.ID
}

// --- unknown student ----------------------------------------------------------------

// TestStudentPage_UnknownStudent_404 pins the lookup key: the page is addressed by
// the SCHOOL id (students.student_id), exact match, and an id nobody on the roster
// owns is a 404 — not an empty 200 that would render as a real, blank student.
func TestStudentPage_UnknownStudent_404(t *testing.T) {
	f := studentPageSetup(t)

	resp, err := f.c.Get(f.ts.URL + "/api/students/B00000000")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown student: got %d want 404", resp.StatusCode)
	}

	resp2, err := f.c.Get(fmt.Sprintf("%s/api/students/B00000000/assessments/%d", f.ts.URL, f.aid))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown student detail: got %d want 404", resp2.StatusCode)
	}
}

// --- summary ------------------------------------------------------------------------

// TestStudentPage_Summary_GradedAndAbsentProblem is the collapsed-card contract: the
// header, one assessment card with its official total, and problem rows covering
// EVERY problem in number order — including the one the student has no answers row
// for, which carries null answer_id/score so the UI can render "absent" instead of a
// fake 0 (D3).
func TestStudentPage_Summary_GradedAndAbsentProblem(t *testing.T) {
	f := studentPageSetup(t)
	f.grade(t, "5", "3.5") // 8.5 / 10 on P1; P2 has no answer at all

	got := getJSON[spSummary](t, f.c, f.summaryURL(), http.StatusOK)

	if got.Student.StudentID != spExternalID || got.Student.Name != "Ada Fake" ||
		got.Student.Email != "ada@example.edu" || got.Student.Withdrawn {
		t.Fatalf("student header = %+v", got.Student)
	}
	if len(got.Assessments) != 1 {
		t.Fatalf("assessments = %d, want 1", len(got.Assessments))
	}
	a := got.Assessments[0]
	if a.AssessmentID != f.aid || a.Name != "Student Page Exam" || a.Kind != "exam" {
		t.Errorf("assessment identity = %+v", a)
	}
	if a.Answers != 1 || a.Graded != 1 {
		t.Errorf("answers/graded = %d/%d, want 1/1", a.Answers, a.Graded)
	}
	if a.Total == nil || *a.Total != "8.5" {
		t.Errorf("total = %v, want \"8.5\" (exact decimal string, D4)", a.Total)
	}
	if a.Max != "20" {
		t.Errorf("max = %q, want \"20\" (Σ max_points over every problem)", a.Max)
	}
	if a.Published {
		t.Errorf("published = true before any publish batch")
	}

	if len(a.Problems) != 2 {
		t.Fatalf("problems = %d, want every problem of the assessment (2)", len(a.Problems))
	}
	p1, p2 := a.Problems[0], a.Problems[1]
	if p1.Number != 1 || p2.Number != 2 {
		t.Fatalf("problems must be in number order, got %d then %d", p1.Number, p2.Number)
	}
	if p1.Title != "Binary search complexity" || p1.Max != "10" {
		t.Errorf("P1 = %+v", p1)
	}
	if p1.AnswerID == nil || *p1.AnswerID != f.answerID {
		t.Errorf("P1 answer_id = %v, want %d", p1.AnswerID, f.answerID)
	}
	if p1.Score == nil || *p1.Score != "8.5" {
		t.Errorf("P1 score = %v, want \"8.5\"", p1.Score)
	}
	if p2.AnswerID != nil {
		t.Errorf("absent P2 answer_id = %v, want null", *p2.AnswerID)
	}
	if p2.Score != nil {
		t.Errorf("absent P2 score = %v, want null (never a fake 0, D3)", *p2.Score)
	}
}

// TestStudentPage_Summary_NullTotalWhenNothingGraded is D3 at the card level: an
// assessment the student has answers in but nothing official yet reports total null,
// not "0" — "0" is a claim about the student's work that nobody has made.
func TestStudentPage_Summary_NullTotalWhenNothingGraded(t *testing.T) {
	f := studentPageSetup(t)

	got := getJSON[spSummary](t, f.c, f.summaryURL(), http.StatusOK)
	if len(got.Assessments) != 1 {
		t.Fatalf("assessments = %d, want 1", len(got.Assessments))
	}
	a := got.Assessments[0]
	if a.Graded != 0 {
		t.Errorf("graded = %d, want 0", a.Graded)
	}
	if a.Total != nil {
		t.Errorf("total = %q, want null when nothing is graded (D3)", *a.Total)
	}
	if len(a.Problems) != 2 || a.Problems[0].Score != nil {
		t.Errorf("problem scores should all be null, got %+v", a.Problems)
	}
}

// TestStudentPage_Summary_OnlyAssessmentsWithAnswers: a second assessment the
// student never sat (no answers row) must not appear as an empty card.
func TestStudentPage_Summary_OnlyAssessmentsWithAnswers(t *testing.T) {
	f := studentPageSetup(t)
	postExpect(t, f.c, f.ts.URL+"/api/assessments",
		map[string]string{"kind": "assignment", "name": "Homework nobody sat"}, http.StatusCreated)

	got := getJSON[spSummary](t, f.c, f.summaryURL(), http.StatusOK)
	if len(got.Assessments) != 1 || got.Assessments[0].AssessmentID != f.aid {
		t.Fatalf("assessments = %+v, want only the one with answers rows", got.Assessments)
	}
}

// TestStudentPage_Summary_OrderedNewestFirst: cards run created_at DESC — the paper
// the student just sat is the one a TA opening this page is looking for.
func TestStudentPage_Summary_OrderedNewestFirst(t *testing.T) {
	f := studentPageSetup(t)

	a2 := postExpect(t, f.c, f.ts.URL+"/api/assessments",
		map[string]string{"kind": "assignment", "name": "Later Homework"}, http.StatusCreated)
	aid2 := int64(a2["id"].(float64))
	postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/problems", f.ts.URL, aid2),
		map[string]any{"number": 1, "title": "Recurrences", "max_points": "5"}, http.StatusCreated)
	uploadFakePDF(t, f.c, f.ts, aid2, spExternalID+".pdf")
	driveDirectUploads(t, f.env, aid2)

	got := getJSON[spSummary](t, f.c, f.summaryURL(), http.StatusOK)
	if len(got.Assessments) != 2 {
		t.Fatalf("assessments = %d, want 2", len(got.Assessments))
	}
	if got.Assessments[0].AssessmentID != aid2 || got.Assessments[1].AssessmentID != f.aid {
		t.Errorf("order = [%d %d], want newest first [%d %d]",
			got.Assessments[0].AssessmentID, got.Assessments[1].AssessmentID, aid2, f.aid)
	}
	if got.Assessments[0].Kind != "assignment" || got.Assessments[0].Max != "5" {
		t.Errorf("newest card = %+v, want the assignment with max \"5\"", got.Assessments[0])
	}
}

// TestStudentPage_Summary_WithdrawnFlag: withdrawn is withdrawn_at IS NOT NULL (D23)
// — history stays fully visible, the header just says so.
func TestStudentPage_Summary_WithdrawnFlag(t *testing.T) {
	f := studentPageSetup(t)
	if _, err := f.st.Q.SetStudentWithdrawn(t.Context(), db.SetStudentWithdrawnParams{
		ID: f.studentID, Withdrawn: true,
	}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	got := getJSON[spSummary](t, f.c, f.summaryURL(), http.StatusOK)
	if !got.Student.Withdrawn {
		t.Error("withdrawn = false after withdrawing the student")
	}
	if len(got.Assessments) != 1 {
		t.Errorf("withdrawing must not hide history, got %d assessments", len(got.Assessments))
	}
}

// --- detail: publish -----------------------------------------------------------------

// TestStudentPage_Detail_PublishNullWhenNoBatch: before any publish there is nothing
// the student believes, so the whole publish object is null (not a zero-valued one
// that would render as "sent, blank").
func TestStudentPage_Detail_PublishNullWhenNoBatch(t *testing.T) {
	f := studentPageSetup(t)
	f.grade(t, "5", "3.5")

	got := getJSON[spDetail](t, f.c, f.detailURL(), http.StatusOK)
	if got.Publish != nil {
		t.Fatalf("publish = %+v, want null with no publish batch", got.Publish)
	}
	if len(got.Problems) != 2 {
		t.Fatalf("problems = %d, want every problem (2)", len(got.Problems))
	}
	p1 := got.Problems[0]
	if p1.Source == nil || *p1.Source != "human" {
		t.Errorf("P1 source = %v, want \"human\"", p1.Source)
	}
	if p1.ModelID != nil {
		t.Errorf("P1 model_id = %v, want null for a human record", *p1.ModelID)
	}
	if p1.CurrentScore == nil || *p1.CurrentScore != "8.5" {
		t.Errorf("P1 current_score = %v, want \"8.5\"", p1.CurrentScore)
	}
	if p1.PublishedScore != nil {
		t.Errorf("P1 published_score = %v, want null with no snapshot", *p1.PublishedScore)
	}
	if p1.Changed {
		t.Error("P1 changed = true with nothing published")
	}
	if p1.Flags == nil {
		t.Error("flags must be an array, never null")
	}
	if got.Regrades == nil {
		t.Error("regrades must be an array, never null")
	}
}

// TestStudentPage_Detail_PublishStateFromLiveBatch: with a live (non-superseded)
// batch the publish object carries the delivery facts and the snapshot total, and
// nothing has changed since — the badge stays off.
func TestStudentPage_Detail_PublishStateFromLiveBatch(t *testing.T) {
	f := studentPageSetup(t)
	f.grade(t, "5", "3.5")
	item := f.publish(t)

	got := getJSON[spDetail](t, f.c, f.detailURL(), http.StatusOK)
	if got.Publish == nil {
		t.Fatal("publish = null after publishing")
	}
	if got.Publish.EmailStatus != item.EmailStatus {
		t.Errorf("email_status = %q, want %q", got.Publish.EmailStatus, item.EmailStatus)
	}
	if got.Publish.RecipientEmail != "ada@example.edu" {
		t.Errorf("recipient_email = %q", got.Publish.RecipientEmail)
	}
	if got.Publish.BatchCreatedAt == nil {
		t.Error("batch_created_at = null")
	}
	if got.Publish.SnapshotTotal == nil || *got.Publish.SnapshotTotal != "8.5" {
		t.Errorf("snapshot_total = %v, want \"8.5\"", got.Publish.SnapshotTotal)
	}
	if got.Publish.ChangedSincePublish {
		t.Error("changed_since_publish = true immediately after publishing")
	}
	p1 := got.Problems[0]
	if p1.PublishedScore == nil || *p1.PublishedScore != "8.5" {
		t.Errorf("P1 published_score = %v, want \"8.5\"", p1.PublishedScore)
	}
	if p1.Changed {
		t.Error("P1 changed = true immediately after publishing")
	}
}

// TestStudentPage_Detail_ChangedSincePublishAfterOverride is the badge the page
// exists for: what the student was told vs. what is true now. Publishing freezes the
// snapshot AND the answer (published answers are immutable — RecomputeOfficials only
// touches published_at IS NULL rows), so the only way the effective grade moves under
// a LIVE batch is an adjudicated regrade overlay (rounds design, 0028). That is
// exactly the state the badge must catch: a regrade was adopted, and nobody has
// re-published the new number to the student yet.
func TestStudentPage_Detail_ChangedSincePublishAfterOverride(t *testing.T) {
	f := studentPageSetup(t)
	f.grade(t, "5", "3.5") // published as 8.5
	item := f.publish(t)

	// The corrected grade, adopted as this turn's overlay.
	newRecord := f.grade(t, "6", "4") // 10
	_, subItemID := f.seedRegrade(t, item.ID, "resolved_regraded", "please recheck part b", "reply body")
	if _, err := f.st.Q.SetProblemVerdictAndAdoption(t.Context(), db.SetProblemVerdictAndAdoptionParams{
		ID: subItemID, Verdict: pgtype.Text{String: "regraded", Valid: true},
		VerdictNote:     "accepted",
		AdoptedRecordID: pgtype.Int8{Int64: newRecord, Valid: true},
	}); err != nil {
		t.Fatalf("SetProblemVerdictAndAdoption: %v", err)
	}

	got := getJSON[spDetail](t, f.c, f.detailURL(), http.StatusOK)
	if got.Publish == nil {
		t.Fatal("publish = null; the batch is still live")
	}
	if *got.Publish.SnapshotTotal != "8.5" {
		t.Errorf("snapshot_total = %v, want the frozen \"8.5\"", got.Publish.SnapshotTotal)
	}
	if !got.Publish.ChangedSincePublish {
		t.Error("changed_since_publish = false after an adopted regrade moved the grade")
	}
	p1 := got.Problems[0]
	if p1.PublishedScore == nil || *p1.PublishedScore != "8.5" {
		t.Errorf("P1 published_score = %v, want \"8.5\"", p1.PublishedScore)
	}
	if p1.CurrentScore == nil || *p1.CurrentScore != "10" {
		t.Errorf("P1 current_score = %v, want \"10\"", p1.CurrentScore)
	}
	if !p1.Changed {
		t.Error("P1 changed = false though published 8.5 ≠ current 10")
	}

	// The summary card must agree with the detail — it is the same effective grade.
	sum := getJSON[spSummary](t, f.c, f.summaryURL(), http.StatusOK)
	if sum.Assessments[0].Total == nil || *sum.Assessments[0].Total != "10" {
		t.Errorf("summary total = %v, want \"10\" (same effective grade as the detail)", sum.Assessments[0].Total)
	}
	if !sum.Assessments[0].Published {
		t.Error("summary published = false with a live batch containing this student")
	}
}

// --- detail: regrades ----------------------------------------------------------------

// TestStudentPage_Detail_RegradeRowsPresent: the regrade thread summary — request,
// its own status column value (migrations 0017/0025/0028), and per-problem verdicts,
// with null for a sub-item still in progress.
func TestStudentPage_Detail_RegradeRowsPresent(t *testing.T) {
	f := studentPageSetup(t)
	f.grade(t, "5", "3.5")
	item := f.publish(t)
	reqID, subItemID := f.seedRegrade(t, item.ID, "received", "part b was marked wrong", "reply body")

	got := getJSON[spDetail](t, f.c, f.detailURL(), http.StatusOK)
	if len(got.Regrades) != 1 {
		t.Fatalf("regrades = %d, want 1", len(got.Regrades))
	}
	rg := got.Regrades[0]
	if rg.RequestID != reqID {
		t.Errorf("request_id = %d, want %d", rg.RequestID, reqID)
	}
	if rg.Status != "received" {
		t.Errorf("status = %q, want the request's own status column value \"received\"", rg.Status)
	}
	if rg.ReceivedAt.IsZero() {
		t.Error("received_at is zero")
	}
	if len(rg.Problems) != 1 || rg.Problems[0].Number != 1 {
		t.Fatalf("regrade problems = %+v, want one entry for P1", rg.Problems)
	}
	if rg.Problems[0].Verdict != nil {
		t.Errorf("verdict = %v, want null while in progress", *rg.Problems[0].Verdict)
	}

	// Adjudicate: the verdict shows up.
	if _, err := f.st.Q.SetProblemVerdict(t.Context(), db.SetProblemVerdictParams{
		ID: subItemID, Verdict: pgtype.Text{String: "upheld", Valid: true}, VerdictNote: "no change",
	}); err != nil {
		t.Fatalf("SetProblemVerdict: %v", err)
	}
	got2 := getJSON[spDetail](t, f.c, f.detailURL(), http.StatusOK)
	v := got2.Regrades[0].Problems[0].Verdict
	if v == nil || *v != "upheld" {
		t.Errorf("verdict = %v, want \"upheld\"", v)
	}
}

// TestStudentPage_Detail_NeverLeaksStudentText is the PII rule as an assertion
// (CLAUDE.md, D14): regrade_request_problems.complaint_text and regrade_requests.body
// are student content of the same class as transcriptions. Verdicts, statuses,
// numbers and timestamps are the whole payload — the raw response bytes must not
// contain either string.
func TestStudentPage_Detail_NeverLeaksStudentText(t *testing.T) {
	f := studentPageSetup(t)
	f.grade(t, "5", "3.5")
	item := f.publish(t)
	const complaint = "COMPLAINTNEEDLE-my proof of part b was correct"
	const body = "BODYNEEDLE-please look again"
	_, subItemID := f.seedRegrade(t, item.ID, "under_review", complaint, body)

	// Non-vacuity: the needles really are in the database, so a clean response is
	// the endpoint withholding them, not the fixture forgetting to store them.
	sub, err := f.st.Q.GetRequestProblem(t.Context(), subItemID)
	if err != nil {
		t.Fatalf("GetRequestProblem: %v", err)
	}
	if sub.ComplaintText != complaint {
		t.Fatalf("fixture did not store the complaint text (got %d bytes)", len(sub.ComplaintText))
	}

	raw := getJSONRaw(t, f.c, f.detailURL())
	if bytes.Contains(raw, []byte("COMPLAINTNEEDLE")) {
		t.Error("detail response leaked regrade_request_problems.complaint_text")
	}
	if bytes.Contains(raw, []byte("BODYNEEDLE")) {
		t.Error("detail response leaked regrade_requests.body")
	}
	rawSummary := getJSONRaw(t, f.c, f.summaryURL())
	if bytes.Contains(rawSummary, []byte("COMPLAINTNEEDLE")) || bytes.Contains(rawSummary, []byte("BODYNEEDLE")) {
		t.Error("summary response leaked student regrade text")
	}
}

// --- detail: misc guards --------------------------------------------------------------

// TestStudentPage_Detail_UnknownAssessment_404 keeps a typo'd URL from rendering as
// a real, empty exam.
func TestStudentPage_Detail_UnknownAssessment_404(t *testing.T) {
	f := studentPageSetup(t)
	resp, err := f.c.Get(fmt.Sprintf("%s/api/students/%s/assessments/999999", f.ts.URL, spExternalID))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown assessment: got %d want 404", resp.StatusCode)
	}
}

// TestStudentPage_RequiresSession: ordinary authed staff routes — no anonymous read
// of a student's whole history.
func TestStudentPage_RequiresSession(t *testing.T) {
	f := studentPageSetup(t)
	anon := &http.Client{}
	for _, url := range []string{f.summaryURL(), f.detailURL()} {
		resp, err := anon.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without session: got %d want 401", url, resp.StatusCode)
		}
	}
}

// TestStudentPage_TAMayRead: the page inherits AnswerView's exposure (design doc:
// TA data-scoping B-M15 stays open repo-wide) — it must not be lecturer-gated.
func TestStudentPage_TAMayRead(t *testing.T) {
	f := studentPageSetup(t)
	ta := loginAs(t, f.ts, f.st, "ta-sp@ntu.edu.tw", "ta")
	getJSON[spSummary](t, ta, f.summaryURL(), http.StatusOK)
	getJSON[spDetail](t, ta, f.detailURL(), http.StatusOK)
}
