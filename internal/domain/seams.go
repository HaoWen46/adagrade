// Package domain holds ADA-Marker's core types and the five "seam" interfaces that
// keep every external dependency swappable (spec §3): Renderer, BlobStore,
// VisionProvider, EmailProvider, Queue.
//
// These signatures are intentionally first-draft. The `QUESTION:` comments mark
// decisions the product plan left open that writing the interface forced into the
// open — they are harvested into docs/PLAN_GAPS.md, not left as silent TODOs.
package domain

import (
	"context"
	"io"
	"time"
)

// ---------------------------------------------------------------------------
// Seam 1: Renderer — PDF pages → JPEG images (spec §7). Impl: go-pdfium (WASM).
// ---------------------------------------------------------------------------

// RenderOptions are the per-assessment knobs for rasterization.
type RenderOptions struct {
	DPI           int  // 200–300 typical for handwriting legibility.
	MaxLongEdgePx int  // downscale cap so images stay under vision-model limits (also a cost lever).
	Grayscale     bool // grayscale often preserves pen legibility while shrinking payload.
}

// RenderedPage is one rasterized page. Width/Height are needed to scale normalized
// mask regions back to pixels.
type RenderedPage struct {
	Index         int // 0-based page index within the source PDF.
	JPEG          []byte
	Width, Height int
}

// Renderer splits a PDF and rasterizes its pages.
//
// QUESTION: is splitting the Renderer's job or a separate pdfcpu step feeding it one
// page at a time? The spec assigns split to pdfcpu and render to go-pdfium; this
// single method blurs that. Decide whether the seam is Render(pageStream) or
// SplitAndRender(pdf).
type Renderer interface {
	Render(ctx context.Context, pdf io.Reader, opts RenderOptions) ([]RenderedPage, error)
}

// ---------------------------------------------------------------------------
// Seam 2: BlobStore — source PDFs, rendered JPGs, masked JPGs (spec §2). Impl: local disk.
// ---------------------------------------------------------------------------

// BlobStore is content storage behind a swappable interface. Keys are app-defined
// paths (e.g. "answers/<id>/original.jpg", "answers/<id>/masked.jpg").
//
// QUESTION: how are PDF/JPG bytes served to the browser — streamed through the Go
// process (auth-gated) or via signed paths? Local disk has no native signed URLs, so
// the answer likely is "always stream through an authenticated handler". Confirm.
type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
}

// ---------------------------------------------------------------------------
// Seam 3: VisionProvider — grade one masked answer image (spec §2, §5). Impl: gemini/claude/openai.
// ---------------------------------------------------------------------------

// GradeRequest is a provider-agnostic grading call. Images are the MASKED copies only.
type GradeRequest struct {
	Images         [][]byte // masked JPGs; never the unmasked original.
	System         string
	Prompt         string
	JSONSchema     []byte // the run's frozen rubric schema (transcription + per-criterion).
	ReasoningLevel string // abstract "off|low|medium|high", mapped per provider.
}

// GradeResult carries the structured grade plus everything needed for audit and cost.
type GradeResult struct {
	Structured                []byte // schema-valid JSON; app re-validates + clamps scores (strict schemas lack numeric bounds).
	Raw                       string // raw model output, stored for audit.
	InputTokens, OutputTokens int
	CostUSD                   float64
	// QUESTION: confidence/legibility is emitted *inside* Structured by the transcribe-
	// then-grade prompt, yet flagging logic needs it out here. Should the seam surface a
	// typed Confidence field, or does grading/ parse it from Structured? Decide the boundary.
}

// VisionProvider is the synchronous grading path (single-answer / interactive re-grades).
type VisionProvider interface {
	Grade(ctx context.Context, req GradeRequest) (GradeResult, error)
	Name() string // e.g. "google", "anthropic", "openai" — also the River queue name.
}

// BatchVisionProvider is the default path for multi-answer runs (50% cheaper, async).
//
// QUESTION: batch APIs differ sharply per provider (submit → poll → fetch, with expiry
// and partial results). Is this the right seam shape, or should batch be modeled as a
// Queue job type rather than a provider capability? Not all providers support batch —
// define the sync fallback contract.
type BatchVisionProvider interface {
	VisionProvider
	SubmitBatch(ctx context.Context, reqs []GradeRequest) (batchID string, err error)
	PollBatch(ctx context.Context, batchID string) (done bool, results []GradeResult, err error)
}

// ---------------------------------------------------------------------------
// Seam 4: EmailProvider — outbound grades + inbound regrade parse (spec §9). Impl: Postmark.
// ---------------------------------------------------------------------------

// Attachment is one file attached to an OutboundEmail (report-attachments
// spec §3, D42/D44): the per-student result PDF or the ZIP-of-images
// fallback. Content is the fully-built bytes — attachments are assembled in
// the send job from blob-stored page images, never persisted alongside
// publish_items (spec §3: "blobs stay the source of truth; rebuild-on-resend
// is deterministic").
type Attachment struct {
	Filename string
	MIME     string
	Content  []byte
}

// OutboundEmail is one individually-addressed grade notification.
type OutboundEmail struct {
	// DeliveryKey is the caller's stable, opaque idempotency key for one logical
	// delivery. Providers use a one-way-derived correlation token rather than
	// exposing this value directly in filenames or wire headers. Empty preserves
	// the legacy best-effort behavior for call sites that have no durable key.
	DeliveryKey string
	To          string
	ReplyTo     string // regrade+<signed-token>@inbound.<domain>
	Subject     string
	TextBody    string
	HTMLBody    string
	Attachments []Attachment
}

// InboundEmail is a parsed reply from the provider webhook. SPFPass/DKIMValid come from
// the provider's verdict headers; From is the claimed sender (must be checked against the
// token's bound roster email).
type InboundEmail struct {
	From        string
	MailboxHash string // the plus-address carrying the signed reply token.
	Subject     string // the reply's Subject header, verbatim.
	TextBody    string // the reply's plain-text body (reply-stripped when the provider offers it). Student PII — never log.
	SPFPass     bool
	DKIMValid   bool
	ReceivedAt  time.Time
	RawJSON     []byte
	// MessageID is the provider's unique id for this specific delivery attempt
	// (Postmark's top-level MessageID field). Webhook retries on timeout/non-2xx
	// re-deliver the SAME MessageID, so callers use it as an idempotency key —
	// never derived from content, since two distinct legitimate replies could
	// otherwise collide. May be empty for providers/fixtures that don't supply one.
	MessageID string
}

// EmailProvider sends notifications and parses inbound webhook payloads.
type EmailProvider interface {
	Send(ctx context.Context, msg OutboundEmail) (providerID string, err error)
	ParseInbound(raw []byte) (InboundEmail, error) // provider-specific decoding of the webhook body.
}

// ---------------------------------------------------------------------------
// Seam 5: Queue — durable async run execution (spec §6). Impl: River on Postgres.
// ---------------------------------------------------------------------------

// RunStatus is a snapshot of a grading run's progress, derived from a GROUP BY over
// leaf jobs/records so it survives restarts.
type RunStatus struct {
	Total, Graded, Failed, Remaining int
	State                            string // pending|running|paused|cancelled|completed|failed
	UpdatedAt                        time.Time
}

// Queue wraps the job system so River stays swappable and grading/ never imports it directly.
type Queue interface {
	// PlanRun enqueues the planner that fans a run out into per-(answer,model) jobs.
	PlanRun(ctx context.Context, runID string) error
	Cancel(ctx context.Context, runID string) error
	Pause(ctx context.Context, runID string) error
	Resume(ctx context.Context, runID string) error
	Status(ctx context.Context, runID string) (RunStatus, error)
}
