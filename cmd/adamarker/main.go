// Command adamarker is the single ADA-Marker binary: it serves the API and the
// embedded React SPA, and (later) hosts the River grading workers in the same process.
//
// Startup order: config → DB pool → migrations → bootstrap admin → HTTP server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HaoWen46/adagrade/internal/auth"
	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/domain"
	"github.com/HaoWen46/adagrade/internal/email"
	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/httpapi"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/llm/registry"
	"github.com/HaoWen46/adagrade/internal/localocr"
	"github.com/HaoWen46/adagrade/internal/publish"
	"github.com/HaoWen46/adagrade/internal/queue"
	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/report"
	"github.com/HaoWen46/adagrade/internal/scan"
	"github.com/HaoWen46/adagrade/internal/secrets"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/web"
)

func main() {
	verifyBlobs := flag.Bool("verify-blobs", false, "check every blob reference in the DB against the blob store and exit (Task U3, docs/OPERATIONS.md); does not start the HTTP server or worker fleet")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// -verify-blobs is a standalone, read-only check (docs/OPERATIONS.md §5 step
	// 5): it must exit before anything below touches the queue or HTTP server, so
	// it's handled first and separately from the normal run() startup path.
	if *verifyBlobs {
		cfg, err := config.Load(os.Getenv)
		if err != nil {
			logger.Error("fatal", "err", err)
			os.Exit(1)
		}
		os.Exit(runVerifyBlobs(context.Background(), cfg, os.Stdout, logger))
	}

	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if cfg.DatabaseURL == "" {
		return errors.New("ADAMARKER_DATABASE_URL is not set — `make dev` starts a local Postgres and points at it")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	// Acquire the fleet/deploy lock BEFORE either application or River migrations.
	// A new binary must never change the schema while an older lock-holder is still
	// executing sqlc generated against the prior shape. The deliberate multi-worker
	// escape hatch may start against an already-current schema, but it skips migrations
	// when another owner holds the lock so it cannot break that live process.
	runMigrations := true
	releaseWorkerLock, err := queue.AcquireWorkerLock(ctx, cfg.DatabaseURL)
	if err != nil {
		if errors.Is(err, queue.ErrWorkerLockHeld) && cfg.AllowMultipleWorkers {
			runMigrations = false
			logger.Warn("worker-fleet lock unavailable but ADAMARKER_ALLOW_MULTIPLE_WORKERS is set — skipping all migrations and starting workers against the existing schema; multiple instances may process jobs",
				"err", err)
		} else {
			return fmt.Errorf("worker-fleet lock: %w (set ADAMARKER_ALLOW_MULTIPLE_WORKERS=1 only for deliberate same-schema multi-instance experiments)", err)
		}
	} else {
		defer releaseWorkerLock()
	}

	if runMigrations {
		if err := store.RunMigrations(ctx, cfg.DatabaseURL); err != nil {
			return err
		}
	}
	if err := auth.EnsureBootstrapAdmin(ctx, bootstrapStore{st}, cfg.BootstrapAdminEmail); err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}

	blobs, err := blobstore.NewLocalDisk(cfg.BlobDir)
	if err != nil {
		return err
	}
	renderer, err := render.NewPDFium()
	if err != nil {
		return err
	}
	defer func() { _ = renderer.Close() }()

	ing := &ingest.Service{Store: st, Blobs: blobs, Renderer: renderer, Log: logger}

	// Credentials master key (auto-generated, D16) + app-managed providers (D11 v1).
	secretKey, err := secrets.LoadOrCreateKey(cfg.SecretKeyFile)
	if err != nil {
		return err
	}
	if err := registry.ImportEnvProviders(ctx, st, secretKey, cfg.Providers, logger); err != nil {
		return err
	}
	source := registry.NewDBSource(st, secretKey)

	runner := &grading.Runner{Store: st, Blobs: blobs, Providers: source, Log: logger}
	scans := &scan.Service{
		Store: st, Blobs: blobs, Renderer: renderer,
		Providers: source, Ingest: ing, Log: logger,
	}

	// Local OCR (D24): fully offline first identification rung. Optional and
	// never fatal — but its ABSENCE is loud (privacy audit 2026-07-12): see
	// setupLocalOCR's contract below.
	closeLocalOCR := setupLocalOCR(cfg, scans, logger)
	defer closeLocalOCR()

	// Report PDF font (spec §3, D42/D43): feature-gated like local OCR but
	// with one knob, not three. Unset ⇒ the attachment feature stays off;
	// publish without attachments is unaffected either way (D43). This
	// package is DB-free (internal/report), so there is nothing to store on
	// a Deps struct here — the publish send job (a later task) resolves
	// cfg.ReportFontPath itself when it calls report.Build. This block's
	// only job is the startup-time sanity check: report.CheckFont actually
	// loads the font through fpdf (catching the "readable file but not a
	// real TrueType font" case — see internal/report/layout.go's newDoc doc
	// comment) so a misconfigured ADAMARKER_REPORT_FONT is a loud warning at
	// boot, not a silent failure the first time a TA tries to publish with
	// attachments.
	if cfg.ReportFontConfigured() {
		if err := report.CheckFont(cfg.ReportFontPath); err != nil {
			logger.Warn("report PDF attachments disabled: font failed to load",
				"font_path", cfg.ReportFontPath, "err", err)
		} else {
			logger.Info("report PDF attachments enabled", "font_path", cfg.ReportFontPath)
		}
	}
	// Typst renderer (typst-report spec 2026-07-20): sanity-check the binary
	// at boot so a typo'd ADAMARKER_TYPST_BIN warns loudly instead of every
	// send silently falling back to fpdf.
	if cfg.TypstBinPath != "" {
		switch {
		case !cfg.ReportFontConfigured():
			logger.Warn("ADAMARKER_TYPST_BIN is set but ADAMARKER_REPORT_FONT is not — attachments (and the Typst renderer) stay disabled")
		default:
			if _, err := os.Stat(cfg.TypstBinPath); err != nil {
				logger.Warn("Typst report renderer disabled: binary not found — PDF attachments fall back to fpdf",
					"typst_bin", cfg.TypstBinPath, "err", err)
			} else {
				logger.Info("Typst report renderer enabled (LaTeX math typeset via mitex)", "typst_bin", cfg.TypstBinPath)
			}
		}
	}

	// Email (publish-email-regrade spec §3, D31). Constructed here so the
	// none-in-production default gets a loud startup warning. The send pipeline
	// (spec §3) is a River job on a dedicated rate-limited "email" queue: the
	// publish.Sender is the worker's seam. With provider "none", deliberately do NOT
	// register the email worker: durable jobs left by an earlier enabled-provider run
	// must remain queued until delivery is re-enabled, not be consumed by NoneProvider
	// and falsely marked sent.
	emailProvider, err := buildEmailProvider(cfg, logger)
	if err != nil {
		return err
	}
	tokenKey := secrets.Derive(secretKey, "regrade-token-v1")
	var emailSender queue.EmailSender
	if cfg.Email.Provider != "none" {
		emailSender = publish.NewSender(st, emailProvider, tokenKey, cfg.RegradeWindow, cfg.Email.ReplyDomain, logger, blobs, cfg.ReportFontPath, cfg.TypstBinPath)
	}

	if runMigrations {
		if err := queue.Migrate(ctx, st.Pool); err != nil {
			return err
		}
	}
	qc, err := queue.New(st.Pool, queue.Deps{
		Runner: runner, Scans: scans, Ingest: ing,
		Email: emailSender, EmailRate: cfg.Email.Rate,
	}, logger)
	if err != nil {
		return err
	}

	// Start workers on a context DETACHED from the signal (F17): the signal must not
	// insta-cancel in-flight jobs. Shutdown (below) drives the graceful drain
	// explicitly on ctx.Done; the errCh branch below calls Stop explicitly for the
	// bind/serve-failure teardown path. Both exits already tear the queue down
	// exactly once, so no additional deferred Stop is needed here (a prior version
	// had one, which double-stopped on every exit path).
	if err := qc.Start(context.Background()); err != nil {
		return err
	}

	if err := grading.EnsureSeeds(ctx, st, logger); err != nil {
		return err
	}

	spa, err := web.Handler()
	if err != nil {
		return err
	}
	api := httpapi.New(cfg, httpapi.Deps{
		Store: st, Ingest: ing, Scans: scans, Queue: qc,
		Providers: source, SecretKey: secretKey, Logger: logger,
		EmailProvider: emailProvider,
	})

	// F5: no global ReadTimeout/WriteTimeout. In HTTP/1.x both are anchored at the
	// request-header read and span the entire body read + response write, so a 30 s
	// ReadTimeout kills a healthy large zip/submissions upload mid-body. Per-request
	// read/write deadlines are set instead by httpapi's deadlineMiddleware (30 s/60 s
	// default; the upload handlers extend to 20 min before reading the body).
	// ReadHeaderTimeout + IdleTimeout stay on the server so slow-loris (slow headers,
	// idle keep-alives) is still guarded.
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(spa),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening",
			"addr", cfg.HTTPAddr,
			"env", string(cfg.Env),
			"hosted_domain", cfg.HostedDomain,
			"google_oauth", cfg.GoogleClientID != "",
			"dev_login", cfg.DevLogin,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		// Bind/serve failure: still tear the queue down (bounded) so we don't leak
		// River goroutines on the way out, then surface the fatal error.
		stopCtx, cancel := context.WithTimeout(context.Background(), hardShutdownGrace)
		defer cancel()
		_ = qc.Stop(stopCtx)
		return err
	case <-ctx.Done():
		logger.Info("shutting down: draining jobs", "drain", cfg.ShutdownDrain)
		// Close HTTP first (bounded) — new requests should stop immediately — then
		// drain the queue. The process exits only after the queue shutdown returns,
		// so an in-flight grading/identify job gets its full drain window. NOTE:
		// systemd's TimeoutStopSec must exceed ADAMARKER_SHUTDOWN_DRAIN, or SIGKILL
		// arrives mid-drain (see .env.adamarker.example).
		httpCtx, cancelHTTP := context.WithTimeout(context.Background(), httpShutdownGrace)
		defer cancelHTTP()
		if err := srv.Shutdown(httpCtx); err != nil {
			logger.Warn("http shutdown", "err", err)
		}
		return qc.Shutdown(context.Background(), cfg.ShutdownDrain)
	}
}

// Shutdown grace windows (F17). httpShutdownGrace bounds draining in-flight HTTP
// requests; hardShutdownGrace bounds the fatal-path queue teardown. The graceful
// queue-drain window itself is ADAMARKER_SHUTDOWN_DRAIN (cfg.ShutdownDrain).
const (
	httpShutdownGrace = 10 * time.Second
	hardShutdownGrace = 15 * time.Second
)

// localOCRFix is the operator remedy every local-OCR-absent warning carries:
// exactly what to run and which env vars to set (mirrors `make ocr-models`'
// own next-steps output and .env.adamarker.example's D24 section).
const localOCRFix = "run `make ocr-models`, install onnxruntime >= 1.27, then set ADAMARKER_OCR_MODEL, ADAMARKER_OCR_KEYS and ADAMARKER_ONNXRUNTIME together (see .env.adamarker.example)"

// localOCRConsequence spells out what a missing local reader actually means —
// the masking feature exists to keep student identity off cloud providers, so
// this rung being absent must never be a quiet default.
const localOCRConsequence = "every scan identification will send student ID/name crops to the configured cloud provider"

// setupLocalOCR wires the local-OCR rung (D24) onto scans and returns a close
// func for the engine (a no-op when none was built).
//
// Loudness contract (privacy audit 2026-07-12): EVERY path that ends without
// a local reader — fully unconfigured, partially configured, or configured but
// unusable — logs a WARN naming the cloud consequence and the fix. Previously
// the fully-unset case logged nothing, which made the privacy-preserving rung
// silently absent while identify shipped identity crops to the cloud.
func setupLocalOCR(cfg config.Config, scans *scan.Service, logger *slog.Logger) func() {
	configured, partial := cfg.LocalOCRConfigured()
	switch {
	case configured:
		engine, err := localocr.New(localocr.Config{
			ModelPath:          cfg.OCRModelPath,
			KeysPath:           cfg.OCRKeysPath,
			ONNXRuntimeLibPath: cfg.ONNXRuntimeLibPath,
		})
		if err != nil {
			logger.Warn("local OCR unavailable: configured but failed to load — "+localOCRConsequence,
				"model_path", cfg.OCRModelPath,
				"keys_path", cfg.OCRKeysPath,
				"onnxruntime_path", cfg.ONNXRuntimeLibPath,
				"err", err,
				"fix", localOCRFix)
			return func() {}
		}
		scans.Local = engine
		logger.Info("local OCR enabled — identification reads ID crops on this machine first", "model_path", cfg.OCRModelPath)
		return func() { _ = engine.Close() }
	case partial:
		logger.Warn("local OCR unavailable: ADAMARKER_OCR_MODEL/ADAMARKER_OCR_KEYS/ADAMARKER_ONNXRUNTIME must be set together — "+localOCRConsequence,
			"model_path", cfg.OCRModelPath,
			"keys_path", cfg.OCRKeysPath,
			"onnxruntime_path", cfg.ONNXRuntimeLibPath,
			"fix", localOCRFix)
	default:
		logger.Warn("local OCR is not installed — "+localOCRConsequence,
			"fix", localOCRFix)
	}
	return func() {}
}

// buildEmailProvider constructs the domain.EmailProvider selected by
// cfg.Email.Provider (publish-email-regrade spec §3, D31). It is the single
// wiring point a later publish task (send pipeline, River queue, publish
// handlers) will consume — kept separate from httpapi.Deps because that
// struct's field would be premature before the publish task defines how it's
// actually used (queue job vs. direct handler dependency).
//
// The "none" provider absent an explicit choice in production is not a config
// error (config.Load already accepts it) but IS a loud operational warning
// here: it means publish will run and silently mark every item "skipped"
// with no email sent, which is easy to miss in a log stream otherwise.
func buildEmailProvider(cfg config.Config, logger *slog.Logger) (domain.EmailProvider, error) {
	if cfg.Env == config.EnvProduction && cfg.Email.Provider == "none" {
		logger.Warn("ADAMARKER_EMAIL_PROVIDER is unset (none) in production — publishing will mark every item skipped and send no student email; set file|smtp|postmark to enable delivery")
	}
	// I4: smtp advertises a regrade Reply-To (regrade+<token>@<reply-domain>) that only
	// the postmark provider can ever receive — smtp has no inbound webhook. Warn loudly
	// but don't refuse: an operator may deliberately run smtp without inbound, in which
	// case they should unset ADAMARKER_EMAIL_REPLY_DOMAIN so emails say replies are not
	// monitored (email.Config gates Reply-To on that field).
	if cfg.Email.Provider == "smtp" && cfg.Email.ReplyDomain != "" {
		logger.Warn("ADAMARKER_EMAIL_REPLY_DOMAIN is set with the smtp provider, but smtp cannot parse inbound replies — grade emails will advertise a regrade Reply-To no webhook can receive, so students' regrade replies are silently lost; use postmark for the regrade channel, or unset the reply domain")
	}
	provider, err := email.New(cfg.Email)
	if err != nil {
		return nil, fmt.Errorf("email provider: %w", err)
	}
	logger.Info("email provider configured", "provider", cfg.Email.Provider)
	return provider, nil
}

// bootstrapStore adapts store to auth.BootstrapStore.
type bootstrapStore struct{ st *store.Store }

func (b bootstrapStore) CountActiveAdmins(ctx context.Context) (int64, error) {
	return b.st.Q.CountActiveAdmins(ctx)
}

func (b bootstrapStore) UpsertActiveAdmin(ctx context.Context, email string) error {
	_, err := b.st.Q.UpsertActiveAdmin(ctx, email)
	return err
}
