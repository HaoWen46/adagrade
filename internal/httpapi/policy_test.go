package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// D25 slice 6 (HTTP surface): grading-policy catalog, per-method validation guard,
// prompt-preview override, record/analysis provenance, and the official policy-mix
// check. See docs/superpowers/sdd/p2-policy-http-report.md.

func TestGradingPolicies_CatalogEndpoint(t *testing.T) {
	env, c, _, _, _ := phase4Setup(t)
	got := getJSON[map[string]any](t, c, env.ts.URL+"/api/grading-policies", http.StatusOK)
	policies, ok := got["policies"].([]any)
	if !ok || len(policies) != len(grading.Policies) {
		t.Fatalf("policies: %v", got)
	}
	for i, p := range policies {
		pm := p.(map[string]any)
		want := grading.Policies[i]
		if pm["key"] != want.Key || pm["label"] != want.Label ||
			pm["tagline"] != want.Tagline || pm["when_to_use"] != want.WhenToUse {
			t.Errorf("policy %d: got %v want %+v", i, pm, want)
		}
	}
}

// legacyTemplate inserts a v1-era (policy-free) template version directly, bypassing
// the seeded policy-aware v2, so validateMethodConfig has something pre-D25 to reject.
func legacyTemplate(t *testing.T, env *testEnv, name string) db.PromptTemplateVersion {
	t.Helper()
	tv, err := env.st.Q.CreatePromptTemplateVersion(t.Context(), db.CreatePromptTemplateVersionParams{
		Name:           name,
		SystemTemplate: "You are a teaching assistant grading one student's handwritten answer.",
		UserTemplate:   "# Problem {{.ProblemNumber}}\n{{.ProblemStatement}}\n{{range .Criteria}}{{.Description}}{{end}}",
	})
	if err != nil {
		t.Fatalf("legacy template: %v", err)
	}
	return tv
}

func TestValidateMethodConfig_RejectsNonStandardPolicyOnLegacyTemplate(t *testing.T) {
	env, c, _, _, _ := phase4Setup(t)
	tv := legacyTemplate(t, env, "legacy-template")

	// strict on a policy-free template → 400 with the specific message.
	resp := postJSON(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Strict on legacy",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1", "policy": "strict",
			"prompt_template_version_id": tv.ID,
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d want 400", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "predates grading policies") {
		t.Errorf("error message: %q", msg)
	}

	// standard on the same legacy template → 201 (legacy behavior stays allowed).
	postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Standard on legacy",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1", "policy": "standard",
			"prompt_template_version_id": tv.ID,
		},
	}, http.StatusCreated)
}

func TestPromptPreview_PolicyOverride(t *testing.T) {
	env, c, _, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c) // default config has no policy → standard

	strict := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/prompt-preview?method_id=%d&policy=strict", env.ts.URL, pid, methodID), http.StatusOK)
	lenient := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/prompt-preview?method_id=%d&policy=lenient", env.ts.URL, pid, methodID), http.StatusOK)

	strictUser := strict["user"].(string)
	lenientUser := lenient["user"].(string)
	if strictUser == lenientUser {
		t.Errorf("strict and lenient previews should differ")
	}
	if !strings.Contains(strictUser, "# Judgment policy") || !strings.Contains(lenientUser, "# Judgment policy") {
		t.Errorf("previews missing judgment policy section")
	}

	pins := strict["pins"].(map[string]any)
	if pins["policy"] != "strict" {
		t.Errorf("pins.policy: got %v want strict", pins["policy"])
	}
	pinsLenient := lenient["pins"].(map[string]any)
	if pinsLenient["policy"] != "lenient" {
		t.Errorf("pins.policy: got %v want lenient", pinsLenient["policy"])
	}

	// Invalid policy override → 400.
	bogus := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/prompt-preview?method_id=%d&policy=bogus", env.ts.URL, pid, methodID), http.StatusBadRequest)
	if _, ok := bogus["error"]; !ok {
		t.Errorf("expected an error message for bogus policy: %v", bogus)
	}
}

func TestRecordJSON_PolicyProvenance(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	driveRun(t, env, int64(run["id"].(float64)), false)

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answerID), http.StatusOK)
	records := got["records"].([]any)
	if len(records) != 1 {
		t.Fatalf("records: %v", records)
	}
	rec := records[0].(map[string]any)
	if rec["policy"] != "standard" {
		t.Errorf("model record policy: got %v want standard", rec["policy"])
	}
	if _, ok := rec["method_version_id"]; !ok {
		t.Errorf("model record missing method_version_id: %v", rec)
	}
	if _, ok := rec["prompt_template_version_id"]; !ok {
		t.Errorf("model record missing prompt_template_version_id: %v", rec)
	}
	// Human version integers alongside the raw DB ids — AnswerView shows
	// "method v1 · prompt v2", never "v-id 57" (demo polish, Task B).
	mv, err := env.st.Q.GetMethodVersion(t.Context(), int64(rec["method_version_id"].(float64)))
	if err != nil {
		t.Fatalf("method version lookup: %v", err)
	}
	if got, want := rec["method_version"], float64(mv.Version); got != want {
		t.Errorf("method_version: got %v want %v", got, want)
	}
	tv, err := env.st.Q.GetPromptTemplateVersion(t.Context(), int64(rec["prompt_template_version_id"].(float64)))
	if err != nil {
		t.Fatalf("prompt template version lookup: %v", err)
	}
	if got, want := rec["prompt_version"], float64(tv.Version); got != want {
		t.Errorf("prompt_version: got %v want %v", got, want)
	}

	// A human record omits policy (and, per spec, omitempty means it's simply absent
	// or null — check it's not one of the curated policy strings).
	rubric := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/rubric", env.ts.URL, pid), http.StatusOK)
	current := rubric["current"].(map[string]any)
	var scores []map[string]any
	for _, cr := range current["criteria"].([]any) {
		scores = append(scores, map[string]any{"criterion_id": int64(cr.(map[string]any)["id"].(float64)), "score": "1"})
	}
	postExpect(t, c, fmt.Sprintf("%s/api/answers/%d/records", env.ts.URL, answerID), map[string]any{
		"rubric_version_id": int64(current["id"].(float64)),
		"scores":            scores,
	}, http.StatusCreated)

	got = getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answerID), http.StatusOK)
	records = got["records"].([]any)
	if len(records) != 2 {
		t.Fatalf("records after human grade: %v", records)
	}
	var humanRec map[string]any
	for _, r := range records {
		rm := r.(map[string]any)
		if rm["source"] == "human" {
			humanRec = rm
		}
	}
	if humanRec == nil {
		t.Fatalf("no human record found: %v", records)
	}
	if v, ok := humanRec["policy"]; ok && v != nil {
		t.Errorf("human record should omit policy, got %v", v)
	}
	for _, key := range []string{"method_version", "prompt_version"} {
		if v, ok := humanRec[key]; ok && v != nil {
			t.Errorf("human record should omit %s, got %v", key, v)
		}
	}
}

func TestAnalysis_PolicyExposure(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	_ = pid
	if err := grading.EnsureSeeds(t.Context(), env.st, discardLogger()); err != nil {
		t.Fatal(err)
	}
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Strict method",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1", "policy": "strict",
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	methodID := int64(m["id"].(float64))
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	driveRun(t, env, int64(run["id"].(float64)), false)

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/analysis", env.ts.URL, aid), http.StatusOK)
	stats := got["stats"].([]any)
	if len(stats) != 1 {
		t.Fatalf("stats: %v", stats)
	}
	st0 := stats[0].(map[string]any)
	if st0["policy"] != "strict" {
		t.Errorf("stats policy: got %v want strict", st0["policy"])
	}
}

func TestAssessmentAnalysis_PolicyMix(t *testing.T) {
	env, c, aid, pid, rvID := phase4Setup(t)

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	if len(students["students"]) < 2 {
		t.Fatalf("need 2 students: %v", students)
	}
	a1 := int64(students["students"][0]["answer_id"].(float64))
	a2 := int64(students["students"][1]["answer_id"].(float64))

	insertOfficialWithPolicy := func(answerID int64, policy string) {
		var recID int64
		err := env.st.Pool.QueryRow(t.Context(), `
			INSERT INTO grading_records (answer_id, source, model_id, rubric_version_id, graded_image_shas, criterion_scores, total, comment, adjustments, policy)
			VALUES ($1, 'model', 'fake-vision-1', $2, '{}', '[]', 2, '', '[]', $3)
			RETURNING id`, answerID, rvID, policy).Scan(&recID)
		if err != nil {
			t.Fatalf("insert record: %v", err)
		}
		mustExec(t, env.st, `UPDATE answers SET official_record_id = $1 WHERE id = $2`, recID, answerID)
	}
	insertOfficialWithPolicy(a1, "strict")
	insertOfficialWithPolicy(a2, "lenient")

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/analysis", env.ts.URL, aid), http.StatusOK)
	mix, ok := got["policy_mix"].([]any)
	if !ok || len(mix) != 1 {
		t.Fatalf("policy_mix: %v", got["policy_mix"])
	}
	entry := mix[0].(map[string]any)
	if entry["problem_id"] != float64(pid) {
		t.Errorf("policy_mix problem_id: %v", entry)
	}
	policies := entry["policies"].([]any)
	if len(policies) != 2 {
		t.Errorf("policy_mix policies: %v", policies)
	}
}

func TestAssessmentAnalysis_PolicyMixEmptyWhenUniform(t *testing.T) {
	env, c, aid, pid, rvID := phase4Setup(t)

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	a1 := int64(students["students"][0]["answer_id"].(float64))
	a2 := int64(students["students"][1]["answer_id"].(float64))
	_ = pid

	insertOfficialWithPolicy := func(answerID int64, policy string) {
		var recID int64
		err := env.st.Pool.QueryRow(t.Context(), `
			INSERT INTO grading_records (answer_id, source, model_id, rubric_version_id, graded_image_shas, criterion_scores, total, comment, adjustments, policy)
			VALUES ($1, 'model', 'fake-vision-1', $2, '{}', '[]', 2, '', '[]', $3)
			RETURNING id`, answerID, rvID, policy).Scan(&recID)
		if err != nil {
			t.Fatalf("insert record: %v", err)
		}
		mustExec(t, env.st, `UPDATE answers SET official_record_id = $1 WHERE id = $2`, recID, answerID)
	}
	insertOfficialWithPolicy(a1, "strict")
	insertOfficialWithPolicy(a2, "strict")

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/analysis", env.ts.URL, aid), http.StatusOK)
	mix, ok := got["policy_mix"].([]any)
	if !ok || len(mix) != 0 {
		t.Fatalf("policy_mix should be empty: %v", got["policy_mix"])
	}
}
