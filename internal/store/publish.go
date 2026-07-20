package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// CreatePublishBatchParams is the input to CreatePublishBatch: the batch header plus
// one item per included student. Items carry the pre-built snapshot JSONB (Q3's
// shape), recipient email captured at publish time (B-H10), and regrade token.
// Attachment/Zip (migration 0022, spec §3/§9/§10) are the batch-level report-PDF
// attachment settings chosen at publish time; Attachment empty defaults to "none"
// (the pre-existing no-attachment behaviour) so callers that don't yet pass it keep
// working unchanged.
type CreatePublishBatchParams struct {
	AssessmentID int64
	Note         string
	ResendAll    bool
	CreatedBy    int64
	Attachment   string // "none" (default), "compressed", or "original"
	Zip          bool
	Items        []CreatePublishItemInput
	// ExpectedFinal* closes the source-selection race between snapshot building
	// and batch creation, and additionally gates the A9 coverage re-check: when
	// Kind is non-empty, CreatePublishBatch re-verifies BOTH the final-source
	// fields AND the coverage gate (Blocked/NotIngested == 0) inside its
	// assessment lock. Empty kind is reserved for lower-level tests/callers that
	// intentionally opt out of these service-level invariants.
	ExpectedFinalSourceKind string
	ExpectedFinalMethodID   int64
	ExpectedFinalRunID      int64
	// EnqueuePending, when set, inserts delivery jobs using the SAME transaction as
	// the batch/items/published_at writes. Only pending items are passed; skipped
	// items never receive a job. Returning an error rolls the entire publish back.
	EnqueuePending func(context.Context, pgx.Tx, []PublishItem) error
}

// CreatePublishItemInput is one student's item within a publish batch.
type CreatePublishItemInput struct {
	StudentID      int64
	Snapshot       []byte // JSONB
	RecipientEmail string
	RegradeToken   string
	EmailStatus    string // "pending" or "skipped" (all-no_submission students)
}

// PublishBatch and PublishItem alias the generated row types — later tasks (Q3, the
// httpapi publish routes) refer to these by name per the brief.
type PublishBatch = db.PublishBatch
type PublishItem = db.PublishItem

// ErrNotLatestBatch is returned by SupersedePublishBatch when the target batch is not
// the latest non-superseded batch for its assessment (or is already superseded). A
// newer batch is live, so superseding this one would incorrectly clear published_at
// assessment-wide out from under it. Callers in httpapi should map this to 409.
var ErrNotLatestBatch = errors.New("store: batch is not the latest non-superseded batch for its assessment")

// ErrLiveBatchExists is returned by CreatePublishBatch when the migration-0021 partial
// unique index (one non-superseded batch per assessment) rejects the insert — i.e. a
// concurrent publish already created the live batch after this caller's pre-check read.
// The publish service maps it to publish.ErrAlreadyPublished so the race loses cleanly.
var ErrLiveBatchExists = errors.New("store: a live (non-superseded) publish batch already exists for this assessment")

// ErrFinalSourceChanged means the final source moved after the publish service
// built its snapshots but before the batch transaction acquired the assessment
// lock. The caller must refresh and rebuild rather than publish stale bytes.
var ErrFinalSourceChanged = errors.New("store: final source changed while preparing publish")

// ErrCoverageGateChanged means the coverage gate (spec §2: every roster-student x
// problem answer either official or no_submission, and every active roster student
// ingested) no longer holds when re-verified inside CreatePublishBatch's assessment
// lock (A9) — an answer that satisfied the gate at the caller's earlier, unlocked
// preview read lost its official record (or a student became not-ingested) before
// this locked check ran. The caller must refresh preview and rebuild rather than
// publish from stale coverage state.
var ErrCoverageGateChanged = errors.New("store: coverage gate no longer satisfied")

// ErrPublishBatchSuperseded means a delivery tried to cross the provider boundary
// after its batch had been unpublished. The item is already terminally skipped by
// SupersedePublishBatch; callers must not send it.
var ErrPublishBatchSuperseded = errors.New("store: publish batch was superseded before delivery")

// ErrPublishDeliveryInProgress means unpublish raced with an item that had already
// crossed the durable "sending" boundary. Unpublish must wait for that attempt to
// reach sent/failed/uncertain rather than invalidating its batch mid-provider-call.
var ErrPublishDeliveryInProgress = errors.New("store: publish delivery is in progress")

// CreatePublishBatch inserts the batch + all items and sets answers.published_at for
// the whole assessment, in one transaction (spec §2 "Effects of publishing").
//
// Verification contract: when arg.ExpectedFinalSourceKind is non-empty (every real
// publish.Service call), this method re-verifies the final-source fields AND the
// coverage gate (A9) inside its assessment lock, aborting with
// ErrFinalSourceChanged / ErrCoverageGateChanged respectively — the caller's own
// preview-time checks are unlocked pre-reads and cannot be trusted at commit time.
// When the kind is empty (lower-level tests/callers), neither is verified and the
// full gate is the caller's responsibility (see PublishPreview) — that opt-out keeps
// the method reusable for fixtures and an admin escape hatch if one is ever needed.
func (s *Store) CreatePublishBatch(ctx context.Context, arg CreatePublishBatchParams) (PublishBatch, []PublishItem, error) {
	var batch db.PublishBatch
	items := make([]PublishItem, 0, len(arg.Items))

	attachment := arg.Attachment
	if attachment == "" {
		attachment = "none"
	}

	err := s.WithTxPgx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		locked, err := q.GetAssessmentForUpdate(ctx, arg.AssessmentID)
		if err != nil {
			return fmt.Errorf("lock assessment for publish: %w", err)
		}
		if arg.ExpectedFinalSourceKind != "" {
			same := locked.FinalSourceKind.Valid && locked.FinalSourceKind.String == arg.ExpectedFinalSourceKind
			switch arg.ExpectedFinalSourceKind {
			case "method":
				same = same && locked.FinalMethodID.Valid && locked.FinalMethodID.Int64 == arg.ExpectedFinalMethodID &&
					locked.FinalRunID.Valid && locked.FinalRunID.Int64 == arg.ExpectedFinalRunID
			case "consensus":
				same = same && !locked.FinalMethodID.Valid && !locked.FinalRunID.Valid
			default:
				same = false
			}
			if !same {
				return ErrFinalSourceChanged
			}
			// A9: re-verify the coverage gate inside this same lock, using the same
			// queries PublishPreview uses — the caller's coverage check (publish.
			// Service.Publish's pv.Publishable()) reads from an UNLOCKED pre-read
			// well before this transaction started. An answer that satisfied the
			// gate then can lose its official record (a concurrent regrade/
			// recompute) before this point; publishing the caller's now-stale
			// snapshot bytes would misrepresent that answer as graded.
			// Residual window: official-record writers (Store.RecomputeOfficials
			// and its httpapi/runner callers) do NOT take this assessment lock, so
			// this recheck narrows the preview→create race, it does not close it —
			// a write landing between this read and commit can still slip through.
			counts, err := q.PublishCoverageCounts(ctx, arg.AssessmentID)
			if err != nil {
				return fmt.Errorf("re-check coverage gate: %w", err)
			}
			if counts.Blocked != 0 || counts.NotIngested != 0 {
				return ErrCoverageGateChanged
			}
		}
		batch, err = q.CreatePublishBatch(ctx, db.CreatePublishBatchParams{
			AssessmentID: arg.AssessmentID,
			Note:         arg.Note,
			ResendAll:    arg.ResendAll,
			CreatedBy:    pgtype.Int8{Int64: arg.CreatedBy, Valid: arg.CreatedBy != 0},
			Attachment:   attachment,
			Zip:          arg.Zip,
		})
		if err != nil {
			// The migration-0021 partial unique index (one live batch per assessment)
			// rejects a racing second create — map it to the sentinel the publish service
			// turns into ErrAlreadyPublished (M2).
			if IsUniqueViolation(err) {
				return ErrLiveBatchExists
			}
			return fmt.Errorf("create publish batch: %w", err)
		}

		for _, in := range arg.Items {
			status := in.EmailStatus
			if status == "" {
				status = "pending"
			}
			item, err := q.CreatePublishItem(ctx, db.CreatePublishItemParams{
				BatchID:        batch.ID,
				StudentID:      in.StudentID,
				Snapshot:       in.Snapshot,
				RecipientEmail: in.RecipientEmail,
				RegradeToken:   in.RegradeToken,
				EmailStatus:    status,
			})
			if err != nil {
				return fmt.Errorf("create publish item (student %d): %w", in.StudentID, err)
			}
			items = append(items, item)
		}

		// Coverage gate already spans every answer of the assessment (D1/D6 guard
		// column) — publishing locks official-grade changes assessment-wide.
		if err := q.SetPublishedAtForAssessment(ctx, arg.AssessmentID); err != nil {
			return fmt.Errorf("set published_at: %w", err)
		}
		if arg.EnqueuePending != nil {
			pending := make([]PublishItem, 0, len(items))
			for _, item := range items {
				if item.EmailStatus == "pending" {
					pending = append(pending, item)
				}
			}
			if len(pending) > 0 {
				if err := arg.EnqueuePending(ctx, tx, pending); err != nil {
					return fmt.Errorf("enqueue pending publish items: %w", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return db.PublishBatch{}, nil, err
	}
	return batch, items, nil
}

// SupersedePublishBatch is the D29 unpublish escape hatch: clears published_at on
// every answer of the batch's assessment and stamps the batch superseded_at, in one
// transaction. It does not un-send email (spec §2) — it re-opens grading so a
// correction + re-publish can happen. actorID is accepted for the caller's audit log
// (the audit write itself happens in httpapi, per Global Constraints "s.audit(...) on
// state changes" — this method only performs the DB effect).
//
// The supersede is guarded to the LATEST non-superseded batch for the assessment (see
// queries/publish.sql): if batchID names an older batch, or one already superseded,
// this returns ErrNotLatestBatch and leaves published_at untouched — clearing
// published_at assessment-wide while a newer batch is still live would silently
// unpublish work the newer batch is responsible for.
func (s *Store) SupersedePublishBatch(ctx context.Context, batchID, actorID int64) error {
	_ = actorID // audit logging is the caller's responsibility (httpapi layer)
	return s.WithTx(ctx, func(q *db.Queries) error {
		// Lock the batch before inspecting delivery state. BeginPublishItemSending
		// takes this same lock before entering "sending", so exactly one side of an
		// unpublish/provider-boundary race wins.
		batch, err := q.GetPublishBatchForUpdate(ctx, batchID)
		if err != nil {
			return fmt.Errorf("lock publish batch: %w", err)
		}
		if batch.SupersededAt.Valid {
			return ErrNotLatestBatch
		}
		sending, err := q.HasSendingPublishItems(ctx, batchID)
		if err != nil {
			return fmt.Errorf("check in-progress publish deliveries: %w", err)
		}
		if sending {
			return ErrPublishDeliveryInProgress
		}
		assessment, err := q.GetAssessmentForUpdate(ctx, batch.AssessmentID)
		if err != nil {
			return fmt.Errorf("lock assessment for unpublish: %w", err)
		}
		if _, err := q.SupersedePublishBatch(ctx, batchID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotLatestBatch
			}
			return fmt.Errorf("supersede publish batch: %w", err)
		}
		if _, err := q.SkipUndeliveredPublishItemsForBatch(ctx, batchID); err != nil {
			return fmt.Errorf("skip undelivered publish items: %w", err)
		}
		if err := q.ClearPublishedAtForAssessment(ctx, batch.AssessmentID); err != nil {
			return fmt.Errorf("clear published_at: %w", err)
		}
		// Officials were frozen while published. Rubrics, flags, or a legacy
		// pre-pin source may have changed around that window; re-derive before the
		// transaction re-opens grading so a re-publish cannot reuse stale pointers.
		if _, err := recomputeOfficialsWithQueries(ctx, q, assessment); err != nil {
			return fmt.Errorf("recompute officials after unpublish: %w", err)
		}
		return nil
	})
}

// ListPublishBatches returns every batch for the assessment, newest first.
func (s *Store) ListPublishBatches(ctx context.Context, assessmentID int64) ([]PublishBatch, error) {
	return s.Q.ListPublishBatches(ctx, assessmentID)
}

// GetPublishItemForSend loads one item plus its batch/assessment context (spec §3
// send pipeline) — the single row the email_send job reads.
func (s *Store) GetPublishItemForSend(ctx context.Context, itemID int64) (db.GetPublishItemForSendRow, error) {
	return s.Q.GetPublishItemForSend(ctx, itemID)
}

// GetPublishItemForResend loads one item plus its parent batch's attachment settings
// (spec §4 D46 individual resend) — the resend action reconstructs the same
// attachment/zip behaviour as the original send from this single row.
func (s *Store) GetPublishItemForResend(ctx context.Context, itemID int64) (db.GetPublishItemForResendRow, error) {
	return s.Q.GetPublishItemForResend(ctx, itemID)
}

// SetPublishItemRegradeToken persists the token minted from the item id at send time
// (spec §4 — display/debug only; verification is by recomputation, not lookup).
func (s *Store) SetPublishItemRegradeToken(ctx context.Context, itemID int64, token string) error {
	return s.Q.SetPublishItemRegradeToken(ctx, db.SetPublishItemRegradeTokenParams{ID: itemID, RegradeToken: token})
}

// ResetPublishItemToPending flips a failed item back to pending (resend-failed, spec
// §7). Reports whether the item was actually failed (false ⇒ a concurrent send already
// moved it on, so the caller must not re-enqueue it).
func (s *Store) ResetPublishItemToPending(ctx context.Context, itemID int64) (bool, error) {
	_, err := s.Q.ResetPublishItemToPending(ctx, itemID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// PublishCriteria returns criterion metadata (id → description/points/position) for
// every rubric criterion referenced by an official record of the assessment — the
// snapshot builder joins these onto grading_records.criterion_scores (spec §2).
func (s *Store) PublishCriteria(ctx context.Context, assessmentID int64) ([]db.PublishCriteriaRow, error) {
	return s.Q.PublishCriteria(ctx, assessmentID)
}

// ListPublishItems returns every item of a batch (joined with student identity),
// ordered by roster student id.
func (s *Store) ListPublishItems(ctx context.Context, batchID int64) ([]db.ListPublishItemsRow, error) {
	return s.Q.ListPublishItems(ctx, batchID)
}

// PublishBatchItemCounts returns one batch's email-status breakdown (items, sent,
// failed, uncertain, skipped) for the batches-history summary (B5) — lets a caller
// show an accurate summary row without needing to filter the full items list itself,
// and makes the previously-invisible skipped count a first-class field.
func (s *Store) PublishBatchItemCounts(ctx context.Context, batchID int64) (db.PublishBatchItemCountsRow, error) {
	return s.Q.PublishBatchItemCounts(ctx, batchID)
}

// PublishItemsByStatus filters a batch's items to one email_status — used by the
// resend-failed action (spec §7 POST /api/publish/batches/{id}/resend-failed).
func (s *Store) PublishItemsByStatus(ctx context.Context, batchID int64, status string) ([]db.PublishItemsByStatusRow, error) {
	return s.Q.PublishItemsByStatus(ctx, db.PublishItemsByStatusParams{BatchID: batchID, EmailStatus: status})
}

// UpdatePublishItemEmailStatus records a send attempt's outcome (spec §3 send
// pipeline): providerID and errText are both optional (empty string = SQL NULL).
func (s *Store) UpdatePublishItemEmailStatus(ctx context.Context, itemID int64, status, providerID, errText string) error {
	return s.Q.UpdatePublishItemEmailStatus(ctx, db.UpdatePublishItemEmailStatusParams{
		ID:                itemID,
		EmailStatus:       status,
		ProviderMessageID: pgtype.Text{String: providerID, Valid: providerID != ""},
		Error:             pgtype.Text{String: errText, Valid: errText != ""},
	})
}

// PublishDeliveryAttempt identifies one immutable queued generation. River job ids
// are recorded at claim time; every later transition includes all three fields so a
// stale/duplicate job cannot mutate a newer resend generation.
type PublishDeliveryAttempt struct {
	ItemID     int64
	Generation int32
	JobID      int64
}

func deliveryItemCAS(row db.PublishItem, err error) (PublishItem, bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return PublishItem{}, false, nil
	}
	if err != nil {
		return PublishItem{}, false, err
	}
	return row, true, nil
}

func deliveryIDCAS(_ int64, err error) (bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func deliveryJobID(id int64) pgtype.Int8 {
	return pgtype.Int8{Int64: id, Valid: true}
}

// ArmPublishItemResend creates a new pending generation from a terminal delivery
// state. uncertain is excluded unless allowUncertain is explicitly true because a
// resend may duplicate an email whose provider outcome is unknown.
func (s *Store) ArmPublishItemResend(ctx context.Context, itemID int64, expectedGeneration int32, allowUncertain bool) (PublishItem, bool, error) {
	var (
		row db.PublishItem
		ok  bool
	)
	err := s.WithTx(ctx, func(q *db.Queries) error {
		var err error
		row, ok, err = s.ArmPublishItemResendTx(ctx, q, itemID, expectedGeneration, allowUncertain)
		return err
	})
	if err != nil {
		return PublishItem{}, false, err
	}
	return row, ok, nil
}

// ArmPublishItemResendTx is the transaction-scoped arm variant used to commit the
// generation change and its River job atomically. q must be bound to the caller's
// transaction.
func (s *Store) ArmPublishItemResendTx(ctx context.Context, q *db.Queries, itemID int64, expectedGeneration int32, allowUncertain bool) (PublishItem, bool, error) {
	batch, err := q.GetPublishBatchForItemForUpdate(ctx, itemID)
	if err != nil {
		return PublishItem{}, false, err
	}
	if batch.SupersededAt.Valid {
		return PublishItem{}, false, ErrPublishBatchSuperseded
	}
	row, err := q.ArmPublishItemResend(ctx, db.ArmPublishItemResendParams{
		ID: itemID, EmailGeneration: expectedGeneration, AllowUncertain: allowUncertain,
	})
	return deliveryItemCAS(row, err)
}

// ClaimPublishItemDelivery assigns one River job to one pending generation. A
// duplicate or stale job gets (false, nil) and must exit without sending.
func (s *Store) ClaimPublishItemDelivery(ctx context.Context, attempt PublishDeliveryAttempt) (PublishItem, bool, error) {
	row, err := s.Q.ClaimPublishItemDelivery(ctx, db.ClaimPublishItemDeliveryParams{
		ID: attempt.ItemID, EmailGeneration: attempt.Generation, DeliveryJobID: deliveryJobID(attempt.JobID),
	})
	return deliveryItemCAS(row, err)
}

// ClaimLegacyUncertainPublishItemDelivery rescues a migration-0036-backfilled row
// (audit A2, see the query's doc for the full discriminator argument). The caller
// (publish.Sender.SendItem) is responsible for restricting this to a job's first
// attempt before calling — this CAS itself only enforces the row-shape half
// (email_status='uncertain' AND delivery_job_id IS NULL) of the safety argument.
func (s *Store) ClaimLegacyUncertainPublishItemDelivery(ctx context.Context, attempt PublishDeliveryAttempt) (PublishItem, bool, error) {
	row, err := s.Q.ClaimLegacyUncertainPublishItemDelivery(ctx, db.ClaimLegacyUncertainPublishItemDeliveryParams{
		ID: attempt.ItemID, EmailGeneration: attempt.Generation, DeliveryJobID: deliveryJobID(attempt.JobID),
	})
	return deliveryItemCAS(row, err)
}

// BeginPublishItemSending crosses the durable provider side-effect boundary. It
// locks the parent batch and student row, then checks superseded/withdrawn state in
// the same transaction as the claimed->sending CAS. This serializes the provider
// boundary with both unpublish and roster withdrawal.
func (s *Store) BeginPublishItemSending(ctx context.Context, attempt PublishDeliveryAttempt) (PublishItem, bool, error) {
	var (
		row db.PublishItem
		ok  bool
	)
	err := s.WithTx(ctx, func(q *db.Queries) error {
		var err error
		row, ok, err = s.BeginPublishItemSendingTx(ctx, q, attempt)
		return err
	})
	if err != nil {
		return PublishItem{}, false, err
	}
	return row, ok, nil
}

// BeginPublishItemSendingTx is BeginPublishItemSending for callers that already own
// a transaction. q must be transaction-bound.
func (s *Store) BeginPublishItemSendingTx(ctx context.Context, q *db.Queries, attempt PublishDeliveryAttempt) (PublishItem, bool, error) {
	batch, err := q.GetPublishBatchForItemForUpdate(ctx, attempt.ItemID)
	if err != nil {
		return PublishItem{}, false, err
	}
	if batch.SupersededAt.Valid {
		return PublishItem{}, false, ErrPublishBatchSuperseded
	}
	student, err := q.GetPublishItemStudentForUpdate(ctx, attempt.ItemID)
	if err != nil {
		return PublishItem{}, false, err
	}
	if student.WithdrawnAt.Valid {
		skipped, err := q.SkipWithdrawnPublishItemDelivery(ctx, db.SkipWithdrawnPublishItemDeliveryParams{
			ID: attempt.ItemID, EmailGeneration: attempt.Generation, DeliveryJobID: deliveryJobID(attempt.JobID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return PublishItem{}, false, nil
		}
		if err != nil {
			return PublishItem{}, false, err
		}
		return skipped, false, nil
	}
	row, err := q.BeginPublishItemSendingCAS(ctx, db.BeginPublishItemSendingCASParams{
		ID: attempt.ItemID, EmailGeneration: attempt.Generation, DeliveryJobID: deliveryJobID(attempt.JobID),
	})
	return deliveryItemCAS(row, err)
}

// MarkPublishItemDeliverySent completes only the matching sending generation.
func (s *Store) MarkPublishItemDeliverySent(ctx context.Context, attempt PublishDeliveryAttempt, providerID, warning string) (bool, error) {
	id, err := s.Q.MarkPublishItemDeliverySent(ctx, db.MarkPublishItemDeliverySentParams{
		ID:                attempt.ItemID,
		EmailGeneration:   attempt.Generation,
		DeliveryJobID:     deliveryJobID(attempt.JobID),
		ProviderMessageID: pgtype.Text{String: providerID, Valid: providerID != ""},
		Error:             pgtype.Text{String: warning, Valid: warning != ""},
	})
	return deliveryIDCAS(id, err)
}

// MarkPublishItemDeliveryFailed terminates the matching claimed/sending generation.
// expectedStatus is part of the CAS and is itself allow-listed by SQL so callers
// cannot overwrite pending or terminal state through this generic failure path.
func (s *Store) MarkPublishItemDeliveryFailed(ctx context.Context, attempt PublishDeliveryAttempt, expectedStatus, errText string) (bool, error) {
	id, err := s.Q.MarkPublishItemDeliveryFailed(ctx, db.MarkPublishItemDeliveryFailedParams{
		ID:              attempt.ItemID,
		EmailGeneration: attempt.Generation,
		DeliveryJobID:   deliveryJobID(attempt.JobID),
		EmailStatus:     expectedStatus,
		Error:           pgtype.Text{String: errText, Valid: errText != ""},
	})
	return deliveryIDCAS(id, err)
}

// MarkPublishItemDeliveryUncertain quarantines an ambiguous provider outcome. It
// cannot be re-armed without explicit duplicate-risk acknowledgement.
func (s *Store) MarkPublishItemDeliveryUncertain(ctx context.Context, attempt PublishDeliveryAttempt, providerID, errText string) (bool, error) {
	id, err := s.Q.MarkPublishItemDeliveryUncertain(ctx, db.MarkPublishItemDeliveryUncertainParams{
		ID:                attempt.ItemID,
		EmailGeneration:   attempt.Generation,
		DeliveryJobID:     deliveryJobID(attempt.JobID),
		ProviderMessageID: pgtype.Text{String: providerID, Valid: providerID != ""},
		ErrorText:         pgtype.Text{String: errText, Valid: errText != ""},
	})
	return deliveryIDCAS(id, err)
}

// ReleasePublishItemDeliveryClaim returns a pre-provider claim to pending so the
// generation can be retried. It intentionally cannot release sending.
func (s *Store) ReleasePublishItemDeliveryClaim(ctx context.Context, attempt PublishDeliveryAttempt) (bool, error) {
	id, err := s.Q.ReleasePublishItemDeliveryClaim(ctx, db.ReleasePublishItemDeliveryClaimParams{
		ID: attempt.ItemID, EmailGeneration: attempt.Generation, DeliveryJobID: deliveryJobID(attempt.JobID),
	})
	return deliveryIDCAS(id, err)
}

// ReleasePublishItemSending retries the same generation only when the provider
// returned a typed guarantee that it did not accept the request. Unknown errors,
// timeouts, and cancellation after BeginPublishItemSending must use uncertain.
func (s *Store) ReleasePublishItemSending(ctx context.Context, attempt PublishDeliveryAttempt) (bool, error) {
	id, err := s.Q.ReleasePublishItemSending(ctx, db.ReleasePublishItemSendingParams{
		ID: attempt.ItemID, EmailGeneration: attempt.Generation, DeliveryJobID: deliveryJobID(attempt.JobID),
	})
	return deliveryIDCAS(id, err)
}

// HasSendingPublishItems reports whether a batch currently has any delivery across
// the provider boundary. SupersedePublishBatch calls it while holding the batch lock.
func (s *Store) HasSendingPublishItems(ctx context.Context, batchID int64) (bool, error) {
	return s.Q.HasSendingPublishItems(ctx, batchID)
}

// PublishPreviewRow is PublishPreview's composed result: coverage counts, the
// blockers list (answers with pages but no official grade — spec §2), the raw
// per-(student,problem) inputs Q3 uses to build snapshot JSONB, and the set of
// student ids whose latest-batch item would change if published again now
// (changed-vs-latest-batch, D30's default "changed-only" re-publish selection).
//
// Deliberately NOT one mega-query (per the brief): coverage counts, blockers, and
// snapshot inputs are independent SELECTs composed here in Go, which keeps each SQL
// query simple and lets ChangedStudentIDs be computed with an ordinary JSON compare
// instead of a database-side JSONB diff.
type PublishPreviewRow struct {
	AssessmentID int64
	// TotalAnswers (B3) is the coverage-percentage DENOMINATOR, not a literal row
	// count: real answers rows (graded+no_submission+blocked) PLUS each
	// not-ingested student's missing (student x problem) cells
	// (NotIngested*problem count) — see PublishPreview. Without the latter term, a
	// caller computing (graded+no_submission)/TotalAnswers reads 100% covered even
	// while not_ingested students block publish.
	TotalAnswers      int64
	Graded            int64
	NoSubmission      int64
	Blocked           int64
	NotIngested       int64 // active roster students with zero answers rows (roster-add-after-ingest gap)
	Blockers          []db.PublishBlockersRow
	SnapshotInputs    []db.PublishSnapshotInputsRow
	LatestBatch       *db.PublishBatch // nil if the assessment has never been published
	ChangedStudentIDs []int64          // vs LatestBatch, by comparing snapshot bytes (nil if never published)
}

// Publishable reports whether the coverage gate is satisfied (spec §2): every
// roster-student x problem answer either has an official grade or is effectively
// no_submission, AND every active roster student has been ingested (at least one
// answers row exists for them) — a roster student with zero answers rows for the
// assessment (added to the roster after ingest, or never ingested) fails closed
// rather than silently passing the gate and receiving nothing. Blocked/NotIngested > 0
// means Blockers is non-empty and publish must refuse.
func (p PublishPreviewRow) Publishable() bool { return p.Blocked == 0 && p.NotIngested == 0 }

// PublishPreview composes the read-only preview the publish UI and the publish
// endpoint's pre-flight check both need: coverage gate state + blockers, the raw
// inputs for building each student's snapshot, and (when the assessment has a prior
// non-superseded batch) which students' snapshots would actually change — the input
// to changed-only re-publish (D30).
func (s *Store) PublishPreview(ctx context.Context, assessmentID int64) (PublishPreviewRow, error) {
	out := PublishPreviewRow{AssessmentID: assessmentID}

	counts, err := s.Q.PublishCoverageCounts(ctx, assessmentID)
	if err != nil {
		return out, fmt.Errorf("publish coverage counts: %w", err)
	}
	out.TotalAnswers, out.Graded, out.NoSubmission, out.Blocked, out.NotIngested =
		counts.TotalAnswers, counts.Graded, counts.NoSubmission, counts.Blocked, counts.NotIngested
	// B3: a not-ingested student has ZERO answers rows, so a plain count(*) FROM
	// answers denominator never sees them — a caller computing a coverage percentage
	// as (graded+no_submission)/total_answers would read 100% while a not_ingested
	// blocker exists (e.g. "COVERAGE 100%" next to a red "NOT INGESTED 4" tile). Add
	// their missing (student x problem) cells to the denominator — one not-ingested
	// student is missing an answer for EVERY problem, not just one — so the ratio
	// actually reflects the gap. Publishable() is unaffected: it already fails closed
	// on NotIngested>0 regardless of this denominator.
	out.TotalAnswers += counts.NotIngested * counts.ProblemCount

	blockers, err := s.Q.PublishBlockers(ctx, assessmentID)
	if err != nil {
		return out, fmt.Errorf("publish blockers: %w", err)
	}
	out.Blockers = blockers

	inputs, err := s.Q.PublishSnapshotInputs(ctx, assessmentID)
	if err != nil {
		return out, fmt.Errorf("publish snapshot inputs: %w", err)
	}
	out.SnapshotInputs = inputs

	latest, err := s.Q.LatestNonSupersededBatch(ctx, assessmentID)
	if err == nil {
		out.LatestBatch = &latest
		prevItems, err := s.Q.LatestBatchItemsByStudent(ctx, assessmentID)
		if err != nil {
			return out, fmt.Errorf("latest batch items: %w", err)
		}
		out.ChangedStudentIDs = changedStudentIDs(inputs, prevItems)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("latest publish batch: %w", err)
	}

	return out, nil
}

// changedStudentIDs is a placeholder comparison hook: Q3 owns the snapshot JSONB
// shape (per the brief), so the real byte-for-byte diff against a freshly-built
// snapshot happens there. Store-level preview cannot build that snapshot itself
// without duplicating Q3's shape decision, so it reports every student who has a
// prior-batch item as a superset candidate; Q3's publish service does the final
// build-and-compare per student before selecting resend targets.
func changedStudentIDs(_ []db.PublishSnapshotInputsRow, prevItems []db.PublishItem) []int64 {
	ids := make([]int64, 0, len(prevItems))
	for _, it := range prevItems {
		ids = append(ids, it.StudentID)
	}
	return ids
}
