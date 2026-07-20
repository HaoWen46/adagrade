package publish

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	mrand "math/rand"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/image/font/gofont/goregular"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/domain"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// hasWarningPrefix checks the sender's non-terminal-warning error-text convention
// (spec §3 15MB guard) via the package's own exported WarningPrefix constant.
func hasWarningPrefix(s string) bool {
	return len(s) >= len(WarningPrefix) && s[:len(WarningPrefix)] == WarningPrefix
}

// fakeProvider is a controllable EmailProvider for send-path tests (invented data
// only). blockUntil, if non-nil, makes Send wait on the context and return its error —
// used to simulate a shutdown-drain cancellation mid-send. lastMsg captures the most
// recently sent OutboundEmail (attachment tests inspect its Attachments).
type fakeProvider struct {
	blockUntilCancel bool
	// blockUntilCancelOutcome, when set alongside blockUntilCancel, wraps the
	// observed ctx.Err() with this classification instead of returning it raw (A1):
	// it simulates a stage-proven provider classification that arrives (or merely
	// completes) after the job's own context has already expired, so sender_test.go
	// can drive both halves of the A1 fix — a definitely-not-accepted classification
	// must be trusted even under an expired ctx, while an outcome-unknown one must
	// still be quarantined.
	blockUntilCancelOutcome domain.EmailDeliveryOutcome
	failWith                error
	sent                    int
	lastMsg                 domain.OutboundEmail
	messages                []domain.OutboundEmail
}

func (p *fakeProvider) Send(ctx context.Context, msg domain.OutboundEmail) (string, error) {
	p.sent++
	p.lastMsg = msg
	p.messages = append(p.messages, msg)
	if p.blockUntilCancel {
		<-ctx.Done()
		if p.blockUntilCancelOutcome != "" {
			return "", domain.NewEmailDeliveryError(p.blockUntilCancelOutcome, ctx.Err())
		}
		return "", ctx.Err()
	}
	if p.failWith != nil {
		return "", p.failWith
	}
	return "fake-msg-1", nil
}

func (p *fakeProvider) ParseInbound(raw []byte) (domain.InboundEmail, error) {
	return domain.InboundEmail{}, errors.New("not implemented")
}

// seedPublishItem creates the minimal assessment/student/batch/item chain and returns
// the item id, using the same store API the publish service uses.
func seedPublishItem(t *testing.T, st *store.Store, status string) int64 {
	t.Helper()
	return seedPublishItemOpts(t, st, seedOpts{status: status})
}

// seedOpts extends seedPublishItem for the attachment tests: attachment/zip are the
// batch-level settings (spec §3). This helper never materializes a real problems/
// answers row — it's only used by tests that either skip the attachment build
// entirely (attachment="none") or fail before any DB lookup of the answer id (the
// font-unconfigured/blobs-nil terminal-error tests): the answer id is now resolved
// LIVE from (assessment, student, problem number) rather than read off the snapshot.
type seedOpts struct {
	status     string
	attachment string
	zip        bool
}

func seedPublishItemOpts(t *testing.T, st *store.Store, o seedOpts) int64 {
	t.Helper()
	ctx := context.Background()
	a, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Fake Exam"})
	if err != nil {
		t.Fatal(err)
	}
	stu, err := st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: "z99", Name: "Zed Fake", Email: "zed@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	snap := Snapshot{
		AssessmentName: "Fake Exam", StudentExternalID: "z99", StudentName: "Zed Fake",
		Total: "8", Max: "10",
		Problems: []SnapProblem{{Number: 1, Title: "P1", Max: "10", Total: "8", Criteria: []SnapCriterion{{Name: "C", Score: "8", Max: "10"}}}},
	}
	b, _ := canonicalJSON(snap)
	_, items, err := st.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: a.ID, Note: "t", CreatedBy: 0, Attachment: o.attachment, Zip: o.zip,
		Items: []store.CreatePublishItemInput{{StudentID: stu.ID, Snapshot: b, RecipientEmail: "zed@example.edu", EmailStatus: o.status}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return items[0].ID
}

// seedAnswerWithPage creates a real answer row (assessment/problem/student already
// created by the caller) with one answer_page whose image_ref points at a blob key,
// and writes pageJPEG into blobs at that key — the send job's report-attachment path
// (spec §3) resolves ORIGINAL page images through exactly this ref.
func seedAnswerWithPage(t *testing.T, st *store.Store, blobs blobstore.Store, assessmentID, studentID int64, pageJPEG []byte) int64 {
	t.Helper()
	ctx := context.Background()
	p, err := st.Q.CreateProblem(ctx, db.CreateProblemParams{AssessmentID: assessmentID, Number: 1, Title: "P1", MaxPoints: numText("10")})
	if err != nil {
		t.Fatal(err)
	}
	ans, err := st.Q.EnsureAnswer(ctx, db.EnsureAnswerParams{AssessmentID: assessmentID, StudentID: studentID, ProblemID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := st.Q.CreateSubmission(ctx, db.CreateSubmissionParams{
		AssessmentID: assessmentID, StudentID: studentID, OriginalFilename: "fake.pdf",
		SourceRef: "test/source.pdf", SourceSha256: "fakesha", SourceKind: "pdf", PageCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("test/answers/%d/page-0.jpg", ans.ID)
	if _, _, err := blobs.Put(ctx, key, bytes.NewReader(pageJPEG)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Q.CreateAnswerPage(ctx, db.CreateAnswerPageParams{
		AnswerID: ans.ID, PageIndex: 0, SubmissionID: sub.ID, PdfPageIndex: 0,
		ImageRef: key, ImageSha256: "fake", ImageWidth: 10, ImageHeight: 10,
	}); err != nil {
		t.Fatal(err)
	}
	return ans.ID
}

func newTestSender(st *store.Store, prov domain.EmailProvider) *Sender {
	return NewSender(st, prov, []byte("0123456789abcdef0123456789abcdef"), 14*24*time.Hour, "inbound.example.edu", nil, nil, "")
}

func newTestSenderWithReport(st *store.Store, prov domain.EmailProvider, blobs blobstore.Store, fontPath string) *Sender {
	return NewSender(st, prov, []byte("0123456789abcdef0123456789abcdef"), 14*24*time.Hour, "inbound.example.edu", nil, blobs, fontPath)
}

// deliveryRef reads the current durable generation for an item. senderJobID is
// stable for that item/generation, mirroring River retries of one job row.
func deliveryRef(t *testing.T, st *store.Store, itemID int64) DeliveryRef {
	t.Helper()
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	return DeliveryRef{ItemID: itemID, Generation: it.EmailGeneration}
}

func senderJobID(ref DeliveryRef) int64 {
	return ref.ItemID*1000 + int64(ref.Generation)
}

// sendTestItem drives a single simulated send for itemID. isFirstAttempt is always
// true here (A2): every sendTestItem call represents a fresh generation's first (and,
// in most tests, only) delivery attempt. Tests that specifically simulate a
// second/redelivered attempt of the SAME job call s.SendItem directly instead.
func sendTestItem(t *testing.T, s *Sender, st *store.Store, ctx context.Context, itemID int64, final bool) error {
	t.Helper()
	ref := deliveryRef(t, st, itemID)
	return s.SendItem(ctx, ref, senderJobID(ref), final, true)
}

// TestGradeDataFromSnapshot_ProblemCommentGoesOnProblemBreakdown is B4-glue: Task 1
// added email.ProblemBreakdown.Comment so both grade-email templates render a
// whole-problem comment as its own "Note: ..." line, separate from any per-criterion
// comment. The sender previously worked around the missing field by folding the
// problem comment into the LAST criterion's Comment — that workaround must be gone,
// and the snapshot's problem-level comment must flow straight into
// ProblemBreakdown.Comment with no criterion line carrying it. gradeDataFromSnapshot
// touches no store state, so this runs as a pure unit test (nil store, no DB needed).
func TestGradeDataFromSnapshot_ProblemCommentGoesOnProblemBreakdown(t *testing.T) {
	s := newTestSender(nil, &fakeProvider{})
	snap := Snapshot{
		AssessmentName: "Fake Exam", StudentExternalID: "z99", StudentName: "Zed Fake",
		Total: "8", Max: "10",
		Problems: []SnapProblem{{
			Number: 1, Title: "P1", Max: "10", Total: "8", Comment: "Nice work overall",
			Criteria: []SnapCriterion{{Name: "C", Score: "8", Max: "10"}},
		}},
	}
	data := s.gradeDataFromSnapshot(snap, "Zed Fake", false, "", time.Now())
	if len(data.Problems) != 1 {
		t.Fatalf("problems = %d, want 1", len(data.Problems))
	}
	p := data.Problems[0]
	if p.Comment != "Nice work overall" {
		t.Errorf("problem breakdown comment = %q, want the snapshot's problem comment", p.Comment)
	}
	for _, c := range p.Criteria {
		if c.Comment != "" {
			t.Errorf("criterion %q carries comment %q, want no criterion to carry the whole-problem comment", c.Name, c.Comment)
		}
	}
}

// TestSendItem_Success delivers via the fake provider and marks the item sent.
func TestSendItem_Success(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	prov := &fakeProvider{}
	s := newTestSender(st, prov)

	if err := sendTestItem(t, s, st, context.Background(), itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	it, _ := st.Q.GetPublishItem(context.Background(), itemID)
	if it.EmailStatus != "sent" || !it.ProviderMessageID.Valid {
		t.Errorf("after send: status=%q providerID.valid=%v, want sent + a provider id", it.EmailStatus, it.ProviderMessageID.Valid)
	}
	if it.RegradeToken == "" {
		t.Error("regrade token not persisted on send")
	}
}

// Once sending has begun, a cancellation cannot prove the provider rejected the
// message. Quarantine the attempt instead of letting River retry and duplicate it.
func TestSendItem_DrainCancelAfterProviderBoundary_MarksUncertain(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	prov := &fakeProvider{blockUntilCancel: true}
	s := newTestSender(st, prov)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := sendTestItem(t, s, st, ctx, itemID, true)
	if err != nil {
		t.Fatalf("SendItem on ambiguous drain = %v, want nil to prevent automatic retry", err)
	}
	it, _ := st.Q.GetPublishItem(context.Background(), itemID)
	if it.EmailStatus != "uncertain" {
		t.Errorf("drained send left item %q, want uncertain", it.EmailStatus)
	}
}

// TestSendItem_CtxExpiredDefinitelyNotAcceptedStage_ReleasedForRetry is A1: once a
// provider PROVES (by protocol stage — dial/handshake/envelope, before the message
// could possibly have reached the remote side) that it did not accept the message,
// that proof must be trusted even though the job's own context has already expired.
// Before the fix, sender.go's `ctx.Err() == nil` gate quarantined this exact case as
// "uncertain", forcing a manual per-item acknowledgment for what is actually a
// perfectly safe, provably-not-duplicated retry — one slow relay during a 200-student
// publish becomes mass manual work. Non-final: released back to pending for River's
// ordinary retry.
func TestSendItem_CtxExpiredDefinitelyNotAcceptedStage_ReleasedForRetry(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	prov := &fakeProvider{blockUntilCancel: true, blockUntilCancelOutcome: domain.EmailDeliveryDefinitelyNotAccepted}
	s := newTestSender(st, prov)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := sendTestItem(t, s, st, ctx, itemID, false)
	if err == nil {
		t.Fatal("stage-proven rejection after ctx expiry should return an error for River to retry, not be swallowed as uncertain")
	}
	it, _ := st.Q.GetPublishItem(context.Background(), itemID)
	if it.EmailStatus != "pending" {
		t.Errorf("ctx-expired dial-stage failure (non-final) status = %q, want pending (retryable, NOT uncertain)", it.EmailStatus)
	}
}

// TestSendItem_CtxExpiredDefinitelyNotAcceptedStage_FinalMarksFailed is the
// final-attempt half of the same A1 fix: a stage-proven rejection observed after ctx
// expiry on the last attempt is a plain terminal failure, not a manual-acknowledgment
// quarantine. (A10 is layered in here too: once that terminal write durably lands,
// SendItem returns nil rather than an error — see the A10 tests for that contract on
// its own.)
func TestSendItem_CtxExpiredDefinitelyNotAcceptedStage_FinalMarksFailed(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	prov := &fakeProvider{blockUntilCancel: true, blockUntilCancelOutcome: domain.EmailDeliveryDefinitelyNotAccepted}
	s := newTestSender(st, prov)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := sendTestItem(t, s, st, ctx, itemID, true)
	if err != nil {
		t.Fatalf("final stage-proven rejection with a durable failed write = %v, want nil (A10)", err)
	}
	it, _ := st.Q.GetPublishItem(context.Background(), itemID)
	if it.EmailStatus != "failed" {
		t.Errorf("ctx-expired final dial-stage failure status = %q, want failed (NOT uncertain)", it.EmailStatus)
	}
}

// TestSendItem_CtxExpiredAmbiguousStage_StillMarksUncertain is A1's safety boundary:
// the fix must NOT over-trust every classification once ctx has expired — only a
// stage-PROVEN definitely-not-accepted classification skips the quarantine. A
// provider that (correctly, per its own protocol-stage classification) reports
// outcome-unknown after ctx expiry — e.g. it lost the response to a write that may
// already have reached the remote side — must still be quarantined pending manual
// review, exactly as before this fix.
func TestSendItem_CtxExpiredAmbiguousStage_StillMarksUncertain(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	prov := &fakeProvider{blockUntilCancel: true, blockUntilCancelOutcome: domain.EmailDeliveryOutcomeUnknown}
	s := newTestSender(st, prov)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := sendTestItem(t, s, st, ctx, itemID, false)
	if err != nil {
		t.Fatalf("ambiguous ctx-expired outcome = %v, want nil to stop River retry", err)
	}
	it, _ := st.Q.GetPublishItem(context.Background(), itemID)
	if it.EmailStatus != "uncertain" {
		t.Errorf("ctx-expired ambiguous-stage failure status = %q, want uncertain", it.EmailStatus)
	}
}

// TestSendItem_FinalFailure_MarksFailed: a real (non-cancel) provider error on the
// final attempt marks the item failed with the error text (spec §3 terminal failure).
func TestSendItem_FinalFailure_MarksFailed(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	prov := &fakeProvider{failWith: domain.NewEmailDeliveryError(domain.EmailDeliveryDefinitelyNotAccepted, errors.New("smtp 550"))}
	s := newTestSender(st, prov)

	// Non-final: returns the error for River to retry, item stays pending.
	if err := sendTestItem(t, s, st, context.Background(), itemID, false); err == nil {
		t.Fatal("non-final failure should return an error for retry")
	}
	if it, _ := st.Q.GetPublishItem(context.Background(), itemID); it.EmailStatus != "pending" {
		t.Errorf("non-final failure: status=%q, want pending (retryable)", it.EmailStatus)
	}
	// Final: marks failed. A10: once the terminal write durably lands, SendItem
	// returns nil (not the cause) so river.go's `final && err != nil` branch does
	// not burn a wasted emailFinalStateRetrySnooze cycle on an item that already
	// reached "failed" — there is nothing left to retry.
	if err := sendTestItem(t, s, st, context.Background(), itemID, true); err != nil {
		t.Fatalf("final failure with a durable failed write = %v, want nil (A10)", err)
	}
	it, _ := st.Q.GetPublishItem(context.Background(), itemID)
	if it.EmailStatus != "failed" || !it.Error.Valid {
		t.Errorf("final failure: status=%q error.valid=%v, want failed + error text", it.EmailStatus, it.Error.Valid)
	}
}

// TestSendItem_SkipsNonPending: an already-sent item is not re-sent (idempotency).
func TestSendItem_SkipsNonPending(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "skipped")
	prov := &fakeProvider{}
	s := newTestSender(st, prov)
	if err := sendTestItem(t, s, st, context.Background(), itemID, false); err != nil {
		t.Fatalf("SendItem on non-pending: %v", err)
	}
	if prov.sent != 0 {
		t.Errorf("provider.Send called %d times for a non-pending item, want 0", prov.sent)
	}
}

func TestSendItem_StudentWithdrawnAfterPublishSkipsBeforeProvider(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Q.SetStudentWithdrawn(context.Background(), db.SetStudentWithdrawnParams{ID: it.StudentID, Withdrawn: true}); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProvider{}
	s := newTestSender(st, prov)
	if err := sendTestItem(t, s, st, context.Background(), itemID, false); err != nil {
		t.Fatalf("withdrawn send: %v", err)
	}
	it, err = st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "skipped" || it.DeliveryJobID.Valid {
		t.Fatalf("withdrawn delivery state = status %q job %+v, want skipped/unclaimed", it.EmailStatus, it.DeliveryJobID)
	}
	if !it.Error.Valid || it.Error.String != "student withdrawn before delivery" {
		t.Fatalf("withdrawn delivery reason = %+v", it.Error)
	}
	if prov.sent != 0 {
		t.Fatalf("withdrawn delivery called provider %d times, want 0", prov.sent)
	}
}

func TestSendItem_AmbiguousProviderErrorMarksUncertainWithoutAutomaticRetry(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	prov := &fakeProvider{failWith: errors.New("provider response lost")}
	s := newTestSender(st, prov)
	ref := deliveryRef(t, st, itemID)
	jobID := senderJobID(ref)

	if err := s.SendItem(context.Background(), ref, jobID, false, true); err != nil {
		t.Fatalf("ambiguous provider error = %v, want nil to stop River retry", err)
	}
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "uncertain" {
		t.Fatalf("ambiguous provider status = %q, want uncertain", it.EmailStatus)
	}
	if prov.sent != 1 {
		t.Fatalf("provider calls = %d, want 1", prov.sent)
	}

	// Even if the same River job is delivered again, uncertain is terminal until
	// an instructor explicitly acknowledges duplicate risk and requests a resend.
	// isFirstAttempt=false: this simulates the job's second execution attempt, so
	// the A2 legacy-uncertain rescue must not apply even though status=uncertain
	// (it also requires delivery_job_id IS NULL, which is not the case here).
	if err := s.SendItem(context.Background(), ref, jobID, false, false); err != nil {
		t.Fatalf("uncertain redelivery: %v", err)
	}
	if prov.sent != 1 {
		t.Fatalf("uncertain redelivery called provider %d times, want 1", prov.sent)
	}
}

func TestSendItem_RedeliveryWhileSendingMarksUncertainWithoutProviderCall(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	ref := deliveryRef(t, st, itemID)
	jobID := senderJobID(ref)
	attempt := store.PublishDeliveryAttempt{ItemID: ref.ItemID, Generation: ref.Generation, JobID: jobID}
	if _, claimed, err := st.ClaimPublishItemDelivery(context.Background(), attempt); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if _, begun, err := st.BeginPublishItemSending(context.Background(), attempt); err != nil || !begun {
		t.Fatalf("begin sending = %v, %v", begun, err)
	}

	prov := &fakeProvider{}
	s := newTestSender(st, prov)
	// isFirstAttempt=false: this call simulates a redelivery of a job that already
	// ran once (that earlier run is what left the row in "sending").
	if err := s.SendItem(context.Background(), ref, jobID, false, false); err != nil {
		t.Fatalf("rescued redelivery: %v", err)
	}
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "uncertain" {
		t.Fatalf("rescued redelivery status = %q, want uncertain", it.EmailStatus)
	}
	if prov.sent != 0 {
		t.Fatalf("rescued redelivery called provider %d times, want 0", prov.sent)
	}
}

// --- A2: rescuing migration-0036-backfilled legacy uncertain rows ------------------

// seedLegacyUncertainPublishItem creates a normal pending item then reproduces the
// exact row shape migration 0036's backfill produces: email_status flipped to
// 'uncertain' with delivery_job_id left NULL (0036 added that column and ran its
// blind backfill UPDATE before any application code ever set it). This shape is not
// reachable through any store method — MarkPublishItemDeliveryUncertain's CAS always
// requires delivery_job_id to already match a concrete job id — so a direct SQL write
// is the only way to reproduce it for a test, exactly as the real 0036 migration did.
func seedLegacyUncertainPublishItem(t *testing.T, st *store.Store) int64 {
	t.Helper()
	itemID := seedPublishItem(t, st, "pending")
	if _, err := st.Pool.Exec(context.Background(),
		`UPDATE publish_items SET email_status = 'uncertain', delivery_job_id = NULL WHERE id = $1`, itemID); err != nil {
		t.Fatal(err)
	}
	return itemID
}

// TestSendItem_LegacyUncertainRow_FirstAttempt_RescuedAndSent is A2 case (a): a row
// shaped exactly like migration 0036's backfill arrives with a first-attempt job.
// Post-0036 code always sets delivery_job_id via the claim CAS before any path that
// marks uncertain, so this shape can only be a legacy backfilled row; the job's own
// first attempt proves it never invoked the provider before, so the send provably
// never happened. SendItem must rescue it exactly like a pending row, instead of
// leaving it stuck awaiting manual acknowledgment forever.
func TestSendItem_LegacyUncertainRow_FirstAttempt_RescuedAndSent(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedLegacyUncertainPublishItem(t, st)
	prov := &fakeProvider{}
	s := newTestSender(st, prov)

	ref := deliveryRef(t, st, itemID)
	if err := s.SendItem(context.Background(), ref, senderJobID(ref), false, true); err != nil {
		t.Fatalf("SendItem on a rescuable legacy-uncertain row = %v, want nil", err)
	}
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "sent" {
		t.Fatalf("rescued legacy row status = %q, want sent", it.EmailStatus)
	}
	if prov.sent != 1 {
		t.Fatalf("rescued legacy row provider calls = %d, want 1", prov.sent)
	}
}

// TestSendItem_LegacyUncertainRow_LaterAttempt_Untouched is A2 case (b): the SAME
// backfilled-shape row, but the arriving job reports attempt 2 -- the conservative
// discriminator (job.Attempt == 1) must refuse the rescue, leaving the row exactly as
// it was (manual acknowledgment still required) rather than guessing.
func TestSendItem_LegacyUncertainRow_LaterAttempt_Untouched(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedLegacyUncertainPublishItem(t, st)
	prov := &fakeProvider{}
	s := newTestSender(st, prov)

	ref := deliveryRef(t, st, itemID)
	if err := s.SendItem(context.Background(), ref, senderJobID(ref), false, false); err != nil {
		t.Fatalf("SendItem on a legacy-uncertain row with attempt > 1 = %v, want nil (must not error, just skip)", err)
	}
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "uncertain" || it.DeliveryJobID.Valid {
		t.Fatalf("legacy row after attempt-2 delivery = status %q job %+v, want untouched uncertain/unclaimed", it.EmailStatus, it.DeliveryJobID)
	}
	if prov.sent != 0 {
		t.Fatalf("legacy row after attempt-2 delivery called provider %d times, want 0", prov.sent)
	}
}

// TestSendItem_GenuinePostMigrationUncertainRow_Untouched is A2 case (c): a row that
// reached 'uncertain' through the normal application CAS (delivery_job_id IS set) must
// never be rescued, no matter what attempt number arrives -- the discriminator is
// delivery_job_id IS NULL, not just email_status='uncertain'. This is the ordinary
// duplicate-risk quarantine and must keep requiring explicit manual acknowledgment.
func TestSendItem_GenuinePostMigrationUncertainRow_Untouched(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	prov := &fakeProvider{failWith: errors.New("provider response lost")}
	s := newTestSender(st, prov)
	ref := deliveryRef(t, st, itemID)
	jobID := senderJobID(ref)

	// Drive a real ambiguous-outcome send so the row reaches 'uncertain' through the
	// normal CAS path, with delivery_job_id durably set to jobID.
	if err := s.SendItem(context.Background(), ref, jobID, false, true); err != nil {
		t.Fatalf("setup send: %v", err)
	}
	setup, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if setup.EmailStatus != "uncertain" || !setup.DeliveryJobID.Valid {
		t.Fatalf("setup: status=%q job.valid=%v, want uncertain with a job id set", setup.EmailStatus, setup.DeliveryJobID.Valid)
	}

	// A brand new job (first attempt) redelivering the SAME generation must still
	// refuse to touch it: delivery_job_id is already set, so this is not the legacy
	// shape the rescue is meant for.
	newJobID := jobID + 1
	prov2 := &fakeProvider{}
	s2 := newTestSender(st, prov2)
	if err := s2.SendItem(context.Background(), ref, newJobID, false, true); err != nil {
		t.Fatalf("SendItem on a genuine post-migration uncertain row = %v, want nil (must not error, just skip)", err)
	}
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "uncertain" || it.DeliveryJobID.Int64 != setup.DeliveryJobID.Int64 {
		t.Fatalf("genuine uncertain row after a rescue attempt = status %q job %+v, want untouched (original job id %d)", it.EmailStatus, it.DeliveryJobID, setup.DeliveryJobID.Int64)
	}
	if prov2.sent != 0 {
		t.Fatalf("genuine uncertain row rescue attempt called provider %d times, want 0", prov2.sent)
	}
}

func TestSendItem_DefiniteRetryReusesStableDatabaseDeliveryKey(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	initial, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := hex.EncodeToString(initial.DeliveryKey.Bytes[:])
	prov := &fakeProvider{failWith: domain.NewEmailDeliveryError(domain.EmailDeliveryDefinitelyNotAccepted, errors.New("rejected before acceptance"))}
	s := newTestSender(st, prov)
	ref := deliveryRef(t, st, itemID)
	jobID := senderJobID(ref)

	if err := s.SendItem(context.Background(), ref, jobID, false, true); err == nil {
		t.Fatal("definitely rejected attempt should return an error for River retry")
	}
	if len(prov.messages) != 1 || prov.messages[0].DeliveryKey == "" {
		t.Fatalf("first provider message lacks delivery key: %+v", prov.messages)
	}
	prov.failWith = nil
	if err := s.SendItem(context.Background(), ref, jobID, false, false); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(prov.messages) != 2 {
		t.Fatalf("provider messages = %d, want 2", len(prov.messages))
	}
	if got := prov.messages[0].DeliveryKey; got != wantKey || prov.messages[1].DeliveryKey != got {
		t.Fatalf("delivery keys = %q, %q; want stable DB key %q", got, prov.messages[1].DeliveryKey, wantKey)
	}
}

func TestPreProviderFinalTimeoutMarksFailed(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	ref := deliveryRef(t, st, itemID)
	attempt := store.PublishDeliveryAttempt{ItemID: itemID, Generation: ref.Generation, JobID: senderJobID(ref)}
	if _, claimed, err := st.ClaimPublishItemDelivery(context.Background(), attempt); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	s := newTestSender(st, &fakeProvider{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	// A10: once the terminal DB transition durably lands, preProviderTransient
	// (via markDeliveryFailed) returns nil rather than the joined cause — a non-nil
	// return here would make river.go's `final && err != nil` branch burn a wasted
	// emailFinalStateRetrySnooze cycle on an item that already reached "failed".
	if err := s.preProviderTransient(ctx, attempt, true, ctx.Err()); err != nil {
		t.Fatalf("final timeout with a durable failed write = %v, want nil (A10)", err)
	}
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "failed" {
		t.Fatalf("final ordinary timeout status = %q, want failed", it.EmailStatus)
	}
}

// TestMarkDeliveryFailed_BenignStateChange_ReturnsNil is A10's second required case:
// the CAS found nothing to update (a benign race — e.g. a concurrent skip/supersede
// already moved the row on) rather than a database error. The emailFinalStateRetrySnooze
// mechanism exists to protect against a failed DB WRITE, not this.
func TestMarkDeliveryFailed_BenignStateChange_ReturnsNil(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	ref := deliveryRef(t, st, itemID)
	attempt := store.PublishDeliveryAttempt{ItemID: itemID, Generation: ref.Generation, JobID: senderJobID(ref)}
	// Never claimed: expectedStatus "claimed" cannot match any row, so the CAS is a
	// benign 0-row miss rather than a database error.
	s := newTestSender(st, &fakeProvider{})
	if err := s.markDeliveryFailed(context.Background(), attempt, "claimed", errors.New("boom")); err != nil {
		t.Fatalf("markDeliveryFailed on a benign state-already-changed CAS miss = %v, want nil (A10)", err)
	}
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "pending" {
		t.Fatalf("status changed unexpectedly = %q, want untouched pending", it.EmailStatus)
	}
}

// TestMarkDeliveryFailed_StoreWriteFailure_StillReturnsJoinedError is A10's carve-out:
// when the DB write ITSELF fails (not merely a CAS miss), markDeliveryFailed must still
// surface a joined error so river.go's snooze-and-retry mechanism gets a chance to
// durably record the failure on a later attempt.
func TestMarkDeliveryFailed_StoreWriteFailure_StillReturnsJoinedError(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	ref := deliveryRef(t, st, itemID)
	attempt := store.PublishDeliveryAttempt{ItemID: itemID, Generation: ref.Generation, JobID: senderJobID(ref)}
	if _, claimed, err := st.ClaimPublishItemDelivery(context.Background(), attempt); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if _, err := st.Pool.Exec(context.Background(), `
		CREATE FUNCTION reject_failed_for_sender_test() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.email_status = 'failed' THEN
				RAISE EXCEPTION 'simulated failed write failure';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER reject_failed_for_sender_test
		BEFORE UPDATE ON publish_items
		FOR EACH ROW EXECUTE FUNCTION reject_failed_for_sender_test()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS reject_failed_for_sender_test ON publish_items;
			DROP FUNCTION IF EXISTS reject_failed_for_sender_test()`)
	})

	s := newTestSender(st, &fakeProvider{})
	cause := errors.New("boom")
	err := s.markDeliveryFailed(context.Background(), attempt, "claimed", cause)
	if err == nil {
		t.Fatal("markDeliveryFailed on a genuine store write failure should still return an error")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("store-write-failure error = %v, want it to still wrap the original cause", err)
	}
	it, gerr := st.Q.GetPublishItem(context.Background(), itemID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if it.EmailStatus != "claimed" {
		t.Fatalf("status after rejected write = %q, want unchanged claimed", it.EmailStatus)
	}
}

func TestPreProviderFinalShutdownReleasesPendingForSnooze(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	ref := deliveryRef(t, st, itemID)
	attempt := store.PublishDeliveryAttempt{ItemID: itemID, Generation: ref.Generation, JobID: senderJobID(ref)}
	if _, claimed, err := st.ClaimPublishItemDelivery(context.Background(), attempt); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	s := newTestSender(st, &fakeProvider{})
	shuttingDown := false
	base, cancel := context.WithCancel(context.Background())
	ctx := WithEmailShutdownCheck(base, func() bool { return shuttingDown })
	cancel()
	shuttingDown = true // predicate is dynamic and checked after cancellation.
	if err := s.preProviderTransient(ctx, attempt, true, ctx.Err()); !errors.Is(err, context.Canceled) {
		t.Fatalf("final shutdown error = %v, want context canceled for queue snooze", err)
	}
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "pending" || it.DeliveryJobID.Valid {
		t.Fatalf("final shutdown state = status %q job %+v, want pending/unclaimed", it.EmailStatus, it.DeliveryJobID)
	}
}

func TestSendItem_FinalTimeoutBeforeClaimMarksFailed(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	ref := deliveryRef(t, st, itemID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	prov := &fakeProvider{}
	// A10: a durably recorded terminal failure returns nil, not the timeout error.
	err := newTestSender(st, prov).SendItem(ctx, ref, senderJobID(ref), true, true)
	if err != nil {
		t.Fatalf("final timeout before claim with a durable failed write = %v, want nil (A10)", err)
	}
	it, getErr := st.Q.GetPublishItem(context.Background(), itemID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if it.EmailStatus != "failed" || prov.sent != 0 {
		t.Fatalf("final pre-claim timeout = status %q provider calls %d, want failed/0", it.EmailStatus, prov.sent)
	}
}

func TestSendItem_FinalShutdownBeforeClaimStaysPending(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	ref := deliveryRef(t, st, itemID)
	shuttingDown := true
	base, cancel := context.WithCancel(context.Background())
	ctx := WithEmailShutdownCheck(base, func() bool { return shuttingDown })
	cancel()
	prov := &fakeProvider{}
	err := newTestSender(st, prov).SendItem(ctx, ref, senderJobID(ref), true, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("final shutdown error = %v, want context canceled", err)
	}
	it, getErr := st.Q.GetPublishItem(context.Background(), itemID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if it.EmailStatus != "pending" || prov.sent != 0 {
		t.Fatalf("final pre-claim shutdown = status %q provider calls %d, want pending/0", it.EmailStatus, prov.sent)
	}
}

func TestSendItem_SentWriteFailureLeavesSendingThenRedeliveryQuarantines(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItem(t, st, "pending")
	ref := deliveryRef(t, st, itemID)
	jobID := senderJobID(ref)
	if _, err := st.Pool.Exec(context.Background(), `
		CREATE FUNCTION reject_sent_for_sender_test() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.email_status = 'sent' THEN
				RAISE EXCEPTION 'simulated sent write failure';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER reject_sent_for_sender_test
		BEFORE UPDATE ON publish_items
		FOR EACH ROW EXECUTE FUNCTION reject_sent_for_sender_test()`); err != nil {
		t.Fatal(err)
	}

	prov := &fakeProvider{}
	s := newTestSender(st, prov)
	if err := s.SendItem(context.Background(), ref, jobID, false, true); err == nil {
		t.Fatal("sent status write failure should surface an error")
	}
	it, err := st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "sending" || prov.sent != 1 {
		t.Fatalf("after sent write failure: status=%q provider calls=%d, want sending/1", it.EmailStatus, prov.sent)
	}
	if _, err := st.Pool.Exec(context.Background(), `
		DROP TRIGGER reject_sent_for_sender_test ON publish_items;
		DROP FUNCTION reject_sent_for_sender_test()`); err != nil {
		t.Fatal(err)
	}

	if err := s.SendItem(context.Background(), ref, jobID, false, false); err != nil {
		t.Fatalf("rescued redelivery: %v", err)
	}
	it, err = st.Q.GetPublishItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "uncertain" || prov.sent != 1 {
		t.Fatalf("rescued sent-write failure: status=%q provider calls=%d, want uncertain/1", it.EmailStatus, prov.sent)
	}
}

// --- report attachments through the send pipeline (spec §3, D42/D44/D45) -----------

// testFontPath writes a real (non-CJK) UTF-8 TTF to a temp file — a stand-in for
// `make report-fonts`'s Noto Sans TC, mirroring internal/report/report_test.go's
// helper (this package can't import report's unexported test helper, so it's
// duplicated minimally rather than exporting test-only surface from report).
func testFontPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/test-font.ttf"
	if err := os.WriteFile(path, goregular.TTF, 0o600); err != nil {
		t.Fatalf("write test font: %v", err)
	}
	return path
}

// solidJPEG returns a minimal valid JPEG — stand-in "student page image" bytes.
func solidJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test JPEG: %v", err)
	}
	return buf.Bytes()
}

// TestSendItem_AttachesPDF_None_ByteIdenticalToTodaysEmail: the "none" attachment
// setting (the default, and today's behaviour before D42) must send exactly zero
// attachments — the report-attachment build path must not run at all.
func TestSendItem_AttachesPDF_None_ByteIdenticalToTodaysEmail(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItemOpts(t, st, seedOpts{status: "pending", attachment: "none"})
	prov := &fakeProvider{}
	s := newTestSenderWithReport(st, prov, nil, "") // no blobs/font wired — proves "none" never touches them
	if err := sendTestItem(t, s, st, context.Background(), itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	if len(prov.lastMsg.Attachments) != 0 {
		t.Errorf("attachment=none sent %d attachments, want 0", len(prov.lastMsg.Attachments))
	}
}

// TestSendItem_AttachesPDF_Compressed: attachment="compressed" builds a PDF and
// attaches it under an ASCII-only constant filename (never the student's name).
func TestSendItem_AttachesPDF_Compressed(t *testing.T) {
	st := storetest.Fresh(t)
	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Fake Exam"})
	if err != nil {
		t.Fatal(err)
	}
	stu, err := st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: "z99", Name: "Zed Fake", Email: "zed@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	seedAnswerWithPage(t, st, blobs, a.ID, stu.ID, solidJPEG(t, 400, 600))

	snap := Snapshot{
		AssessmentName: "Fake Exam", StudentExternalID: "z99", StudentName: "Zed Fake",
		Total: "8", Max: "10",
		Problems: []SnapProblem{{Number: 1, Title: "P1", Max: "10", Total: "8", Criteria: []SnapCriterion{{Name: "C", Score: "8", Max: "10"}}}},
	}
	b, _ := canonicalJSON(snap)
	_, items, err := st.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: a.ID, Note: "t", Attachment: "compressed",
		Items: []store.CreatePublishItemInput{{StudentID: stu.ID, Snapshot: b, RecipientEmail: "zed@example.edu", EmailStatus: "pending"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	itemID := items[0].ID

	prov := &fakeProvider{}
	s := newTestSenderWithReport(st, prov, blobs, testFontPath(t))
	if err := sendTestItem(t, s, st, context.Background(), itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	if len(prov.lastMsg.Attachments) != 1 {
		t.Fatalf("attachment=compressed sent %d attachments, want 1", len(prov.lastMsg.Attachments))
	}
	att := prov.lastMsg.Attachments[0]
	if att.Filename != "results.pdf" {
		t.Errorf("attachment filename = %q, want ASCII constant %q", att.Filename, "results.pdf")
	}
	for _, r := range att.Filename {
		if r > 127 {
			t.Fatalf("attachment filename %q has a non-ASCII rune", att.Filename)
		}
	}
	if len(att.Content) == 0 {
		t.Error("attachment content is empty")
	}
}

// TestSendItem_ZipFlag_SwitchesFormat: zip=true swaps the PDF for a ZIP-of-images
// (spec §3 D45) — same settings otherwise, different filename/extension and content
// shape (a real ZIP, not a PDF).
func TestSendItem_ZipFlag_SwitchesFormat(t *testing.T) {
	st := storetest.Fresh(t)
	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Fake Exam"})
	if err != nil {
		t.Fatal(err)
	}
	stu, err := st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: "z99", Name: "Zed Fake", Email: "zed@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	seedAnswerWithPage(t, st, blobs, a.ID, stu.ID, solidJPEG(t, 400, 600))

	snap := Snapshot{
		AssessmentName: "Fake Exam", StudentExternalID: "z99", StudentName: "Zed Fake",
		Total: "8", Max: "10",
		Problems: []SnapProblem{{Number: 1, Title: "P1", Max: "10", Total: "8", Criteria: []SnapCriterion{{Name: "C", Score: "8", Max: "10"}}}},
	}
	b, _ := canonicalJSON(snap)
	_, items, err := st.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: a.ID, Note: "t", Attachment: "compressed", Zip: true,
		Items: []store.CreatePublishItemInput{{StudentID: stu.ID, Snapshot: b, RecipientEmail: "zed@example.edu", EmailStatus: "pending"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	itemID := items[0].ID

	prov := &fakeProvider{}
	s := newTestSenderWithReport(st, prov, blobs, testFontPath(t))
	if err := sendTestItem(t, s, st, context.Background(), itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	if len(prov.lastMsg.Attachments) != 1 {
		t.Fatalf("zip=true sent %d attachments, want 1", len(prov.lastMsg.Attachments))
	}
	att := prov.lastMsg.Attachments[0]
	if att.Filename != "results.zip" {
		t.Errorf("attachment filename = %q, want %q", att.Filename, "results.zip")
	}
	if _, err := zip.NewReader(bytes.NewReader(att.Content), int64(len(att.Content))); err != nil {
		t.Errorf("attachment content is not a valid zip: %v", err)
	}
}

// TestSendItem_AttachmentDeterministic_SameBytesOnResend: spec §3 "rebuild-on-resend
// is deterministic" — SendItem is called twice for the same item (simulating a
// resend rebuilding from the same stored snapshot + blobs) and must produce
// byte-identical attachment content both times.
func TestSendItem_AttachmentDeterministic_SameBytesOnResend(t *testing.T) {
	st := storetest.Fresh(t)
	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Fake Exam"})
	if err != nil {
		t.Fatal(err)
	}
	stu, err := st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: "z99", Name: "Zed Fake", Email: "zed@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	seedAnswerWithPage(t, st, blobs, a.ID, stu.ID, solidJPEG(t, 400, 600))

	snap := Snapshot{
		AssessmentName: "Fake Exam", StudentExternalID: "z99", StudentName: "Zed Fake",
		Total: "8", Max: "10",
		Problems: []SnapProblem{{Number: 1, Title: "P1", Max: "10", Total: "8", Criteria: []SnapCriterion{{Name: "C", Score: "8", Max: "10"}}}},
	}
	b, _ := canonicalJSON(snap)
	_, items, err := st.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: a.ID, Note: "t", Attachment: "compressed",
		Items: []store.CreatePublishItemInput{{StudentID: stu.ID, Snapshot: b, RecipientEmail: "zed@example.edu", EmailStatus: "pending"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	itemID := items[0].ID
	fontPath := testFontPath(t)

	prov1 := &fakeProvider{}
	s1 := newTestSenderWithReport(st, prov1, blobs, fontPath)
	if err := sendTestItem(t, s1, st, context.Background(), itemID, false); err != nil {
		t.Fatalf("first SendItem: %v", err)
	}
	first := prov1.lastMsg.Attachments[0].Content

	// Arm a real new resend generation. It rotates the delivery key while keeping
	// the same snapshot/blobs/settings, so rebuilt attachment bytes must still match.
	firstItem, err := st.Q.GetPublishItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if _, armed, err := st.ArmPublishItemResend(ctx, itemID, firstItem.EmailGeneration, false); err != nil || !armed {
		t.Fatalf("arm resend = %v, %v", armed, err)
	}

	prov2 := &fakeProvider{}
	s2 := newTestSenderWithReport(st, prov2, blobs, fontPath)
	if err := sendTestItem(t, s2, st, context.Background(), itemID, false); err != nil {
		t.Fatalf("second SendItem: %v", err)
	}
	second := prov2.lastMsg.Attachments[0].Content

	if !bytes.Equal(first, second) {
		t.Error("attachment bytes differ between two builds from the same snapshot+blobs — resend must be deterministic")
	}
}

// TestSendItem_15MBWarning_RecordedNonTerminally: an attachment over 15 MiB records a
// non-terminal warning on the item (spec §3 "send still proceeds") — the item still
// ends up "sent", not "failed", with the warning surfaced via the existing error
// column (prefixed so it reads distinctly from a real failure).
func TestSendItem_15MBWarning_RecordedNonTerminally(t *testing.T) {
	st := storetest.Fresh(t)
	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Fake Exam"})
	if err != nil {
		t.Fatal(err)
	}
	stu, err := st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: "z99", Name: "Zed Fake", Email: "zed@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	// A large, low-entropy-but-not-trivially-compressible page image: "original"
	// quality skips downscaling, so a big enough source JPEG produces a >15MB PDF
	// attachment (many megapixels of photographic noise won't shrink much under the
	// PDF's own JPEG re-embedding).
	bigPage := noisyJPEG(t, 4600, 3400)
	seedAnswerWithPage(t, st, blobs, a.ID, stu.ID, bigPage)

	snap := Snapshot{
		AssessmentName: "Fake Exam", StudentExternalID: "z99", StudentName: "Zed Fake",
		Total: "8", Max: "10",
		Problems: []SnapProblem{{Number: 1, Title: "P1", Max: "10", Total: "8", Criteria: []SnapCriterion{{Name: "C", Score: "8", Max: "10"}}}},
	}
	b, _ := canonicalJSON(snap)
	_, items, err := st.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: a.ID, Note: "t", Attachment: "original",
		Items: []store.CreatePublishItemInput{{StudentID: stu.ID, Snapshot: b, RecipientEmail: "zed@example.edu", EmailStatus: "pending"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	itemID := items[0].ID

	prov := &fakeProvider{}
	s := newTestSenderWithReport(st, prov, blobs, testFontPath(t))
	if err := sendTestItem(t, s, st, context.Background(), itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	if len(prov.lastMsg.Attachments) != 1 || len(prov.lastMsg.Attachments[0].Content) <= 15*1024*1024 {
		got := 0
		if len(prov.lastMsg.Attachments) == 1 {
			got = len(prov.lastMsg.Attachments[0].Content)
		}
		t.Fatalf("test fixture attachment size = %d bytes, want > 15MiB (fixture too small to exercise the warning)", got)
	}

	it, err := st.Q.GetPublishItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.EmailStatus != "sent" {
		t.Errorf("15MB-over item status = %q, want sent (warning is non-terminal)", it.EmailStatus)
	}
	if !it.Error.Valid || !hasWarningPrefix(it.Error.String) {
		t.Errorf("15MB-over item error = %+v, want a warning-prefixed message", it.Error)
	}
}

// --- terminal vs transient attachment-build error classification (finding 2) -------

// TestSendItem_FontUnconfigured_FailsTerminallyOnFirstAttempt: font-unconfigured is a
// "won't ever succeed by retrying" condition (the sender's own buildAttachment comment
// already called it terminal), but before this fix ALL buildAttachment errors were
// routed through s.transient — so a non-final attempt just returned the error for
// River to retry up to 5 times before finally landing failed. This test simulates a
// batch that requested a report attachment while the font *was* configured, then the
// font became unconfigured before the (re)send — the item must land failed with a
// clear message on the VERY FIRST attempt (final=false), not require 5 retries first.
func TestSendItem_FontUnconfigured_FailsTerminallyOnFirstAttempt(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItemOpts(t, st, seedOpts{status: "pending", attachment: "compressed"})
	prov := &fakeProvider{}
	// reportFontPath="" — font unconfigured — but blobs non-nil so only the font check
	// trips.
	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newTestSenderWithReport(st, prov, blobs, "")

	// Non-final (attempt 1 of however many River would allow): must land failed
	// immediately, not be returned as a retryable error. A10: since the terminal
	// write durably lands, SendItem returns nil — River sees the job complete
	// rather than retrying a permanently doomed send.
	err = sendTestItem(t, s, st, context.Background(), itemID, false)
	if err != nil {
		t.Fatalf("SendItem with unconfigured font and a durable failed write = %v, want nil (A10)", err)
	}
	it, gerr := st.Q.GetPublishItem(context.Background(), itemID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if it.EmailStatus != "failed" {
		t.Fatalf("font-unconfigured item status = %q after attempt 1 (final=false), want failed immediately (terminal, not retryable)", it.EmailStatus)
	}
	if !it.Error.Valid || it.Error.String == "" {
		t.Error("font-unconfigured item should carry a clear error message")
	}
	if prov.sent != 0 {
		t.Errorf("provider.Send called %d times, want 0 (terminal failure must not reach the provider)", prov.sent)
	}
}

// TestSendItem_BlobsNilWithAttachment_FailsTerminallyOnFirstAttempt: no blob store
// wired is the other terminal build precondition the sender's own comment already
// called out — same first-attempt-failed requirement as the font case.
func TestSendItem_BlobsNilWithAttachment_FailsTerminallyOnFirstAttempt(t *testing.T) {
	st := storetest.Fresh(t)
	itemID := seedPublishItemOpts(t, st, seedOpts{status: "pending", attachment: "compressed"})
	prov := &fakeProvider{}
	s := newTestSenderWithReport(st, prov, nil, testFontPath(t)) // font configured, blobs nil

	// A10: a durably recorded terminal failure returns nil, not an error.
	err := sendTestItem(t, s, st, context.Background(), itemID, false)
	if err != nil {
		t.Fatalf("SendItem with nil blob store and a durable failed write = %v, want nil (A10)", err)
	}
	it, gerr := st.Q.GetPublishItem(context.Background(), itemID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if it.EmailStatus != "failed" {
		t.Fatalf("blobs-nil item status = %q after attempt 1 (final=false), want failed immediately (terminal, not retryable)", it.EmailStatus)
	}
}

// TestSendItem_BlobReadFailure_StaysRetryable: a blob store that errors on Get (a
// storage hiccup) must remain transient — a non-final attempt returns the error for
// River to retry and the item stays pending, exactly like the pre-fix behaviour for
// this specific case (unlike the font/blobs-nil preconditions above, which must now be
// terminal).
type failingBlobStore struct{ blobstore.Store }

func (failingBlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return nil, errors.New("simulated storage hiccup")
}

func TestSendItem_BlobReadFailure_StaysRetryable(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()
	realBlobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Fake Exam"})
	if err != nil {
		t.Fatal(err)
	}
	stu, err := st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: "z99", Name: "Zed Fake", Email: "zed@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	// A real answer+page exists (so buildAttachment gets past the font/blobs-nil
	// preconditions and reaches the actual blob read; the sender resolves the answer id
	// live from (assessment, student, problem 1) rather than reading it off the
	// snapshot), but the blob store handed to the sender fails on Get.
	seedAnswerWithPage(t, st, realBlobs, a.ID, stu.ID, solidJPEG(t, 400, 600))

	snap := Snapshot{
		AssessmentName: "Fake Exam", StudentExternalID: "z99", StudentName: "Zed Fake",
		Total: "8", Max: "10",
		Problems: []SnapProblem{{Number: 1, Title: "P1", Max: "10", Total: "8", Criteria: []SnapCriterion{{Name: "C", Score: "8", Max: "10"}}}},
	}
	b, _ := canonicalJSON(snap)
	_, items, err := st.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: a.ID, Note: "t", Attachment: "compressed",
		Items: []store.CreatePublishItemInput{{StudentID: stu.ID, Snapshot: b, RecipientEmail: "zed@example.edu", EmailStatus: "pending"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	itemID := items[0].ID

	prov := &fakeProvider{}
	s := newTestSenderWithReport(st, prov, failingBlobStore{}, testFontPath(t))

	// Non-final: must return an error for River to retry, item stays pending.
	if err := sendTestItem(t, s, st, context.Background(), itemID, false); err == nil {
		t.Fatal("SendItem with a blob-read failure should return an error")
	}
	it, gerr := st.Q.GetPublishItem(ctx, itemID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if it.EmailStatus != "pending" {
		t.Errorf("blob-read failure (non-final) status = %q, want pending (retryable)", it.EmailStatus)
	}
}

// --- honest advertised regrade deadline (workflow-guards 2026-07-10) ---------------

// seedPublishItemForDeadline is seedPublishItem returning the assessment id too, so
// the deadline tests can set the assessment's regrade_deadline (invented data only).
func seedPublishItemForDeadline(t *testing.T, st *store.Store) (itemID, assessmentID int64) {
	t.Helper()
	ctx := context.Background()
	a, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Fake Exam"})
	if err != nil {
		t.Fatal(err)
	}
	stu, err := st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: "z99", Name: "Zed Fake", Email: "zed@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	snap := Snapshot{
		AssessmentName: "Fake Exam", StudentExternalID: "z99", StudentName: "Zed Fake",
		Total: "8", Max: "10",
		Problems: []SnapProblem{{Number: 1, Title: "P1", Max: "10", Total: "8", Criteria: []SnapCriterion{{Name: "C", Score: "8", Max: "10"}}}},
	}
	b, _ := canonicalJSON(snap)
	_, items, err := st.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: a.ID, Note: "t", CreatedBy: 0,
		Items: []store.CreatePublishItemInput{{StudentID: stu.ID, Snapshot: b, RecipientEmail: "zed@example.edu", EmailStatus: "pending"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return items[0].ID, a.ID
}

// TestSendItem_AdvertisedDeadline_AssessmentDeadlineWinsWhenEarlier: the grade email
// must not promise a regrade date past what is actually enforced. The inbound webhook
// enforces BOTH the token window and the assessment's regrade_deadline, so when the
// assessment deadline lands BEFORE send-time+window, the email must advertise the
// assessment deadline — not the token window it advertised before this fix.
func TestSendItem_AdvertisedDeadline_AssessmentDeadlineWinsWhenEarlier(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()
	itemID, aid := seedPublishItemForDeadline(t, st)

	fixedNow := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	// Assessment deadline 3 days out — well inside the 14-day token window.
	if _, err := st.Q.SetAssessmentRegradeDeadline(ctx, db.SetAssessmentRegradeDeadlineParams{
		ID: aid, Deadline: pgtype.Timestamptz{Time: fixedNow.Add(3 * 24 * time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	// Read the stored deadline back so the expected date string goes through the same
	// timestamptz round-trip SendItem's own lookup does (zone-proof compare).
	a, err := st.Q.GetAssessment(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	wantDate := a.RegradeDeadline.Time.Format("2006-01-02")
	windowDate := fixedNow.Add(14 * 24 * time.Hour).Format("2006-01-02")

	prov := &fakeProvider{}
	s := newTestSender(st, prov)
	s.now = func() time.Time { return fixedNow }
	if err := sendTestItem(t, s, st, ctx, itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	if !bytes.Contains([]byte(prov.lastMsg.TextBody), []byte(wantDate)) {
		t.Errorf("email body does not advertise the assessment regrade deadline %s:\n%s", wantDate, prov.lastMsg.TextBody)
	}
	if bytes.Contains([]byte(prov.lastMsg.TextBody), []byte(windowDate)) {
		t.Errorf("email body still advertises the token-window date %s (dishonest — the webhook enforces the earlier assessment deadline):\n%s", windowDate, prov.lastMsg.TextBody)
	}
}

// TestSendItem_AdvertisedDeadline_TokenWindowWhenUnset: no assessment deadline set ⇒
// behavior unchanged — the email advertises send-time+window.
func TestSendItem_AdvertisedDeadline_TokenWindowWhenUnset(t *testing.T) {
	st := storetest.Fresh(t)
	itemID, _ := seedPublishItemForDeadline(t, st)

	fixedNow := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	windowDate := fixedNow.Add(14 * 24 * time.Hour).Format("2006-01-02")

	prov := &fakeProvider{}
	s := newTestSender(st, prov)
	s.now = func() time.Time { return fixedNow }
	if err := sendTestItem(t, s, st, context.Background(), itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	if !bytes.Contains([]byte(prov.lastMsg.TextBody), []byte(windowDate)) {
		t.Errorf("with no assessment deadline the email must advertise the token window date %s:\n%s", windowDate, prov.lastMsg.TextBody)
	}
}

// TestSendItem_AdvertisedDeadline_TokenWindowWhenAssessmentLater: an assessment
// deadline AFTER send-time+window must not stretch the advertised date past the token
// window — the token dies first, so the window date is the honest one ("earlier of").
func TestSendItem_AdvertisedDeadline_TokenWindowWhenAssessmentLater(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()
	itemID, aid := seedPublishItemForDeadline(t, st)

	fixedNow := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if _, err := st.Q.SetAssessmentRegradeDeadline(ctx, db.SetAssessmentRegradeDeadlineParams{
		ID: aid, Deadline: pgtype.Timestamptz{Time: fixedNow.Add(60 * 24 * time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	a, err := st.Q.GetAssessment(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	laterDate := a.RegradeDeadline.Time.Format("2006-01-02")
	windowDate := fixedNow.Add(14 * 24 * time.Hour).Format("2006-01-02")

	prov := &fakeProvider{}
	s := newTestSender(st, prov)
	s.now = func() time.Time { return fixedNow }
	if err := sendTestItem(t, s, st, ctx, itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	if !bytes.Contains([]byte(prov.lastMsg.TextBody), []byte(windowDate)) {
		t.Errorf("email must advertise the token window date %s (the earlier bound):\n%s", windowDate, prov.lastMsg.TextBody)
	}
	if bytes.Contains([]byte(prov.lastMsg.TextBody), []byte(laterDate)) {
		t.Errorf("email advertises the later assessment deadline %s the token can't honor:\n%s", laterDate, prov.lastMsg.TextBody)
	}
}

// noisyJPEG returns a large, high-entropy JPEG (per-pixel pseudo-random noise) —
// unlike a solid fill, this resists JPEG's DCT compression enough that a big canvas
// reliably produces a multi-megabyte source image, which the PDF then re-embeds at
// a comparable size for the "original" (no downscale) quality path.
func noisyJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := mrand.New(mrand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(rnd.Intn(256)), G: uint8(rnd.Intn(256)), B: uint8(rnd.Intn(256)), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode noisy JPEG: %v", err)
	}
	return buf.Bytes()
}
