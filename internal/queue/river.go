// Package queue wraps River (spec §2/§6): durable async run execution on the same
// Postgres with transactional enqueue. Grading leaves share one "llm" queue;
// per-provider throttling is the provider source's rate limiters (D11 v1).
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"golang.org/x/time/rate"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/publish"
	"github.com/HaoWen46/adagrade/internal/scan"
)

// leafMaxAttempts bounds provider/transient retries per leaf (D12).
const leafMaxAttempts = 3

// scanExpandMaxAttempts bounds retries for the scan.expand job (design spec §5).
const scanExpandMaxAttempts = 5

// scanSplitMaxAttempts / scanRenderMaxAttempts / scanIdentifyMaxAttempts /
// scanPromoteMaxAttempts bound retries for the page-level scan-intake jobs
// (design spec 2026-07-04, Task 9): split, render_pages, identify_page, and
// promote_page each get the same transient-retry budget as a grading leaf.
const (
	scanSplitMaxAttempts    = 3
	scanRenderMaxAttempts   = 3
	scanIdentifyMaxAttempts = 3
	scanPromoteMaxAttempts  = 3
)

// maskPageMaxAttempts / directIngestMaxAttempts bound retries for the D27
// async batch-op jobs (audit finding F1/F2): per-page mask and per-file
// direct-upload ingest.
const (
	maskPageMaxAttempts     = 3
	directIngestMaxAttempts = 3
)

// emailSendMaxAttempts bounds retries for one grade-email send (spec §3). River's
// backoff spaces the attempts; a final-attempt failure marks the item failed.
const emailSendMaxAttempts = 5

// A final Sender error may mean its terminal DB transition failed after the provider
// boundary. River must not discard that job and strand the item in `sending`; snooze
// the same attempt until it durably reaches/observes a terminal state.
const emailFinalStateRetrySnooze = 10 * time.Second

// emailLimiterCtxDoneSnooze is the delay used when limiter.Wait fails because the job's
// context is already done (shutdown drain or an ordinary final job timeout, A8). It must
// be nonzero: JobSnooze(0) makes the job immediately available, and a context that keeps
// arriving already-done (a misconfigured/expired deadline) would otherwise spin the
// scheduler in a zero-delay hot loop instead of merely re-running once work resumes.
const emailLimiterCtxDoneSnooze = time.Second

// llmQueue is the single shared queue for grading leaves and scan identify jobs
// (shares provider rate limiting); scanQueue is the CPU/IO-bound expand+render
// queue (design spec §5).
const (
	llmQueue  = "llm"
	scanQueue = "scan"
	// emailQueue is the dedicated outbound grade-email queue (spec §3): one worker
	// paced by a shared rate limiter (default 1/s) so a large publish doesn't burst
	// past a provider's throughput limit.
	emailQueue = "email"
)

// PlanRunArgs plans one run into leaves.
type PlanRunArgs struct {
	RunID int64 `json:"run_id"` // IDs only in job args (D14)
}

func (PlanRunArgs) Kind() string { return "run.plan" }

func (PlanRunArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "control", MaxAttempts: 5}
}

// GradeLeafArgs grades one (answer, model) item.
type GradeLeafArgs struct {
	ItemID   int64  `json:"item_id"`
	Provider string `json:"provider"`
}

func (GradeLeafArgs) Kind() string { return "run.leaf" }

// regradeAIMaxAttempts bounds provider/transient retries for one AI re-grade job
// (spec §8) — same budget as a grading leaf: a single vision call + re-asks.
const regradeAIMaxAttempts = 3

// RegradeAIArgs runs the stricter AI re-grade for one regrade SUB-ITEM — the (request,
// problem) pair (spec §8, per-sub-item re-scope). IDs only (D14): the worker re-resolves
// the contested answer and re-redacts THIS problem's complaint text at execution time,
// so no student content is ever serialized into the durable job args. Shares the "llm"
// queue (and its provider rate limiters) with grading leaves.
type RegradeAIArgs struct {
	SubItemID int64 `json:"sub_item_id"`
}

func (RegradeAIArgs) Kind() string { return "regrade.ai" }

// ScanExpandArgs unzips one batch's stored zip into scan_sources rows (D14: IDs only).
type ScanExpandArgs struct {
	BatchID int64 `json:"batch_id"`
}

func (ScanExpandArgs) Kind() string { return "scan.expand" }

func (ScanExpandArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: scanQueue, MaxAttempts: scanExpandMaxAttempts}
}

// ScanSplitArgs splits one uploaded source into scan_pages rows (D14: IDs only).
type ScanSplitArgs struct {
	SourceID int64 `json:"source_id"`
}

func (ScanSplitArgs) Kind() string { return "scan.split" }

func (ScanSplitArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: scanQueue, MaxAttempts: scanSplitMaxAttempts}
}

// ScanRenderPagesArgs renders one chunk of a source's pages (one PDFium
// document open per chunk).
type ScanRenderPagesArgs struct {
	SourceID int64   `json:"source_id"`
	PageIDs  []int64 `json:"page_ids"`
}

func (ScanRenderPagesArgs) Kind() string { return "scan.render_pages" }

func (ScanRenderPagesArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: scanQueue, MaxAttempts: scanRenderMaxAttempts}
}

// ScanIdentifyPageArgs OCRs one page's three crops (rides the llm queue —
// shares provider rate limiting).
type ScanIdentifyPageArgs struct {
	PageID int64 `json:"page_id"`
}

func (ScanIdentifyPageArgs) Kind() string { return "scan.identify_page" }

func (ScanIdentifyPageArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: llmQueue, MaxAttempts: scanIdentifyMaxAttempts}
}

// ScanPromotePageArgs promotes one assigned page at finalize.
type ScanPromotePageArgs struct {
	PageID int64 `json:"page_id"`
	Force  bool  `json:"force"`
	Actor  int64 `json:"actor"`
}

func (ScanPromotePageArgs) Kind() string { return "scan.promote_page" }

func (ScanPromotePageArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: scanQueue, MaxAttempts: scanPromoteMaxAttempts}
}

// MaskPageArgs (re-)masks one answer page (D27, F2: IDs only per D14).
type MaskPageArgs struct {
	PageID int64 `json:"page_id"`
}

func (MaskPageArgs) Kind() string { return "mask.page" }

func (MaskPageArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: scanQueue, MaxAttempts: maskPageMaxAttempts}
}

// DirectIngestArgs runs the ingest pipeline for one staged bulk direct upload (D27,
// F1: IDs only per D14).
type DirectIngestArgs struct {
	UploadID int64 `json:"upload_id"`
}

func (DirectIngestArgs) Kind() string { return "ingest.direct" }

func (DirectIngestArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: scanQueue, MaxAttempts: directIngestMaxAttempts}
}

// EmailSendArgs sends one generation of a publish_item's grade email (spec §3,
// D14: IDs only). Generation advances only for an explicit resend; River retries
// retain it, letting the sender reject stale jobs without suppressing new work.
type EmailSendArgs publish.DeliveryRef

func (EmailSendArgs) Kind() string { return "email.send" }

func (EmailSendArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       emailQueue,
		MaxAttempts: emailSendMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: emailSendUniqueStates,
		},
	}
}

// emailSendUniqueStates scopes one delivery generation's uniqueness to jobs
// still capable of running. Terminal jobs are deliberately excluded: a
// recovery tool may safely recreate missing work for the same generation, while
// normal explicit resends advance Generation and are always distinct args.
var emailSendUniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
	rivertype.JobStateRetryable,
}

// Migrate applies River's own schema migrations (called at startup next to goose).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("queue: migrator: %w", err)
	}
	_, err = migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return fmt.Errorf("queue: migrate: %w", err)
	}
	return nil
}

// hardStopGrace is the short window Shutdown gives a HARD stop (StopAndCancel) to
// let jobs observe their cancelled context and exit after the drain window elapses
// (F17). Jobs that don't cooperate are abandoned in 'running' and the rescuer
// re-queues them after restart.
const hardStopGrace = 5 * time.Second

// escalationSoftStopTimeout backstops Shutdown's manual Stop→StopAndCancel
// orchestration (F17). River escalates a soft stop to a hard cancel on its own
// after this timeout even when the work context is detached from the start
// context; we size it well beyond the drain window so Shutdown's own explicit
// escalation is what fires first, and this only ever matters if Shutdown isn't
// called (e.g. a future direct Start-ctx cancel).
const escalationSoftStopTimeout = 24 * time.Hour

// rescueStuckJobsAfter backstops the JobSnooze shutdown path (F17). A job that is
// SIGKILLed after the completer would have flushed its snooze is left in 'running';
// the leader's rescuer re-queues it (if attempts remain) once this much time has
// passed after restart. Sized above the 5m longest job timeout so a legitimately
// long-running job is never rescued out from under itself.
const rescueStuckJobsAfter = 10 * time.Minute

// Client owns the River client and exposes typed enqueue helpers.
type Client struct {
	river *river.Client[pgx.Tx]

	// stopping is set by Shutdown and read by the workers: a context.Canceled
	// observed while stopping is a graceful-shutdown interruption, so the worker
	// returns JobSnooze(0) (does not consume an attempt; re-runs on next start)
	// instead of letting River discard the final attempt (F17).
	stopping atomic.Bool
}

// isShutdownCancel reports whether err is a context cancellation observed during a
// graceful shutdown — the signal for a worker to snooze rather than error the job
// (F17). It requires BOTH the stopping flag (so a plain per-job timeout mid-run is
// still a real error) AND a context.Canceled/DeadlineExceeded in the error chain.
func (c *Client) isShutdownCancel(err error) bool {
	if err == nil || !c.stopping.Load() {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// snoozeOnShutdown converts a worker body's error into JobSnooze(0) when it was a
// graceful-shutdown cancellation (F17). Every worker routes its result through this
// so an interrupted job re-runs on the next start instead of burning an attempt
// (or, on a final attempt, being discarded). All other errors pass through so the
// normal retry/terminal taxonomy is untouched.
func (c *Client) snoozeOnShutdown(err error) error {
	if c.isShutdownCancel(err) {
		return river.JobSnooze(0)
	}
	return err
}

type planWorker struct {
	river.WorkerDefaults[PlanRunArgs]
	client *Client
	runner *grading.Runner
}

func (w *planWorker) Work(ctx context.Context, job *river.Job[PlanRunArgs]) error {
	return w.client.snoozeOnShutdown(w.runner.Plan(ctx, job.Args.RunID))
}

type leafWorker struct {
	river.WorkerDefaults[GradeLeafArgs]
	client *Client
	runner *grading.Runner
}

func (w *leafWorker) Work(ctx context.Context, job *river.Job[GradeLeafArgs]) error {
	final := job.Attempt >= leafMaxAttempts
	return w.client.snoozeOnShutdown(w.runner.ExecuteLeaf(ctx, job.Args.ItemID, final))
}

func (w *leafWorker) Timeout(*river.Job[GradeLeafArgs]) time.Duration {
	return 5 * time.Minute // one vision call + re-asks
}

type regradeAIWorker struct {
	river.WorkerDefaults[RegradeAIArgs]
	client *Client
	runner *grading.Runner
}

// Work runs one AI re-grade (spec §8). A non-terminal error is left to River's backoff
// (retryableError from a provider blip), a terminal one (ErrNoContestedRecord, malformed
// past re-ask cap, provider removed) is returned so the attempt is consumed; on the
// final attempt River gives up and the request simply keeps no AI record — never
// auto-official, so a failed re-grade is inert. F17: a shutdown-drain cancellation is
// turned into JobSnooze(0) by snoozeOnShutdown so the job re-runs on the next start
// rather than burning its final attempt.
func (w *regradeAIWorker) Work(ctx context.Context, job *river.Job[RegradeAIArgs]) error {
	return w.client.snoozeOnShutdown(w.runner.RegradeAssistForSubItem(ctx, job.Args.SubItemID))
}

func (w *regradeAIWorker) Timeout(*river.Job[RegradeAIArgs]) time.Duration {
	return 5 * time.Minute // one vision call + re-asks
}

type expandWorker struct {
	river.WorkerDefaults[ScanExpandArgs]
	client *Client
	scans  *scan.Service
}

func (w *expandWorker) Work(ctx context.Context, job *river.Job[ScanExpandArgs]) error {
	return w.client.snoozeOnShutdown(w.scans.Expand(ctx, job.Args.BatchID))
}

type splitWorker struct {
	river.WorkerDefaults[ScanSplitArgs]
	client *Client
	scans  *scan.Service
}

func (w *splitWorker) Work(ctx context.Context, job *river.Job[ScanSplitArgs]) error {
	return w.client.snoozeOnShutdown(w.scans.SplitSource(ctx, job.Args.SourceID))
}

func (w *splitWorker) Timeout(*river.Job[ScanSplitArgs]) time.Duration {
	return 5 * time.Minute
}

type renderPagesWorker struct {
	river.WorkerDefaults[ScanRenderPagesArgs]
	client *Client
	scans  *scan.Service
}

func (w *renderPagesWorker) Work(ctx context.Context, job *river.Job[ScanRenderPagesArgs]) error {
	return w.client.snoozeOnShutdown(w.scans.RenderPages(ctx, job.Args.SourceID, job.Args.PageIDs))
}

func (w *renderPagesWorker) Timeout(*river.Job[ScanRenderPagesArgs]) time.Duration {
	return 10 * time.Minute
}

type identifyPageWorker struct {
	river.WorkerDefaults[ScanIdentifyPageArgs]
	client *Client
	scans  *scan.Service
}

func (w *identifyPageWorker) Work(ctx context.Context, job *river.Job[ScanIdentifyPageArgs]) error {
	final := job.Attempt >= scanIdentifyMaxAttempts
	return w.client.snoozeOnShutdown(w.scans.IdentifyPage(ctx, job.Args.PageID, final))
}

func (w *identifyPageWorker) Timeout(*river.Job[ScanIdentifyPageArgs]) time.Duration {
	return 5 * time.Minute
}

type promotePageWorker struct {
	river.WorkerDefaults[ScanPromotePageArgs]
	client *Client
	scans  *scan.Service
}

func (w *promotePageWorker) Work(ctx context.Context, job *river.Job[ScanPromotePageArgs]) error {
	final := job.Attempt >= scanPromoteMaxAttempts
	return w.client.snoozeOnShutdown(w.scans.PromotePage(ctx, job.Args.PageID, job.Args.Force, job.Args.Actor, final))
}

func (w *promotePageWorker) Timeout(*river.Job[ScanPromotePageArgs]) time.Duration {
	return 5 * time.Minute
}

type maskPageWorker struct {
	river.WorkerDefaults[MaskPageArgs]
	client *Client
	ingest *ingest.Service
}

func (w *maskPageWorker) Work(ctx context.Context, job *river.Job[MaskPageArgs]) error {
	final := job.Attempt >= maskPageMaxAttempts
	return w.client.snoozeOnShutdown(w.ingest.MaskPage(ctx, job.Args.PageID, final))
}

func (w *maskPageWorker) Timeout(*river.Job[MaskPageArgs]) time.Duration {
	return 2 * time.Minute
}

type directIngestWorker struct {
	river.WorkerDefaults[DirectIngestArgs]
	client *Client
	ingest *ingest.Service
}

func (w *directIngestWorker) Work(ctx context.Context, job *river.Job[DirectIngestArgs]) error {
	final := job.Attempt >= directIngestMaxAttempts
	return w.client.snoozeOnShutdown(w.ingest.IngestDirectUpload(ctx, job.Args.UploadID, final))
}

func (w *directIngestWorker) Timeout(*river.Job[DirectIngestArgs]) time.Duration {
	return 5 * time.Minute
}

type emailSendWorker struct {
	river.WorkerDefaults[EmailSendArgs]
	client  *Client
	sender  EmailSender
	limiter *rate.Limiter
}

// EmailSender is the queue's narrow delivery seam. The River job ID identifies
// the worker claim, while DeliveryRef identifies the publish item generation;
// together they let the sender compare-and-set ownership before any provider IO.
// isFirstAttempt (job.Attempt == 1) lets the sender distinguish a job that has never
// once invoked the provider from a redelivery — the discriminator the A2
// legacy-uncertain-row rescue needs (migration 0036 backfilled some pending rows to
// uncertain with delivery_job_id left NULL; a first-attempt job arriving for that
// exact shape provably never reached the provider, so it can be rescued like a
// pending row instead of being stuck awaiting manual acknowledgment forever).
type EmailSender interface {
	SendItem(ctx context.Context, ref publish.DeliveryRef, jobID int64, final bool, isFirstAttempt bool) error
}

// Work paces each send by the shared rate limiter (spec §3, default 1/s) then hands
// off to the publish Sender. F17: a shutdown-drain cancellation observed while waiting
// on the limiter is snoozed (see the ctx.Err() branch below, A8); one observed inside
// SendItem is turned into JobSnooze(0) by isShutdownCancel. Either way the item stays
// pending and is re-sent on the next start — never marked failed.
func (w *emailSendWorker) Work(ctx context.Context, job *river.Job[EmailSendArgs]) error {
	if err := w.limiter.Wait(ctx); err != nil {
		// rate.Limiter.Wait fails for two distinct reasons (A8): the context is
		// already done (shutdown drain, or an ordinary final job timeout — no item
		// has been claimed and no provider call can have happened), or the wait is
		// impossible to ever satisfy given the limiter's burst/rate configuration
		// (a live context). Only the former is safe to snooze forever: folding both
		// into an unconditional JobSnooze(0) let a misconfigured limiter spin at
		// zero delay forever, bypassing emailSendMaxAttempts invisibly.
		if ctx.Err() != nil {
			return river.JobSnooze(emailLimiterCtxDoneSnooze)
		}
		// A real, recurring problem (e.g. burst 0) — let River apply its normal
		// attempt-consuming retry/backoff instead of snoozing forever.
		return err
	}
	final := job.Attempt >= emailSendMaxAttempts
	ref := publish.DeliveryRef(job.Args)
	// Sender must distinguish a final ordinary job timeout (terminal) from a final
	// shutdown cancellation (safe to release + snooze). Pass a dynamic predicate:
	// Stop may begin after Work enters SendItem.
	sendCtx := publish.WithEmailShutdownCheck(ctx, func() bool { return w.client.stopping.Load() })
	err := w.sender.SendItem(sendCtx, ref, job.ID, final, job.Attempt == 1)
	if w.client.isShutdownCancel(err) {
		return river.JobSnooze(0)
	}
	if final && err != nil {
		return river.JobSnooze(emailFinalStateRetrySnooze)
	}
	return err
}

func (w *emailSendWorker) Timeout(*river.Job[EmailSendArgs]) time.Duration {
	return 2 * time.Minute // one SMTP/HTTP send + retries
}

// Deps supplies the service seams New's workers dispatch into. Scans/Ingest/Email may
// be nil (API-only processes, or tests that only exercise grading) — the corresponding
// workers and enqueue closures are only registered/injected when set.
type Deps struct {
	Runner *grading.Runner
	Scans  *scan.Service
	Ingest *ingest.Service
	// Email is the publish send seam (spec §3). When set, the "email" queue and its
	// rate-limited worker are registered. EmailRate is sends/sec (default 1/s applied
	// when <= 0).
	Email     EmailSender
	EmailRate float64
}

// defaultEmailRate is the outbound-email pacing when Deps.EmailRate is unset (spec §3).
const defaultEmailRate = 1.0

// New builds the client with the control queue, the shared "llm" queue, and — when
// deps.Scans is set — the "scan" queue for the scan-intake pipeline (design spec §5).
// Providers are dynamic rows now (D11 v1), so the queue set can't depend on them;
// per-provider fairness comes from the rate limiters inside the worker. Call Start
// to work jobs; an unstarted client can still insert (tests, API-only processes).
func New(pool *pgxpool.Pool, deps Deps, logger *slog.Logger) (*Client, error) {
	// The client is allocated up front so each worker can hold a back-reference to
	// it (for snoozeOnShutdown, F17); c.river is filled in below once NewClient has
	// built the underlying River client from the worker set.
	c := &Client{}

	workers := river.NewWorkers()
	river.AddWorker(workers, &planWorker{client: c, runner: deps.Runner})
	river.AddWorker(workers, &leafWorker{client: c, runner: deps.Runner})
	river.AddWorker(workers, &regradeAIWorker{client: c, runner: deps.Runner})

	queues := map[string]river.QueueConfig{
		"control": {MaxWorkers: 2},
		llmQueue:  {MaxWorkers: 4},
	}

	if deps.Scans != nil {
		river.AddWorker(workers, &expandWorker{client: c, scans: deps.Scans})
		river.AddWorker(workers, &splitWorker{client: c, scans: deps.Scans})
		river.AddWorker(workers, &renderPagesWorker{client: c, scans: deps.Scans})
		river.AddWorker(workers, &identifyPageWorker{client: c, scans: deps.Scans})
		river.AddWorker(workers, &promotePageWorker{client: c, scans: deps.Scans})
	}
	if deps.Ingest != nil {
		river.AddWorker(workers, &maskPageWorker{client: c, ingest: deps.Ingest})
		river.AddWorker(workers, &directIngestWorker{client: c, ingest: deps.Ingest})
	}
	// The scan queue carries both the scan-intake jobs and the ingest-side mask/
	// direct-ingest jobs (D27, F4): register it once when either service is wired,
	// rather than assigning the same config twice.
	if deps.Scans != nil || deps.Ingest != nil {
		queues[scanQueue] = river.QueueConfig{MaxWorkers: 2}
	}
	if deps.Email != nil {
		emailRate := deps.EmailRate
		if emailRate <= 0 {
			emailRate = defaultEmailRate
		}
		// A shared token-bucket limiter paces sends across the single worker; burst 1
		// keeps it a strict rate rather than an initial flood (spec §3).
		limiter := rate.NewLimiter(rate.Limit(emailRate), 1)
		river.AddWorker(workers, &emailSendWorker{client: c, sender: deps.Email, limiter: limiter})
		// One worker + the limiter enforce the send rate; more workers would race the
		// limiter without raising throughput past the configured rate.
		queues[emailQueue] = river.QueueConfig{MaxWorkers: 1}
	}

	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  queues,
		Workers: workers,
		Logger:  logger,
		// F17 backstops. SoftStopTimeout detaches the work context from the Start
		// context so a Start-ctx cancel is a soft stop, not a hard one — belt to
		// Shutdown's braces; sized huge so Shutdown's explicit drain window is the
		// effective one. RescueStuckJobsAfter re-queues jobs left 'running' by a
		// SIGKILL before their snooze flushed.
		SoftStopTimeout:      escalationSoftStopTimeout,
		RescueStuckJobsAfter: rescueStuckJobsAfter,
	})
	if err != nil {
		return nil, fmt.Errorf("queue: client: %w", err)
	}
	c.river = rc

	deps.Runner.EnqueueLeaves = c.EnqueueLeavesTx
	// The runner's F17 stopping hook: ExecuteLeaf uses it to tell a shutdown
	// drain (worker snoozes, item safely stays 'running') from a plain
	// final-attempt timeout (job discarded, item must be failed to stay
	// recoverable via retry-failed).
	deps.Runner.Stopping = c.stopping.Load
	if deps.Scans != nil {
		deps.Scans.EnqueueExpand = c.enqueueScanExpandTx
		deps.Scans.EnqueueSplit = c.enqueueScanSplitTx
		deps.Scans.EnqueueRenderPages = c.enqueueScanRenderPagesTx
		deps.Scans.EnqueueIdentifyPages = c.enqueueScanIdentifyPagesTx
		deps.Scans.EnqueuePromotePages = c.enqueueScanPromotePagesTx
	}
	if deps.Ingest != nil {
		deps.Ingest.EnqueueDirectIngest = c.enqueueDirectIngestTx
	}
	return c, nil
}

// Start begins working jobs. It MUST be given a context NOT bound to the process
// signal (F17): pass context.Background(). Cancelling the Start context is River's
// implicit stop trigger, and we want shutdown driven explicitly by Shutdown so the
// drain window and escalation are under our control, not the signal's.
func (c *Client) Start(ctx context.Context) error { return c.river.Start(ctx) }

// Stop is River's plain graceful stop (waits for in-flight jobs, bounded by ctx).
// main.go uses Shutdown for the drain-then-escalate sequence; Stop remains for the
// bind-failure teardown path and tests.
func (c *Client) Stop(ctx context.Context) error { return c.river.Stop(ctx) }

// Shutdown gracefully drains in-flight jobs on SIGTERM, then escalates (F17).
//
// Sequence:
//  1. Set the stopping flag so workers turn a context.Canceled into JobSnooze(0)
//     rather than a discarded final attempt.
//  2. river.Stop with a `drain`-bounded context: a SOFT stop — producers stop
//     fetching new jobs, in-flight jobs are allowed to finish. If everything
//     drains within `drain`, Stop returns nil and we're done.
//  3. If the drain window expires, river.StopAndCancel with a short grace: a HARD
//     stop that cancels the in-flight job contexts. Cooperative jobs observe the
//     cancellation, and their workers snooze; any that ignore cancellation are
//     abandoned in 'running' and re-queued by the rescuer after restart.
//
// River semantics this relies on (verified against river@v0.39.0 source):
//   - Stop = soft (waits for jobs); StopAndCancel = hard (cancels job ctxs, still
//     waits for the worker funcs to return). Both are bounded by the ctx passed in.
//   - JobSnooze does NOT consume an attempt (state written with Attempt-1), and its
//     state write goes through the completer, which persists on an uncancelled
//     context (jobcompleter withRetries uses context.WithoutCancel) — so a snooze
//     returned during EITHER the soft OR the hard phase is durably recorded.
//   - JobSnooze(0) makes the job immediately available, so it reworks on next start.
//
// It returns the error from whichever stop phase ultimately settles (nil on a clean
// drain). The process should exit only after this returns.
func (c *Client) Shutdown(ctx context.Context, drain time.Duration) error {
	c.stopping.Store(true)

	softCtx, cancelSoft := context.WithTimeout(ctx, drain)
	defer cancelSoft()
	if err := c.river.Stop(softCtx); err == nil {
		return nil // drained cleanly within the window
	} else if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return err // a real stop error, not a drain-window expiry
	}

	// Drain window elapsed (or the outer ctx was cancelled): escalate to a hard
	// stop with a short grace so cancelled jobs can snooze and exit.
	hardCtx, cancelHard := context.WithTimeout(context.WithoutCancel(ctx), hardStopGrace)
	defer cancelHard()
	return c.river.StopAndCancel(hardCtx)
}

// EnqueuePlanTx inserts the PlanRun job in the caller's transaction — the run row
// and its planner job commit atomically (spec §6.1).
func (c *Client) EnqueuePlanTx(ctx context.Context, tx pgx.Tx, runID int64) error {
	_, err := c.river.InsertTx(ctx, tx, PlanRunArgs{RunID: runID}, nil)
	return err
}

// EnqueueLeavesTx fans leaf jobs out inside the caller's transaction: the
// planning transaction (via Runner.EnqueueLeaves) and handleRetryFailed's
// reset-status-enqueue transaction — both need "items written ⇒ jobs written"
// to hold atomically (spec §6.1).
func (c *Client) EnqueueLeavesTx(ctx context.Context, tx pgx.Tx, provider string, itemIDs []int64) error {
	params := make([]river.InsertManyParams, 0, len(itemIDs))
	for _, id := range itemIDs {
		params = append(params, river.InsertManyParams{
			Args: GradeLeafArgs{ItemID: id, Provider: provider},
			InsertOpts: &river.InsertOpts{
				Queue:       llmQueue,
				MaxAttempts: leafMaxAttempts,
			},
		})
	}
	if len(params) == 0 {
		return nil
	}
	_, err := c.river.InsertManyFastTx(ctx, tx, params)
	return err
}

// enqueueScanExpandTx inserts the scan.expand job in the caller's transaction so
// the batch row/zip_ref and its expand job commit atomically (mirrors
// EnqueuePlanTx). ScanExpandArgs' own InsertOpts carry the queue + attempts.
func (c *Client) enqueueScanExpandTx(ctx context.Context, tx pgx.Tx, batchID int64) error {
	_, err := c.river.InsertTx(ctx, tx, ScanExpandArgs{BatchID: batchID}, nil)
	return err
}

// enqueueScanSplitTx fans scan.split jobs out inside the caller's transaction
// (one job per source) — mirrors enqueueDirectIngestTx.
func (c *Client) enqueueScanSplitTx(ctx context.Context, tx pgx.Tx, sourceIDs []int64) error {
	params := make([]river.InsertManyParams, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		params = append(params, river.InsertManyParams{
			Args: ScanSplitArgs{SourceID: id},
			InsertOpts: &river.InsertOpts{
				Queue:       scanQueue,
				MaxAttempts: scanSplitMaxAttempts,
			},
		})
	}
	if len(params) == 0 {
		return nil
	}
	_, err := c.river.InsertManyFastTx(ctx, tx, params)
	return err
}

// enqueueScanRenderPagesTx inserts one scan.render_pages job for a single chunk
// of a source's pages in the caller's transaction — mirrors enqueueScanExpandTx
// (one job per call rather than a fan-out: the chunking happens at the call
// site, one PDFium document open per chunk).
func (c *Client) enqueueScanRenderPagesTx(ctx context.Context, tx pgx.Tx, sourceID int64, pageIDs []int64) error {
	_, err := c.river.InsertTx(ctx, tx, ScanRenderPagesArgs{SourceID: sourceID, PageIDs: pageIDs}, nil)
	return err
}

// enqueueScanIdentifyPagesTx fans scan.identify_page jobs out inside the
// caller's transaction (one job per page) — mirrors enqueueScanSplitTx.
func (c *Client) enqueueScanIdentifyPagesTx(ctx context.Context, tx pgx.Tx, pageIDs []int64) error {
	params := make([]river.InsertManyParams, 0, len(pageIDs))
	for _, id := range pageIDs {
		params = append(params, river.InsertManyParams{
			Args: ScanIdentifyPageArgs{PageID: id},
			InsertOpts: &river.InsertOpts{
				Queue:       llmQueue,
				MaxAttempts: scanIdentifyMaxAttempts,
			},
		})
	}
	if len(params) == 0 {
		return nil
	}
	_, err := c.river.InsertManyFastTx(ctx, tx, params)
	return err
}

// enqueueScanPromotePagesTx fans scan.promote_page jobs out inside the caller's
// transaction (one job per assigned unpromoted page at finalize, D27/F1) —
// mirrors enqueueScanSplitTx.
func (c *Client) enqueueScanPromotePagesTx(ctx context.Context, tx pgx.Tx, items []scan.PromotePage) error {
	params := make([]river.InsertManyParams, 0, len(items))
	for _, item := range items {
		params = append(params, river.InsertManyParams{
			Args: ScanPromotePageArgs{PageID: item.PageID, Force: item.Force, Actor: item.Actor},
			InsertOpts: &river.InsertOpts{
				Queue:       scanQueue,
				MaxAttempts: scanPromoteMaxAttempts,
			},
		})
	}
	if len(params) == 0 {
		return nil
	}
	_, err := c.river.InsertManyFastTx(ctx, tx, params)
	return err
}

// enqueueDirectIngestTx inserts the ingest.direct job(s) in the caller's transaction
// (StageDirectUpload) so the row insert and its ingest job commit atomically —
// mirrors enqueueScanExpandTx/enqueueLeavesTx.
func (c *Client) enqueueDirectIngestTx(ctx context.Context, tx pgx.Tx, ids []int64) error {
	params := make([]river.InsertManyParams, 0, len(ids))
	for _, id := range ids {
		params = append(params, river.InsertManyParams{
			Args: DirectIngestArgs{UploadID: id},
			InsertOpts: &river.InsertOpts{
				Queue:       scanQueue,
				MaxAttempts: directIngestMaxAttempts,
			},
		})
	}
	if len(params) == 0 {
		return nil
	}
	_, err := c.river.InsertManyFastTx(ctx, tx, params)
	return err
}

// EnqueueMaskPages fans mask.page jobs out OUTSIDE a transaction (handleApplyMasks:
// PlanMasks is read-only, with no state-write transaction to piggyback on).
func (c *Client) EnqueueMaskPages(ctx context.Context, pageIDs []int64) error {
	params := make([]river.InsertManyParams, 0, len(pageIDs))
	for _, id := range pageIDs {
		params = append(params, river.InsertManyParams{
			Args: MaskPageArgs{PageID: id},
			InsertOpts: &river.InsertOpts{
				Queue:       scanQueue,
				MaxAttempts: maskPageMaxAttempts,
			},
		})
	}
	if len(params) == 0 {
		return nil
	}
	_, err := c.river.InsertManyFast(ctx, params)
	return err
}

// EnqueueEmailSendsTx inserts one generation-aware email.send job per delivery
// reference in the caller's transaction. Publishing/resending state and its
// durable work therefore commit or roll back together. InsertManyTx (not the
// Fast variant) is required so River resolves same-generation uniqueness
// conflicts as successful no-ops instead of aborting the transaction.
func (c *Client) EnqueueEmailSendsTx(ctx context.Context, tx pgx.Tx, refs []publish.DeliveryRef) error {
	params := make([]river.InsertManyParams, 0, len(refs))
	for _, ref := range refs {
		params = append(params, river.InsertManyParams{
			Args: EmailSendArgs(ref),
		})
	}
	if len(params) == 0 {
		return nil
	}
	_, err := c.river.InsertManyTx(ctx, tx, params)
	return err
}

// regradeAIUniqueStates scopes EnqueueRegradeAI's per-sub-item uniqueness to the
// non-terminal ("in flight") job states only. Double-clicking "Grade pending (N)" must
// not enqueue a second job for a sub-item while one is queued/running (that doubles LLM
// spend, F: regrade-round double-enqueue), but a brand-new batch AFTER the first job has
// COMPLETED must still be allowed — the deliberate re-run-after-completion semantics
// (regrade_service.go does not skip when a record already exists). So Completed (and
// Cancelled/Discarded) are deliberately OMITTED from the set; the required Available/
// Pending/Running/Scheduled states are present, plus Retryable so a transiently-failed
// job still dedups.
var regradeAIUniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
	rivertype.JobStateRetryable,
}

// EnqueueRegradeAI fans one regrade.ai job out per regrade SUB-ITEM id OUTSIDE a
// transaction (the AI-regrade endpoints resolve eligibility read-only, no tx to
// piggyback on) — mirrors EnqueueMaskPages. Jobs land on the "llm" queue so they share
// provider rate limiting with grading leaves (spec §8, per-sub-item re-scope).
//
// Uniqueness (regrade-round double-enqueue fix): each job is unique by its args
// (SubItemID) across the in-flight states above, so a duplicate enqueue of the same
// sub-item while one is still queued/running is a graceful no-op rather than a second
// LLM call. This uses InsertMany, NOT InsertManyFast — only the former resolves unique
// conflicts gracefully instead of erroring the whole batch (river@v0.39.0).
func (c *Client) EnqueueRegradeAI(ctx context.Context, subItemIDs []int64) (int, error) {
	params := make([]river.InsertManyParams, 0, len(subItemIDs))
	for _, id := range subItemIDs {
		params = append(params, river.InsertManyParams{
			Args: RegradeAIArgs{SubItemID: id},
			InsertOpts: &river.InsertOpts{
				Queue:       llmQueue,
				MaxAttempts: regradeAIMaxAttempts,
				UniqueOpts: river.UniqueOpts{
					ByArgs:  true,
					ByState: regradeAIUniqueStates,
				},
			},
		})
	}
	if len(params) == 0 {
		return 0, nil
	}
	results, err := c.river.InsertMany(ctx, params)
	if err != nil {
		return 0, err
	}
	inserted := 0
	for _, result := range results {
		if !result.UniqueSkippedAsDuplicate {
			inserted++
		}
	}
	return inserted, nil
}

// (The non-transactional EnqueueLeaves was removed: the retry-failed path now
// enqueues inside its reset transaction via EnqueueLeavesTx, closing the
// "items reset but jobs lost" wedge.)
