package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// mustBatchItem publishes a one-item batch for the fixture student and returns
// (batchID, publishItemID) — the anchor a regrade request files against, and the
// handle a later Supersede uses to open a new publish chain.
func mustBatchItem(t *testing.T, s *store.Store, f fixture) (int64, int64) {
	t.Helper()
	batch, items, err := s.CreatePublishBatch(context.Background(), store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}
	return batch.ID, items[0].ID
}

// mustAdoptedRecord inserts a source='regrade_ai' grading_record with a real total and
// criterion scores — the record a regraded verdict adopts as that turn's overlay grade.
func mustAdoptedRecord(t *testing.T, s *store.Store, answerID, rubricVersionID int64, total, scoresJSON string) int64 {
	t.Helper()
	totalNum, err := store.Num(total)
	if err != nil {
		t.Fatalf("Num total: %v", err)
	}
	rec, err := s.Q.InsertRegradeAIRecord(context.Background(), db.InsertRegradeAIRecordParams{
		AnswerID:        answerID,
		ModelID:         pgtype.Text{String: "overlay-model", Valid: true},
		RubricVersionID: rubricVersionID,
		GradedImageShas: []string{},
		CriterionScores: []byte(scoresJSON),
		Total:           totalNum,
		Comment:         "stricter re-examination",
		Adjustments:     []byte(`[]`),
		Policy:          pgtype.Text{String: "regrade_strict", Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertRegradeAIRecord: %v", err)
	}
	return rec.ID
}

// fileAndAdopt files a kind='filed' regrade request against (publishItemID, turn), adds
// one sub-item for the fixture problem, and adjudicates it with the given verdict +
// adopted record in one atomic write. Returns the sub-item id.
func fileAndAdopt(t *testing.T, s *store.Store, f fixture, publishItemID int64, turn int, verdict string, adoptedRecordID int64) int64 {
	t.Helper()
	ctx := context.Background()
	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: publishItemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Status: "resolved_regraded", Kind: "filed", Turn: turn,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2 (turn %d): %v", turn, err)
	}
	subs, err := s.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{{ProblemID: f.ProblemID}})
	if err != nil || len(subs) != 1 {
		t.Fatalf("InsertRequestProblems: %v", err)
	}
	if _, err := s.SetProblemVerdictAndAdoption(ctx, store.SetProblemVerdictAndAdoptionParams{
		SubItemID: subs[0].ID, Verdict: verdict, Note: "adjudicated", AdoptedRecordID: adoptedRecordID,
	}); err != nil {
		t.Fatalf("SetProblemVerdictAndAdoption: %v", err)
	}
	return subs[0].ID
}

func exportTotal(t *testing.T, s *store.Store, assessmentID int64) string {
	t.Helper()
	rows, err := s.Q.ExportRows(context.Background(), assessmentID)
	if err != nil {
		t.Fatalf("ExportRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ExportRows: got %d rows, want 1", len(rows))
	}
	return store.NumStr(rows[0].Total)
}

// TestOverlay_CrossChain_LiveChainWins is the BUG 2 regression: turn numbers restart per
// publish chain, so ORDER BY turn alone lets an OLD (superseded) chain's turn-2 adoption
// outrank the NEW (live) chain's chronologically-later turn-1 adoption. The effective
// grade must be the LIVE chain's newer adoption, not the superseded chain's higher turn.
func TestOverlay_CrossChain_LiveChainWins(t *testing.T) {
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "4") // round 0 = 4

	// Chain 1 (batch1): the student escalates to turn 2, adjudicated regraded → 7.
	batch1, item1 := mustBatchItem(t, s, f)
	recSeven := mustAdoptedRecord(t, s, f.AnswerID, f.RubricVersionID, "7", `[{"criterion_id":1,"score":"7"}]`)
	fileAndAdopt(t, s, f, item1, 2, "regraded", recSeven)
	if got := exportTotal(t, s, f.AssessmentID); got != "7" {
		t.Fatalf("after chain-1 turn-2 adoption, export total = %q, want 7", got)
	}

	// Unpublish → re-publish: a NEW chain (batch2). The student files fresh at turn 1,
	// adjudicated regraded → 9 (chronologically LATER than chain-1's turn 2).
	if err := s.SupersedePublishBatch(context.Background(), batch1, 0); err != nil {
		t.Fatalf("SupersedePublishBatch: %v", err)
	}
	_, item2 := mustBatchItem(t, s, f)
	recNine := mustAdoptedRecord(t, s, f.AnswerID, f.RubricVersionID, "9", `[{"criterion_id":1,"score":"9"}]`)
	fileAndAdopt(t, s, f, item2, 1, "regraded", recNine)

	// The LIVE chain's turn-1 (9) must win over the superseded chain's turn-2 (7).
	if got := exportTotal(t, s, f.AssessmentID); got != "9" {
		t.Fatalf("cross-chain overlay: export total = %q, want 9 (live chain's newer adoption, not the superseded chain's higher turn)", got)
	}
}

// TestOverlay_IgnoresStaleNonRegradedAdoption is the BUG 4(c) regression: overlay
// consumers must ignore an adopted_record_id whose verdict is not 'regraded'. The
// non-atomic verdict/adoption pair could leave a verdict='upheld' row still carrying a
// stale adopted record; the effective grade must then be round 0, not that stale record.
func TestOverlay_IgnoresStaleNonRegradedAdoption(t *testing.T) {
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "4")

	_, item1 := mustBatchItem(t, s, f)
	recSeven := mustAdoptedRecord(t, s, f.AnswerID, f.RubricVersionID, "7", `[{"criterion_id":1,"score":"7"}]`)
	// Inconsistent row: verdict='upheld' yet adopted_record_id still points at 7.
	fileAndAdopt(t, s, f, item1, 1, "upheld", recSeven)

	if got := exportTotal(t, s, f.AssessmentID); got != "4" {
		t.Fatalf("stale non-regraded adoption leaked into the export: total = %q, want 4 (round 0)", got)
	}
}

// TestContestedAnswerForSubItem_BriefsEffectiveGrade is the BUG 3 regression: a turn-N
// AI re-grade must be briefed against the CURRENT EFFECTIVE grade (round 0 overlaid by
// the latest prior adopted record), not round 0 — and it must EXCLUDE the sub-item being
// adjudicated now so it never briefs against its own prior adoption.
func TestContestedAnswerForSubItem_BriefsEffectiveGrade(t *testing.T) {
	s := storetest.Fresh(t)
	ctx := context.Background()
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "4") // round 0 = 4

	_, item1 := mustBatchItem(t, s, f)

	// Turn 1: adjudicated regraded → 7 (subA).
	recSeven := mustAdoptedRecord(t, s, f.AnswerID, f.RubricVersionID, "7", `[{"criterion_id":1,"score":"7"}]`)
	fileAndAdopt(t, s, f, item1, 1, "regraded", recSeven)

	// Turn 2: a fresh sub-item (subB) is now being adjudicated. Its re-grade must be
	// briefed against the turn-1 adopted record (7), not round 0 (4).
	rrB, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: item1, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Status: "received", Kind: "filed", Turn: 2,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2 (turn 2): %v", err)
	}
	subsB, err := s.InsertRequestProblems(ctx, rrB.ID, []store.RequestProblemInput{{ProblemID: f.ProblemID}})
	if err != nil {
		t.Fatalf("InsertRequestProblems (turn 2): %v", err)
	}
	subB := subsB[0].ID

	rows, err := s.ContestedAnswerForSubItem(ctx, f.AssessmentID, f.StudentID, f.ProblemID, subB)
	if err != nil {
		t.Fatalf("ContestedAnswerForSubItem: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ContestedAnswerForSubItem: got %d rows, want 1", len(rows))
	}
	if rows[0].RecordID != recSeven {
		t.Fatalf("turn-2 briefing record = %d, want the turn-1 adopted record %d (not round 0)", rows[0].RecordID, recSeven)
	}

	// The exclude clause: adopt subB itself (→ 9), then re-query. Excluding subB must
	// still brief against the turn-1 record (7); WITHOUT excluding it, subB's own newer
	// adoption (9) would win — proving the exclude is what protects a re-run.
	recNine := mustAdoptedRecord(t, s, f.AnswerID, f.RubricVersionID, "9", `[{"criterion_id":1,"score":"9"}]`)
	if _, err := s.SetProblemVerdictAndAdoption(ctx, store.SetProblemVerdictAndAdoptionParams{
		SubItemID: subB, Verdict: "regraded", Note: "turn2", AdoptedRecordID: recNine,
	}); err != nil {
		t.Fatalf("adopt subB: %v", err)
	}
	excl, err := s.ContestedAnswerForSubItem(ctx, f.AssessmentID, f.StudentID, f.ProblemID, subB)
	if err != nil {
		t.Fatalf("ContestedAnswerForSubItem (exclude subB): %v", err)
	}
	if len(excl) != 1 || excl[0].RecordID != recSeven {
		t.Fatalf("excluding the current sub-item should brief against the turn-1 record %d, got %+v", recSeven, excl)
	}
	incl, err := s.ContestedAnswerForSubItem(ctx, f.AssessmentID, f.StudentID, f.ProblemID, 0)
	if err != nil {
		t.Fatalf("ContestedAnswerForSubItem (exclude none): %v", err)
	}
	if len(incl) != 1 || incl[0].RecordID != recNine {
		t.Fatalf("without excluding, the latest adoption %d should win, got %+v", recNine, incl)
	}
}
