package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// seedPublishableAssessment builds a minimal but genuinely publishable assessment:
// one roster student, one problem (max 10, rubric criteria 6+4), a submission with one
// answer page, and an official human record (total 8). It satisfies the coverage gate
// (every roster student × problem answer is official, with a real submission) so
// Service.Publish creates a PENDING emailable item. Invented data only (CLAUDE.md).
func seedPublishableAssessment(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	num := func(s string) pgtype.Numeric {
		n, err := store.Num(s)
		if err != nil {
			t.Fatalf("num(%q): %v", s, err)
		}
		return n
	}

	a, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Enqueue Fixture"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.Q.CreateProblem(ctx, db.CreateProblemParams{
		AssessmentID: a.ID, Number: 1, Title: "P1", Statement: "", MaxPoints: num("10"), Position: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rv, err := st.Q.CreateRubricVersion(ctx, db.CreateRubricVersionParams{
		ProblemID: p.ID, Notes: "", ScoreIncrement: num("0.5"),
	})
	if err != nil {
		t.Fatal(err)
	}
	c1, err := st.Q.CreateRubricCriterion(ctx, db.CreateRubricCriterionParams{
		RubricVersionID: rv.ID, Position: 1, Description: "Correctness", Points: num("6"),
	})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := st.Q.CreateRubricCriterion(ctx, db.CreateRubricCriterionParams{
		RubricVersionID: rv.ID, Position: 2, Description: "Clarity", Points: num("4"),
	})
	if err != nil {
		t.Fatal(err)
	}

	grader, err := st.Q.CreateUser(ctx, db.CreateUserParams{
		Email: "grader@ntu.edu.tw", DisplayName: "Grader", Role: "lecturer", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	stu, err := st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: "e01", Name: "Enid Fake", Email: "enid@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := st.Q.CreateSubmission(ctx, db.CreateSubmissionParams{
		AssessmentID: a.ID, StudentID: stu.ID, OriginalFilename: "e01.pdf",
		SourceRef: "fake-src", SourceSha256: "fakesrcsha", SourceKind: "pdf", PageCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ans, err := st.Q.EnsureAnswer(ctx, db.EnsureAnswerParams{AssessmentID: a.ID, StudentID: stu.ID, ProblemID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Q.CreateAnswerPage(ctx, db.CreateAnswerPageParams{
		AnswerID: ans.ID, PageIndex: 0, SubmissionID: sub.ID, PdfPageIndex: 0,
		ImageRef: "fake-ref", ImageSha256: "fakesha", ImageWidth: 100, ImageHeight: 100,
	}); err != nil {
		t.Fatalf("CreateAnswerPage: %v", err)
	}

	scores, _ := json.Marshal([]map[string]any{
		{"criterion_id": c1.ID, "score": "5"},
		{"criterion_id": c2.ID, "score": "3"},
	})
	rec, err := st.Q.InsertHumanRecord(ctx, db.InsertHumanRecordParams{
		AnswerID: ans.ID, RubricVersionID: rv.ID, GradedImageShas: []string{"fakesha"},
		CriterionScores: scores, Total: num("8"), Comment: "graded", Adjustments: []byte("[]"),
		CreatedBy: pgtype.Int8{Int64: grader.ID, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Officials are derived since 0027; fixtures poke the pointer directly.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE answers SET official_record_id = $2, official_set_at = now() WHERE id = $1`,
		ans.ID, rec.ID); err != nil {
		t.Fatalf("force official: %v", err)
	}
	// Publishing also needs a chosen final source (0027). Poke the kind via SQL —
	// unlike the endpoint this triggers no recompute, so the forced official above
	// survives; consensus sources need no spot-check sample.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE assessments SET final_source_kind = 'consensus' WHERE id = $1`, a.ID); err != nil {
		t.Fatalf("choose final source: %v", err)
	}
	return a.ID
}

// TestChangedOnlyDiff_OldFormatSnapshotCompatibility is the finding-1 regression test
// (CRITICAL): before the fix, SnapProblem persisted answer_id. A snapshot stored
// BEFORE that field existed decodes with AnswerID zero-valued and re-marshals as
// "answer_id":0; a freshly rebuilt snapshot for the identical, unchanged grade instead
// carries the real (non-zero) answer_id — so every pre-existing student's snapshot
// would byte-diff as "changed" across that upgrade boundary and the whole cohort would
// be spuriously re-emailed (violates D30). The fix removes answer_id from the
// persisted shape entirely, so this must no longer happen.
//
// This test seeds the "old-format" stored snapshot RAW — hand-built JSON bytes with no
// answer_id key at all, written directly via store.CreatePublishItemInput.Snapshot —
// never through buildSnapshots/canonicalJSON (current code), so it genuinely exercises
// what a pre-migration row looks like on disk, not what the fixed code would produce
// today.
func TestChangedOnlyDiff_OldFormatSnapshotCompatibility(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()
	aid := seedPublishableAssessment(t, st)

	// Look up the seeded student + rubric criteria to build the fresh snapshot the same
	// way the service would today, so we can compare it against a hand-built old-format
	// snapshot for the SAME underlying (unchanged) grade.
	inputs, err := st.Q.PublishSnapshotInputs(ctx, aid)
	if err != nil || len(inputs) != 1 {
		t.Fatalf("PublishSnapshotInputs: %d rows, err=%v", len(inputs), err)
	}
	criteria, err := st.PublishCriteria(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	name, err := st.Q.GetAssessment(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	freshSnaps, _, _, _ := buildSnapshots(name.Name, inputs, criteria)
	studentID := inputs[0].StudentID
	freshBytes, err := canonicalJSON(freshSnaps[studentID])
	if err != nil {
		t.Fatal(err)
	}

	// Hand-build the OLD-FORMAT snapshot for the identical grade: same fields as the
	// canonical shape, PLUS a legacy "answer_id" key on the problem (pre-fix shape) that
	// today's Snapshot struct no longer has. This is deliberately raw JSON, not built via
	// buildSnapshots/canonicalJSON, so it stands in for a row written before this fix
	// shipped.
	oldFormatJSON := []byte(`{
		"assessment_name":"Enqueue Fixture",
		"student_external_id":"e01",
		"student_name":"Enid Fake",
		"total":"8",
		"max":"10",
		"all_no_submission":false,
		"problems":[{
			"number":1,"title":"P1","max":"10","no_submission":false,"total":"8","comment":"graded",
			"criteria":[{"name":"Clarity","score":"3","max":"4"},{"name":"Correctness","score":"5","max":"6"}],
			"answer_id":0
		}]
	}`)

	// Byte-equal compat assertion: canonicalizing the old-format (answer_id-less-in-
	// spirit, answer_id:0-on-disk) stored snapshot must decode+re-marshal to EXACTLY the
	// same bytes as the freshly built snapshot for the identical, unchanged grade —
	// since the current Snapshot/SnapProblem struct has no AnswerID field to round-trip,
	// decoding the old row simply drops the unknown "answer_id" key.
	var decoded Snapshot
	if err := json.Unmarshal(oldFormatJSON, &decoded); err != nil {
		t.Fatalf("decode old-format snapshot: %v", err)
	}
	oldCanonical, err := canonicalJSON(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(oldCanonical) != string(freshBytes) {
		t.Fatalf("old-format snapshot canonicalizes differently from a fresh rebuild of the identical grade:\n old(canon)=%s\nfresh(canon)=%s", oldCanonical, freshBytes)
	}

	// Seed this old-format snapshot as the batch item actually on disk (raw, bypassing
	// buildSnapshots/canonicalJSON) — this is the upgrade-boundary scenario: a batch
	// published before the fix.
	_, _, err = st.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: aid, Note: "pre-fix batch",
		Items: []store.CreatePublishItemInput{{
			StudentID: studentID, Snapshot: oldFormatJSON, RecipientEmail: "enid@example.edu", EmailStatus: "sent",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Unpublish so a second publish is possible (single-live-batch invariant) — the
	// changed-only diff baseline still spans the just-superseded batch (D30), which is
	// exactly the case under test.
	svc := NewService(st, nil, "none", 14*24*time.Hour, "", false, nil)
	if _, err := svc.Unpublish(ctx, aid, 0); err != nil {
		t.Fatal(err)
	}

	// The changed-only diff against that old-format baseline, for the SAME unchanged
	// grade, must select ZERO students — not the whole cohort.
	preview, err := svc.GetPreview(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changed) != 0 {
		t.Fatalf("changed-only diff across the old-format upgrade boundary selected %d student(s) (%+v), want 0 — nothing about the grade actually changed", len(preview.Changed), preview.Changed)
	}
}

// TestPublish_EnqueueFailure_RollsBackBatchAndPublishedState pins the transactional
// outbox invariant: a batch, its published_at freeze, and its River jobs are one
// commit. Queue insertion failure must leave no live batch or pending-without-job row.
func TestPublish_EnqueueFailure_RollsBackBatchAndPublishedState(t *testing.T) {
	st := storetest.Fresh(t)
	aid := seedPublishableAssessment(t, st)

	enqueueErr := errors.New("queue backend unavailable")
	failing := func(ctx context.Context, tx pgx.Tx, refs []DeliveryRef) error { return enqueueErr }
	svc := NewService(st, failing, "file", 14*24*time.Hour, "", false, nil)

	_, err := svc.Publish(context.Background(), aid, "first", false, 0, "none", false)
	if err == nil || !errors.Is(err, enqueueErr) {
		t.Fatalf("Publish should surface the enqueue error, got %v", err)
	}

	// Everything in the transaction rolls back. Retrying Publish is now sufficient;
	// there is no partially published batch to repair first.
	batches, err := st.ListPublishBatches(context.Background(), aid)
	if err != nil || len(batches) != 0 {
		t.Fatalf("enqueue failure committed %d batch(es), want 0 (err %v)", len(batches), err)
	}
	var frozen int
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM answers WHERE assessment_id = $1 AND published_at IS NOT NULL`, aid).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if frozen != 0 {
		t.Fatalf("enqueue failure left %d answer(s) published, want 0", frozen)
	}
}

// TestResendFailed_SkipsWithdrawnItems pins locked semantics (d) at the service seam
// (roster-lifecycle plan 2026-07-10, Task R2): a failed item whose student has since
// withdrawn is skipped — not reset to pending, no send job — and reported via the
// skipped-withdrawn count so the operator sees why the number re-enqueued is short.
func TestResendFailed_SkipsWithdrawnItems(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()
	aid := seedPublishableAssessment(t, st)

	var enqueued []DeliveryRef
	svc := NewService(st, func(ctx context.Context, tx pgx.Tx, refs []DeliveryRef) error {
		enqueued = append(enqueued, refs...)
		return nil
	}, "file", 14*24*time.Hour, "", false, nil)

	if _, err := svc.Publish(ctx, aid, "first", false, 0, "none", false); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	batches, err := st.ListPublishBatches(ctx, aid)
	if err != nil || len(batches) != 1 {
		t.Fatalf("expected 1 batch, got %d (err %v)", len(batches), err)
	}
	items, err := st.ListPublishItems(ctx, batches[0].ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("expected 1 item, got %d (err %v)", len(items), err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE publish_items SET email_status = 'failed', error = 'boom' WHERE id = $1`, items[0].ID); err != nil {
		t.Fatal(err)
	}
	// The student withdraws (停修) after the failed send.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE students SET withdrawn_at = now() WHERE id = $1`, items[0].StudentID); err != nil {
		t.Fatal(err)
	}

	enqueued = nil
	n, skipped, err := svc.ResendFailed(ctx, batches[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(enqueued) != 0 {
		t.Fatalf("withdrawn student's item must not re-enqueue: n=%d enqueued=%v", n, enqueued)
	}
	if skipped != 1 {
		t.Fatalf("skipped_withdrawn = %d, want 1", skipped)
	}
	// The item stays failed — never flipped to a pending that would send later.
	after, err := st.ListPublishItems(ctx, batches[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].EmailStatus != "failed" {
		t.Fatalf("skipped item status = %q, want failed (untouched)", after[0].EmailStatus)
	}

	// Reinstate ⇒ the same call now recovers it.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE students SET withdrawn_at = NULL WHERE id = $1`, items[0].StudentID); err != nil {
		t.Fatal(err)
	}
	n, skipped, err = svc.ResendFailed(ctx, batches[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || skipped != 0 || len(enqueued) != 1 {
		t.Fatalf("after reinstating: n=%d skipped=%d enqueued=%v, want 1/0/[item]", n, skipped, enqueued)
	}
}

func TestResendItem_AtomicGenerationAndUncertainAcknowledgement(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()
	aid := seedPublishableAssessment(t, st)

	var refs []DeliveryRef
	svc := NewService(st, func(ctx context.Context, tx pgx.Tx, got []DeliveryRef) error {
		refs = append(refs, got...)
		return nil
	}, "file", 14*24*time.Hour, "", false, nil)
	if _, err := svc.Publish(ctx, aid, "first", false, 0, "none", false); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	batches, _ := st.ListPublishBatches(ctx, aid)
	items, _ := st.ListPublishItems(ctx, batches[0].ID)
	itemID := items[0].ID
	if len(refs) != 1 || refs[0].Generation != 1 {
		t.Fatalf("initial refs = %+v, want one generation-1 delivery", refs)
	}

	if _, err := st.Pool.Exec(ctx,
		`UPDATE publish_items SET email_status = 'sent', sent_at = now() WHERE id = $1`, itemID); err != nil {
		t.Fatal(err)
	}
	enqueueErr := errors.New("queue write failed")
	svc.enqueueTx = func(context.Context, pgx.Tx, []DeliveryRef) error { return enqueueErr }
	if err := svc.ResendItem(ctx, itemID, false, 0); !errors.Is(err, enqueueErr) {
		t.Fatalf("ResendItem enqueue failure = %v, want %v", err, enqueueErr)
	}
	afterRollback, _ := st.Q.GetPublishItem(ctx, itemID)
	if afterRollback.EmailStatus != "sent" || afterRollback.EmailGeneration != 1 {
		t.Fatalf("failed resend committed state %+v, want sent generation 1", afterRollback)
	}

	if _, err := st.Pool.Exec(ctx,
		`UPDATE publish_items SET email_status = 'uncertain', error = 'provider outcome unknown' WHERE id = $1`, itemID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResendItem(ctx, itemID, false, 0); !errors.Is(err, ErrUncertainNeedsAcknowledgement) {
		t.Fatalf("unacknowledged uncertain resend = %v, want ErrUncertainNeedsAcknowledgement", err)
	}
	refs = nil
	svc.enqueueTx = func(ctx context.Context, tx pgx.Tx, got []DeliveryRef) error {
		refs = append(refs, got...)
		return nil
	}
	// The high-risk acknowledgement is not a best-effort HTTP audit: if its durable
	// row cannot commit, generation rotation and queue work must roll back too.
	if err := svc.ResendItem(ctx, itemID, true, 999_999); err == nil {
		t.Fatal("acknowledged resend with invalid audit actor should fail")
	}
	rolledBack, _ := st.Q.GetPublishItem(ctx, itemID)
	if rolledBack.EmailStatus != "uncertain" || rolledBack.EmailGeneration != 1 || len(refs) != 0 {
		t.Fatalf("failed acknowledgement committed state=%q generation=%d refs=%+v", rolledBack.EmailStatus, rolledBack.EmailGeneration, refs)
	}
	if err := svc.ResendItem(ctx, itemID, true, 0); err != nil {
		t.Fatalf("acknowledged uncertain resend: %v", err)
	}
	armed, _ := st.Q.GetPublishItem(ctx, itemID)
	if armed.EmailStatus != "pending" || armed.EmailGeneration != 2 {
		t.Fatalf("acknowledged resend state = %q generation %d, want pending/2", armed.EmailStatus, armed.EmailGeneration)
	}
	if len(refs) != 1 || refs[0] != (DeliveryRef{ItemID: itemID, Generation: 2}) {
		t.Fatalf("acknowledged resend refs = %+v, want item %d generation 2", refs, itemID)
	}
	var auditDetail []byte
	if err := st.Pool.QueryRow(ctx, `
		SELECT detail FROM audit_log
		WHERE action = 'publish.resend_uncertain_ack' AND target_kind = 'publish_item' AND target_id = $1
		ORDER BY id DESC LIMIT 1`, fmt.Sprint(itemID)).Scan(&auditDetail); err != nil {
		t.Fatalf("durable duplicate-risk acknowledgement: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(auditDetail, &detail); err != nil {
		t.Fatalf("decode acknowledgement audit: %v", err)
	}
	if detail["from_generation"] != float64(1) || detail["to_generation"] != float64(2) || detail["acknowledged_duplicate_risk"] != true {
		t.Fatalf("acknowledgement audit detail = %#v", detail)
	}
}

func TestResendItem_NoneProviderDoesNotConsumeAsSent(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()
	aid := seedPublishableAssessment(t, st)
	svc := NewService(st, func(context.Context, pgx.Tx, []DeliveryRef) error {
		t.Fatal("none provider must never enqueue")
		return nil
	}, "none", 14*24*time.Hour, "", false, nil)
	res, err := svc.Publish(ctx, aid, "disabled", false, 0, "none", false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	items, err := st.ListPublishItems(ctx, res.BatchID)
	if err != nil || len(items) != 1 {
		t.Fatalf("ListPublishItems: len=%d err=%v", len(items), err)
	}
	if err := svc.ResendItem(ctx, items[0].ID, false, 0); !errors.Is(err, ErrEmailDisabled) {
		t.Fatalf("none-provider resend = %v, want ErrEmailDisabled", err)
	}
	after, _ := st.Q.GetPublishItem(ctx, items[0].ID)
	if after.EmailStatus != "skipped" || after.EmailGeneration != 1 {
		t.Fatalf("none-provider resend changed item to %q generation %d", after.EmailStatus, after.EmailGeneration)
	}
}

// TestPublish_ZeroEligibleStudents_ErrNothingToPublish is the A5 regression test: a
// FIRST publish (selectChanged is false, not a changed-only re-publish) on an
// assessment with zero active roster students has nothing to send — the coverage gate
// passes vacuously (there are no answers to be blocked or not-ingested), so before this
// fix the len(items)==0 guard only fired for a changed-only re-publish and this call
// fell through to CreatePublishBatch, inserting a live 0-item batch. Every subsequent
// (real) publish then 409s with ErrAlreadyPublished until an admin finds Unpublish.
// Publish must refuse instead, and no batch row may be created.
func TestPublish_ZeroEligibleStudents_ErrNothingToPublish(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()

	a, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Empty Roster"})
	if err != nil {
		t.Fatal(err)
	}
	// A chosen final source (0027) is required to get past ErrNoFinalSource and reach
	// the empty-items guard under test; consensus needs no spot-check sample.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE assessments SET final_source_kind = 'consensus' WHERE id = $1`, a.ID); err != nil {
		t.Fatalf("choose final source: %v", err)
	}

	svc := NewService(st, nil, "none", 14*24*time.Hour, "", false, nil)
	if _, err := svc.Publish(ctx, a.ID, "", false, 0, "", false); !errors.Is(err, ErrNothingToPublish) {
		t.Fatalf("Publish(zero eligible students) = %v, want ErrNothingToPublish", err)
	}

	var batches int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM publish_batches WHERE assessment_id = $1`, a.ID).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if batches != 0 {
		t.Fatalf("refused publish left %d batch row(s), want 0 (the exact A5 phantom-batch wedge)", batches)
	}
}
