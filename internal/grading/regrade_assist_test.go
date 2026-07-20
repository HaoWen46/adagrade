package grading

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// regradeReady drives a fake-model run to completion, sets the first student's answer
// record official, and creates a FILED regrade request with ONE sub-item for that
// student's contested problem (regrade v2 §5: AI assist re-scopes to one sub-item per
// job). Returns the sub-item id, the contested answer id, and the official record id.
// complaintText is the (un-redacted) student complaint stored on the sub-item.
func regradeReady(t *testing.T, h *spotCheckHarness, complaintText string) (subItemID, answerID, officialRecordID, studentID, assessmentID int64) {
	t.Helper()
	ctx := context.Background()
	q := h.st.Q

	// Seed the regrade_v1 template so RegradeAssistForSubItem can resolve it.
	if _, err := EnsureRegradeTemplateSeed(ctx, h.st); err != nil {
		t.Fatalf("seed regrade template: %v", err)
	}

	runID := h.buildRun(t, 1)
	driveSpotCheckRun(t, h, runID, true)

	run, err := q.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	items, err := q.ListRunItems(ctx, db.ListRunItemsParams{RunID: runID, ItemLimit: 100})
	if err != nil {
		t.Fatalf("list run items: %v", err)
	}
	if len(items) != 1 || items[0].State != "succeeded" || !items[0].RecordID.Valid {
		t.Fatalf("expected 1 succeeded item with a record, got %+v", items)
	}
	answerID = items[0].AnswerID
	officialRecordID = items[0].RecordID.Int64
	answer, err := q.GetAnswer(ctx, answerID)
	if err != nil {
		t.Fatalf("get answer: %v", err)
	}
	studentID = answer.StudentID
	assessmentID = run.AssessmentID
	// Officials are derived since 0027; fixtures poke the pointer directly.
	if _, err := h.st.Pool.Exec(ctx,
		`UPDATE answers SET official_record_id = $2, official_set_at = now() WHERE id = $1`,
		answerID, officialRecordID); err != nil {
		t.Fatalf("set official: %v", err)
	}

	itemID := h.seedPublishItem(t, assessmentID, studentID)
	subItemID = filedRequestWithSubItem(t, h.st, itemID, studentID, assessmentID, answer.ProblemID, complaintText)
	return subItemID, answerID, officialRecordID, studentID, assessmentID
}

// seedPublishItem creates a throwaway publish batch + item for a (student, assessment) so
// a kind='filed' regrade request has a valid publish_item_id to satisfy the FK + the
// filed-needs-slot CHECK (migration 0025). These grading tests never re-verify the token
// chain, so the item's snapshot/token/recipient are inert placeholders. Invented data
// only (CLAUDE.md).
func (h *spotCheckHarness) seedPublishItem(t *testing.T, assessmentID, studentID int64) int64 {
	t.Helper()
	ctx := context.Background()
	batch, err := h.st.Q.CreatePublishBatch(ctx, db.CreatePublishBatchParams{
		AssessmentID: assessmentID, Attachment: "none",
	})
	if err != nil {
		t.Fatalf("create publish batch: %v", err)
	}
	item, err := h.st.Q.CreatePublishItem(ctx, db.CreatePublishItemParams{
		BatchID: batch.ID, StudentID: studentID, Snapshot: []byte("{}"),
		RecipientEmail: "student@x.edu", EmailStatus: "sent",
	})
	if err != nil {
		t.Fatalf("create publish item: %v", err)
	}
	return item.ID
}

// secondGradedProblem creates a SECOND problem (given number) in an assessment with its
// own officially-graded answer for the student — used by the context-isolation test to
// prove a second contested problem's complaint never leaks into the first's prompt.
// Returns the second answer id and problem id. Invented data only (CLAUDE.md).
func (h *spotCheckHarness) secondGradedProblem(t *testing.T, assessmentID, studentID int64, number int32) (answerID, problemID int64) {
	t.Helper()
	ctx := context.Background()
	q := h.st.Q

	tenPts, _ := store.Num("10")
	prob, err := q.CreateProblem(ctx, db.CreateProblemParams{
		AssessmentID: assessmentID, Number: number, Title: "P" + string(rune('0'+number)), MaxPoints: tenPts, Position: int32(number),
	})
	if err != nil {
		t.Fatalf("create second problem: %v", err)
	}
	half, _ := store.Num("0.5")
	rv, err := q.CreateRubricVersion(ctx, db.CreateRubricVersionParams{ProblemID: prob.ID, ScoreIncrement: half})
	if err != nil {
		t.Fatalf("create rubric version: %v", err)
	}
	if _, err := q.CreateRubricCriterion(ctx, db.CreateRubricCriterionParams{
		RubricVersionID: rv.ID, Position: 1, Description: "A", Points: tenPts,
	}); err != nil {
		t.Fatalf("create criterion: %v", err)
	}

	// Reuse the first problem's method version (any pinned method works for the pins).
	var methodVersionID int64
	if err := h.st.Pool.QueryRow(ctx, "SELECT id FROM grading_method_versions ORDER BY id LIMIT 1").Scan(&methodVersionID); err != nil {
		t.Fatalf("find method version: %v", err)
	}
	var tplID int64
	if err := h.st.Pool.QueryRow(ctx, "SELECT id FROM prompt_template_versions WHERE name = 't' ORDER BY id LIMIT 1").Scan(&tplID); err != nil {
		t.Fatalf("find prompt template: %v", err)
	}

	// Reuse the student's existing whole submission (the active-whole unique index
	// forbids a second one) for the new problem's answer + one accepted-masked page.
	var submissionID int64
	if err := h.st.Pool.QueryRow(ctx,
		"SELECT id FROM submissions WHERE assessment_id = $1 AND student_id = $2 ORDER BY id LIMIT 1",
		assessmentID, studentID).Scan(&submissionID); err != nil {
		t.Fatalf("find existing submission: %v", err)
	}
	answer, err := q.EnsureAnswer(ctx, db.EnsureAnswerParams{AssessmentID: assessmentID, StudentID: studentID, ProblemID: prob.ID})
	if err != nil {
		t.Fatalf("ensure answer: %v", err)
	}
	page, err := q.CreateAnswerPage(ctx, db.CreateAnswerPageParams{
		AnswerID: answer.ID, PageIndex: 0, SubmissionID: submissionID, PdfPageIndex: 0,
		ImageRef: "raw/p2", ImageSha256: "img-sha-p2", ImageWidth: 100, ImageHeight: 100,
	})
	if err != nil {
		t.Fatalf("create answer page: %v", err)
	}
	maskedKey := "assessments/masked/p2.jpg"
	if _, _, err := h.blobs.Put(ctx, maskedKey, bytesReader([]byte("fake-jpeg-p2"))); err != nil {
		t.Fatalf("put masked blob: %v", err)
	}
	if _, err := q.SetPageMasked(ctx, db.SetPageMaskedParams{ID: page.ID, MaskedImageRef: pgtype.Text{String: maskedKey, Valid: true}}); err != nil {
		t.Fatalf("set page masked: %v", err)
	}
	if _, err := q.SetMaskReview(ctx, db.SetMaskReviewParams{ID: page.ID, MaskReviewStatus: "accepted"}); err != nil {
		t.Fatalf("accept mask review: %v", err)
	}

	// An official regrade-AI-style record with pinned method/rubric so ContestedAnswerForSubItem finds it.
	scores := []byte(`[{"criterion_id":1,"score":"5","rationale":"ok"}]`)
	fivePts, _ := store.Num("5")
	rec, err := q.InsertRegradeAIRecord(ctx, db.InsertRegradeAIRecordParams{
		AnswerID:                answer.ID,
		Provider:                pgtype.Text{String: "fake", Valid: true},
		ModelID:                 pgtype.Text{String: "fake-vision-1", Valid: true},
		MethodVersionID:         pgtype.Int8{Int64: methodVersionID, Valid: true},
		RubricVersionID:         rv.ID,
		PromptTemplateVersionID: pgtype.Int8{Int64: tplID, Valid: true},
		GradedImageShas:         []string{"img-sha-p2"},
		CriterionScores:         scores,
		Total:                   fivePts,
		Confidence:              pgtype.Text{String: "high", Valid: true},
		Adjustments:             []byte("[]"),
		RawOutput:               []byte("{}"),
	})
	if err != nil {
		t.Fatalf("insert official record for p2: %v", err)
	}
	// Officials are derived since 0027; fixtures poke the pointer directly.
	if _, err := h.st.Pool.Exec(ctx,
		`UPDATE answers SET official_record_id = $2, official_set_at = now() WHERE id = $1`,
		answer.ID, rec.ID); err != nil {
		t.Fatalf("set p2 official: %v", err)
	}
	return answer.ID, prob.ID
}

// filedRequestWithSubItem inserts a filed (turn-1) regrade request for a student and
// attaches ONE sub-item for the contested problem — the v2 shape every AI-regrade test
// starts from. It needs a publish_item id + turn for the kind='filed' CHECK; a synthetic
// item id is fine here (these grading tests never re-verify the token chain), so we mint
// a throwaway publish batch/item to satisfy the FK-free item column. Returns the sub-item
// id. Invented data only (CLAUDE.md).
func filedRequestWithSubItem(t *testing.T, st *store.Store, itemID, studentID, assessmentID, problemID int64, complaint string) int64 {
	t.Helper()
	ctx := context.Background()
	rr, err := st.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID,
		StudentID:     studentID,
		AssessmentID:  assessmentID,
		FromEmail:     "student@x.edu",
		Subject:       "Re: results",
		Body:          complaint,
		Status:        "received",
		Kind:          "filed",
		Turn:          1,
	})
	if err != nil {
		t.Fatalf("insert filed regrade request: %v", err)
	}
	subs, err := st.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{
		{ProblemID: problemID, ComplaintText: complaint},
	})
	if err != nil {
		t.Fatalf("insert sub-item: %v", err)
	}
	return subs[0].ID
}

// TestRegradeAssist_AppendsRecordSourcePolicyLinkage covers the core D50 contract: the
// AI re-grade appends a grading_records row with source='regrade_ai', policy=
// 'regrade_strict', run_id NULL, method/rubric pinned from the contested official record,
// and links it to the SUB-ITEM via ai_record_id — WITHOUT touching the official pointer.
func TestRegradeAssist_AppendsRecordSourcePolicyLinkage(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, answerID, officialRecordID, _, _ := regradeReady(t, h, "please recheck, I think I deserve more")
	ctx := context.Background()
	q := h.st.Q

	contested, err := q.GetRecord(ctx, officialRecordID)
	if err != nil {
		t.Fatalf("get contested record: %v", err)
	}

	if err := h.runner.RegradeAssistForSubItem(ctx, subItemID); err != nil {
		t.Fatalf("RegradeAssistForSubItem: %v", err)
	}

	// The sub-item now links to an AI record.
	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if !sub.AiRecordID.Valid {
		t.Fatalf("sub-item should link an ai_record_id after the AI re-grade")
	}
	aiRec, err := q.GetRecord(ctx, sub.AiRecordID.Int64)
	if err != nil {
		t.Fatalf("get ai record: %v", err)
	}
	if aiRec.Source != "regrade_ai" {
		t.Errorf("source = %q, want regrade_ai", aiRec.Source)
	}
	if !aiRec.Policy.Valid || aiRec.Policy.String != PolicyRegradeStrict {
		t.Errorf("policy = %+v, want regrade_strict", aiRec.Policy)
	}
	if aiRec.RunID.Valid {
		t.Errorf("regrade_ai record must have NULL run_id, got %+v", aiRec.RunID)
	}
	if aiRec.CreatedBy.Valid {
		t.Errorf("regrade_ai record is machine-authored — created_by must be NULL, got %+v", aiRec.CreatedBy)
	}
	if aiRec.MethodVersionID != contested.MethodVersionID {
		t.Errorf("method_version_id = %+v, want pinned-from-contested %+v", aiRec.MethodVersionID, contested.MethodVersionID)
	}
	if aiRec.RubricVersionID != contested.RubricVersionID {
		t.Errorf("rubric_version_id = %d, want pinned-from-contested %d", aiRec.RubricVersionID, contested.RubricVersionID)
	}
	if !aiRec.PromptTemplateVersionID.Valid {
		t.Errorf("regrade record should pin the regrade_v1 prompt template version")
	}

	// The official pointer is UNTOUCHED (never auto-official, D50).
	answer, err := q.GetAnswer(ctx, answerID)
	if err != nil {
		t.Fatalf("get answer: %v", err)
	}
	if !answer.OfficialRecordID.Valid || answer.OfficialRecordID.Int64 != officialRecordID {
		t.Errorf("official pointer moved: got %+v, want unchanged %d", answer.OfficialRecordID, officialRecordID)
	}
}

// TestRegradeAssist_RedactsIdentityBeforeProvider is the D51 privacy test: the student's
// roster name, id, and email appear in the complaint but must NOT reach the provider.
func TestRegradeAssist_RedactsIdentityBeforeProvider(t *testing.T) {
	h := newSpotCheckHarness(t)
	body := "Hi, I'm Wendell Quokka (b09901777) at wendell.quokka@example.edu, reply was regrade+deadbeef99@inbound.example.edu. My recurrence in part (b) is correct, please recheck."
	subItemID, _, _, studentID, _ := regradeReady(t, h, body)
	ctx := context.Background()

	student, err := h.st.Q.GetStudent(ctx, studentID)
	if err != nil {
		t.Fatalf("get student: %v", err)
	}
	const realID = "b09901777"
	if _, err := h.st.Pool.Exec(ctx,
		"UPDATE students SET name = 'Wendell Quokka', student_id = $2, email = 'wendell.quokka@example.edu' WHERE id = $1",
		student.ID, realID); err != nil {
		t.Fatalf("set roster identity: %v", err)
	}
	if _, err := h.st.Pool.Exec(ctx, "UPDATE regrade_request_problems SET complaint_text = $2 WHERE id = $1", subItemID, body); err != nil {
		t.Fatalf("rewrite complaint: %v", err)
	}

	h.fakeProv.Calls = nil
	if err := h.runner.RegradeAssistForSubItem(ctx, subItemID); err != nil {
		t.Fatalf("RegradeAssistForSubItem: %v", err)
	}
	if len(h.fakeProv.Calls) == 0 {
		t.Fatal("fake provider received no Grade call")
	}
	last := h.fakeProv.Calls[len(h.fakeProv.Calls)-1]
	sent := last.Prompt + "\n" + last.System
	for _, leaked := range []string{"Wendell Quokka", realID, "wendell.quokka@example.edu", "regrade+deadbeef99@inbound.example.edu"} {
		if strings.Contains(sent, leaked) {
			t.Errorf("identity %q reached the provider prompt:\n%s", leaked, sent)
		}
	}
	if !strings.Contains(sent, "recurrence in part (b) is correct") {
		t.Errorf("substantive request text was lost:\n%s", sent)
	}
}

// TestRegradeAssist_ContextIsolationPerSubItem (spec §10, NORMATIVE): the per-sub-item
// re-scope means problem 1's re-grade prompt must NEVER contain problem 4's complaint
// text. Two contested problems are filed on one request with distinctive complaints;
// re-grading problem 1's sub-item must send problem 1's complaint and nothing of problem
// 4's.
func TestRegradeAssist_ContextIsolationPerSubItem(t *testing.T) {
	h := newSpotCheckHarness(t)
	// regradeReady sets up one problem + one official answer + a filed request with a
	// sub-item for it (problem 1). Add a SECOND problem (number 4) with its own official
	// answer, and a second sub-item on the SAME request with a distinctive complaint.
	p1SubItemID, _, _, studentID, assessmentID := regradeReady(t, h, "P1 COMPLAINT: my base case n=1 is correct")
	ctx := context.Background()
	q := h.st.Q

	// The p1 sub-item's request, to attach a p4 sub-item to.
	p1Sub, err := h.st.GetRequestProblem(ctx, p1SubItemID)
	if err != nil {
		t.Fatalf("get p1 sub-item: %v", err)
	}

	// A second problem (number 4) with an officially-graded answer for the same student.
	p4AnswerID, p4ProblemID := h.secondGradedProblem(t, assessmentID, studentID, 4)
	const p4Complaint = "P4 SECRET COMPLAINT: exchange argument handles ties uniquely-phrased-marker"
	if _, err := h.st.InsertRequestProblems(ctx, p1Sub.RequestID, []store.RequestProblemInput{
		{ProblemID: p4ProblemID, ComplaintText: p4Complaint},
	}); err != nil {
		t.Fatalf("attach p4 sub-item: %v", err)
	}
	_ = p4AnswerID

	// Re-grade ONLY the p1 sub-item.
	h.fakeProv.Calls = nil
	if err := h.runner.RegradeAssistForSubItem(ctx, p1SubItemID); err != nil {
		t.Fatalf("RegradeAssistForSubItem(p1): %v", err)
	}
	if len(h.fakeProv.Calls) == 0 {
		t.Fatal("fake provider received no Grade call for p1")
	}
	last := h.fakeProv.Calls[len(h.fakeProv.Calls)-1]
	sent := last.Prompt + "\n" + last.System
	if !strings.Contains(sent, "P1 COMPLAINT") {
		t.Errorf("p1's own complaint must be present in its prompt:\n%s", sent)
	}
	if strings.Contains(sent, "P4 SECRET COMPLAINT") || strings.Contains(sent, "uniquely-phrased-marker") {
		t.Errorf("CONTEXT LEAK: problem 4's complaint reached problem 1's prompt:\n%s", sent)
	}
	// The AI record linked to p1's sub-item, never p4's.
	p1After, _ := h.st.GetRequestProblem(ctx, p1SubItemID)
	if !p1After.AiRecordID.Valid {
		t.Errorf("p1 sub-item should link an ai_record after its re-grade")
	}
	if rec, err := q.GetRecord(ctx, p1After.AiRecordID.Int64); err == nil && rec.AnswerID == p4AnswerID {
		t.Errorf("p1's AI record points at p4's answer — wrong answer graded")
	}
}

// TestRegradeAssist_RedactsOriginalGradeIdentityBeforeProvider (I3, D51): the CONTESTED
// record's own comment + per-criterion rationales reach the provider too and must be
// redacted with the SAME identity as the complaint.
func TestRegradeAssist_RedactsOriginalGradeIdentityBeforeProvider(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, officialRecordID, studentID, _ := regradeReady(t, h, "please recheck part (b)")
	ctx := context.Background()

	const realID = "b09902888"
	if _, err := h.st.Pool.Exec(ctx,
		"UPDATE students SET name = 'Priya Pangolin', student_id = $2, email = 'priya.pangolin@example.edu' WHERE id = $1",
		studentID, realID); err != nil {
		t.Fatalf("set roster identity: %v", err)
	}

	if _, err := h.st.Pool.Exec(ctx,
		`UPDATE grading_records
		   SET source = 'human',
		       created_by = $2,
		       comment = 'Priya Pangolin (b09902888) lost a point on the base case',
		       criterion_scores = '[{"criterion_id":1,"score":"3","rationale":"Priya Pangolin did not justify the recurrence"}]'::jsonb
		 WHERE id = $1`, officialRecordID, h.checkerID); err != nil {
		t.Fatalf("seed human contested record with identity: %v", err)
	}

	h.fakeProv.Calls = nil
	if err := h.runner.RegradeAssistForSubItem(ctx, subItemID); err != nil {
		t.Fatalf("RegradeAssistForSubItem: %v", err)
	}
	if len(h.fakeProv.Calls) == 0 {
		t.Fatal("fake provider received no Grade call")
	}
	last := h.fakeProv.Calls[len(h.fakeProv.Calls)-1]
	sent := last.Prompt + "\n" + last.System
	for _, leaked := range []string{"Priya Pangolin", realID} {
		if strings.Contains(sent, leaked) {
			t.Errorf("original-grade identity %q reached the provider prompt:\n%s", leaked, sent)
		}
	}

	if idx := strings.Index(last.Prompt, "# Original grade under review"); idx >= 0 {
		block := last.Prompt[idx:]
		if end := strings.Index(block, "# Student's regrade request"); end >= 0 {
			block = block[:end]
		}
		for _, leaked := range []string{"Priya Pangolin", realID} {
			if strings.Contains(block, leaked) {
				t.Errorf("identity %q leaked in the original-grade block specifically:\n%s", leaked, block)
			}
		}
	}
}

// TestRegradeAssist_ProviderRemovedIsError covers the "AI unavailable — provider removed"
// path (spec §8).
func TestRegradeAssist_ProviderRemovedIsError(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, _, _, _ := regradeReady(t, h, "recheck please")
	ctx := context.Background()

	h.runner.Providers = llm.StaticSource{}
	err := h.runner.RegradeAssistForSubItem(ctx, subItemID)
	if err == nil {
		t.Fatal("expected an error when the contested method's provider is gone")
	}
	var unavailable *llm.ProviderUnavailableError
	if !errors.As(err, &unavailable) {
		t.Errorf("expected a ProviderUnavailableError, got %v", err)
	}
	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if sub.AiRecordID.Valid {
		t.Errorf("no AI record should be linked when the provider is gone, got %+v", sub.AiRecordID)
	}
}

// TestRegradeAssist_ShutdownCancellationNotTerminal is the F17 half for the regrade.ai
// job.
func TestRegradeAssist_ShutdownCancellationNotTerminal(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, _, _, _ := regradeReady(t, h, "recheck please")
	ctx := context.Background()

	h.runner.Providers = llm.StaticSource{"fake": &fake.Provider{
		Script: []fake.Step{{Err: context.Canceled}},
	}}
	err := h.runner.RegradeAssistForSubItem(ctx, subItemID)
	if err == nil {
		t.Fatal("expected the cancellation to surface (so River records a plain attempt)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("returned error should wrap context.Canceled, got %v", err)
	}
	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if sub.AiRecordID.Valid {
		t.Errorf("a cancelled re-grade must NOT link an ai_record_id, got %+v", sub.AiRecordID)
	}
	var n int
	if err := h.st.Pool.QueryRow(ctx, "SELECT count(*) FROM grading_records WHERE source = 'regrade_ai'").Scan(&n); err != nil {
		t.Fatalf("count regrade_ai: %v", err)
	}
	if n != 0 {
		t.Errorf("a cancelled re-grade must append no regrade_ai record, got %d", n)
	}
}

// TestRegradeAssist_NoContestedRecordTerminal: a sub-item whose student has no official
// grade for that problem returns ErrNoContestedRecord (terminal).
func TestRegradeAssist_NoContestedRecordTerminal(t *testing.T) {
	h := newSpotCheckHarness(t)
	ctx := context.Background()
	if _, err := EnsureRegradeTemplateSeed(ctx, h.st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runID := h.buildRun(t, 1)
	driveSpotCheckRun(t, h, runID, true)
	run, _ := h.st.Q.GetRun(ctx, runID)
	items, _ := h.st.Q.ListRunItems(ctx, db.ListRunItemsParams{RunID: runID, ItemLimit: 10})
	answer, _ := h.st.Q.GetAnswer(ctx, items[0].AnswerID)

	// Filed request + sub-item, but NO official record set on the answer.
	itemID := h.seedPublishItem(t, run.AssessmentID, answer.StudentID)
	subItemID := filedRequestWithSubItem(t, h.st, itemID, answer.StudentID, run.AssessmentID, answer.ProblemID, "recheck")
	err := h.runner.RegradeAssistForSubItem(ctx, subItemID)
	if !errors.Is(err, ErrNoContestedRecord) {
		t.Fatalf("want ErrNoContestedRecord, got %v", err)
	}

	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if !sub.AiError.Valid || sub.AiError.String != AIErrorNoContestedRecord {
		t.Errorf("ai_error = %+v, want %q", sub.AiError, AIErrorNoContestedRecord)
	}
}

// TestRegradeAssist_NoMethodPinned_PersistsAIError (M-drift4): a contested record with no
// pinned method version is skipped but persists an ai_error on the sub-item.
func TestRegradeAssist_NoMethodPinned_PersistsAIError(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, officialRecordID, _, _ := regradeReady(t, h, "recheck please")
	ctx := context.Background()

	if _, err := h.st.Pool.Exec(ctx,
		`UPDATE grading_records
		   SET source = 'human', created_by = $2, model_id = NULL, method_version_id = NULL
		 WHERE id = $1`, officialRecordID, h.checkerID); err != nil {
		t.Fatalf("null out method on contested record: %v", err)
	}

	if err := h.runner.RegradeAssistForSubItem(ctx, subItemID); err != nil {
		t.Fatalf("RegradeAssistForSubItem: %v", err)
	}

	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if !sub.AiError.Valid || sub.AiError.String != AIErrorNoMethodPinned {
		t.Errorf("ai_error = %+v, want %q", sub.AiError, AIErrorNoMethodPinned)
	}
	if sub.AiRecordID.Valid {
		t.Errorf("no AI record should be linked for a skipped no-method answer, got %+v", sub.AiRecordID)
	}
}

// TestRegradeAssist_ProviderRemoved_PersistsAIError covers Finding 3: the provider-removed
// failure must be persisted on the SUB-ITEM's ai_error column.
func TestRegradeAssist_ProviderRemoved_PersistsAIError(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, _, _, _ := regradeReady(t, h, "recheck please")
	ctx := context.Background()

	h.runner.Providers = llm.StaticSource{}
	if err := h.runner.RegradeAssistForSubItem(ctx, subItemID); err == nil {
		t.Fatal("expected an error when the contested method's provider is gone")
	}

	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if !sub.AiError.Valid || sub.AiError.String != AIErrorProviderRemoved {
		t.Errorf("ai_error = %+v, want %q", sub.AiError, AIErrorProviderRemoved)
	}
	if sub.AiRecordID.Valid {
		t.Errorf("no AI record should be linked, got %+v", sub.AiRecordID)
	}
}

// TestRegradeAssist_MalformedPastReaskCap_PersistsAIError covers Finding 3's third
// terminal path: malformed-past-cap persists a short constant reason on the sub-item.
func TestRegradeAssist_MalformedPastReaskCap_PersistsAIError(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, _, _, _ := regradeReady(t, h, "recheck please")
	ctx := context.Background()

	h.runner.Providers = llm.StaticSource{"fake": &fake.Provider{
		Script: []fake.Step{{MalformedJSON: true}, {MalformedJSON: true}, {MalformedJSON: true}},
	}}
	err := h.runner.RegradeAssistForSubItem(ctx, subItemID)
	if err == nil {
		t.Fatal("expected a terminal malformed-output error")
	}
	var mal MalformedError
	if !errors.As(err, &mal) {
		t.Fatalf("expected a MalformedError, got %v", err)
	}

	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if !sub.AiError.Valid || sub.AiError.String != AIErrorOutputInvalid {
		t.Errorf("ai_error = %+v, want %q (a short constant, never the model's raw text)", sub.AiError, AIErrorOutputInvalid)
	}
	if sub.AiRecordID.Valid {
		t.Errorf("no AI record should be linked, got %+v", sub.AiRecordID)
	}
}

// TestRegradeAssist_SuccessClearsPriorAIError covers Finding 3's clear-on-success half on
// the sub-item.
func TestRegradeAssist_SuccessClearsPriorAIError(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, _, _, _ := regradeReady(t, h, "recheck please")
	ctx := context.Background()

	if _, err := h.st.SetProblemAIError(ctx, subItemID, AIErrorProviderRemoved); err != nil {
		t.Fatalf("seed prior ai_error: %v", err)
	}

	if err := h.runner.RegradeAssistForSubItem(ctx, subItemID); err != nil {
		t.Fatalf("RegradeAssistForSubItem: %v", err)
	}

	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if sub.AiError.Valid {
		t.Errorf("ai_error should be cleared after a successful re-grade, got %+v", sub.AiError)
	}
	if !sub.AiRecordID.Valid {
		t.Errorf("expected an AI record linked after success")
	}
}

// TestRegradeAssist_ResolvedBetweenEnqueueAndExecution_SkipsQuietly covers Finding 4: a
// request resolved (a result already sent) between enqueue and execution is skipped
// quietly — no record, no ai_error, no error.
func TestRegradeAssist_ResolvedBetweenEnqueueAndExecution_SkipsQuietly(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, _, _, _ := regradeReady(t, h, "recheck please")
	ctx := context.Background()

	sub, _ := h.st.GetRequestProblem(ctx, subItemID)
	if _, err := h.st.SetRegradeStatus(ctx, sub.RequestID, "resolved_upheld"); err != nil {
		t.Fatalf("resolve request: %v", err)
	}

	if err := h.runner.RegradeAssistForSubItem(ctx, subItemID); err != nil {
		t.Fatalf("RegradeAssistForSubItem should skip quietly for a resolved request, got error: %v", err)
	}

	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if sub.AiRecordID.Valid {
		t.Errorf("no AI record should be appended for a request resolved before execution, got %+v", sub.AiRecordID)
	}
	if sub.AiError.Valid {
		t.Errorf("no ai_error should be written for a quiet eligibility skip, got %+v", sub.AiError)
	}
	var n int
	if err := h.st.Pool.QueryRow(ctx, "SELECT count(*) FROM grading_records WHERE source = 'regrade_ai'").Scan(&n); err != nil {
		t.Fatalf("count regrade_ai: %v", err)
	}
	if n != 0 {
		t.Errorf("no regrade_ai record should be appended, got %d", n)
	}
}

// TestRegradeAssist_StillEligible_Proceeds is the control case for Finding 4.
func TestRegradeAssist_StillEligible_Proceeds(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, _, _, _ := regradeReady(t, h, "recheck please")
	ctx := context.Background()

	if err := h.runner.RegradeAssistForSubItem(ctx, subItemID); err != nil {
		t.Fatalf("RegradeAssistForSubItem: %v", err)
	}
	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if !sub.AiRecordID.Valid {
		t.Errorf("expected an AI record linked for a still-eligible request")
	}
}

// TestRegradeAssist_ShutdownCancellation_DoesNotPersistAIError: a mid-flight cancellation
// must NOT persist an ai_error on the sub-item.
func TestRegradeAssist_ShutdownCancellation_DoesNotPersistAIError(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, _, _, _ := regradeReady(t, h, "recheck please")
	ctx := context.Background()

	h.runner.Providers = llm.StaticSource{"fake": &fake.Provider{
		Script: []fake.Step{{Err: context.Canceled}},
	}}
	if err := h.runner.RegradeAssistForSubItem(ctx, subItemID); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a wrapped context.Canceled, got %v", err)
	}

	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if sub.AiError.Valid {
		t.Errorf("ai_error must not be set on a mid-flight cancellation, got %+v", sub.AiError)
	}
}
