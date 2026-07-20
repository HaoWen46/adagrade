package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// sortRefs orders a StudentRef slice by external id for stable API output.
func sortRefs(refs []StudentRef) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].ExternalID < refs[j].ExternalID })
}

// DeliveryRef is the durable identity of one email attempt. Explicit resends
// increment Generation; River retries retain it, so stale jobs can never mutate
// or send a newer attempt for the same publish item.
type DeliveryRef struct {
	ItemID     int64 `json:"item_id"`
	Generation int32 `json:"email_generation"`
}

// EnqueueTxFunc inserts email jobs through the caller's pgx transaction. Batch/
// item writes and their jobs therefore commit or roll back as one outbox unit.
type EnqueueTxFunc func(ctx context.Context, tx pgx.Tx, refs []DeliveryRef) error

// Service is the publish state machine (spec §2, §7): coverage gate, snapshot build,
// changed-only re-publish, unpublish, resend-failed. It holds the seams but performs
// no HTTP concerns — httpapi wraps it.
type Service struct {
	store     *store.Store
	enqueueTx EnqueueTxFunc
	log       *slog.Logger
	provider  string        // ADAMARKER_EMAIL_PROVIDER — "none" ⇒ skip all sends (spec §3)
	window    time.Duration // regrade token validity window (unused here; the send job mints)
	from      string        // ADAMARKER_EMAIL_FROM — displayed in the preview payload (spec §2 D41); empty when unset
	// reportFontConfigured mirrors config.ReportFontConfigured() (spec §3 D43) — the
	// service takes the resolved bool rather than importing internal/config, matching
	// how it already takes `provider`/`from` as plain values instead of a Config.
	reportFontConfigured bool
}

// NewService constructs the publish service. enqueueTx may be nil only when email is
// disabled or for tests that never create/resend an emailable item. Real-provider
// writes fail closed without it so no pending delivery can commit without durable
// River work in the same transaction.
func NewService(st *store.Store, enqueueTx EnqueueTxFunc, provider string, window time.Duration, from string, reportFontConfigured bool, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: st, enqueueTx: enqueueTx, log: log, provider: provider, window: window, from: from, reportFontConfigured: reportFontConfigured}
}

// ErrCoverageGate is returned by Publish when the assessment is not fully graded
// (spec §2). It carries the blockers so httpapi can 409 with the list.
type ErrCoverageGate struct {
	Blockers []db.PublishBlockersRow
}

func (e ErrCoverageGate) Error() string {
	return fmt.Sprintf("publish: coverage gate not satisfied (%d blocker(s))", len(e.Blockers))
}

// ErrNothingToPublish is returned when a publish call would create zero items:
// either a changed-only re-publish finds no changed students (resend_all is false),
// or there are simply no eligible students at all (e.g. a first publish, or a
// resend-all, on an empty/fully-withdrawn roster — A5). Nothing would be sent
// either way, so we refuse rather than create an empty batch.
var ErrNothingToPublish = errors.New("publish: no changed students since the last publish (nothing to re-send)")

// ErrNoFinalSource is returned by Publish when the exam hasn't chosen its final
// grading source yet (0027): with no source there are no derived officials, so
// there is nothing meaningful to publish.
var ErrNoFinalSource = errors.New("publish: choose the exam's final grading source first")

// ErrSpotCheckGate is returned by Publish when the exact run selected as the
// method source hasn't cleared its spot-check sample
// (trust spec §4 — relocated here in 0027 from the removed accept-official).
type ErrSpotCheckGate struct {
	RunID int64
	Total int
	Done  int
}

func (e ErrSpotCheckGate) Error() string {
	return fmt.Sprintf("publish: spot-check gate not cleared for run %d (%d of %d reviewed)", e.RunID, e.Done, e.Total)
}

// ErrAlreadyPublished is returned by Publish when a non-superseded batch already
// exists for the assessment. Without this guard, a second publish call would create a
// SECOND non-superseded batch — duplicate grade emails to every changed/resend_all
// student, and an ambiguous LatestNonSupersededBatch for Unpublish (which one does
// "the" live batch mean?). The single-live-batch invariant holds: exactly zero or one
// non-superseded batch per assessment at all times. To re-publish, the caller must
// unpublish first (that supersedes the live batch and reopens grading); the changed-
// only diff baseline vs the just-superseded batch already makes that re-publish flow
// send only what actually changed (D30).
var ErrAlreadyPublished = errors.New("publish: already published — unpublish first")

// ErrGradingStateChanged asks the operator/client to refresh: source selection
// changed after preview/snapshot construction, so publishing those bytes would
// misrepresent the now-current officials.
var ErrGradingStateChanged = errors.New("publish: grading source changed while preparing results; refresh and try again")

// ErrEmailQueueUnavailable prevents a real-provider publish/resend from committing
// delivery state without the durable River job that owns it.
var ErrEmailQueueUnavailable = errors.New("publish: email queue is unavailable")

// ErrEmailDisabled prevents an explicit resend from creating work while the none
// provider is selected. In particular, it keeps old/new jobs from being consumed as
// fake successes by a no-op provider.
var ErrEmailDisabled = errors.New("publish: email delivery is disabled")

// ErrDeliveryInProgress refuses a second explicit resend while the current delivery
// generation is pending, claimed by a worker, or across the provider side-effect
// boundary. Replacing that generation would defeat its CAS ownership.
var ErrDeliveryInProgress = errors.New("publish: email delivery is already in progress")

// ErrUncertainNeedsAcknowledgement protects an ambiguous provider outcome from a
// blind resend. The operator must explicitly accept that a duplicate is possible.
var ErrUncertainNeedsAcknowledgement = errors.New("publish: previous email outcome is uncertain; acknowledge duplicate-delivery risk to resend")

// ErrResendNotAllowed covers corrupt/unknown delivery states rather than silently
// turning them back into pending.
var ErrResendNotAllowed = errors.New("publish: email delivery cannot be resent from its current state")

// ErrRecipientWithdrawn is the service-level backstop for the HTTP guard: a roster
// withdrawal racing a resend cannot result in a new email job.
var ErrRecipientWithdrawn = errors.New("publish: student is withdrawn")

// StudentRef identifies a roster student in preview/result payloads (no PII beyond
// what the review UI already shows: external id + name).
type StudentRef struct {
	StudentID  int64  `json:"student_id"` // internal id
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
}

// Preview is the read-only publish preview (spec §7 GET .../publish/preview):
// coverage counts, blockers, and — on a re-publish — which students changed and which
// are all-no_submission (skipped).
type Preview struct {
	AssessmentID int64 `json:"assessment_id"`
	// TotalAnswers (B3) is the coverage-percentage DENOMINATOR: real answers rows
	// PLUS not-ingested students' missing (student x problem) cells — see
	// store.PublishPreviewRow.TotalAnswers. A caller computing
	// (graded+no_submission)/total_answers therefore reflects not_ingested
	// blockers instead of reading 100% while they exist.
	TotalAnswers int64                   `json:"total_answers"`
	Graded       int64                   `json:"graded"`
	NoSubmission int64                   `json:"no_submission"`
	Blocked      int64                   `json:"blocked"`
	NotIngested  int64                   `json:"not_ingested"` // roster students with zero answers rows (fail-closed gap)
	Publishable  bool                    `json:"publishable"`
	Blockers     []db.PublishBlockersRow `json:"blockers"`
	// HasLiveBatch: a NON-superseded (live) batch currently exists — gates the "Already
	// published" badge + the Unpublish button (M1). EverPublished: any batch ever existed
	// (superseded or not) — drives the changed-only diff + re-publish messaging. They
	// differ exactly in the unpublished-not-yet-republished window; conflating them made
	// a superseded-only assessment wrongly show as "already published" with an Unpublish
	// button that would 404.
	HasLiveBatch  bool `json:"has_live_batch"`
	EverPublished bool `json:"ever_published"`
	// AlreadyPublished mirrors HasLiveBatch, kept for older clients.
	AlreadyPublished bool         `json:"already_published"`
	Changed          []StudentRef `json:"changed"` // vs latest batch (re-publish only)
	Skipped          []StudentRef `json:"skipped"` // all-no_submission students
	StudentCount     int          `json:"student_count"`
	EmailDisabled    bool         `json:"email_disabled"` // provider == none
	// From is ADAMARKER_EMAIL_FROM, shown so the operator sees what students will see
	// as the sender (spec §2 D41); empty when unset (e.g. provider == none).
	From string `json:"from"`
	// ReportAttachmentsAvailable mirrors config.ReportFontConfigured() (spec §3 D43):
	// the UI disables the attachment options with a hint when the font isn't
	// configured, rather than letting the operator pick an option the publish
	// endpoint will then 400.
	ReportAttachmentsAvailable bool `json:"report_attachments_available"`
	// UnassignedTAProblems lists this assessment's problems with no assigned TA (spec
	// §6 D60: "publish preview warns (not blocks) on unassigned problems"). A
	// lecturer who publishes without assigning TAs first would otherwise only
	// discover the gap when a final-turn handoff silently drops mail for that
	// problem (no assignee ⇒ no target, D60's "no TA assigned" flag) — this is the
	// operational guard surfaced BEFORE that happens. Warn-only: never blocks
	// Publish. Always a non-nil (possibly empty) slice.
	UnassignedTAProblems []UnassignedTAProblem `json:"unassigned_ta_problems"`
	// FinalSource is the exam's chosen grading source (0027); nil = not chosen,
	// which blocks publishing (Publishable=false + ErrNoFinalSource).
	FinalSource *FinalSourceState `json:"final_source"`
}

// FinalSourceState is the preview's view of the chosen source plus, for a
// single-run source, the relocated spot-check gate (trust spec §4): the
// selected run must have its sample reviewed or waived before
// publish. Consensus sources need no sample — every conflict already forces
// human review by construction (the fallback queue).
type FinalSourceState struct {
	Kind       string `json:"kind"` // "method" | "consensus"
	MethodID   int64  `json:"method_id,omitempty"`
	MethodName string `json:"method_name,omitempty"`
	RunID      int64  `json:"run_id,omitempty"`
	RunStatus  string `json:"run_status,omitempty"`
	// MethodVersion and scope make the pinned authority legible in the publish
	// UI; a partial run is allowed but naturally leaves coverage blockers.
	MethodVersion int32  `json:"method_version,omitempty"`
	ScopeKind     string `json:"scope_kind,omitempty"`
	ScopeID       int64  `json:"scope_id,omitempty"`
	// Spot-check gate state; meaningful only for kind=method with a completed
	// run (SpotCheckRunID != 0). SpotCheckOpen=true means the gate clears.
	SpotCheckRunID  int64 `json:"spot_check_run_id,omitempty"`
	SpotCheckTotal  int   `json:"spot_check_total,omitempty"`
	SpotCheckDone   int   `json:"spot_check_done,omitempty"`
	SpotCheckWaived bool  `json:"spot_check_waived,omitempty"`
	SpotCheckOpen   bool  `json:"spot_check_open"`
}

// UnassignedTAProblem is one entry of Preview.UnassignedTAProblems: enough for the UI
// to name the gap (spec §6 D60 publish-preview warning).
type UnassignedTAProblem struct {
	ProblemID     int64 `json:"problem_id"`
	ProblemNumber int32 `json:"problem_number"`
}

// finalSourceState loads the exam's chosen source (0027) plus, for a method
// source, the relocated spot-check gate state anchored on that exact pinned
// completed run. nil = no source chosen yet. Consensus sources report the gate
// open by construction: every consensus conflict already lands in the human
// fallback queue, so a separate sample would double-review the same work.
func (s *Service) finalSourceState(ctx context.Context, assessmentID int64) (*FinalSourceState, error) {
	a, err := s.store.Q.GetAssessment(ctx, assessmentID)
	if err != nil {
		return nil, err
	}
	if !a.FinalSourceKind.Valid {
		return nil, nil
	}
	fs := &FinalSourceState{Kind: a.FinalSourceKind.String}
	if fs.Kind != "method" {
		fs.SpotCheckOpen = true
		return fs, nil
	}
	fs.MethodID = a.FinalMethodID.Int64
	if m, err := s.store.Q.GetGradingMethod(ctx, fs.MethodID); err == nil {
		fs.MethodName = m.Name
	}
	if !a.FinalRunID.Valid {
		return nil, errors.New("publish: method final source has no pinned run")
	}
	run, err := s.store.Q.GetRun(ctx, a.FinalRunID.Int64)
	if err != nil {
		return nil, err
	}
	if run.AssessmentID != assessmentID {
		return nil, errors.New("publish: final source run belongs to another assessment")
	}
	mv, err := s.store.Q.GetMethodVersion(ctx, run.MethodVersionID)
	if err != nil {
		return nil, err
	}
	if mv.MethodID != fs.MethodID {
		return nil, errors.New("publish: final source run belongs to another method")
	}
	fs.RunID = run.ID
	fs.RunStatus = run.Status
	fs.MethodVersion = mv.Version
	fs.ScopeKind = run.ScopeKind
	fs.ScopeID = run.ScopeID
	fs.SpotCheckRunID = run.ID // compatibility alias for existing clients
	if run.Status != "completed" {
		fs.SpotCheckOpen = false
		return fs, nil
	}
	total, done, waived, err := s.store.SpotCheckState(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	fs.SpotCheckTotal, fs.SpotCheckDone, fs.SpotCheckWaived = total, done, waived
	fs.SpotCheckOpen = waived || (total > 0 && done == total)
	return fs, nil
}

// GetPreview composes the publish preview. It builds the fresh snapshots so the
// changed-only diff is exact (the store-level ChangedStudentIDs is a superset
// placeholder by design — Q3 owns the snapshot shape, so the true diff lives here).
func (s *Service) GetPreview(ctx context.Context, assessmentID int64) (Preview, error) {
	pv, err := s.store.PublishPreview(ctx, assessmentID)
	if err != nil {
		return Preview{}, err
	}
	name, err := s.assessmentName(ctx, assessmentID)
	if err != nil {
		return Preview{}, err
	}
	criteria, err := s.store.PublishCriteria(ctx, assessmentID)
	if err != nil {
		return Preview{}, err
	}
	snaps, _, names, exts := buildSnapshots(name, pv.SnapshotInputs, criteria)

	prior, err := s.hasPriorBatch(ctx, assessmentID)
	if err != nil {
		return Preview{}, err
	}
	liveBatch, err := s.hasLiveBatch(ctx, assessmentID)
	if err != nil {
		return Preview{}, err
	}
	fs, err := s.finalSourceState(ctx, assessmentID)
	if err != nil {
		return Preview{}, err
	}
	out := Preview{
		AssessmentID: assessmentID,
		TotalAnswers: pv.TotalAnswers,
		Graded:       pv.Graded,
		NoSubmission: pv.NoSubmission,
		Blocked:      pv.Blocked,
		NotIngested:  pv.NotIngested,
		// Publishable (0027): coverage + a chosen source + (method sources) the
		// relocated spot-check gate.
		Publishable:                pv.Publishable() && fs != nil && fs.SpotCheckOpen,
		FinalSource:                fs,
		Blockers:                   pv.Blockers,
		HasLiveBatch:               liveBatch,
		EverPublished:              prior,
		AlreadyPublished:           liveBatch, // back-compat: mirror live-batch semantics
		StudentCount:               len(snaps),
		EmailDisabled:              s.provider == "none",
		From:                       s.from,
		ReportAttachmentsAvailable: s.reportFontConfigured,
	}
	if out.Blockers == nil {
		out.Blockers = []db.PublishBlockersRow{}
	}

	unassigned, err := s.unassignedTAProblems(ctx, assessmentID)
	if err != nil {
		return Preview{}, err
	}
	out.UnassignedTAProblems = unassigned

	for sid, snap := range snaps {
		if snap.AllNoSubmission {
			out.Skipped = append(out.Skipped, StudentRef{StudentID: sid, ExternalID: exts[sid], Name: names[sid]})
		}
	}

	// Changed-only diff vs the most recent batch (superseded or not — spec §2 D30).
	if prior {
		prev, err := s.latestSnapshotBytes(ctx, assessmentID)
		if err != nil {
			return Preview{}, err
		}
		for sid, snap := range snaps {
			b, err := canonicalJSON(snap)
			if err != nil {
				return Preview{}, err
			}
			if pb, ok := prev[sid]; !ok || !bytes.Equal(pb, b) {
				out.Changed = append(out.Changed, StudentRef{StudentID: sid, ExternalID: exts[sid], Name: names[sid]})
			}
		}
	}
	sortRefs(out.Changed)
	sortRefs(out.Skipped)
	return out, nil
}

// unassignedTAProblems lists an assessment's problems with no assigned TA (spec §6
// D60 publish-preview warning), reusing the same ListTAAssignments query the
// regrade v2 TA-assignment picker backs — a LEFT JOIN that already returns every
// problem with a nullable assignment, so "unassigned" is just "user_id IS NULL"
// filtered client-side of the query. Always returns a non-nil slice.
func (s *Service) unassignedTAProblems(ctx context.Context, assessmentID int64) ([]UnassignedTAProblem, error) {
	rows, err := s.store.ListTAAssignments(ctx, assessmentID)
	if err != nil {
		return nil, err
	}
	out := make([]UnassignedTAProblem, 0, len(rows))
	for _, row := range rows {
		if row.UserID.Valid {
			continue
		}
		out = append(out, UnassignedTAProblem{ProblemID: row.ProblemID, ProblemNumber: row.ProblemNumber})
	}
	return out, nil
}

// PublishResult is returned by Publish (spec §7 POST .../publish).
type PublishResult struct {
	BatchID      int64 `json:"batch_id"`
	ItemsCreated int   `json:"items_created"`
	Enqueued     int   `json:"enqueued"` // pending items with a real send job
	Skipped      int   `json:"skipped"`  // all-no_submission (no email)
	// SkippedWithdrawn counts previously-published students excluded from THIS batch
	// because they are withdrawn now (roster-lifecycle plan 2026-07-10, locked
	// semantics (b)/(d)): no item, no email — their published history is untouched.
	// Always 0 on a first publish (nobody was published before).
	SkippedWithdrawn int    `json:"skipped_withdrawn"`
	EmailDisabled    bool   `json:"email_disabled"` // provider == none
	Warning          string `json:"warning,omitempty"`
}

// validAttachmentValues are the exact three attachment options (spec §3 D44) — used
// by both Publish's validation and (mirrored) the resend row's stored value.
var validAttachmentValues = map[string]bool{"none": true, "compressed": true, "original": true}

// ErrInvalidAttachment is returned by Publish when the attachment value is not one of
// the exact three spec §3 D44 options. httpapi maps it to 400.
type ErrInvalidAttachment struct{ Value string }

func (e ErrInvalidAttachment) Error() string {
	return fmt.Sprintf("publish: invalid attachment %q (want \"none\", \"compressed\", or \"original\")", e.Value)
}

// ErrReportFontUnconfigured is returned by Publish when a non-"none" attachment is
// requested but ADAMARKER_REPORT_FONT isn't configured (spec §3 D43: "unset ⇒
// attachment options disabled in the UI with a hint" — this is the server-side
// backstop for a client that ignores that hint). httpapi maps it to 400.
var ErrReportFontUnconfigured = errors.New("publish: report PDF attachments requested but ADAMARKER_REPORT_FONT is not configured")

// Publish runs the coverage gate, builds a batch of per-student snapshot items,
// stamps published_at assessment-wide, and enqueues one send job per emailable item
// (spec §2 effects of publishing). Selection is changed-only by default on a
// re-publish; resendAll overrides it (spec §2 D30). all-no_submission students get a
// skipped item and no email. When the provider is none, every emailable item is
// recorded skipped and a loud warning is returned (spec §3). attachment/zip are the
// batch-level report-PDF settings (spec §3 D42/D44/D45); attachment="" defaults to
// "none" (unchanged pre-D42 behaviour) so existing callers/tests are unaffected.
func (s *Service) Publish(ctx context.Context, assessmentID int64, note string, resendAll bool, actorID int64, attachment string, zip bool) (PublishResult, error) {
	if attachment == "" {
		attachment = "none"
	}
	if !validAttachmentValues[attachment] {
		return PublishResult{}, ErrInvalidAttachment{Value: attachment}
	}
	if attachment != "none" && !s.reportFontConfigured {
		return PublishResult{}, ErrReportFontUnconfigured
	}

	alreadyPublished, err := s.hasLiveBatch(ctx, assessmentID)
	if err != nil {
		return PublishResult{}, err
	}
	if alreadyPublished {
		return PublishResult{}, ErrAlreadyPublished
	}

	pv, err := s.store.PublishPreview(ctx, assessmentID)
	if err != nil {
		return PublishResult{}, err
	}
	if !pv.Publishable() {
		return PublishResult{}, ErrCoverageGate{Blockers: pv.Blockers}
	}
	// Source + spot-check gates (0027): a chosen source is what MAKES grades
	// official, and a method source's pinned run must have cleared its sample
	// (trust spec §4 — relocated from the removed accept-official).
	fs, err := s.finalSourceState(ctx, assessmentID)
	if err != nil {
		return PublishResult{}, err
	}
	if fs == nil {
		return PublishResult{}, ErrNoFinalSource
	}
	if !fs.SpotCheckOpen {
		return PublishResult{}, ErrSpotCheckGate{RunID: fs.SpotCheckRunID, Total: fs.SpotCheckTotal, Done: fs.SpotCheckDone}
	}

	name, err := s.assessmentName(ctx, assessmentID)
	if err != nil {
		return PublishResult{}, err
	}
	criteria, err := s.store.PublishCriteria(ctx, assessmentID)
	if err != nil {
		return PublishResult{}, err
	}
	snaps, emails, _, _ := buildSnapshots(name, pv.SnapshotInputs, criteria)

	// Selection (spec §2 D30): first publish or resendAll ⇒ everyone; else changed-only.
	// "First publish" means no batch has ever existed for the assessment (superseded or
	// not) — after an unpublish, the just-superseded batch is still the diff baseline.
	rePublish, err := s.hasPriorBatch(ctx, assessmentID)
	if err != nil {
		return PublishResult{}, err
	}
	selectChanged := rePublish && !resendAll
	// The baseline is loaded on EVERY re-publish (not only changed-only): resend_all
	// needs it too, to count previously-published students this batch skips because
	// they are withdrawn now (roster-lifecycle plan 2026-07-10, semantics (d)).
	var prev map[int64][]byte
	if rePublish {
		if prev, err = s.latestSnapshotBytes(ctx, assessmentID); err != nil {
			return PublishResult{}, err
		}
	}

	// Withdrawn students are excluded from snaps at the query level (S0's
	// PublishSnapshotInputs filter), so a baseline student absent from the fresh
	// snapshot population is exactly a student this batch drops. Count the withdrawn
	// ones so resend-all/resend-failed flows report the skip instead of silently
	// shrinking (semantics (d)); the roster check keeps the count honest should a
	// student ever leave the population for any other reason.
	skippedWithdrawn := 0
	for sid := range prev {
		if _, ok := snaps[sid]; ok {
			continue
		}
		stu, err := s.store.Q.GetStudent(ctx, sid)
		if err != nil {
			return PublishResult{}, fmt.Errorf("publish: withdrawn check for student %d: %w", sid, err)
		}
		if stu.WithdrawnAt.Valid {
			skippedWithdrawn++
		}
	}

	emailDisabled := s.provider == "none"

	items := make([]store.CreatePublishItemInput, 0, len(snaps))
	pendingCount := 0
	skippedCount := 0
	for sid, snap := range snaps {
		b, err := canonicalJSON(snap)
		if err != nil {
			return PublishResult{}, err
		}
		// Changed-only: drop students whose snapshot is byte-identical to last batch.
		if selectChanged {
			if pb, ok := prev[sid]; ok && bytes.Equal(pb, b) {
				continue
			}
		}
		status := "pending"
		switch {
		case snap.AllNoSubmission:
			status = "skipped" // spec §2: no email for a student with nothing submitted
			skippedCount++
		case emailDisabled:
			status = "skipped" // spec §3: none provider records but never sends
			skippedCount++
		default:
			pendingCount++
		}
		items = append(items, store.CreatePublishItemInput{
			StudentID:      sid,
			Snapshot:       b,
			RecipientEmail: emails[sid],
			EmailStatus:    status,
			// RegradeToken is minted at send time from the item id and persisted then.
		})
	}

	if len(items) == 0 {
		// Nothing to send either way: a changed-only re-publish found no changed
		// students, OR (A5) there are zero eligible students at all — e.g. a first
		// publish (or resend-all) on an empty/fully-withdrawn roster, where the
		// coverage gate passes vacuously (no answers exist to be blocked). Refuse in
		// both cases rather than write an empty batch: an unconditional-selectChanged
		// guard here previously let the zero-eligible-students case fall through and
		// create a live 0-item batch that wedges the assessment behind
		// ErrAlreadyPublished until an admin finds Unpublish.
		return PublishResult{}, ErrNothingToPublish
	}
	if pendingCount > 0 && s.enqueueTx == nil {
		return PublishResult{}, ErrEmailQueueUnavailable
	}

	batch, created, err := s.store.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID:            assessmentID,
		Note:                    note,
		ResendAll:               resendAll,
		CreatedBy:               actorID,
		Attachment:              attachment,
		Zip:                     zip,
		Items:                   items,
		ExpectedFinalSourceKind: fs.Kind,
		ExpectedFinalMethodID:   fs.MethodID,
		ExpectedFinalRunID:      fs.RunID,
		EnqueuePending: func(ctx context.Context, tx pgx.Tx, pending []store.PublishItem) error {
			if len(pending) == 0 {
				return nil
			}
			refs := make([]DeliveryRef, 0, len(pending))
			for _, item := range pending {
				refs = append(refs, DeliveryRef{ItemID: item.ID, Generation: item.EmailGeneration})
			}
			return s.enqueueTx(ctx, tx, refs)
		},
	})
	if err != nil {
		// A concurrent publish won the race for the single live batch (migration-0021
		// unique index) — surface the same already-published 409 the pre-check produces.
		if errors.Is(err, store.ErrLiveBatchExists) {
			return PublishResult{}, ErrAlreadyPublished
		}
		if errors.Is(err, store.ErrFinalSourceChanged) {
			return PublishResult{}, ErrGradingStateChanged
		}
		// A9: coverage no longer held when CreatePublishBatch re-verified it inside
		// the assessment lock (an answer lost its official record, or a student
		// became not-ingested, after this call's own unlocked pv.Publishable()
		// check above) — same refresh-and-retry story as a final-source race.
		if errors.Is(err, store.ErrCoverageGateChanged) {
			return PublishResult{}, ErrGradingStateChanged
		}
		return PublishResult{}, err
	}

	res := PublishResult{
		BatchID:          batch.ID,
		ItemsCreated:     len(created),
		Enqueued:         pendingCount,
		Skipped:          skippedCount,
		SkippedWithdrawn: skippedWithdrawn,
		EmailDisabled:    emailDisabled,
	}
	if emailDisabled {
		// Loud publish-time warning (spec §3): counts only, no PII.
		res.Warning = fmt.Sprintf("email provider is 'none': %d item(s) recorded as skipped, no student email sent", skippedCount)
		s.log.Warn("publish with email provider none — no student email sent",
			"assessment_id", assessmentID, "batch_id", batch.ID, "skipped", skippedCount)
	}
	s.log.Info("published assessment",
		"assessment_id", assessmentID, "batch_id", batch.ID,
		"items", len(created), "enqueued", pendingCount, "skipped", skippedCount)
	return res, nil
}

// Unpublish is the D29 admin escape hatch: supersede the latest batch and clear
// published_at assessment-wide, re-opening grading (spec §2). Superseding a non-latest
// batch returns store.ErrNotLatestBatch (httpapi maps to 409). It does not un-send
// email.
func (s *Service) Unpublish(ctx context.Context, assessmentID int64, actorID int64) (int64, error) {
	latest, err := s.store.Q.LatestNonSupersededBatch(ctx, assessmentID)
	if err != nil {
		return 0, err
	}
	if err := s.store.SupersedePublishBatch(ctx, latest.ID, actorID); err != nil {
		return 0, err
	}
	s.log.Info("unpublished assessment", "assessment_id", assessmentID, "batch_id", latest.ID)
	return latest.ID, nil
}

// GetItemForResend loads one item's row (including its parent batch's attachment
// settings) for the resend endpoint's existence check + audit detail (spec §4 D46).
func (s *Service) GetItemForResend(ctx context.Context, itemID int64) (db.GetPublishItemForResendRow, error) {
	return s.store.GetPublishItemForResend(ctx, itemID)
}

// ResendItem starts ONE new, generation-scoped delivery. State rotation and River
// enqueue share a transaction, so an enqueue failure leaves the prior terminal state
// untouched. Active generations cannot be replaced; an uncertain provider outcome
// additionally requires explicit duplicate-risk acknowledgement.
func (s *Service) ResendItem(ctx context.Context, itemID int64, acknowledgeUncertain bool, actorID int64) error {
	if s.provider == "none" {
		return ErrEmailDisabled
	}
	if s.enqueueTx == nil {
		return ErrEmailQueueUnavailable
	}
	err := s.store.WithTxPgx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		batch, err := q.GetPublishBatchForItemForUpdate(ctx, itemID)
		if err != nil {
			return err
		}
		if batch.SupersededAt.Valid {
			return store.ErrPublishBatchSuperseded
		}

		item, err := q.GetPublishItemForResend(ctx, itemID)
		if err != nil {
			return err
		}
		student, err := q.GetStudent(ctx, item.StudentID)
		if err != nil {
			return fmt.Errorf("publish: resend item: load student: %w", err)
		}
		if student.WithdrawnAt.Valid {
			return ErrRecipientWithdrawn
		}
		if err := validateResendState(item.EmailStatus, acknowledgeUncertain); err != nil {
			return err
		}

		armed, ok, err := s.store.ArmPublishItemResendTx(ctx, q, itemID, item.EmailGeneration, acknowledgeUncertain)
		if err != nil {
			return fmt.Errorf("publish: resend item: arm delivery: %w", err)
		}
		if !ok {
			return ErrResendNotAllowed
		}
		if item.EmailStatus == "uncertain" {
			// The duplicate-risk acknowledgement is part of the same commit as the
			// generation rotation and River job. A crash after commit can therefore
			// never leave high-risk resend work without its durable operator record.
			detail, err := json.Marshal(map[string]any{
				"batch_id":                    item.BatchID,
				"from_generation":             item.EmailGeneration,
				"to_generation":               armed.EmailGeneration,
				"acknowledged_duplicate_risk": true,
				"previous_delivery_state":     "uncertain",
			})
			if err != nil {
				return fmt.Errorf("publish: encode duplicate-risk acknowledgement: %w", err)
			}
			if err := q.InsertAudit(ctx, db.InsertAuditParams{
				ActorUserID: pgtype.Int8{Int64: actorID, Valid: actorID != 0},
				Action:      "publish.resend_uncertain_ack",
				TargetKind:  "publish_item",
				TargetID:    strconv.FormatInt(itemID, 10),
				Detail:      detail,
			}); err != nil {
				return fmt.Errorf("publish: persist duplicate-risk acknowledgement: %w", err)
			}
		}
		ref := DeliveryRef{ItemID: armed.ID, Generation: armed.EmailGeneration}
		if err := s.enqueueTx(ctx, tx, []DeliveryRef{ref}); err != nil {
			return fmt.Errorf("publish: resend item: enqueue: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.log.Info("resend item", "item_id", itemID)
	return nil
}

// ResendFailed re-enqueues only the failed items of a batch (spec §7). It flips each
// failed item back to pending and enqueues a fresh send job; items that a concurrent
// send already moved on are skipped. A failed item whose student has since WITHDRAWN
// is skipped too — left failed, no send job, no email (roster-lifecycle plan
// 2026-07-10, locked semantics (d)) — and reported via skippedWithdrawn so the
// operator sees why the re-enqueued count is short; reinstating the student makes the
// same call pick the item up again. Returns (re-enqueued, skipped-withdrawn).
func (s *Service) ResendFailed(ctx context.Context, batchID int64) (int, int, error) {
	var refs []DeliveryRef
	skippedWithdrawn := 0
	err := s.store.WithTxPgx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		batch, err := q.GetPublishBatchForUpdate(ctx, batchID)
		if err != nil {
			return err
		}
		if batch.SupersededAt.Valid {
			return store.ErrPublishBatchSuperseded
		}
		failed, err := q.PublishItemsByStatus(ctx, db.PublishItemsByStatusParams{BatchID: batchID, EmailStatus: "failed"})
		if err != nil {
			return err
		}
		for _, item := range failed {
			student, err := q.GetStudent(ctx, item.StudentID)
			if err != nil {
				return fmt.Errorf("publish: resend-failed withdrawn check for student %d: %w", item.StudentID, err)
			}
			if student.WithdrawnAt.Valid {
				skippedWithdrawn++
				continue
			}
			armed, ok, err := s.store.ArmPublishItemResendTx(ctx, q, item.ID, item.EmailGeneration, false)
			if err != nil {
				return fmt.Errorf("publish: resend failed item %d: %w", item.ID, err)
			}
			if ok {
				refs = append(refs, DeliveryRef{ItemID: armed.ID, Generation: armed.EmailGeneration})
			}
		}
		if len(refs) == 0 {
			return nil
		}
		if s.provider == "none" {
			return ErrEmailDisabled
		}
		if s.enqueueTx == nil {
			return ErrEmailQueueUnavailable
		}
		if err := s.enqueueTx(ctx, tx, refs); err != nil {
			return fmt.Errorf("publish: resend enqueue: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	s.log.Info("resend-failed", "batch_id", batchID, "reenqueued", len(refs), "skipped_withdrawn", skippedWithdrawn)
	return len(refs), skippedWithdrawn, nil
}

func validateResendState(status string, acknowledgeUncertain bool) error {
	switch status {
	case "sent", "failed", "skipped":
		return nil
	case "uncertain":
		if !acknowledgeUncertain {
			return ErrUncertainNeedsAcknowledgement
		}
		return nil
	case "pending", "claimed", "sending":
		return ErrDeliveryInProgress
	default:
		return ErrResendNotAllowed
	}
}

// --- internals ----------------------------------------------------------------------

func (s *Service) assessmentName(ctx context.Context, assessmentID int64) (string, error) {
	a, err := s.store.Q.GetAssessment(ctx, assessmentID)
	if err != nil {
		return "", fmt.Errorf("publish: get assessment: %w", err)
	}
	return a.Name, nil
}

// latestSnapshotBytes returns the canonical snapshot bytes of each student's MOST
// RECENT published item across ALL of the assessment's batches (superseded or not),
// keyed by internal student id — the changed-only diff baseline (D30). It is
// per-student-across-batches, not "the newest batch": a changed-only re-publish writes
// a thin batch of only the changed students, so keying the baseline off the newest
// batch alone would make every absent student look changed and re-email the whole
// cohort on the next cycle (C1). Re-publish always follows an unpublish (the lock
// forces it), so the diff deliberately spans superseded batches too.
//
// The stored column is JSONB, which Postgres re-serializes with its own key order and
// spacing — NOT byte-identical to canonicalJSON's output. So each stored snapshot is
// round-tripped back through Snapshot + canonicalJSON here, normalizing it to the same
// canonical form the fresh snapshots use, so the diff is an apples-to-apples []byte
// compare.
func (s *Service) latestSnapshotBytes(ctx context.Context, assessmentID int64) (map[int64][]byte, error) {
	items, err := s.store.Q.LatestBatchItemsByStudentAny(ctx, assessmentID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]byte, len(items))
	for _, it := range items {
		var snap Snapshot
		if err := json.Unmarshal(it.Snapshot, &snap); err != nil {
			return nil, fmt.Errorf("publish: decode stored snapshot (student %d): %w", it.StudentID, err)
		}
		b, err := canonicalJSON(snap)
		if err != nil {
			return nil, err
		}
		out[it.StudentID] = b
	}
	return out, nil
}

// hasPriorBatch reports whether the assessment has ever been published (any batch,
// superseded or not) — a subsequent publish is then a re-publish and defaults to
// changed-only selection (D30).
func (s *Service) hasPriorBatch(ctx context.Context, assessmentID int64) (bool, error) {
	_, err := s.store.Q.LatestBatchAny(ctx, assessmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// hasLiveBatch reports whether a non-superseded batch currently exists for the
// assessment — the already-published guard (ErrAlreadyPublished). Distinct from
// hasPriorBatch (which asks "has this assessment EVER been published", superseded or
// not, to pick changed-only vs everyone): this asks "is there a batch live RIGHT NOW",
// which is exactly the single-live-batch invariant Publish must preserve.
func (s *Service) hasLiveBatch(ctx context.Context, assessmentID int64) (bool, error) {
	_, err := s.store.Q.LatestNonSupersededBatch(ctx, assessmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// LatestNonSupersededBatch exposes the store lookup for httpapi's read paths.
func (s *Service) LatestNonSupersededBatch(ctx context.Context, assessmentID int64) (db.PublishBatch, error) {
	return s.store.Q.LatestNonSupersededBatch(ctx, assessmentID)
}

// ListBatches / ListItems back the batches-history endpoint (spec §7).
func (s *Service) ListBatches(ctx context.Context, assessmentID int64) ([]db.PublishBatch, error) {
	return s.store.ListPublishBatches(ctx, assessmentID)
}

func (s *Service) ListItems(ctx context.Context, batchID int64) ([]db.ListPublishItemsRow, error) {
	return s.store.ListPublishItems(ctx, batchID)
}

// GetBatch fetches a batch header (for authorizing resend-failed by assessment).
func (s *Service) GetBatch(ctx context.Context, batchID int64) (db.PublishBatch, error) {
	return s.store.Q.GetPublishBatch(ctx, batchID)
}
