package publish

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/domain"
	"github.com/HaoWen46/adagrade/internal/email"
	"github.com/HaoWen46/adagrade/internal/report"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// errTerminalAttachmentBuild marks a buildAttachment failure that retrying can never
// fix (finding 2): font unconfigured, no blob store wired. SendItem checks
// errors.Is(err, errTerminalAttachmentBuild) to route these straight to a claimed
// failure instead of releasing for retry — everything else buildAttachment/
// fetchOriginalPages returns stays transient.
var errTerminalAttachmentBuild = errors.New("terminal: attachment build precondition")

// Sender is the send half of the pipeline (spec §3): the seam the River email_send
// job calls into per item. It is separate from Service so the queue worker depends on
// a small, send-only surface. PII rule: it logs only item ids and statuses — never the
// snapshot, recipient address, or body.
type Sender struct {
	store       *store.Store
	provider    domain.EmailProvider
	tokenKey    []byte        // HKDF subkey (secrets.Derive(master, "regrade-token-v1"))
	window      time.Duration // token validity window from send time (spec §4)
	replyDomain string        // regrade+<token>@<replyDomain>; empty ⇒ no Reply-To (replies not monitored)
	log         *slog.Logger
	now         func() time.Time // injectable clock for tests

	// blobs/reportFontPath are the report-attachment seam (spec §3, D42/D43-D45).
	// blobs resolves each problem's ORIGINAL (unmasked) page images by the same
	// image_ref the AnswerView streams (masking exists only for LLM calls —
	// students see their own work). reportFontPath is config.Config.ReportFontPath;
	// empty means the font isn't configured, so any item whose batch requested a
	// non-"none" attachment is a build-time bug the publish handler should have
	// caught at publish time (400) — buildAttachment treats it as terminal rather
	// than retrying forever.
	blobs          blobstore.Store
	reportFontPath string
	// typstBin, when non-empty, renders PDF attachments with Typst (LaTeX
	// math typeset via mitex — typst-report spec 2026-07-20); fpdf remains
	// the automatic fallback on any compile failure, so a Typst hiccup can
	// never fail a send.
	typstBin string
}

// NewSender constructs the send seam. provider must be non-nil (the none provider is
// handled at publish time — items are recorded skipped and never enqueued, so a send
// job for a none-provider item should not exist). blobs/reportFontPath may be zero
// (nil/"") for callers that never publish with attachments — SendItem only touches
// them when the item's batch attachment setting is not "none".
func NewSender(st *store.Store, provider domain.EmailProvider, tokenKey []byte, window time.Duration, replyDomain string, log *slog.Logger, blobs blobstore.Store, reportFontPath, typstBin string) *Sender {
	if log == nil {
		log = slog.Default()
	}
	return &Sender{
		store: st, provider: provider, tokenKey: tokenKey,
		window: window, replyDomain: replyDomain, log: log, now: time.Now,
		blobs: blobs, reportFontPath: reportFontPath, typstBin: typstBin,
	}
}

type emailShutdownCheckContextKey struct{}

// WithEmailShutdownCheck lets the queue distinguish a graceful shutdown drain
// from an ordinary per-job timeout after the context is cancelled. The predicate
// is intentionally evaluated at the failure site, not when the context is wrapped,
// because the queue's stopping flag changes dynamically during job execution.
func WithEmailShutdownCheck(ctx context.Context, isShuttingDown func() bool) context.Context {
	return context.WithValue(ctx, emailShutdownCheckContextKey{}, isShuttingDown)
}

func emailShutdownInProgress(ctx context.Context) bool {
	check, _ := ctx.Value(emailShutdownCheckContextKey{}).(func() bool)
	return check != nil && check()
}

// SendItem sends one immutable publish-item generation. The item/generation/job
// triple is claimed before local work, then moved to sending immediately before the
// provider call. A redelivery that finds its own attempt already sending must assume
// the prior process may have crossed the external side-effect boundary: it marks the
// item uncertain and never calls the provider again. isFirstAttempt (job.Attempt == 1,
// A2) lets a first-ever execution of this job rescue a migration-0036-backfilled
// legacy row stuck in uncertain — see the rescue block below.
func (s *Sender) SendItem(ctx context.Context, ref DeliveryRef, jobID int64, final bool, isFirstAttempt bool) error {
	attempt := store.PublishDeliveryAttempt{ItemID: ref.ItemID, Generation: ref.Generation, JobID: jobID}
	_, claimed, err := s.store.ClaimPublishItemDelivery(ctx, attempt)
	if err != nil {
		cause := fmt.Errorf("claim delivery: %w", err)
		if ctx.Err() != nil {
			return s.reconcileCancelledClaim(ctx, attempt, final, cause)
		}
		return cause
	}

	row, err := s.store.GetPublishItemForSend(ctx, ref.ItemID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) && !claimed {
			s.log.Info("send: item disappeared before claim, skipping", "item_id", ref.ItemID)
			return nil
		}
		cause := fmt.Errorf("load claimed item: %w", err)
		if claimed {
			return s.preProviderTransient(ctx, attempt, final, cause)
		}
		return cause
	}

	status, jobIDValid, durableJobID := row.EmailStatus, row.DeliveryJobID.Valid, row.DeliveryJobID.Int64
	if !claimed && isFirstAttempt && status == "uncertain" && !jobIDValid && row.EmailGeneration == attempt.Generation {
		// A2: this row's shape (uncertain + no delivery_job_id) can only be a
		// migration-0036-backfilled legacy row (see
		// ClaimLegacyUncertainPublishItemDelivery's doc) -- post-0036 code always sets
		// delivery_job_id via a claim CAS before any path that marks a row uncertain.
		// This job's first attempt proves it never invoked the provider before, so the
		// send provably never happened yet; rescue it exactly like a pending row
		// instead of leaving it stuck awaiting manual acknowledgment forever.
		if _, rescued, rescueErr := s.store.ClaimLegacyUncertainPublishItemDelivery(ctx, attempt); rescueErr != nil {
			return fmt.Errorf("rescue legacy uncertain delivery: %w", rescueErr)
		} else if rescued {
			s.log.Info("send: rescued legacy backfilled-uncertain row", "item_id", ref.ItemID)
			status, jobIDValid, durableJobID = "claimed", true, attempt.JobID
		}
	}

	proceed, err := s.resumeClaimedAttempt(ctx, status, row.EmailGeneration, jobIDValid, durableJobID, attempt)
	if err != nil || !proceed {
		return err
	}

	// Mint the regrade token from the item id + expiry, persist it (display/debug),
	// and build the Reply-To (spec §4). The grade email carries the turn-1 token of
	// the v2 single-use chain (regrade-v2 §4, D57): consuming it files the student's
	// first-turn request. When no reply domain is configured the email says replies
	// are not monitored (email.RenderGradeEmail handles the empty case).
	expiry := s.now().Add(s.window)
	token := email.MintTokenV2(s.tokenKey, ref.ItemID, 1, expiry)
	if err := s.store.SetPublishItemRegradeToken(ctx, ref.ItemID, token); err != nil {
		return s.preProviderTransient(ctx, attempt, final, fmt.Errorf("persist token: %w", err))
	}
	replyTo := ""
	if s.replyDomain != "" {
		replyTo = fmt.Sprintf("regrade+%s@%s", token, s.replyDomain)
	}

	var snap Snapshot
	if err := json.Unmarshal(row.Snapshot, &snap); err != nil {
		return s.preProviderTerminal(ctx, attempt, fmt.Errorf("decode snapshot: %w", err))
	}

	// The email advertises the earlier of token expiry and assessment deadline.
	a, err := s.store.Q.GetAssessment(ctx, row.AssessmentID)
	if err != nil {
		return s.preProviderTransient(ctx, attempt, final, fmt.Errorf("load assessment deadline: %w", err))
	}
	advertised := expiry
	if a.RegradeDeadline.Valid && a.RegradeDeadline.Time.Before(advertised) {
		advertised = a.RegradeDeadline.Time
	}

	data := s.gradeDataFromSnapshot(snap, row.StudentName, row.Corrected, replyTo, advertised)
	msg, err := email.RenderGradeEmail(data)
	if err != nil {
		return s.preProviderTerminal(ctx, attempt, fmt.Errorf("render email: %w", err))
	}
	msg.To = row.RecipientEmail
	if !row.DeliveryKey.Valid {
		return s.preProviderTerminal(ctx, attempt, errors.New("delivery key missing from publish item"))
	}
	msg.DeliveryKey = hex.EncodeToString(row.DeliveryKey.Bytes[:])

	// Build every attachment before crossing the provider boundary.
	warning := ""
	if row.Attachment != "" && row.Attachment != "none" {
		att, w, err := s.buildAttachment(ctx, row.AssessmentID, row.StudentID, snap, row.Attachment, row.Zip)
		if err != nil {
			if ctx.Err() != nil {
				return s.preProviderTransient(ctx, attempt, final, ctx.Err())
			}
			if errors.Is(err, errTerminalAttachmentBuild) {
				return s.preProviderTerminal(ctx, attempt, fmt.Errorf("build attachment: %w", err))
			}
			return s.preProviderTransient(ctx, attempt, final, fmt.Errorf("build attachment: %w", err))
		}
		msg.Attachments = []domain.Attachment{att}
		warning = w
	}

	// Serialize this transition with unpublish under the parent batch lock.
	_, begun, err := s.store.BeginPublishItemSending(ctx, attempt)
	if errors.Is(err, store.ErrPublishBatchSuperseded) {
		s.log.Info("send: batch superseded before provider boundary, skipping", "item_id", ref.ItemID, "batch_id", row.BatchID)
		return nil
	}
	if err != nil {
		return s.preProviderTransient(ctx, attempt, final, fmt.Errorf("begin provider send: %w", err))
	}
	if !begun {
		s.log.Info("send: delivery state changed before provider boundary, skipping", "item_id", ref.ItemID)
		return nil
	}

	providerID, err := s.provider.Send(ctx, msg)
	if err != nil {
		cause := fmt.Errorf("provider send: %w", err)
		postCtx := context.WithoutCancel(ctx)
		// A1: trust a stage-proven definitely-not-accepted classification even when
		// ctx has already expired. Every EmailProvider in this codebase classifies by
		// protocol stage (verified: SMTPProvider/PostmarkProvider/FileProvider) — a
		// pre-DATA/pre-acceptance failure (dial, handshake, envelope commands) is
		// definitely-not-accepted regardless of whether it was caused by ctx
		// cancellation mid-dial, because acceptance racing cancellation is provably
		// impossible at that stage. A post-acceptance/ambiguous-stage failure (e.g. a
		// lost write or response after DATA opened) must classify as outcome-unknown
		// instead, so this gate no longer needs to second-guess ctx state on top of
		// the classification: gating on ctx.Err() == nil quarantined every timed-out
		// send as "uncertain" and forced a manual per-item acknowledgment even for a
		// stage-proven rejection — one slow relay during a large publish became mass
		// manual work.
		if domain.IsEmailDefinitelyNotAccepted(err) {
			if final {
				return s.postProviderFailed(postCtx, attempt, cause)
			}
			released, releaseErr := s.store.ReleasePublishItemSending(postCtx, attempt)
			if releaseErr != nil {
				return errors.Join(cause, fmt.Errorf("release definitely rejected send: %w", releaseErr))
			}
			if !released {
				return errors.Join(cause, errors.New("release definitely rejected send: delivery state changed"))
			}
			return cause
		}
		if markErr := s.markProviderOutcomeUncertain(postCtx, attempt, providerID, cause); markErr != nil {
			return markErr
		}
		return nil
	}

	// A provider success followed by a DB error deliberately leaves sending. The
	// redelivered job will quarantine it rather than duplicate the external send.
	marked, err := s.store.MarkPublishItemDeliverySent(context.WithoutCancel(ctx), attempt, providerID, warning)
	if err != nil {
		return fmt.Errorf("mark delivery sent: %w", err)
	}
	if !marked {
		return errors.New("mark delivery sent: delivery state changed")
	}
	s.log.Info("send: item sent", "item_id", ref.ItemID, "warning", warning != "")
	return nil
}

// reconcileCancelledClaim handles cancellation racing the initial claim. A final
// ordinary timeout cannot simply return with a pending item because River will
// discard that job; claim durably without the cancelled context and record failed.
// A shutdown drain leaves/reverts the attempt pending for the queue's snooze path.
func (s *Sender) reconcileCancelledClaim(ctx context.Context, attempt store.PublishDeliveryAttempt, final bool, cause error) error {
	durableCtx := context.WithoutCancel(ctx)
	row, err := s.store.GetPublishItemForSend(durableCtx, attempt.ItemID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return errors.Join(cause, fmt.Errorf("inspect cancelled claim: %w", err))
	}
	if row.EmailGeneration != attempt.Generation {
		return nil
	}
	if row.DeliveryJobID.Valid {
		if row.DeliveryJobID.Int64 != attempt.JobID {
			return nil
		}
		switch row.EmailStatus {
		case "claimed":
			return s.preProviderTransient(ctx, attempt, final, cause)
		case "sending":
			if err := s.markProviderOutcomeUncertain(durableCtx, attempt, "", errors.New("delivery recovered in sending state; provider outcome unknown")); err != nil {
				return err
			}
			return nil
		default:
			return nil
		}
	}
	if row.EmailStatus != "pending" {
		return nil
	}
	if !final || emailShutdownInProgress(ctx) {
		return cause
	}
	if _, claimed, err := s.store.ClaimPublishItemDelivery(durableCtx, attempt); err != nil {
		return errors.Join(cause, fmt.Errorf("claim timed-out final delivery for failure: %w", err))
	} else if !claimed {
		return cause
	}
	return s.preProviderTerminal(durableCtx, attempt, cause)
}

func (s *Sender) resumeClaimedAttempt(ctx context.Context, status string, generation int32, jobIDValid bool, durableJobID int64, attempt store.PublishDeliveryAttempt) (bool, error) {
	if generation != attempt.Generation {
		s.log.Info("send: stale generation, skipping", "item_id", attempt.ItemID, "generation", attempt.Generation)
		return false, nil
	}
	if !jobIDValid || durableJobID != attempt.JobID {
		s.log.Info("send: delivery owned by another job or terminal, skipping", "item_id", attempt.ItemID, "status", status)
		return false, nil
	}
	switch status {
	case "claimed":
		return true, nil
	case "sending":
		cause := errors.New("delivery recovered in sending state; provider outcome unknown")
		if err := s.markProviderOutcomeUncertain(context.WithoutCancel(ctx), attempt, "", cause); err != nil {
			return false, err
		}
		s.log.Warn("send: recovered ambiguous sending attempt", "item_id", attempt.ItemID)
		return false, nil
	default:
		s.log.Info("send: delivery already terminal or no longer sendable, skipping", "item_id", attempt.ItemID, "status", status)
		return false, nil
	}
}

// WarningPrefix marks a non-terminal item warning stored in publish_items.error
// (spec §3 15MB guard) — distinguishing it from a real terminal-failure error string
// sharing the same column. Exported so httpapi's batches-list endpoint can derive a
// `warning` boolean for the UI without duplicating the prefix string.
const WarningPrefix = "warning: "

// resultFilenamePDF / resultFilenameZIP are ASCII-only constant attachment filenames
// (spec §3 plumbing note: "a review flagged non-ASCII filenames as un-encoded" — and
// CLAUDE.md's PII rule rules out the student's name in a filename regardless).
const (
	resultFilenamePDF = "results.pdf"
	resultFilenameZIP = "results.zip"
)

// preProviderTransient releases a safe claim for retry. On the final River attempt,
// an ordinary failure/timeout becomes failed because River will discard that job;
// only a verified shutdown drain releases for the queue's snooze-and-resume path.
func (s *Sender) preProviderTransient(ctx context.Context, attempt store.PublishDeliveryAttempt, final bool, cause error) error {
	shutdownDrain := ctx.Err() != nil && emailShutdownInProgress(ctx)
	if final && !shutdownDrain {
		return s.preProviderTerminal(ctx, attempt, cause)
	}
	released, err := s.store.ReleasePublishItemDeliveryClaim(context.WithoutCancel(ctx), attempt)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("release delivery claim: %w", err))
	}
	if !released {
		// Unpublish may have atomically moved claimed -> skipped. The original
		// cause still asks River to retry once; that retry observes terminal state.
		s.log.Info("send: pre-provider claim already changed", "item_id", attempt.ItemID)
	}
	return cause
}

func (s *Sender) preProviderTerminal(ctx context.Context, attempt store.PublishDeliveryAttempt, cause error) error {
	return s.markDeliveryFailed(context.WithoutCancel(ctx), attempt, "claimed", cause)
}

func (s *Sender) postProviderFailed(ctx context.Context, attempt store.PublishDeliveryAttempt, cause error) error {
	return s.markDeliveryFailed(context.WithoutCancel(ctx), attempt, "sending", cause)
}

// markDeliveryFailed records cause as the item's terminal failure. A10: it returns
// nil once the DB state is durably settled — whether this call itself wrote the
// 'failed' row (marked) or found the state had already moved on benignly (!marked,
// e.g. a concurrent skip/supersede) — because there is nothing left for River to
// retry either way. Only a genuine store WRITE failure (err != nil) still returns a
// joined error: that is the case the queue's emailFinalStateRetrySnooze exists to
// protect, by giving a later attempt another chance to durably record the failure.
// Before this fix, markDeliveryFailed always returned cause even on a fully
// successful write, so river.go's `final && err != nil` branch burned one wasted
// snooze-and-redeliver cycle per permanently-failed item.
func (s *Sender) markDeliveryFailed(ctx context.Context, attempt store.PublishDeliveryAttempt, expectedStatus string, cause error) error {
	marked, err := s.store.MarkPublishItemDeliveryFailed(ctx, attempt, expectedStatus, cause.Error())
	if err != nil {
		s.log.Error("send: mark failed also failed", "item_id", attempt.ItemID, "err", err)
		return errors.Join(cause, fmt.Errorf("mark delivery failed: %w", err))
	}
	if !marked {
		s.log.Info("send: delivery state changed before failure record", "item_id", attempt.ItemID)
		return nil
	}
	s.log.Warn("send: item failed terminally", "item_id", attempt.ItemID)
	return nil
}

func (s *Sender) markProviderOutcomeUncertain(ctx context.Context, attempt store.PublishDeliveryAttempt, providerID string, cause error) error {
	marked, err := s.store.MarkPublishItemDeliveryUncertain(ctx, attempt, providerID, cause.Error())
	if err != nil {
		return errors.Join(cause, fmt.Errorf("mark delivery uncertain: %w", err))
	}
	if !marked {
		return errors.Join(cause, errors.New("mark delivery uncertain: delivery state changed"))
	}
	return nil
}

// gradeDataFromSnapshot reconstructs the email data from the decoded snapshot — the
// send job works purely from the snapshot, so a later grade change never alters an
// already-published email's content.
func (s *Sender) gradeDataFromSnapshot(snap Snapshot, studentName string, corrected bool, replyTo string, advertisedDeadline time.Time) email.GradeEmailData {
	problems := make([]email.ProblemBreakdown, 0, len(snap.Problems))
	for _, p := range snap.Problems {
		crit := make([]email.CriterionLine, 0, len(p.Criteria))
		for _, c := range p.Criteria {
			crit = append(crit, email.CriterionLine{Name: c.Name, Score: c.Score, Max: c.Max, Comment: ""})
		}
		problems = append(problems, email.ProblemBreakdown{
			Label: fmt.Sprintf("Problem %d: %s", p.Number, p.Title), Criteria: crit, Comment: p.Comment,
		})
	}
	// Prefer the snapshot's own student name (captured at publish time); fall back to
	// the live roster name only if the snapshot somehow lacks one.
	name := snap.StudentName
	if name == "" {
		name = studentName
	}
	return email.GradeEmailData{
		AssessmentName:  snap.AssessmentName,
		StudentName:     name,
		Problems:        problems,
		Total:           snap.Total,
		Max:             snap.Max,
		ReplyTo:         replyTo,
		RegradeDeadline: advertisedDeadline,
		Corrected:       corrected,
		FormatTemplate:  email.RegradeReplyFormatTemplate,
	}
}

// buildAttachment builds the per-student result PDF or ZIP (spec §3, D42/D44/D45): for
// each snapshot problem it resolves that problem's answer id LIVE from
// (assessmentID, studentID, problem number) — the snapshot itself no longer carries an
// answer_id (finding 1) — then fetches that answer's ORIGINAL (unmasked) page images
// from blobs (the same image_ref the AnswerView streams; masking exists only for LLM
// calls, spec §3: "students see their own work") and hands them to the report package.
// It returns the built attachment, a non-empty warning string when the built content
// exceeds attachmentSizeLimit (spec §3's 15MB per-item guard — send still proceeds),
// or an error for a build failure. The error is errors.Is-wrapped with
// errTerminalAttachmentBuild for the two "retrying can never succeed" preconditions
// (font unconfigured, no blob store wired, finding 2); a blob read failure or a
// report-build error is returned unwrapped and stays transient (retryable) as before.
func (s *Sender) buildAttachment(ctx context.Context, assessmentID, studentID int64, snap Snapshot, quality string, zip bool) (domain.Attachment, string, error) {
	if s.reportFontPath == "" {
		// The publish handler is expected to 400 a non-"none" attachment request when
		// the font isn't configured, so reaching here means a batch was created before
		// the font was unconfigured (or a caller bypassed the guard) — terminal, not
		// retryable: retrying won't make the font appear.
		return domain.Attachment{}, "", fmt.Errorf("%w: report attachment requested but ADAMARKER_REPORT_FONT is not configured", errTerminalAttachmentBuild)
	}
	if s.blobs == nil {
		return domain.Attachment{}, "", fmt.Errorf("%w: report attachment requested but no blob store is wired", errTerminalAttachmentBuild)
	}

	in := report.ReportInput{
		AssessmentName: snap.AssessmentName,
		StudentName:    snap.StudentName,
		StudentID:      snap.StudentExternalID,
		Quality:        quality,
		Total:          snap.Total,
		Max:            snap.Max,
	}
	for _, p := range snap.Problems {
		if p.NoSubmission {
			continue // nothing to attach for a problem the student never submitted
		}
		answerID, err := s.resolveAnswerID(ctx, assessmentID, studentID, p.Number)
		if err != nil {
			return domain.Attachment{}, "", fmt.Errorf("resolve answer id for problem %d: %w", p.Number, err)
		}
		if answerID == 0 {
			continue // answer never materialized for this (student, problem) — nothing to attach
		}
		pages, err := s.fetchOriginalPages(ctx, answerID)
		if err != nil {
			return domain.Attachment{}, "", fmt.Errorf("fetch pages for answer %d: %w", answerID, err)
		}
		if len(pages) == 0 {
			continue
		}
		crit := make([]report.CriterionLine, 0, len(p.Criteria))
		for _, c := range p.Criteria {
			crit = append(crit, report.CriterionLine{Name: c.Name, Score: c.Score, Max: c.Max})
		}
		in.Problems = append(in.Problems, report.ProblemReport{
			Label:    fmt.Sprintf("Problem %d: %s", p.Number, p.Title),
			Pages:    pages,
			Criteria: crit,
			Total:    p.Total,
			Max:      p.Max,
			// Same problem-level note the email body already discloses
			// (gradeDataFromSnapshot); per-criterion rationales stay out of
			// student output on both surfaces.
			Comment: p.Comment,
		})
	}

	var content []byte
	var err error
	filename := resultFilenamePDF
	mime := "application/pdf"
	if zip {
		filename, mime = resultFilenameZIP, "application/zip"
		content, err = report.BuildZIP(in)
	} else if s.typstBin != "" {
		// Typst renderer (typst-report spec 2026-07-20): LaTeX math in
		// comments typesets via mitex. --font-path gets the report-fonts dir
		// so Noto Sans TC resolves without OS installation. Any failure falls
		// back to fpdf — a Typst hiccup must never fail a send. The logged
		// error is PII-safe by construction (BuildTypst suppresses stderr).
		content, err = report.BuildTypst(ctx, s.typstBin, filepath.Dir(s.reportFontPath), in)
		if err != nil {
			s.log.Warn("typst report failed; falling back to fpdf", "err", err)
			content, err = report.Build(s.reportFontPath, in)
		}
	} else {
		content, err = report.Build(s.reportFontPath, in)
	}
	if err != nil {
		return domain.Attachment{}, "", fmt.Errorf("report build: %w", err)
	}

	warning := ""
	if report.ExceedsSizeGuard(content) {
		warning = fmt.Sprintf("%sattachment is %d bytes, over the report package's size guideline", WarningPrefix, len(content))
	}
	return domain.Attachment{Filename: filename, MIME: mime, Content: content}, warning, nil
}

// resolveAnswerID looks up the CURRENT answer id for (assessmentID, studentID,
// problemNumber), LIVE (finding 1): the persisted snapshot no longer carries an
// answer_id, since doing so broke the changed-only republish diff for snapshots
// stored before that field existed. answers has a natural unique key
// (assessment_id, student_id, problem_id) and rows are pre-materialized at ingest, so
// this resolution is stable and deterministic across send/resend — the same
// (student, problem) always maps to the same answer row. Zero (no error) means the
// answer was never materialized for this (student, problem) — the caller treats that
// like an empty attachment section, same as the old answerID==0 sentinel.
func (s *Sender) resolveAnswerID(ctx context.Context, assessmentID, studentID int64, problemNumber int32) (int64, error) {
	id, err := s.store.Q.AnswerIDForStudentProblem(ctx, db.AnswerIDForStudentProblemParams{
		AssessmentID: assessmentID, StudentID: studentID, Number: problemNumber,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// fetchOriginalPages loads one answer's page images in order, ORIGINAL (unmasked) —
// masking exists only for LLM calls; students see their own work (spec §3). A
// missing blob is surfaced as an error (retryable — see buildAttachment's caller),
// never silently skipped, so a storage hiccup shows up as a retried job rather than a
// quietly incomplete PDF.
func (s *Sender) fetchOriginalPages(ctx context.Context, answerID int64) ([][]byte, error) {
	if answerID == 0 {
		return nil, nil
	}
	rows, err := s.store.Q.ListAnswerPages(ctx, answerID)
	if err != nil {
		return nil, err
	}
	pages := make([][]byte, 0, len(rows))
	for _, r := range rows {
		rc, err := s.blobs.Get(ctx, r.ImageRef)
		if err != nil {
			return nil, fmt.Errorf("get blob %q: %w", r.ImageRef, err)
		}
		b, err := readAllAndClose(rc)
		if err != nil {
			return nil, fmt.Errorf("read blob %q: %w", r.ImageRef, err)
		}
		pages = append(pages, b)
	}
	return pages, nil
}

// readAllAndClose drains and closes a blob reader in one step — every
// fetchOriginalPages read is small (one page image), so no separate size cap is
// needed here (blobstore.Get already streams from local disk, not an untrusted
// remote).
func readAllAndClose(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(rc)
}
