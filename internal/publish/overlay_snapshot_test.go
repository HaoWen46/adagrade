package publish

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// TestPublishSnapshot_AppliesAdoptedOverlay_AndFlagsChanged is the BUG 1 regression: the
// snapshot pipeline (PublishSnapshotInputs → buildSnapshots) must apply the SAME
// effective-grade overlay as ExportRows. Before the fix it read round-0 official only, so
// after a regrade raised the grade a republish/resend re-emailed the pre-regrade figure
// and the changed-only diff never flagged the student. After the fix the rebuilt snapshot
// carries the adopted total and the diff flags the student.
func TestPublishSnapshot_AppliesAdoptedOverlay_AndFlagsChanged(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()
	aid := seedPublishableAssessment(t, st) // one student (e01), official total 8

	svc := NewService(st, nil, "none", 14*24*time.Hour, "", false, nil)
	if _, err := svc.Publish(ctx, aid, "first", false, 0, "none", false); err != nil {
		t.Fatalf("initial Publish: %v", err)
	}

	// Resolve the seeded student/answer/problem + the official record's rubric version.
	inputs, err := st.Q.PublishSnapshotInputs(ctx, aid)
	if err != nil || len(inputs) != 1 {
		t.Fatalf("PublishSnapshotInputs: %d rows, err=%v", len(inputs), err)
	}
	studentID, answerID, problemID := inputs[0].StudentID, inputs[0].AnswerID, inputs[0].ProblemID
	if store.NumStr(inputs[0].Total) != "8" {
		t.Fatalf("baseline snapshot input total = %q, want 8", store.NumStr(inputs[0].Total))
	}
	var rubricVersionID int64
	if err := st.Pool.QueryRow(ctx,
		`SELECT gr.rubric_version_id FROM grading_records gr
		 JOIN answers a ON a.official_record_id = gr.id WHERE a.id = $1`, answerID).Scan(&rubricVersionID); err != nil {
		t.Fatalf("resolve rubric version: %v", err)
	}

	// The live batch's publish item for e01 — the anchor a regrade files against.
	batches, err := st.ListPublishBatches(ctx, aid)
	if err != nil || len(batches) != 1 {
		t.Fatalf("ListPublishBatches: %d, err=%v", len(batches), err)
	}
	items, err := st.ListPublishItems(ctx, batches[0].ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("ListPublishItems: %d, err=%v", len(items), err)
	}
	itemID := items[0].ID

	// Adopt a regrade overlay raising the grade to 9.5.
	total95, err := store.Num("9.5")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.Q.InsertRegradeAIRecord(ctx, db.InsertRegradeAIRecordParams{
		AnswerID:        answerID,
		ModelID:         pgtype.Text{String: "overlay-model", Valid: true},
		RubricVersionID: rubricVersionID,
		GradedImageShas: []string{},
		CriterionScores: []byte(`[]`),
		Total:           total95,
		Comment:         "point restored",
		Adjustments:     []byte(`[]`),
		Policy:          pgtype.Text{String: "regrade_strict", Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertRegradeAIRecord: %v", err)
	}
	rr, err := st.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: studentID, AssessmentID: aid,
		FromEmail: "enid@example.edu", Status: "resolved_regraded", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}
	subs, err := st.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{{ProblemID: problemID}})
	if err != nil || len(subs) != 1 {
		t.Fatalf("InsertRequestProblems: %v", err)
	}
	if _, err := st.SetProblemVerdictAndAdoption(ctx, store.SetProblemVerdictAndAdoptionParams{
		SubItemID: subs[0].ID, Verdict: "regraded", Note: "restored", AdoptedRecordID: rec.ID,
	}); err != nil {
		t.Fatalf("SetProblemVerdictAndAdoption: %v", err)
	}

	// The rebuilt snapshot must now carry the ADOPTED total (9.5), not round 0 (8).
	inputs2, err := st.Q.PublishSnapshotInputs(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	criteria, err := st.PublishCriteria(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	snaps, _, _, _ := buildSnapshots("Enqueue Fixture", inputs2, criteria)
	if got := snaps[studentID].Total; got != "9.5" {
		t.Fatalf("rebuilt snapshot total = %q, want 9.5 (the adopted overlay, not round-0 official 8)", got)
	}

	// The changed-only diff must now flag e01 (its snapshot moved 8 → 9.5), so a
	// republish/resend picks the student up instead of silently re-sending the old grade.
	preview, err := svc.GetPreview(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	flagged := false
	for _, ref := range preview.Changed {
		if ref.StudentID == studentID {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("changed-only diff did not flag the regraded student (Changed=%+v)", preview.Changed)
	}
}
