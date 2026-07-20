package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

func mustPendingPublishItem(t *testing.T) (*store.Store, store.PublishBatch, store.PublishItem) {
	t.Helper()
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "8")

	batch, items, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID:      f.StudentID,
			Snapshot:       []byte(`{"total":"8"}`),
			RecipientEmail: "delivery@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("CreatePublishBatch items = %d, want 1", len(items))
	}
	return s, batch, items[0]
}

func TestCreatePublishBatch_EnqueuePendingIsTransactional(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "8")
	wantErr := errors.New("synthetic queue failure")
	callbackCalled := false

	_, _, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID:      f.StudentID,
			Snapshot:       []byte(`{"total":"8"}`),
			RecipientEmail: "delivery@example.test",
		}},
		EnqueuePending: func(_ context.Context, _ pgx.Tx, pending []store.PublishItem) error {
			callbackCalled = true
			if len(pending) != 1 || pending[0].EmailStatus != "pending" || pending[0].EmailGeneration != 1 {
				t.Fatalf("pending callback items = %+v", pending)
			}
			return wantErr
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("CreatePublishBatch error = %v, want wrapped queue error", err)
	}
	if !callbackCalled {
		t.Fatal("EnqueuePending callback was not called")
	}

	var batches, items int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM publish_batches WHERE assessment_id = $1`, f.AssessmentID).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM publish_items`).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if batches != 0 || items != 0 {
		t.Fatalf("failed transactional enqueue left batches=%d items=%d, want 0/0", batches, items)
	}
	var published bool
	if err := s.Pool.QueryRow(ctx, `SELECT published_at IS NOT NULL FROM answers WHERE id = $1`, f.AnswerID).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published {
		t.Fatal("failed transactional enqueue left answer published")
	}
}

func TestPublishDeliveryCAS_StateMachineAndGeneration(t *testing.T) {
	ctx := context.Background()
	s, _, item := mustPendingPublishItem(t)
	if item.EmailStatus != "pending" || item.EmailGeneration != 1 {
		t.Fatalf("initial delivery state = status %q generation %d, want pending/1", item.EmailStatus, item.EmailGeneration)
	}
	if !item.DeliveryKey.Valid || item.DeliveryJobID.Valid || !item.DeliveryStateAt.Valid {
		t.Fatalf("initial delivery metadata = key %+v job %+v at %+v", item.DeliveryKey, item.DeliveryJobID, item.DeliveryStateAt)
	}
	originalKey := item.DeliveryKey.Bytes

	// A stale job cannot claim a newer generation.
	if _, claimed, err := s.ClaimPublishItemDelivery(ctx, store.PublishDeliveryAttempt{
		ItemID: item.ID, Generation: 2, JobID: 101,
	}); err != nil || claimed {
		t.Fatalf("ClaimPublishItemDelivery(stale generation) = claimed %v err %v, want false/nil", claimed, err)
	}

	claimedItem, claimed, err := s.ClaimPublishItemDelivery(ctx, store.PublishDeliveryAttempt{
		ItemID: item.ID, Generation: 1, JobID: 101,
	})
	if err != nil || !claimed {
		t.Fatalf("ClaimPublishItemDelivery = claimed %v err %v", claimed, err)
	}
	if claimedItem.EmailStatus != "claimed" || !claimedItem.DeliveryJobID.Valid || claimedItem.DeliveryJobID.Int64 != 101 {
		t.Fatalf("claimed item = %+v", claimedItem)
	}
	// A duplicate job for the same generation loses the CAS.
	if _, claimed, err := s.ClaimPublishItemDelivery(ctx, store.PublishDeliveryAttempt{
		ItemID: item.ID, Generation: 1, JobID: 102,
	}); err != nil || claimed {
		t.Fatalf("ClaimPublishItemDelivery(duplicate job) = claimed %v err %v, want false/nil", claimed, err)
	}

	attempt := store.PublishDeliveryAttempt{ItemID: item.ID, Generation: 1, JobID: 101}
	sending, begun, err := s.BeginPublishItemSending(ctx, attempt)
	if err != nil || !begun {
		t.Fatalf("BeginPublishItemSending = begun %v err %v", begun, err)
	}
	if sending.EmailStatus != "sending" {
		t.Fatalf("BeginPublishItemSending status = %q, want sending", sending.EmailStatus)
	}
	if released, err := s.ReleasePublishItemDeliveryClaim(ctx, attempt); err != nil || released {
		t.Fatalf("ReleasePublishItemDeliveryClaim(from sending) = released %v err %v, want false/nil", released, err)
	}
	if sent, err := s.MarkPublishItemDeliverySent(ctx,
		store.PublishDeliveryAttempt{ItemID: item.ID, Generation: 1, JobID: 999},
		"provider-wrong", ""); err != nil || sent {
		t.Fatalf("MarkPublishItemDeliverySent(wrong job) = sent %v err %v, want false/nil", sent, err)
	}
	if sent, err := s.MarkPublishItemDeliverySent(ctx, attempt, "provider-1", "warning: large attachment"); err != nil || !sent {
		t.Fatalf("MarkPublishItemDeliverySent = sent %v err %v", sent, err)
	}

	armed, ok, err := s.ArmPublishItemResend(ctx, item.ID, 1, false)
	if err != nil || !ok {
		t.Fatalf("ArmPublishItemResend(sent) = armed %v err %v", ok, err)
	}
	if armed.EmailStatus != "pending" || armed.EmailGeneration != 2 {
		t.Fatalf("resend state = status %q generation %d, want pending/2", armed.EmailStatus, armed.EmailGeneration)
	}
	if !armed.DeliveryKey.Valid || armed.DeliveryKey.Bytes == originalKey {
		t.Fatalf("resend did not rotate delivery key: before %x after %+v", originalKey, armed.DeliveryKey)
	}
	if armed.DeliveryJobID.Valid || armed.ProviderMessageID.Valid || armed.SentAt.Valid || armed.Error.Valid {
		t.Fatalf("resend did not clear prior attempt metadata: %+v", armed)
	}
	if _, claimed, err := s.ClaimPublishItemDelivery(ctx, attempt); err != nil || claimed {
		t.Fatalf("old generation reclaimed after resend = %v err %v", claimed, err)
	}

	attempt2 := store.PublishDeliveryAttempt{ItemID: item.ID, Generation: 2, JobID: 201}
	if _, claimed, err := s.ClaimPublishItemDelivery(ctx, attempt2); err != nil || !claimed {
		t.Fatalf("ClaimPublishItemDelivery(generation 2) = claimed %v err %v", claimed, err)
	}
	if released, err := s.ReleasePublishItemDeliveryClaim(ctx, attempt2); err != nil || !released {
		t.Fatalf("ReleasePublishItemDeliveryClaim = released %v err %v", released, err)
	}
	attempt2.JobID = 202
	if _, claimed, err := s.ClaimPublishItemDelivery(ctx, attempt2); err != nil || !claimed {
		t.Fatalf("ClaimPublishItemDelivery(after release) = claimed %v err %v", claimed, err)
	}
	if _, begun, err := s.BeginPublishItemSending(ctx, attempt2); err != nil || !begun {
		t.Fatalf("BeginPublishItemSending(generation 2) = begun %v err %v", begun, err)
	}
	if released, err := s.ReleasePublishItemSending(ctx, attempt2); err != nil || !released {
		t.Fatalf("ReleasePublishItemSending = released %v err %v", released, err)
	}
	attempt2.JobID = 203
	if _, claimed, err := s.ClaimPublishItemDelivery(ctx, attempt2); err != nil || !claimed {
		t.Fatalf("ClaimPublishItemDelivery(after definitely-not-accepted release) = claimed %v err %v", claimed, err)
	}
	if failed, err := s.MarkPublishItemDeliveryFailed(ctx, attempt2, "sending", "wrong expected state"); err != nil || failed {
		t.Fatalf("MarkPublishItemDeliveryFailed(wrong state) = failed %v err %v, want false/nil", failed, err)
	}
	if failed, err := s.MarkPublishItemDeliveryFailed(ctx, attempt2, "claimed", "render failed"); err != nil || !failed {
		t.Fatalf("MarkPublishItemDeliveryFailed = failed %v err %v", failed, err)
	}
}

func TestPublishDeliveryCAS_UncertainRequiresExplicitAcknowledgement(t *testing.T) {
	ctx := context.Background()
	s, _, item := mustPendingPublishItem(t)
	attempt := store.PublishDeliveryAttempt{ItemID: item.ID, Generation: 1, JobID: 301}
	if _, claimed, err := s.ClaimPublishItemDelivery(ctx, attempt); err != nil || !claimed {
		t.Fatalf("ClaimPublishItemDelivery = claimed %v err %v", claimed, err)
	}
	if _, begun, err := s.BeginPublishItemSending(ctx, attempt); err != nil || !begun {
		t.Fatalf("BeginPublishItemSending = begun %v err %v", begun, err)
	}
	if uncertain, err := s.MarkPublishItemDeliveryUncertain(ctx, attempt, "provider-maybe", "delivery outcome unknown"); err != nil || !uncertain {
		t.Fatalf("MarkPublishItemDeliveryUncertain = uncertain %v err %v", uncertain, err)
	}

	if _, armed, err := s.ArmPublishItemResend(ctx, item.ID, 1, false); err != nil || armed {
		t.Fatalf("ArmPublishItemResend(uncertain, no ack) = armed %v err %v, want false/nil", armed, err)
	}
	resend, armed, err := s.ArmPublishItemResend(ctx, item.ID, 1, true)
	if err != nil || !armed {
		t.Fatalf("ArmPublishItemResend(uncertain, ack) = armed %v err %v", armed, err)
	}
	if resend.EmailStatus != "pending" || resend.EmailGeneration != 2 {
		t.Fatalf("acknowledged uncertain resend = status %q generation %d, want pending/2", resend.EmailStatus, resend.EmailGeneration)
	}
}

func TestBeginPublishItemSending_RejectsSupersededBatch(t *testing.T) {
	ctx := context.Background()
	s, batch, item := mustPendingPublishItem(t)
	attempt := store.PublishDeliveryAttempt{ItemID: item.ID, Generation: 1, JobID: 401}
	if _, claimed, err := s.ClaimPublishItemDelivery(ctx, attempt); err != nil || !claimed {
		t.Fatalf("ClaimPublishItemDelivery = claimed %v err %v", claimed, err)
	}
	if err := s.SupersedePublishBatch(ctx, batch.ID, 0); err != nil {
		t.Fatalf("SupersedePublishBatch: %v", err)
	}
	skipped, err := s.Q.GetPublishItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.EmailStatus != "skipped" || skipped.DeliveryJobID.Valid || skipped.Error.String != "batch unpublished before delivery" {
		t.Fatalf("unpublished claimed item = %+v, want skipped with cleared job", skipped)
	}
	if _, begun, err := s.BeginPublishItemSending(ctx, attempt); !errors.Is(err, store.ErrPublishBatchSuperseded) || begun {
		t.Fatalf("BeginPublishItemSending(superseded) = begun %v err %v, want false/ErrPublishBatchSuperseded", begun, err)
	}
	if _, armed, err := s.ArmPublishItemResend(ctx, item.ID, 1, false); !errors.Is(err, store.ErrPublishBatchSuperseded) || armed {
		t.Fatalf("ArmPublishItemResend(superseded) = armed %v err %v, want false/ErrPublishBatchSuperseded", armed, err)
	}
}

func TestBeginPublishItemSending_SkipsWithdrawnStudent(t *testing.T) {
	ctx := context.Background()
	s, _, item := mustPendingPublishItem(t)
	attempt := store.PublishDeliveryAttempt{ItemID: item.ID, Generation: 1, JobID: 451}
	if _, claimed, err := s.ClaimPublishItemDelivery(ctx, attempt); err != nil || !claimed {
		t.Fatalf("ClaimPublishItemDelivery = claimed %v err %v", claimed, err)
	}
	if _, err := s.Pool.Exec(ctx, `
		UPDATE students SET withdrawn_at = now(), updated_at = now() WHERE id = $1`,
		item.StudentID,
	); err != nil {
		t.Fatalf("withdraw student: %v", err)
	}

	skipped, begun, err := s.BeginPublishItemSending(ctx, attempt)
	if err != nil || begun {
		t.Fatalf("BeginPublishItemSending(withdrawn) = begun %v err %v, want false/nil", begun, err)
	}
	if skipped.EmailStatus != "skipped" || skipped.DeliveryJobID.Valid || skipped.Error.String != "student withdrawn before delivery" {
		t.Fatalf("withdrawn delivery = %+v, want skipped with cleared job", skipped)
	}
	durable, err := s.Q.GetPublishItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.EmailStatus != "skipped" || durable.DeliveryJobID.Valid {
		t.Fatalf("durable withdrawn delivery = %+v", durable)
	}
	if sent, err := s.MarkPublishItemDeliverySent(ctx, attempt, "must-not-send", ""); err != nil || sent {
		t.Fatalf("MarkPublishItemDeliverySent(after withdrawal skip) = sent %v err %v, want false/nil", sent, err)
	}
}

func TestHasSendingPublishItems(t *testing.T) {
	ctx := context.Background()
	s, batch, item := mustPendingPublishItem(t)
	attempt := store.PublishDeliveryAttempt{ItemID: item.ID, Generation: 1, JobID: 501}
	if sending, err := s.HasSendingPublishItems(ctx, batch.ID); err != nil || sending {
		t.Fatalf("HasSendingPublishItems(initial) = %v err %v, want false/nil", sending, err)
	}
	if _, claimed, err := s.ClaimPublishItemDelivery(ctx, attempt); err != nil || !claimed {
		t.Fatalf("ClaimPublishItemDelivery = claimed %v err %v", claimed, err)
	}
	if _, begun, err := s.BeginPublishItemSending(ctx, attempt); err != nil || !begun {
		t.Fatalf("BeginPublishItemSending = begun %v err %v", begun, err)
	}
	if sending, err := s.HasSendingPublishItems(ctx, batch.ID); err != nil || !sending {
		t.Fatalf("HasSendingPublishItems(sending) = %v err %v, want true/nil", sending, err)
	}
	if err := s.SupersedePublishBatch(ctx, batch.ID, 0); !errors.Is(err, store.ErrPublishDeliveryInProgress) {
		t.Fatalf("SupersedePublishBatch(sending) = %v, want ErrPublishDeliveryInProgress", err)
	}
	gotBatch, err := s.Q.GetPublishBatch(ctx, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotBatch.SupersededAt.Valid {
		t.Fatal("delivery-in-progress conflict superseded the batch")
	}
}

// TestCreatePublishBatch_CoverageRecheckedUnderLock is the A9 regression test:
// coverage was previously checked only from an unlocked pre-read (publish.Service's
// PublishPreview, well before CreatePublishBatch's transaction even begins). This
// simulates the exact race directly at the store layer: an answer that satisfied the
// coverage gate at "preview" time loses its official record (standing in for a
// concurrent regrade/recompute) before CreatePublishBatch's locked re-check runs.
// CreatePublishBatch must abort with ErrCoverageGateChanged and create no batch row.
//
// The re-check is gated on ExpectedFinalSourceKind being set — mirrors A9's sibling
// final-source-changed check and CreatePublishBatchParams' own doc ("empty kind is
// reserved for lower-level tests/callers that intentionally opt out of this
// service-level invariant"): it only activates for a real publish.Service call, which
// always supplies its expected source.
func TestCreatePublishBatch_CoverageRecheckedUnderLock(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	// The answer needs a real page: losing the official record on a no_submission
	// answer (no pages) leaves it merely no_submission, which never blocks — the race
	// must turn a genuinely-blocked-if-ungraded answer back into blocked.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO submissions (assessment_id, student_id, original_filename, source_ref, source_sha256, page_count)
		VALUES ($1, $2, 'f.pdf', 'ref', 'sha', 1)`, f.AssessmentID, f.StudentID); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	var subID int64
	if err := s.Pool.QueryRow(ctx, `SELECT id FROM submissions WHERE assessment_id = $1`, f.AssessmentID).Scan(&subID); err != nil {
		t.Fatalf("get submission id: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO answer_pages (answer_id, page_index, submission_id, pdf_page_index, image_ref, image_sha256, image_width, image_height)
		VALUES ($1, 0, $2, 0, 'img', 'imgsha', 100, 100)`, f.AnswerID, subID); err != nil {
		t.Fatalf("seed answer_pages: %v", err)
	}
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "8")

	if _, _, err := s.SetAssessmentFinalSource(ctx, f.AssessmentID, "consensus", 0); err != nil {
		t.Fatalf("SetAssessmentFinalSource: %v", err)
	}

	preview, err := s.PublishPreview(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("PublishPreview: %v", err)
	}
	if !preview.Publishable() {
		t.Fatalf("expected publishable before the race, got blockers %+v", preview.Blockers)
	}

	// The race: the answer loses its official record after "preview", before the batch
	// transaction's locked re-check.
	if _, err := s.Pool.Exec(ctx,
		`UPDATE answers SET official_record_id = NULL, official_set_at = NULL WHERE id = $1`, f.AnswerID); err != nil {
		t.Fatalf("simulate coverage race: %v", err)
	}

	_, _, err = s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{"total":"8"}`), RecipientEmail: "delivery@example.test",
		}},
		ExpectedFinalSourceKind: "consensus",
	})
	if !errors.Is(err, store.ErrCoverageGateChanged) {
		t.Fatalf("CreatePublishBatch after coverage race = %v, want ErrCoverageGateChanged", err)
	}

	var batches int
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM publish_batches WHERE assessment_id = $1`, f.AssessmentID).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if batches != 0 {
		t.Fatalf("coverage race left %d batch row(s), want 0", batches)
	}
}

// TestPublishPreview_NotIngestedLowersCoverageDenominator is the B3-backend
// regression test: a not-ingested roster student has ZERO answers rows, so a naive
// count(*) FROM answers denominator never sees them — letting a caller's derived
// coverage percentage ((graded+no_submission)/total_answers) read 100% while a
// not_ingested blocker exists (audit B3: "COVERAGE 100%" tile next to a red
// "NOT INGESTED 4" tile). TotalAnswers must grow by not_ingested x problem_count
// "missing cells" so any percentage derived from it actually reflects the gap.
// Publishable() itself is untouched — NotIngested>0 already blocks regardless of the
// denominator.
func TestPublishPreview_NotIngestedLowersCoverageDenominator(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "8")

	preview, err := s.PublishPreview(ctx, f.AssessmentID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.TotalAnswers != 1 {
		t.Fatalf("baseline TotalAnswers = %d, want 1 (one real, graded answer row)", preview.TotalAnswers)
	}

	// A second roster student never ingested for this assessment (roster-add-after-
	// ingest gap): zero answers rows, against the assessment's one problem.
	if _, err := s.Q.UpsertStudent(ctx, db.UpsertStudentParams{
		StudentID: t.Name() + "-gap", Name: "Gap Student", Email: "gap@example.test",
	}); err != nil {
		t.Fatalf("UpsertStudent (gap): %v", err)
	}

	preview, err = s.PublishPreview(ctx, f.AssessmentID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.NotIngested != 1 {
		t.Fatalf("NotIngested = %d, want 1", preview.NotIngested)
	}
	if preview.TotalAnswers != 2 {
		t.Fatalf("TotalAnswers after 1 not-ingested student x 1 problem = %d, want 2 (1 real answer + 1 missing cell)", preview.TotalAnswers)
	}
	if preview.Publishable() {
		t.Fatalf("expected not publishable (not_ingested=1), got publishable")
	}
}
