// Package httpapi is the HTTP surface: routing, auth/session middleware, RBAC, and
// the JSON API the embedded SPA consumes. All PII-bearing bytes (images, PDFs) also
// stream through here behind the session (docs/DECISIONS.md D10).
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/auth"
	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/domain"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/llm/registry"
	"github.com/HaoWen46/adagrade/internal/publish"
	"github.com/HaoWen46/adagrade/internal/queue"
	"github.com/HaoWen46/adagrade/internal/scan"
	"github.com/HaoWen46/adagrade/internal/secrets"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// int8Of wraps a non-zero id as a nullable pgtype.Int8.
func int8Of(id int64) pgtype.Int8 {
	return pgtype.Int8{Int64: id, Valid: id != 0}
}

// Server owns the handlers and their dependencies.
type Server struct {
	cfg       config.Config
	store     *store.Store
	sessions  *scs.SessionManager
	log       *slog.Logger
	flow      *auth.Flow
	ingest    *ingest.Service
	scans     *scan.Service
	blobs     blobstore.Store
	queue     *queue.Client
	providers *registry.DBSource
	secretKey [32]byte
	publish   *publish.Service
	email     domain.EmailProvider // regrade confirmation/resolution sends (spec §5/§6); nil-safe
	tokenKey  []byte               // HKDF subkey (secrets.Derive(master, "regrade-token-v1")) — regrade token verification
}

// Deps are the seams main (or a test harness) owns and injects.
type Deps struct {
	Store     *store.Store
	Ingest    *ingest.Service
	Scans     *scan.Service
	Queue     *queue.Client
	Providers *registry.DBSource
	SecretKey [32]byte
	Logger    *slog.Logger
	// EmailProvider is the seam regrade confirmation/resolution emails send through
	// (spec §5/§6) — the same provider publish's Sender uses, reused directly here
	// (rather than the email.send River job, which is keyed by publish_item and
	// reconstructs its content from a stored snapshot) since these are ad-hoc,
	// synchronous, one-off sends with no publish_item snapshot of their own to
	// reconstruct from later. May be nil (e.g. a test harness that never exercises
	// the regrade routes); handlers treat a nil provider as "sending is a no-op".
	EmailProvider domain.EmailProvider
}

// New wires the server.
func New(cfg config.Config, d Deps) *Server {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	sm := auth.NewSessionManager(d.Store.Pool, cfg.Env == config.EnvProduction)

	s := &Server{
		cfg: cfg, store: d.Store, sessions: sm, log: logger,
		ingest: d.Ingest, scans: d.Scans, queue: d.Queue,
		providers: d.Providers, secretKey: d.SecretKey,
		email:    d.EmailProvider,
		tokenKey: secrets.Derive(d.SecretKey, "regrade-token-v1"),
	}
	if d.Ingest != nil {
		s.blobs = d.Ingest.Blobs
	}
	// Publish service (spec §2, §7): River jobs are inserted through the same pgx
	// transaction as publish/resend state. An API-only test harness may omit the queue
	// only when it does not commit real-provider email work.
	var enqueue publish.EnqueueTxFunc
	if d.Queue != nil {
		enqueue = d.Queue.EnqueueEmailSendsTx
	}
	s.publish = publish.NewService(d.Store, enqueue, cfg.Email.Provider, cfg.RegradeWindow, cfg.Email.From, cfg.ReportFontConfigured(), logger)
	if cfg.GoogleClientID != "" {
		s.flow = &auth.Flow{
			ClientID:     cfg.GoogleClientID,
			AuthURL:      auth.GoogleAuthURL,
			RedirectURL:  cfg.OAuthRedirectURL,
			HostedDomain: cfg.HostedDomain,
			Sessions:     sm,
			Exchange:     auth.NewGoogleExchanger(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.OAuthRedirectURL),
			Users:        userStore{d.Store},
			Logger:       logger,
		}
	}
	return s
}

// Sessions exposes the manager so main can share it (e.g. for SSE endpoints later).
func (s *Server) Sessions() *scs.SessionManager { return s.sessions }

// Handler assembles the full route tree; spa serves everything unmatched.
func (s *Server) Handler(spa http.Handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "env": string(s.cfg.Env)})
	})
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Auth.
	if s.flow != nil {
		mux.HandleFunc("GET /auth/login", s.flow.BeginLogin)
		mux.HandleFunc("GET /auth/callback", s.flow.Callback)
	}
	mux.HandleFunc("POST /auth/email-login", s.handleEmailLogin)
	// Registered even when email login is unconfigured: without configuration no
	// valid tokens exist, so the handler can only ever reject.
	mux.HandleFunc("POST /auth/email-callback", s.handleEmailCallback)
	if s.devLoginEnabled() {
		mux.HandleFunc("POST /auth/dev-login", s.handleDevLogin)
	}
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/modes", s.handleAuthModes)

	// Inbound email webhook (spec §5, D33): a top-level route registered exactly like
	// the OAuth callback above — OUTSIDE the /api/ sub-mux and its session-auth
	// middleware, since the caller is the email provider, not a signed-in user. The
	// path secret is checked inside the handler (constant-time, 404 on mismatch).
	mux.HandleFunc("POST /webhooks/email/inbound/{secret}", s.handleInboundWebhook)

	// Authenticated API.
	api := http.NewServeMux()
	api.HandleFunc("GET /api/me", s.handleMe)
	api.HandleFunc("GET /api/users", s.requireRole(auth.RoleAdmin, s.handleListUsers))
	api.HandleFunc("POST /api/users", s.requireRole(auth.RoleAdmin, s.handleCreateUser))
	api.HandleFunc("PATCH /api/users/{id}", s.requireRole(auth.RoleAdmin, s.handleUpdateUser))

	// Phase 1: assessments, problems, rubrics, solutions, roster.
	api.HandleFunc("GET /api/assessments", s.handleListAssessments)
	api.HandleFunc("POST /api/assessments", s.requireRole(auth.RoleLecturer, s.handleCreateAssessment))
	api.HandleFunc("GET /api/assessments/{id}", s.handleGetAssessment)
	api.HandleFunc("PATCH /api/assessments/{id}", s.requireRole(auth.RoleLecturer, s.handleRenameAssessment))
	api.HandleFunc("POST /api/assessments/{id}/archive", s.requireRole(auth.RoleLecturer, s.handleArchiveAssessment))
	api.HandleFunc("DELETE /api/assessments/{id}", s.requireRole(auth.RoleAdmin, s.handleDeleteAssessment))
	api.HandleFunc("POST /api/assessments/{id}/problems", s.requireRole(auth.RoleLecturer, s.handleCreateProblem))
	api.HandleFunc("PATCH /api/problems/{id}", s.requireRole(auth.RoleLecturer, s.handleUpdateProblem))
	api.HandleFunc("DELETE /api/problems/{id}", s.requireRole(auth.RoleAdmin, s.handleDeleteProblem))
	api.HandleFunc("GET /api/problems/{id}/rubric", s.handleGetRubric)
	api.HandleFunc("POST /api/problems/{id}/rubric", s.handleCreateRubricVersion)
	api.HandleFunc("GET /api/rubric-versions/{id}", s.handleGetRubricVersion)
	api.HandleFunc("GET /api/problems/{id}/solutions", s.handleGetSolutions)
	api.HandleFunc("POST /api/problems/{id}/solutions", s.handleCreateSolutionVersion)
	api.HandleFunc("GET /api/solution-versions/{id}", s.handleGetSolutionVersion)
	api.HandleFunc("GET /api/students", s.handleListStudents)
	api.HandleFunc("PATCH /api/students/{id}", s.requireRole(auth.RoleLecturer, s.handleUpdateStudent))
	api.HandleFunc("POST /api/students/import", s.requireRole(auth.RoleLecturer, s.handleImportRoster))
	// Roster-lifecycle plan 2026-07-10: bulk sync actions the import diff proposes.
	api.HandleFunc("POST /api/students/bulk-withdraw", s.requireRole(auth.RoleLecturer, s.handleBulkWithdrawStudents))
	api.HandleFunc("POST /api/students/bulk-reinstate", s.requireRole(auth.RoleLecturer, s.handleBulkReinstateStudents))
	// Roster hard-delete (audit finding B15): admin-only, one rung above the
	// lecturer-gated actions above — Withdraw is reversible and TA/lecturer-safe,
	// this is not.
	api.HandleFunc("DELETE /api/students/{id}", s.requireRole(auth.RoleAdmin, s.handleDeleteStudent))
	// Per-student page (design doc 2026-07-28-student-page-design.md): read-only,
	// zero-mutation staff view of one student's history, keyed by the SCHOOL id
	// (students.student_id) rather than the internal row id the mutating routes
	// above use. Ordinary authed routes, no role gate — the page inherits
	// AnswerView's exposure and does not widen it (TA data-scoping B-M15 stays
	// open repo-wide; recorded, not changed, here). See students_page.go.
	api.HandleFunc("GET /api/students/{sid}", s.handleStudentPage)
	api.HandleFunc("GET /api/students/{sid}/assessments/{aid}", s.handleStudentAssessmentDetail)

	// Phase 2: ingestion, mapping, masking, blob streaming.
	api.HandleFunc("POST /api/assessments/{id}/submissions", s.handleUploadSubmissions)
	api.HandleFunc("GET /api/assessments/{id}/uploads", s.handleListDirectUploads)
	api.HandleFunc("GET /api/assessments/{id}/ingest/report", s.handleIngestReport)
	// Late-add unblock (roster-lifecycle plan 2026-07-10 fix 6): create answer
	// rows for active students added after the last materialization.
	api.HandleFunc("POST /api/assessments/{id}/materialize-answers", s.requireRole(auth.RoleLecturer, s.handleMaterializeAnswers))
	api.HandleFunc("POST /api/quarantine/{id}/assign", s.handleAssignQuarantine)
	api.HandleFunc("POST /api/quarantine/{id}/dismiss", s.handleDismissQuarantine)
	api.HandleFunc("GET /api/assessments/{id}/mask-regions", s.handleGetMaskRegions)
	api.HandleFunc("PUT /api/assessments/{id}/mask-regions", s.handlePutMaskRegions)
	api.HandleFunc("POST /api/assessments/{id}/masks/apply", s.handleApplyMasks)
	api.HandleFunc("GET /api/assessments/{id}/masks/review", s.handleMaskReviewList)
	api.HandleFunc("POST /api/assessments/{id}/masks/accept-pending", s.handleAcceptPendingMasks)
	api.HandleFunc("POST /api/answer-pages/{id}/mask-review", s.handleMaskReview)
	api.HandleFunc("POST /api/answer-pages/{id}/move", s.handleMovePage)
	api.HandleFunc("POST /api/answers/{id}/flags", s.handleAnswerFlag)
	api.HandleFunc("GET /api/answer-pages/{id}/image", s.handlePageImage)
	api.HandleFunc("GET /api/submissions/{id}/pdf", s.handleSubmissionPDF)
	// Retract (HCI audit fix): unassign a live submission — the control the
	// Submissions tab / scan-park / post-Finalize surfaces already point TAs at.
	// Same intake-mutation role (any signed-in user) + CSRF as the routes above.
	api.HandleFunc("POST /api/submissions/{id}/retract", s.handleRetractSubmission)

	// Scan intake (page-level, design spec 2026-07-04).
	api.HandleFunc("POST /api/assessments/{id}/scan-batches", s.handleCreateScanBatch)
	api.HandleFunc("GET /api/assessments/{id}/scan-batches", s.handleListScanBatches)
	// Batch-level bulk recovery for errored pages (2026-07-11): retry them all
	// (optionally against a different provider) or discard them all.
	api.HandleFunc("POST /api/scan-batches/{id}/retry-errored", s.handleRetryErroredScanBatch)
	api.HandleFunc("POST /api/scan-batches/{id}/discard-errored", s.handleDiscardErroredScanBatch)
	api.HandleFunc("GET /api/assessments/{id}/scan-pages", s.handleListScanPages)
	api.HandleFunc("GET /api/assessments/{id}/scan-matrix", s.handleScanMatrix)
	api.HandleFunc("POST /api/assessments/{id}/scan-finalize", s.handleScanFinalize)
	api.HandleFunc("GET /api/assessments/{id}/scan-missing", s.handleScanMissing)
	api.HandleFunc("POST /api/scan-pages/{id}/assign", s.handleAssignScanPage)
	api.HandleFunc("POST /api/scan-pages/{id}/unassign", s.handleUnassignScanPage)
	api.HandleFunc("POST /api/scan-pages/{id}/discard", s.handleDiscardScanPage)
	api.HandleFunc("POST /api/scan-pages/{id}/undiscard", s.handleUndiscardScanPage)
	api.HandleFunc("POST /api/scan-pages/{id}/retry", s.handleRetryScanPage)
	api.HandleFunc("POST /api/scan-pages/{id}/resolve-conflict", s.handleResolveScanPageConflict)
	api.HandleFunc("GET /api/scan-pages/{id}/image", s.handleScanPageImage)
	api.HandleFunc("GET /api/scan-pages/{id}/crop", s.handleScanPageCrop)
	api.HandleFunc("GET /api/assessments/{id}/id-regions", s.handleGetIDRegions)
	api.HandleFunc("PUT /api/assessments/{id}/id-regions", s.handlePutIDRegions)
	// Identify status (privacy loudness 2026-07-12): whether the local OCR
	// rung (D24) is installed — the Identify tab's cloud-shipping banner and
	// the upload card's opt-in copy read this. See identify_status.go.
	api.HandleFunc("GET /api/identify/status", s.handleIdentifyStatus)

	// Workflow warnings (hazard audit 2026-07-10): the standing derived-state
	// hazard list any signed-in role can read; see warnings.go.
	api.HandleFunc("GET /api/assessments/{id}/workflow-warnings", s.handleWorkflowWarnings)

	// Phase 3: drill-down review + manual grading.
	api.HandleFunc("GET /api/assessments/{id}/problems/summary", s.handleProblemSummaries)
	api.HandleFunc("GET /api/problems/{id}/students", s.handleProblemStudents)
	api.HandleFunc("GET /api/answers/{id}", s.handleGetAnswer)
	api.HandleFunc("GET /api/assessments/{id}/totals", s.handleAssessmentTotals)
	api.HandleFunc("GET /api/assessments/{id}/students/{sid}/submission", s.handleStudentSubmission)
	api.HandleFunc("POST /api/answers/{id}/records", s.handleCreateManualRecord)
	// (POST /api/answers/{id}/official removed in 0027 — officials are derived
	// from the assessment's final source; humans grade fallbacks, not pointers.)
	api.HandleFunc("PUT /api/assessments/{id}/final-source", s.requireRole(auth.RoleLecturer, s.handleSetFinalSource))

	// Providers (app-managed LLM endpoints + keys, D11 v1). Lecturer+ manages;
	// any signed-in role may list (method editor needs names/models).
	api.HandleFunc("GET /api/providers", s.handleListProviders)
	api.HandleFunc("POST /api/providers", s.requireRole(auth.RoleLecturer, s.handleCreateProvider))
	api.HandleFunc("PATCH /api/providers/{id}", s.requireRole(auth.RoleLecturer, s.handleUpdateProvider))
	api.HandleFunc("DELETE /api/providers/{id}", s.requireRole(auth.RoleLecturer, s.handleDeleteProvider))
	api.HandleFunc("POST /api/providers/{id}/test", s.requireRole(auth.RoleLecturer, s.handleTestProvider))
	api.HandleFunc("GET /api/providers/{id}/pricing", s.handleListPricing)
	api.HandleFunc("PUT /api/providers/{id}/pricing", s.requireRole(auth.RoleLecturer, s.handlePutPricing))

	// Phase 4: methods, prompt templates, grading runs.
	api.HandleFunc("GET /api/methods", s.handleListMethods)
	api.HandleFunc("POST /api/methods", s.handleCreateMethod)
	api.HandleFunc("GET /api/methods/{id}", s.handleGetMethod)
	api.HandleFunc("POST /api/methods/{id}/versions", s.handleCreateMethodVersion)
	api.HandleFunc("POST /api/methods/{id}/archive", s.handleArchiveMethod)
	api.HandleFunc("GET /api/prompt-templates/{name}", s.handleGetPromptTemplate)
	api.HandleFunc("GET /api/grading-policies", s.handleGradingPolicies)
	api.HandleFunc("GET /api/runs/preview", s.handleRunPreview)
	api.HandleFunc("POST /api/runs", s.handleCreateRun)
	api.HandleFunc("GET /api/runs", s.handleListRuns)
	api.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	api.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancelRun)
	api.HandleFunc("POST /api/runs/{id}/retry-failed", s.handleRetryFailed)
	// (POST /api/runs/{id}/accept-official removed in 0027 — superseded by the
	// assessment-level final source; publish enforces the spot-check gate.)
	api.HandleFunc("GET /api/assessments/{id}/export.csv", s.handleExportCSV)

	// LaTeX transcription export (spec 2026-07-25). Read-only from the grading
	// system's point of view: it never writes a grading_record and never moves
	// an official pointer.
	api.HandleFunc("GET /api/assessments/{id}/transcription-status", s.handleTranscriptionStatus)
	api.HandleFunc("GET /api/assessments/{id}/problems/{number}/transcription.zip", s.handleTranscriptionZIP)
	api.HandleFunc("GET /api/assessments/{id}/transcription.zip", s.handleExamTranscriptionZIP)

	// Spot-check gate (trust spec §4, D37): since 0027 the sample must be
	// verdicted (or admin-waived) before an assessment whose final source is an
	// AI method can be PUBLISHED (internal/httpapi/publish.go). Verdicts are
	// TA-level review work, same as manual grading above; the waiver override
	// is admin-only.
	api.HandleFunc("GET /api/runs/{id}/spot-check", s.handleGetSpotCheck)
	api.HandleFunc("POST /api/runs/{id}/spot-check/{recordID}", s.handleSetSpotCheckVerdict)
	api.HandleFunc("POST /api/runs/{id}/spot-check/waive", s.requireRole(auth.RoleAdmin, s.handleWaiveSpotCheck))

	// Analysis + prompt preview (methods-as-experiments tooling).
	api.HandleFunc("GET /api/assessments/{id}/analysis", s.handleAssessmentAnalysis)
	api.HandleFunc("GET /api/problems/{id}/prompt-preview", s.handlePromptPreview)
	api.HandleFunc("GET /api/problems/{id}/score-distribution", s.handleScoreDistribution)

	// Multi-model consensus (D17): per-assessment policy + re-runnable aggregation.
	api.HandleFunc("GET /api/assessments/{id}/aggregation", s.handleGetAggregationPolicy)
	api.HandleFunc("PUT /api/assessments/{id}/aggregation", s.handlePutAggregationPolicy)
	api.HandleFunc("POST /api/assessments/{id}/aggregate", s.handleRunAggregation)

	// Phase 6: publish + grade-email send pipeline (spec §2, §3, §7). Preview/publish/
	// history/resend are lecturer+ (teacher action); unpublish is the admin escape hatch.
	api.HandleFunc("GET /api/assessments/{id}/publish/preview", s.requireRole(auth.RoleLecturer, s.handlePublishPreview))
	api.HandleFunc("POST /api/assessments/{id}/publish", s.requireRole(auth.RoleLecturer, s.handlePublish))
	api.HandleFunc("POST /api/assessments/{id}/unpublish", s.requireRole(auth.RoleAdmin, s.handleUnpublish))
	api.HandleFunc("GET /api/assessments/{id}/publish/batches", s.requireRole(auth.RoleLecturer, s.handlePublishBatches))
	api.HandleFunc("POST /api/publish/batches/{id}/resend-failed", s.requireRole(auth.RoleLecturer, s.handleResendFailed))
	// Individual resend (spec §4 D46): any status, reuses the batch's attachment
	// settings, audited publish.resend_item.
	api.HandleFunc("POST /api/publish/items/{id}/resend", s.requireRole(auth.RoleLecturer, s.handleResendItem))

	// Regrade v2 queue (spec §5-§8). Inbound replies land here via the webhook above;
	// any signed-in role may triage (list/detail) the queue. Adjudication, result send,
	// reminders, and sub-item edits are TA+ (the plan's "TAs resolve"). Per-problem TA
	// assignment is lecturer+.
	api.HandleFunc("GET /api/regrades", s.handleListRegrades)
	api.HandleFunc("GET /api/regrades/{id}", s.handleGetRegrade)
	api.HandleFunc("POST /api/regrades/{id}/send-result", s.requireRole(auth.RoleTA, s.handleSendResult))
	// resend-result (whole-branch review F1): send-failure recovery for a resolved
	// request whose result email never actually delivered.
	api.HandleFunc("POST /api/regrades/{id}/resend-result", s.requireRole(auth.RoleTA, s.handleResendResult))
	api.HandleFunc("POST /api/regrades/{id}/remind", s.requireRole(auth.RoleTA, s.handleRemindRegrade))
	api.HandleFunc("POST /api/regrades/{id}/problems", s.requireRole(auth.RoleTA, s.handleAddRegradeProblem))
	api.HandleFunc("PATCH /api/regrades/{id}/problems/{pid}", s.requireRole(auth.RoleTA, s.handlePatchRegradeProblem))
	// Per-problem TA assignment (spec §6/§8): lecturer+ assigns/unassigns; the assignee
	// must hold TA-or-higher (handler-enforced). The assignment-state GET (gap 1, backs
	// the queue's "handed to <TA>" badge + the problems-editor picker's current
	// assignment) is gated the SAME as the queue's other read routes above — any
	// signed-in role, no requireRole wrapper — so a TA viewing the queue can see badges.
	api.HandleFunc("GET /api/assessments/{id}/ta-assignments", s.handleListTAAssignments)
	api.HandleFunc("PUT /api/problems/{id}/ta", s.requireRole(auth.RoleLecturer, s.handleAssignProblemTA))
	// Assignable-graders list (gap 2): the TA-assignment picker's data source, lecturer+
	// (matching the picker + PUT .../ta above) — deliberately NOT admin-only like
	// GET /api/users, and deliberately minimal-fields (no email/PII beyond name).
	api.HandleFunc("GET /api/graders", s.requireRole(auth.RoleLecturer, s.handleListGraders))
	// AI re-grade assist (spec §8): TA+ only — students can never cause API spend. The
	// Regrade rounds (rounds design, 0027/0028): the assessment-scoped cockpit —
	// per-round method config (lecturer+), the reply deadline (lecturer+), and
	// the manual batch-grade button (TA+, budget-gated).
	api.HandleFunc("GET /api/assessments/{id}/regrade-rounds", s.handleGetRegradeRounds)
	api.HandleFunc("PUT /api/assessments/{id}/regrade-rounds/{turn}", s.requireRole(auth.RoleLecturer, s.handleSetRegradeRoundMethod))
	api.HandleFunc("PUT /api/assessments/{id}/regrade-deadline", s.requireRole(auth.RoleLecturer, s.handleSetRegradeDeadline))
	api.HandleFunc("POST /api/assessments/{id}/regrade-rounds/{turn}/grade", s.requireRole(auth.RoleTA, s.handleGradeRegradeRound))

	// batch route is registered BEFORE the {id} route so "ai-regrade-all" isn't captured
	// as an {id} path value.
	api.HandleFunc("POST /api/regrades/ai-regrade-all", s.requireRole(auth.RoleTA, s.handleAIRegradeAll))
	api.HandleFunc("POST /api/regrades/{id}/ai-regrade", s.requireRole(auth.RoleTA, s.handleAIRegrade))

	// Ops (Task U3, docs/OPERATIONS.md §4): monitoring polls this for River queue
	// depth, blob/DB disk usage, and last-backup freshness.
	api.HandleFunc("GET /api/ops/status", s.requireRole(auth.RoleAdmin, s.handleOpsStatus))

	// Audit log read path (trust spec §6, D39): admin-only, the write side
	// (s.audit) already exists across 41+ action types.
	api.HandleFunc("GET /api/audit", s.requireRole(auth.RoleAdmin, s.handleListAudit))

	s.mountAPI(mux, api)

	mux.Handle("/", spa)

	// deadlineMiddleware runs outermost (F5): it installs the per-request read/write
	// deadlines that replace the removed global Server.ReadTimeout/WriteTimeout, so
	// the deadline is in force before session/CSRF/handler code reads any body.
	return deadlineMiddleware(s.sessions.LoadAndSave(csrfGuard(mux)))
}

// mountAPI registers the authenticated sub-mux under /api/ with the user middleware.
func (s *Server) mountAPI(root *http.ServeMux, api *http.ServeMux) {
	root.Handle("/api/", s.withUser(api))
}

func (s *Server) devLoginEnabled() bool {
	return s.cfg.DevLogin && s.cfg.Env == config.EnvDevelopment
}

// --- middleware ---------------------------------------------------------------

type ctxKey int

const ctxUserKey ctxKey = 1

// currentUser returns the authenticated user attached by withUser.
func currentUser(ctx context.Context) (db.User, bool) {
	u, ok := ctx.Value(ctxUserKey).(db.User)
	return u, ok
}

// withUser resolves the session to a live user row on every request — role/active
// changes take effect immediately (server-side sessions, spec §2).
func (s *Server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := s.sessions.GetInt64(r.Context(), auth.SessionUserIDKey)
		if id == 0 {
			apiError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		u, err := s.store.Q.GetUserByID(r.Context(), id)
		if err != nil || !u.Active {
			// Session refers to a deleted/deactivated user: kill it.
			_ = s.sessions.Destroy(r.Context())
			apiError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUserKey, u)))
	})
}

// requireRole allows admins and lecturers everywhere `role` is lecturer, etc.
// Roles are a strict ladder: admin > lecturer > ta (plan §8).
func (s *Server) requireRole(min auth.Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := currentUser(r.Context())
		if !ok {
			apiError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if !auth.RoleAtLeast(auth.Role(u.Role), min) {
			apiError(w, http.StatusForbidden, "insufficient role")
			return
		}
		next(w, r)
	}
}

// inboundWebhookPathPrefix is the top-level route csrfGuard must exempt: the caller
// is the email provider (a machine POST), not the SPA, so it never carries the
// X-ADA-CSRF header. The path secret inside the handler is this route's actual
// authentication (constant-time compare, 404 on mismatch) — CSRF protection exists to
// stop a browser-driven cross-site request from riding a user's session cookie, which
// does not apply here since there is no session.
const inboundWebhookPathPrefix = "/webhooks/email/inbound/"

// csrfGuard rejects mutating requests that lack the SPA's custom header (D7). The
// OAuth callback is a top-level GET and unaffected; the inbound email webhook is a
// top-level POST from the email provider and is exempted by path prefix (see
// inboundWebhookPathPrefix).
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, inboundWebhookPathPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("X-ADA-CSRF") == "" {
			apiError(w, http.StatusForbidden, "missing CSRF header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- auth handlers -------------------------------------------------------------

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Pool.Ping(ctx); err != nil {
		apiError(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleAuthModes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"email":  s.emailLoginEnabled(),
		"google": s.flow != nil,
		"dev":    s.devLoginEnabled(),
	})
}

// handleDevLogin is the development-only sign-in (D8): double-gated in config, and
// still strictly allowlist-checked via the same Authorize policy as OAuth.
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	email := auth.NormalizeEmail(body.Email)
	rec, found, err := userStore{s.store}.ByEmail(r.Context(), email)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	decision := auth.Authorize(
		auth.Claims{Email: email, EmailVerified: true},
		s.cfg.HostedDomain,
		func(string) (auth.AllowlistEntry, bool) {
			if !found {
				return auth.AllowlistEntry{}, false
			}
			return auth.AllowlistEntry{Role: rec.Role, Active: rec.Active}, true
		},
	)
	if !decision.Authorized {
		s.log.Warn("dev-login denied", "reason", decision.Reason)
		apiError(w, http.StatusForbidden, "not authorized")
		return
	}
	if err := s.sessions.RenewToken(r.Context()); err != nil {
		apiError(w, http.StatusInternalServerError, "session error")
		return
	}
	s.sessions.Put(r.Context(), auth.SessionUserIDKey, rec.ID)
	s.log.Info("dev-login", "user_id", rec.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.Destroy(r.Context()); err != nil {
		apiError(w, http.StatusInternalServerError, "session error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- user (allowlist) management -------------------------------------------------

// userJSON is the API shape for a user row.
type userJSON struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Active      bool   `json:"active"`
}

func toUserJSON(u db.User) userJSON {
	return userJSON{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName, Role: u.Role, Active: u.Active}
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := currentUser(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserJSON(u)})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.Q.ListUsers(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]userJSON, 0, len(users))
	for _, u := range users {
		out = append(out, toUserJSON(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func validRole(role string) bool {
	switch auth.Role(role) {
	case auth.RoleAdmin, auth.RoleLecturer, auth.RoleTA:
		return true
	}
	return false
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	email := auth.NormalizeEmail(body.Email)
	if email == "" || !validRole(body.Role) {
		apiError(w, http.StatusBadRequest, "email and a valid role (admin|lecturer|ta) are required")
		return
	}
	u, err := s.store.Q.CreateUser(r.Context(), db.CreateUserParams{
		Email: email, DisplayName: body.DisplayName, Role: body.Role, Active: true,
	})
	if err != nil {
		apiError(w, http.StatusConflict, "user already exists or could not be created")
		return
	}
	s.audit(r, "user.create", "user", strconv.FormatInt(u.ID, 10), nil)
	writeJSON(w, http.StatusCreated, toUserJSON(u))
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	existing, err := s.store.Q.GetUserByID(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such user")
		return
	}

	// Partial update: absent fields keep their value.
	body := struct {
		DisplayName *string `json:"display_name"`
		Role        *string `json:"role"`
		Active      *bool   `json:"active"`
	}{}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	role, active, name := existing.Role, existing.Active, existing.DisplayName
	if body.Role != nil {
		role = *body.Role
	}
	if body.Active != nil {
		active = *body.Active
	}
	if body.DisplayName != nil {
		name = *body.DisplayName
	}
	if !validRole(role) {
		apiError(w, http.StatusBadRequest, "invalid role")
		return
	}

	// Self-lockout guard: you cannot deactivate or demote yourself.
	me, _ := currentUser(r.Context())
	if me.ID == id && (!active || auth.Role(role) != auth.Role(existing.Role)) {
		apiError(w, http.StatusBadRequest, "cannot deactivate or change your own role")
		return
	}

	u, err := s.store.Q.UpdateUser(r.Context(), db.UpdateUserParams{
		ID: id, Role: role, Active: active, DisplayName: name,
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "update failed")
		return
	}
	s.audit(r, "user.update", "user", strconv.FormatInt(id, 10), map[string]any{
		"role": role, "active": active,
	})
	writeJSON(w, http.StatusOK, toUserJSON(u))
}

// audit best-effort writes an audit row; failures are logged, never fatal to the
// request (the mutation already committed).
func (s *Server) audit(r *http.Request, action, kind, id string, detail map[string]any) {
	u, _ := currentUser(r.Context())
	if err := s.store.InsertAudit(r.Context(), u.ID, action, kind, id, detail); err != nil {
		s.log.Error("audit write failed", "action", action, "err", err)
	}
}

// --- store adapters ---------------------------------------------------------------

// userStore adapts the sqlc queries to auth.UserStore.
type userStore struct{ st *store.Store }

func (u userStore) ByEmail(ctx context.Context, email string) (auth.UserRecord, bool, error) {
	row, err := u.st.Q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.UserRecord{}, false, nil
	}
	if err != nil {
		return auth.UserRecord{}, false, err
	}
	return auth.UserRecord{
		ID: row.ID, Email: row.Email, DisplayName: row.DisplayName,
		Role: auth.Role(row.Role), Active: row.Active,
	}, true, nil
}
