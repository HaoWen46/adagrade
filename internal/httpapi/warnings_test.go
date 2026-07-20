package httpapi

import (
	"fmt"
	"net/http"
	"testing"
)

// warningsFor fetches GET /api/assessments/{id}/workflow-warnings and indexes
// the result by code (each code appears at most once).
func warningsFor(t *testing.T, c *http.Client, baseURL string, aid int64) map[string]map[string]any {
	t.Helper()
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/workflow-warnings", baseURL, aid), http.StatusOK)
	raw, ok := got["warnings"].([]any)
	if !ok {
		t.Fatalf("workflow-warnings body must carry a warnings array: %v", got)
	}
	out := make(map[string]map[string]any, len(raw))
	for _, w := range raw {
		wm := w.(map[string]any)
		code := wm["code"].(string)
		if _, dup := out[code]; dup {
			t.Fatalf("warning code %q emitted twice: %v", code, raw)
		}
		out[code] = wm
	}
	return out
}

// wantWarning asserts one warning's presence, severity, and count, returning it
// for further (e.g. detail) checks.
func wantWarning(t *testing.T, ws map[string]map[string]any, code, severity string, count int) map[string]any {
	t.Helper()
	w, ok := ws[code]
	if !ok {
		t.Fatalf("warning %q missing: %v", code, ws)
	}
	if w["severity"] != severity {
		t.Errorf("%s severity = %v, want %s", code, w["severity"], severity)
	}
	got := 0
	if v, ok := w["count"].(float64); ok {
		got = int(v)
	}
	if got != count {
		t.Errorf("%s count = %d, want %d", code, got, count)
	}
	return w
}

func refuteWarning(t *testing.T, ws map[string]map[string]any, code string) {
	t.Helper()
	if w, ok := ws[code]; ok {
		t.Errorf("warning %q should be absent, got %v", code, w)
	}
}

// A fully-ingested assessment with a rubric'd problem and nothing risky in
// flight emits NO warnings — and the empty case is an empty array, not null.
func TestWorkflowWarnings_CleanAssessmentEmptyAnd404(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/workflow-warnings", env.ts.URL, aid), http.StatusOK)
	raw, ok := got["warnings"].([]any)
	if !ok {
		t.Fatalf("warnings must be a JSON array even when empty: %v", got)
	}
	if len(raw) != 0 {
		t.Fatalf("clean assessment should have no warnings, got %v", raw)
	}

	resp, err := c.Get(env.ts.URL + "/api/assessments/999999/workflow-warnings")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown assessment: got %d want 404", resp.StatusCode)
	}
}

// Scan-intake hazards: stranded pages whose (student, problem) cell has no
// live submission (orphaned + parked + failed, with the breakdown detail),
// assigned-but-unpromoted pages, still-processing pages, and problems without
// a rubric (scanSetup's three problems have none). Every stranded page here
// carries an identity (assigned or OCR-proposed), so nothing is unidentified.
func TestWorkflowWarnings_ScanIntakeStates(t *testing.T) {
	env, c, aid, problems := scanSetup(t)
	rec := wireScanEnqueues(env)

	// Three loose files, OCR off: after split every page sits "processing".
	uploadLooseFilesExpect(t, c, env.ts, aid, []string{"a.pdf", "b.pdf", "c.pdf"}, map[string]string{"ocr_enabled": "0"}, http.StatusOK)
	driveSplit(t, env, rec.splits)

	pagesResp := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages", env.ts.URL, aid), http.StatusOK)
	pages := pagesResp["pages"].([]any)
	if len(pages) < 5 {
		t.Fatalf("need at least 5 pages to cover every state, got %d", len(pages))
	}
	ids := make([]int64, 0, len(pages))
	for _, raw := range pages {
		ids = append(ids, int64(raw.(map[string]any)["id"].(float64)))
	}

	// Nudge column facts into each derived state (D2: state derives, never
	// stored), each with a distinct, identifiable (student, problem) cell that
	// no live submission covers.
	mustExec(t, env.st, `UPDATE scan_pages SET identified_at = now(),
		proposed_student_id = (SELECT id FROM students WHERE student_id = 'B11902001'),
		proposed_problem_id = $2 WHERE id = $1`, ids[0], problems[1]) // orphaned
	mustExec(t, env.st, `UPDATE scan_pages SET identified_at = now(), parked_reason = 'conflict',
		proposed_student_id = (SELECT id FROM students WHERE student_id = 'B11902001'),
		proposed_problem_id = $2 WHERE id = $1`, ids[1], problems[2]) // parked
	postExpect(t, c, fmt.Sprintf("%s/api/scan-pages/%d/assign", env.ts.URL, ids[2]),
		map[string]any{"student_id": "B11902002", "problem_id": problems[1]}, http.StatusOK)
	mustExec(t, env.st, `UPDATE scan_pages SET error = 'render failed: boom' WHERE id = $1`, ids[2]) // errored (assigned)
	postExpect(t, c, fmt.Sprintf("%s/api/scan-pages/%d/assign", env.ts.URL, ids[3]),
		map[string]any{"student_id": "B11902001", "problem_id": problems[3]}, http.StatusOK) // assigned, unpromoted
	processing := len(ids) - 4 // the rest untouched

	ws := warningsFor(t, c, env.ts.URL, aid)
	stranded := wantWarning(t, ws, "stranded_scan_pages", "warning", 3)
	if stranded["detail"] != "1 orphaned, 1 parked, 1 failed; answers affected: 3" {
		t.Errorf("stranded detail = %v, want '1 orphaned, 1 parked, 1 failed; answers affected: 3'", stranded["detail"])
	}
	wantWarning(t, ws, "assigned_unpromoted_pages", "warning", 1)
	wantWarning(t, ws, "batch_processing", "info", processing)
	wantWarning(t, ws, "no_rubric_problems", "warning", 3)
	refuteWarning(t, ws, "unidentified_scan_pages")
	refuteWarning(t, ws, "dead_scan_pages")
	refuteWarning(t, ws, "quarantined_uploads")
	refuteWarning(t, ws, "mask_errors")
	refuteWarning(t, ws, "run_in_progress")
}

// The false-alarm fix (2026-07-11): a stranded page only claims "answers grade
// incomplete" when its (student, problem) cell has NO live submission. Pages
// with no identity at all split into their own unidentified code (warning
// while work is genuinely missing, info once every cell is covered), and pages
// whose cell IS covered become an informational dead-batch note — the exact
// "failed batch superseded by a successful re-upload" scenario that used to
// send TAs discarding 40 dead pages one at a time.
func TestWorkflowWarnings_StrandedCoverageSplit(t *testing.T) {
	env, c, aid, problems := scanSetup(t)
	rec := wireScanEnqueues(env)

	uploadLooseFilesExpect(t, c, env.ts, aid, []string{"a.pdf"}, map[string]string{"ocr_enabled": "0"}, http.StatusOK)
	driveSplit(t, env, rec.splits)

	pagesResp := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages", env.ts.URL, aid), http.StatusOK)
	pages := pagesResp["pages"].([]any)
	if len(pages) < 2 {
		t.Fatalf("need at least 2 pages, got %d", len(pages))
	}
	id0 := int64(pages[0].(map[string]any)["id"].(float64))
	id1 := int64(pages[1].(map[string]any)["id"].(float64))

	// id0: failed, but OCR proposed a full cell. id1: failed with no identity.
	mustExec(t, env.st, `UPDATE scan_pages SET error = 'ocr failed: boom',
		proposed_student_id = (SELECT id FROM students WHERE student_id = 'B11902001'),
		proposed_problem_id = $2 WHERE id = $1`, id0, problems[1])
	mustExec(t, env.st, `UPDATE scan_pages SET error = 'ocr failed: boom' WHERE id = $1`, id1)

	// No submissions anywhere: id0's cell is uncovered (real hazard), id1 can't
	// be checked — and with cells still missing it stays a warning.
	ws := warningsFor(t, c, env.ts.URL, aid)
	stranded := wantWarning(t, ws, "stranded_scan_pages", "warning", 1)
	if stranded["detail"] != "0 orphaned, 0 parked, 1 failed; answers affected: 1" {
		t.Errorf("stranded detail = %v, want '0 orphaned, 0 parked, 1 failed; answers affected: 1'", stranded["detail"])
	}
	wantWarning(t, ws, "unidentified_scan_pages", "warning", 1)
	refuteWarning(t, ws, "dead_scan_pages")

	// A successful re-upload covers every cell: whole-assessment live
	// submissions for both roster students.
	mustExec(t, env.st, `INSERT INTO submissions (assessment_id, student_id, original_filename, source_ref, source_sha256, page_count)
		SELECT $1, id, 'redo.pdf', 'blob/redo', 'shasha', 3 FROM students WHERE student_id IN ('B11902001', 'B11902002')`, aid)

	// The dead batch must never claim answers grade incomplete now: the covered
	// page becomes an info note, and the unidentifiable page drops to info too
	// (there is no missing work it could be).
	ws = warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "stranded_scan_pages")
	wantWarning(t, ws, "dead_scan_pages", "info", 1)
	wantWarning(t, ws, "unidentified_scan_pages", "info", 1)

	// The launch preview reshapes identically (same helper).
	lw := runPreviewWarnings(t, c, env.ts.URL, fmt.Sprintf("assessment_id=%d&scope_kind=assessment", aid))
	refuteWarning(t, lw, "stranded_scan_pages")
	wantWarning(t, lw, "dead_scan_pages", "info", 1)
	wantWarning(t, lw, "unidentified_scan_pages", "info", 1)
}

// Intake/grading integrity hazards: an open quarantine row, a graded answer
// whose images were force-replaced (image_superseded), and a page whose mask
// job failed terminally (danger — the AI would see a stale/unmasked image).
func TestWorkflowWarnings_QuarantineSupersededAndMaskErrors(t *testing.T) {
	f := publishSetup(t)

	// An unmatchable upload lands in quarantine, unresolved.
	uploadFakePDF(t, f.c, f.ts, f.aid, "not-on-roster.pdf")
	driveDirectUploads(t, f.env, f.aid)

	// b01 graded, then its images force-replaced (ingest stamps image_superseded).
	f.gradeOfficial(t, f.answers["b01"], "6", "4")
	mustExec(t, f.st, `UPDATE answers SET flags = array_append(flags, 'image_superseded') WHERE id = $1`, f.answers["b01"])

	// One page's MaskPage job failed terminally (migration 0015 mask_error).
	mustExec(t, f.st, `UPDATE answer_pages SET mask_error = 'decode_failed'
		WHERE id = (SELECT ap.id FROM answer_pages ap JOIN answers a ON a.id = ap.answer_id
		            WHERE a.assessment_id = $1 ORDER BY ap.id LIMIT 1)`, f.aid)

	ws := warningsFor(t, f.c, f.ts.URL, f.aid)
	wantWarning(t, ws, "quarantined_uploads", "warning", 1)
	wantWarning(t, ws, "superseded_answers", "warning", 1)
	wantWarning(t, ws, "mask_errors", "danger", 1)
	refuteWarning(t, ws, "stranded_scan_pages")
	refuteWarning(t, ws, "no_rubric_problems") // publishSetup's problem has a rubric
}

// Roster hazards (roster-lifecycle plan 2026-07-10): duplicate names (info —
// Identify always needs manual confirmation for them), duplicate active emails
// (danger — grade emails share a mailbox), and unmaterialized late-adds
// (warning — active students with zero answers while the assessment has some).
// Only ACTIVE students count everywhere, and a brand-new assessment with no
// answers at all stays silent on unmaterialized_students.
func TestWorkflowWarnings_RosterCodes(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)

	// Baseline: distinct names/emails, both students ingested — no roster codes
	// (and per TestWorkflowWarnings_CleanAssessmentEmptyAnd404, nothing else).
	ws := warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "duplicate_student_names")
	refuteWarning(t, ws, "duplicate_emails")
	refuteWarning(t, ws, "unmaterialized_students")

	// b02 renamed to b01's exact name: BOTH sharers count. b02's email collides
	// with b01's up to case (email routing is case-insensitive), and the count is
	// of shared EMAILS, not students — so 1, not 2. b03 joins after the upload:
	// active with zero answers rows while the assessment already has some.
	mustExec(t, env.st, `UPDATE students SET name = 'Student b01' WHERE student_id = 'b02'`)
	mustExec(t, env.st, `UPDATE students SET email = 'B01@x.edu' WHERE student_id = 'b02'`)
	seedStudent(t, env.st, "b03", "Student b03", "b03@x.edu")

	ws = warningsFor(t, c, env.ts.URL, aid)
	wantWarning(t, ws, "duplicate_student_names", "info", 2)
	wantWarning(t, ws, "duplicate_emails", "danger", 1)
	wantWarning(t, ws, "unmaterialized_students", "warning", 1)

	// Withdrawing b02 clears both duplicate codes (withdrawn students don't
	// count) but leaves b03's unmaterialized warning standing.
	mustExec(t, env.st, `UPDATE students SET withdrawn_at = now() WHERE student_id = 'b02'`)
	ws = warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "duplicate_student_names")
	refuteWarning(t, ws, "duplicate_emails")
	wantWarning(t, ws, "unmaterialized_students", "warning", 1)

	// A withdrawn late-add is nobody's problem.
	mustExec(t, env.st, `UPDATE students SET withdrawn_at = now() WHERE student_id = 'b03'`)
	ws = warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "unmaterialized_students")

	// A brand-new assessment has no answers rows at all, so the ≥1-answers guard
	// keeps unmaterialized_students silent even though active b01 has no rows.
	a2 := postExpect(t, c, env.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Empty"}, http.StatusCreated)
	ws = warningsFor(t, c, env.ts.URL, int64(a2["id"].(float64)))
	refuteWarning(t, ws, "unmaterialized_students")
}

// stale_masks (stale-mask fix 2026-07-11): an ACCEPTED page whose stored
// mask_input_sha no longer matches the fingerprint of the current region set
// passes the "masked + accepted" grading gates while sending an OUTDATED —
// possibly identity-revealing — masked image to providers. The standing danger
// code surfaces it everywhere (workflow warnings, launch preview, publish
// preview), and a mask-regions PUT reconciles it away by knocking the stale
// acceptance back to pending.
func TestWorkflowWarnings_StaleMasks(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	acceptAllMasks(t, env, c, aid)

	// Everything accepted and up to date: no stale_masks anywhere.
	ws := warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "stale_masks")

	// Drift one accepted page's fingerprint (the pre-fix world: regions were
	// edited after acceptance without any invalidation).
	mustExec(t, env.st, `UPDATE answer_pages SET mask_input_sha = 'stale-fingerprint'
		WHERE id = (SELECT ap.id FROM answer_pages ap JOIN answers a ON a.id = ap.answer_id
		            WHERE a.assessment_id = $1 ORDER BY ap.id LIMIT 1)`, aid)

	ws = warningsFor(t, c, env.ts.URL, aid)
	wantWarning(t, ws, "stale_masks", "danger", 1)
	lw := runPreviewWarnings(t, c, env.ts.URL, fmt.Sprintf("assessment_id=%d&scope_kind=assessment", aid))
	wantWarning(t, lw, "stale_masks", "danger", 1)
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)
	pws := pubPreviewWarnings(t, c, env.ts.URL, aid)
	wantWarning(t, pws, "stale_masks", "danger", 1)

	// Re-saving the region set (even unchanged) reconciles acceptances against
	// the current regions: the drifted page drops to pending, so it now blocks
	// via the ordinary mask gate instead of silently passing as stale-accepted.
	saved := putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/mask-regions", env.ts.URL, aid),
		map[string]any{"regions": []map[string]any{
			{"page_scope": "all", "x": 0.05, "y": 0.02, "w": 0.4, "h": 0.08, "color": "#4a4a4a", "padding": 0.01},
		}}, http.StatusOK)
	if saved["stale"].(float64) != 1 {
		t.Errorf("PUT mask-regions stale = %v, want 1", saved["stale"])
	}
	ws = warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "stale_masks")
	preview := getJSON[map[string]any](t, c,
		fmt.Sprintf("%s/api/runs/preview?assessment_id=%d&scope_kind=assessment", env.ts.URL, aid), http.StatusOK)
	if preview["mask_blockers"].(float64) != 1 {
		t.Errorf("mask_blockers after reconcile = %v, want 1", preview["mask_blockers"])
	}
}

// final_source_no_records (analysis redesign plan, Task B1): the chosen final
// source is a completed run that produced no model record on this assessment
// — deriving officials from it yields nothing, so publish would send holes.
func TestWorkflowWarnings_FinalSourceNoRecords(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodA := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodA,
	}, http.StatusCreated)
	runAID := int64(run["id"].(float64))
	driveRun(t, env, runAID, false)

	// No final source chosen yet: silent (the clean-assessment test covers the
	// brand-new case; this covers "graded but undecided").
	ws := warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "final_source_no_records")

	// Final source = the method that actually graded: silent.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runAID}, http.StatusOK)
	ws = warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "final_source_no_records")

	// Re-point at a method with zero records on this assessment: danger.
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m2 := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Never ran",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1",
			"temperature": 0, "ref_solutions": 0, "reask_cap": 2,
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	emptyRun := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": int64(m2["id"].(float64)),
	}, http.StatusCreated)
	emptyRunID := int64(emptyRun["id"].(float64))
	// A completed run with no successful model records is possible when every
	// leaf failed. Seed that terminal edge directly; the warning is a corruption/
	// provider-failure defense for state that predates (or otherwise bypasses)
	// the API-level guard — SetAssessmentFinalSource now refuses to pin a
	// zero-succeeded run via the normal PUT (audit A3), so both the run AND the
	// pin itself are forced directly through SQL here rather than the endpoint,
	// to exercise the warning against exactly the state it exists to catch.
	mustExec(t, env.st, `UPDATE grading_runs SET status = 'completed', finished_at = now() WHERE id = $1`, emptyRunID)
	mustExec(t, env.st, `UPDATE assessments SET final_source_kind = 'method', final_method_id = $2, final_run_id = $3 WHERE id = $1`,
		aid, int64(m2["id"].(float64)), emptyRunID)
	ws = warningsFor(t, c, env.ts.URL, aid)
	wantWarning(t, ws, "final_source_no_records", "danger", 0)

	// The same code must reach the publish preview's warnings[] (the dialog's
	// banner + ack-checkbox surface), via the publish-scoped allowlist.
	pws := pubPreviewWarnings(t, c, env.ts.URL, aid)
	wantWarning(t, pws, "final_source_no_records", "danger", 0)

	// Consensus is out of this code's scope (its own hazards live elsewhere).
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)
	ws = warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "final_source_no_records")
}

// Run-lifecycle hazards: run_in_progress appears for a pending run and clears
// on completion; an "adjusted" spot-check verdict whose checked record is still
// the official flags adjusted_spot_checks; officials spanning two versions of
// the final method flag mixed_method_versions with the stale-answer count.
func TestWorkflowWarnings_RunSpotCheckAndMixedVersions(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))

	ws := warningsFor(t, c, env.ts.URL, aid)
	wantWarning(t, ws, "run_in_progress", "info", 1)

	driveRun(t, env, runID, false)
	ws = warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "run_in_progress")

	// Choosing the run's method as the final source derives officials — all on
	// the method's only version, so nothing is mixed and nothing adjusted yet.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID}, http.StatusOK)
	ws = warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "adjusted_spot_checks")
	refuteWarning(t, ws, "mixed_method_versions")

	// An "adjusted" verdict records disagreement but changes no grade — the
	// checked record stays official, so the warning fires.
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d/spot-check", env.ts.URL, runID), http.StatusOK)
	samples := got["samples"].([]any)
	if len(samples) == 0 {
		t.Fatal("expected spot-check samples for the completed run")
	}
	sample := samples[0].(map[string]any)
	postExpect(t, c, fmt.Sprintf("%s/api/runs/%d/spot-check/%d", env.ts.URL, runID, int64(sample["id"].(float64))),
		map[string]any{"verdict": "adjusted", "note": "score too generous"}, http.StatusOK)
	ws = warningsFor(t, c, env.ts.URL, aid)
	wantWarning(t, ws, "adjusted_spot_checks", "warning", 1)

	// A new method version + a run over ONE answer must not move the exam's
	// already-selected source. The source is the exact completed run selected
	// above, not the mutable logical method or whichever run completed latest.
	checkedAnswer := int64(sample["answer_id"].(float64))
	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	var otherAnswer int64
	for _, s := range students["students"] {
		if id := int64(s["answer_id"].(float64)); id != checkedAnswer {
			otherAnswer = id
		}
	}
	if otherAnswer == 0 {
		t.Fatal("no second answer to re-grade")
	}
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	postExpect(t, c, fmt.Sprintf("%s/api/methods/%d/versions", env.ts.URL, methodID), map[string]any{
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1",
			"temperature": 0, "ref_solutions": 0, "reask_cap": 3,
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	run2 := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "answer", "scope_id": otherAnswer, "method_id": methodID,
	}, http.StatusCreated)
	run2ID := int64(run2["id"].(float64))
	driveRun(t, env, run2ID, false)

	ws = warningsFor(t, c, env.ts.URL, aid)
	refuteWarning(t, ws, "mixed_method_versions")
	// run2 never touched the adjusted answer's official — still outstanding.
	wantWarning(t, ws, "adjusted_spot_checks", "warning", 1)

	pv := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", env.ts.URL, aid), http.StatusOK)
	fs := pv["final_source"].(map[string]any)
	if got := int64(fs["spot_check_run_id"].(float64)); got != runID {
		t.Fatalf("final source spot-check moved to newer run %d; want pinned run %d", got, runID)
	}
	var officialsFromNewRun int
	if err := env.st.Pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM answers a
		JOIN grading_records gr ON gr.id = a.official_record_id
		WHERE a.assessment_id = $1 AND gr.run_id = $2`, aid, run2ID).Scan(&officialsFromNewRun); err != nil {
		t.Fatal(err)
	}
	if officialsFromNewRun != 0 {
		t.Fatalf("newer run replaced %d pinned official(s)", officialsFromNewRun)
	}
}
