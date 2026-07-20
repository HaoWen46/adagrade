package httpapi

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// phase3Fixture: assessment + problem (max 10, rubric 6+4 inc 0.5) + roster b01 +
// uploaded fake submission. Returns the logged-in lecturer client and ids.
type phase3Fixture struct {
	ts        *httptest.Server
	c         *http.Client
	st        storeSeeder
	env       *testEnv
	aid       int64
	problemID int64
	rubricID  int64
	critIDs   []int64
	answerID  int64
}

func phase3Setup(t *testing.T) phase3Fixture {
	t.Helper()
	env := harnessEnv(t)
	ts, st := env.ts, env.st
	c := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	a := postExpect(t, c, ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Quiz"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	p := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/problems", ts.URL, aid),
		map[string]any{"number": 1, "title": "Greedy", "max_points": "10"}, http.StatusCreated)
	pid := int64(p["id"].(float64))
	rv := postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/rubric", ts.URL, pid), map[string]any{
		"score_increment": "0.5",
		"criteria": []map[string]string{
			{"description": "Correct algorithm", "points": "6"},
			{"description": "Proof of optimality", "points": "4"},
		},
	}, http.StatusCreated)
	rubricID := int64(rv["id"].(float64))
	var critIDs []int64
	for _, cr := range rv["criteria"].([]any) {
		critIDs = append(critIDs, int64(cr.(map[string]any)["id"].(float64)))
	}

	seedStudent(t, st, "b01", "Alice", "a@x.edu")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("files", "b01.pdf")
	_, _ = fw.Write([]byte("%PDF-fake"))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/assessments/%d/submissions", ts.URL, aid), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var upload struct {
		Results []struct {
			Status   string `json:"status"`
			UploadID int64  `json:"upload_id"`
		} `json:"results"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&upload)
	resp.Body.Close()
	if len(upload.Results) != 1 || upload.Results[0].Status != "queued" {
		t.Fatalf("upload: %+v", upload)
	}
	// The upload only stages + enqueues now (D27, F1); drive the ingest worker body
	// directly so the submission actually lands before the rest of the fixture reads
	// it back through the review/summary endpoints.
	driveDirectUploads(t, env, aid)

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", ts.URL, pid), http.StatusOK)
	if len(students["students"]) != 1 {
		t.Fatalf("students: %v", students)
	}
	answerID := int64(students["students"][0]["answer_id"].(float64))

	return phase3Fixture{ts: ts, c: c, st: st, env: env, aid: aid, problemID: pid, rubricID: rubricID, critIDs: critIDs, answerID: answerID}
}

func TestPhase3_ManualGradingWorkflow(t *testing.T) {
	f := phase3Setup(t)

	// Problem summary shows 1 answer with pages, nothing graded.
	sum := getJSON[map[string][]map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/problems/summary", f.ts.URL, f.aid), http.StatusOK)
	ps := sum["problems"][0]
	if ps["answers"].(float64) != 1 || ps["with_pages"].(float64) != 1 || ps["official_set"].(float64) != 0 {
		t.Errorf("summary: %v", ps)
	}

	// Manual record: 5.3 snaps to 5.5; 4.5 clamps to 4. Total = 9.5.
	rec := postExpect(t, f.c, fmt.Sprintf("%s/api/answers/%d/records", f.ts.URL, f.answerID), map[string]any{
		"rubric_version_id": f.rubricID,
		"comment":           "solid work",
		"scores": []map[string]any{
			{"criterion_id": f.critIDs[0], "score": "5.3", "rationale": "minor slip"},
			{"criterion_id": f.critIDs[1], "score": "4.5"},
		},
	}, http.StatusCreated)
	if rec["total"] != "9.5" {
		t.Errorf("total: got %v want 9.5", rec["total"])
	}
	adjustments := rec["adjustments"].([]any)
	if len(adjustments) != 2 {
		t.Errorf("both scores were adjusted, adjustments: %v", adjustments)
	}

	// Wrong criterion set → 400.
	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/answers/%d/records", f.ts.URL, f.answerID), map[string]any{
		"rubric_version_id": f.rubricID,
		"scores":            []map[string]any{{"criterion_id": f.critIDs[0], "score": "5"}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("incomplete scores: got %d want 400", resp.StatusCode)
	}

	// Officials are derived under round-based grading (0027): nothing is official
	// until the exam chooses its final source. Choosing consensus (no aggregates
	// exist here) makes the recompute fall back to the latest human record.
	res := putJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/final-source", f.ts.URL, f.aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)
	if moved := int(res["officials_moved"].(float64)); moved != 1 {
		t.Fatalf("choosing consensus should derive 1 official (human fallback), moved %d", moved)
	}

	// Student row + totals reflect the official grade.
	students := getJSON[map[string][]map[string]any](t, f.c, fmt.Sprintf("%s/api/problems/%d/students", f.ts.URL, f.problemID), http.StatusOK)
	srow := students["students"][0]
	if srow["official_total"] != "9.5" || srow["status"] != "official_set" || srow["official_source"] != "human" {
		t.Errorf("student row: %v", srow)
	}
	totals := getJSON[map[string][]map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/totals", f.ts.URL, f.aid), http.StatusOK)
	if totals["students"][0]["total"] != "9.5" {
		t.Errorf("assessment totals: %v", totals["students"][0])
	}

	// Answer detail carries pages + full history.
	detail := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/answers/%d", f.ts.URL, f.answerID), http.StatusOK)
	if len(detail["records"].([]any)) != 1 || len(detail["pages"].([]any)) != 1 {
		t.Errorf("answer detail: records=%v pages=%v", detail["records"], detail["pages"])
	}
	if detail["student"].(map[string]any)["student_id"] != "b01" {
		t.Errorf("answer student: %v", detail["student"])
	}
}

func TestStudentSubmissionView_WorksUngraded(t *testing.T) {
	f := phase3Setup(t)

	// Ungraded, unmasked, no records — the view must still work (browsing is
	// never gated on grading state).
	got := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/students/b01/submission", f.ts.URL, f.aid), http.StatusOK)
	if got["student"].(map[string]any)["student_id"] != "b01" {
		t.Fatalf("student: %v", got["student"])
	}
	sub := got["submission"].(map[string]any)
	if sub == nil || sub["filename"] != "b01.pdf" {
		t.Errorf("submission: %v", sub)
	}
	answers := got["answers"].([]any)
	if len(answers) != 1 {
		t.Fatalf("answers: %v", answers)
	}
	a0 := answers[0].(map[string]any)
	if a0["record_count"].(float64) != 0 || a0["has_official"] != false {
		t.Errorf("ungraded answer state: %v", a0)
	}
	if len(a0["pages"].([]any)) != 1 {
		t.Errorf("pages: %v", a0["pages"])
	}

	// A rostered student with NO submission still resolves (empty pages).
	seedStudent(t, f.st, "b09", "Nadia", "n@x.edu")
	got = getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/students/b09/submission", f.ts.URL, f.aid), http.StatusOK)
	if got["submission"] != nil {
		t.Errorf("no-submission student: %v", got["submission"])
	}
}

func TestExportCSV_ReflectsOfficialGrades(t *testing.T) {
	f := phase3Setup(t)

	// Grade, then choose the final source (0027): consensus with no aggregates
	// derives the human record official.
	postExpect(t, f.c, fmt.Sprintf("%s/api/answers/%d/records", f.ts.URL, f.answerID), map[string]any{
		"rubric_version_id": f.rubricID,
		"scores": []map[string]any{
			{"criterion_id": f.critIDs[0], "score": "5.5"},
			{"criterion_id": f.critIDs[1], "score": "3"},
		},
	}, http.StatusCreated)
	putJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/final-source", f.ts.URL, f.aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)

	resp, err := f.c.Get(fmt.Sprintf("%s/api/assessments/%d/export.csv", f.ts.URL, f.aid))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("export: status %d type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	records, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("csv rows: %v", records)
	}
	head, row := records[0], records[1]
	if head[0] != "student_id" || !strings.HasPrefix(head[2], "P1") {
		t.Errorf("header: %v", head)
	}
	if row[0] != "b01" || row[2] != "8.5" || row[3] != "8.5" || row[4] != "1/1" || row[5] != "human" {
		t.Errorf("row: %v", row)
	}
}

// TestPhase3_ManualRecordDerivesOfficialUnderConsensus pins the manual-record
// recompute hook (0027, the successor of the removed grade+set-official one-call
// path): with the final source already chosen, POSTing a human record makes it
// official immediately — consensus with no aggregates falls back to the latest
// human record, with no separate set-official action.
func TestPhase3_ManualRecordDerivesOfficialUnderConsensus(t *testing.T) {
	f := phase3Setup(t)
	putJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/final-source", f.ts.URL, f.aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)
	rec := postExpect(t, f.c, fmt.Sprintf("%s/api/answers/%d/records", f.ts.URL, f.answerID), map[string]any{
		"rubric_version_id": f.rubricID,
		"scores": []map[string]any{
			{"criterion_id": f.critIDs[0], "score": "6"},
			{"criterion_id": f.critIDs[1], "score": "4"},
		},
	}, http.StatusCreated)
	if rec["total"] != "10" {
		t.Errorf("total: %v", rec["total"])
	}
	detail := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/answers/%d", f.ts.URL, f.answerID), http.StatusOK)
	official := detail["answer"].(map[string]any)["official_record_id"]
	if official == nil || int64(official.(float64)) != int64(rec["id"].(float64)) {
		t.Errorf("official should be the derived new record: %v vs %v", official, rec["id"])
	}
}
