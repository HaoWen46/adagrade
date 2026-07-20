package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

// loginAs seeds a user with the given role and returns a logged-in client.
func loginAs(t *testing.T, ts *httptest.Server, st storeSeeder, email, role string) *http.Client {
	t.Helper()
	seedRole(t, st, email, role)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp := devLogin(t, ts, c, email)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("dev-login as %s: got %d", role, resp.StatusCode)
	}
	return c
}

func getJSON[T any](t *testing.T, c *http.Client, url string, wantStatus int) T {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s: got %d want %d", url, resp.StatusCode, wantStatus)
	}
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("GET %s: decode: %v", url, err)
	}
	return v
}

// getJSONRaw GETs the URL and returns the raw response bytes (200 required) —
// for tests that need to inspect the wire shape itself (e.g. asserting a field
// name never appears) rather than a decoded/typed value.
func getJSONRaw(t *testing.T, c *http.Client, url string) []byte {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: got %d want 200", url, resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func postExpect(t *testing.T, c *http.Client, url string, body any, wantStatus int) map[string]any {
	t.Helper()
	resp := postJSON(t, c, url, body)
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		var eb bytes.Buffer
		_, _ = eb.ReadFrom(resp.Body)
		t.Fatalf("POST %s: got %d want %d — %s", url, resp.StatusCode, wantStatus, eb.String())
	}
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v
}

func TestAssessments_LifecycleAndGuardrails(t *testing.T) {
	ts, _, st := harness(t)
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")
	ta := loginAs(t, ts, st, "ta@ntu.edu.tw", "ta")
	admin := loginAs(t, ts, st, "admin@ntu.edu.tw", "admin")

	// TA cannot create; lecturer can.
	resp := postJSON(t, ta, ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Midterm 1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ta create assessment: got %d want 403", resp.StatusCode)
	}
	created := postExpect(t, lecturer, ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Midterm 1"}, http.StatusCreated)
	aid := int64(created["id"].(float64))

	// Duplicate live name rejected.
	resp = postJSON(t, lecturer, ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Midterm 1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate name: got %d want 409", resp.StatusCode)
	}

	// Archive hides it from the default list.
	postExpect(t, lecturer, fmt.Sprintf("%s/api/assessments/%d/archive", ts.URL, aid), map[string]bool{"archived": true}, http.StatusOK)
	list := getJSON[map[string][]map[string]any](t, ta, ts.URL+"/api/assessments", http.StatusOK)
	if len(list["assessments"]) != 0 {
		t.Errorf("archived assessment should be hidden: %v", list)
	}
	listAll := getJSON[map[string][]map[string]any](t, ta, ts.URL+"/api/assessments?include_archived=1", http.StatusOK)
	if len(listAll["assessments"]) != 1 {
		t.Errorf("include_archived should show it: %v", listAll)
	}

	// Hard delete: wrong confirm name → 400; with data → 409 unless forced.
	req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/assessments/%d", ts.URL, aid), strings.NewReader(`{"confirm_name":"wrong"}`))
	req.Header.Set("X-ADA-CSRF", "1")
	dresp, err := admin.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusBadRequest {
		t.Errorf("delete with wrong confirm_name: got %d want 400", dresp.StatusCode)
	}

	// Seed a submission row directly to trip the guardrail.
	seedStudent(t, st, "b01", "Alice", "a@x.edu")
	mustExec(t, st, `INSERT INTO submissions (assessment_id, student_id, original_filename, source_ref, source_sha256, page_count)
		SELECT $1, id, 'b01.pdf', 'ref', 'sha', 3 FROM students WHERE student_id = 'b01'`, aid)

	req, _ = http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/assessments/%d", ts.URL, aid), strings.NewReader(`{"confirm_name":"Midterm 1"}`))
	req.Header.Set("X-ADA-CSRF", "1")
	dresp, _ = admin.Do(req)
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusConflict {
		t.Errorf("delete with submissions, no force: got %d want 409", dresp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/assessments/%d", ts.URL, aid), strings.NewReader(`{"confirm_name":"Midterm 1","force":true}`))
	req.Header.Set("X-ADA-CSRF", "1")
	dresp, _ = admin.Do(req)
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Errorf("forced delete: got %d want 204", dresp.StatusCode)
	}
}

func TestRubrics_SumInvariantAndVersioning(t *testing.T) {
	ts, _, st := harness(t)
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	a := postExpect(t, lecturer, ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Final"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	p := postExpect(t, lecturer, fmt.Sprintf("%s/api/assessments/%d/problems", ts.URL, aid),
		map[string]any{"number": 1, "title": "DP", "max_points": "10"}, http.StatusCreated)
	pid := int64(p["id"].(float64))

	rubricBody := func(pts1, pts2 string) map[string]any {
		return map[string]any{
			"score_increment": "0.5",
			"criteria": []map[string]string{
				{"description": "Correct recurrence", "points": pts1},
				{"description": "Complexity analysis", "points": pts2},
			},
		}
	}

	// Σ != max_points → 400.
	resp := postJSON(t, lecturer, fmt.Sprintf("%s/api/problems/%d/rubric", ts.URL, pid), rubricBody("6", "3.5"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad sum: got %d want 400", resp.StatusCode)
	}

	// Valid rubric → v1; second → v2; GET shows v2 current with criteria.
	v1 := postExpect(t, lecturer, fmt.Sprintf("%s/api/problems/%d/rubric", ts.URL, pid), rubricBody("6", "4"), http.StatusCreated)
	if v1["version"].(float64) != 1 {
		t.Errorf("first rubric version: got %v want 1", v1["version"])
	}
	postExpect(t, lecturer, fmt.Sprintf("%s/api/problems/%d/rubric", ts.URL, pid), rubricBody("5.5", "4.5"), http.StatusCreated)

	got := getJSON[map[string]any](t, lecturer, fmt.Sprintf("%s/api/problems/%d/rubric", ts.URL, pid), http.StatusOK)
	current := got["current"].(map[string]any)
	if current["version"].(float64) != 2 {
		t.Errorf("current version: got %v want 2", current["version"])
	}
	crits := current["criteria"].([]any)
	if len(crits) != 2 || crits[0].(map[string]any)["points"] != "5.5" {
		t.Errorf("current criteria wrong: %v", crits)
	}
	if len(got["versions"].([]any)) != 2 {
		t.Errorf("versions list: %v", got["versions"])
	}

	// max_points now locked while a rubric exists.
	req, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/api/problems/%d", ts.URL, pid), strings.NewReader(`{"max_points":"12"}`))
	req.Header.Set("X-ADA-CSRF", "1")
	presp, _ := lecturer.Do(req)
	presp.Body.Close()
	if presp.StatusCode != http.StatusConflict {
		t.Errorf("max_points change with rubric: got %d want 409", presp.StatusCode)
	}
}

func TestSolutions_Versioning(t *testing.T) {
	ts, _, st := harness(t)
	ta := loginAs(t, ts, st, "ta@ntu.edu.tw", "ta")
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	a := postExpect(t, lecturer, ts.URL+"/api/assessments", map[string]string{"kind": "assignment", "name": "HW1"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	p := postExpect(t, lecturer, fmt.Sprintf("%s/api/assessments/%d/problems", ts.URL, aid),
		map[string]any{"number": 1, "max_points": "10"}, http.StatusCreated)
	pid := int64(p["id"].(float64))

	// TAs may add solution versions.
	postExpect(t, ta, fmt.Sprintf("%s/api/problems/%d/solutions", ts.URL, pid), map[string]string{"content": "v1 solution"}, http.StatusCreated)
	v2 := postExpect(t, ta, fmt.Sprintf("%s/api/problems/%d/solutions", ts.URL, pid), map[string]string{"content": "v2 solution"}, http.StatusCreated)
	if v2["version"].(float64) != 2 {
		t.Errorf("second solution version: got %v", v2["version"])
	}
	got := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/problems/%d/solutions", ts.URL, pid), http.StatusOK)
	if got["current"].(map[string]any)["content"] != "v2 solution" {
		t.Errorf("current solution: %v", got["current"])
	}
}

func TestRosterImport_EndToEnd(t *testing.T) {
	ts, _, st := harness(t)
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	upload := func(csv string) *http.Response {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "roster.csv")
		_, _ = fw.Write([]byte(csv))
		mw.Close()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/students/import", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-ADA-CSRF", "1")
		resp, err := lecturer.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// First import: 2 added.
	resp := upload("student_id,name,email\nb01,Alice,a@x.edu\nb02,Bob,b@x.edu\n")
	var rep struct{ Added, Updated, Unchanged, Total int }
	_ = json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || rep.Added != 2 || rep.Total != 2 {
		t.Fatalf("first import: %d %+v", resp.StatusCode, rep)
	}

	// Re-import with one change: 1 updated, 1 unchanged.
	resp = upload("student_id,name,email\nb01,Alice,a@x.edu\nb02,Bobby,b@x.edu\n")
	_ = json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if rep.Updated != 1 || rep.Unchanged != 1 {
		t.Errorf("re-import: %+v", rep)
	}

	// Duplicate id → whole file rejected, roster unchanged.
	resp = upload("student_id,name,email\nb03,Carl,c@x.edu\nb03,Carla,c2@x.edu\n")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("dup import: got %d want 400", resp.StatusCode)
	}
	students := getJSON[map[string][]map[string]any](t, lecturer, ts.URL+"/api/students", http.StatusOK)
	if len(students["students"]) != 2 {
		t.Errorf("roster should still have 2 students: %v", students)
	}
}
