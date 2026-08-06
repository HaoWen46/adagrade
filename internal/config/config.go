// Package config loads ADA-Marker's runtime configuration from the environment.
//
// Load is a pure function of a getenv callback so it can be unit-tested without
// touching the real process environment. Secrets (OAuth client secret, provider API
// keys) are intentionally NOT given defaults — the app must fail loudly rather than
// boot with a guessable secret.
package config

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/HaoWen46/adagrade/internal/email"
)

// Environment distinguishes local development from a real deployment. Some safety
// checks (e.g. requiring a database URL and OAuth credentials) only bind in production.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvProduction  Environment = "production"
)

// ProviderKind selects the wire protocol an LLM provider speaks.
type ProviderKind string

const (
	// ProviderKindAnthropicCompat is the Anthropic Messages API shape, spoken natively
	// by Anthropic and by DeepSeek/Qwen compatibility endpoints (docs/DECISIONS.md D11).
	ProviderKindAnthropicCompat ProviderKind = "anthropic-compat"
	// ProviderKindOpenAICompat is the OpenAI Chat Completions shape (OpenRouter,
	// OpenAI, and most self-hosted gateways).
	ProviderKindOpenAICompat ProviderKind = "openai-compat"
)

func validProviderKind(k ProviderKind) bool {
	return k == ProviderKindAnthropicCompat || k == ProviderKindOpenAICompat
}

// Provider is one configured vision-LLM endpoint. Credentials come from env only —
// never the database.
type Provider struct {
	Name    string
	Kind    ProviderKind
	BaseURL string
	APIKey  string
}

// Config is the fully-resolved configuration the process runs with.
type Config struct {
	Env          Environment
	HTTPAddr     string
	DatabaseURL  string
	HostedDomain string // Google Workspace hd claim to enforce (see internal/auth).
	BlobDir      string // local-disk root for source PDFs + rendered/masked JPGs.

	// Google OAuth (optional production auth path; email magic-link login can be
	// used instead. Development can also use the double-gated dev login —
	// docs/DECISIONS.md D7/D8).
	GoogleClientID     string
	GoogleClientSecret string
	OAuthRedirectURL   string

	// BootstrapAdminEmail, when set, is upserted as an active admin at startup if no
	// active admin exists — fixes the fresh-deploy allowlist lockout (D8).
	BootstrapAdminEmail string

	// DevLogin enables POST /auth/dev-login. Only honored in development; setting it
	// in production is a configuration error, not a no-op.
	DevLogin bool

	// AllowMultipleWorkers is the escape hatch for the single-worker-fleet guard
	// (docs/DECISIONS.md D26). By default a process that cannot take the
	// worker-fleet advisory lock is fatal — this is the fix for the CRITICAL audit
	// finding where stale ./bin/adamarker zombies graded new jobs with old code
	// against the shared dev DB. Setting ADAMARKER_ALLOW_MULTIPLE_WORKERS=1
	// downgrades that fatal to a loud warning and lets the process continue, for
	// deliberate multi-instance experiments only.
	AllowMultipleWorkers bool

	// ShutdownDrain is the graceful-shutdown drain window (F17). On SIGTERM the
	// process soft-stops the River client — it stops fetching new jobs but lets
	// in-flight ones finish — for up to this long before escalating to a hard
	// cancel. It defaults to 5m30s, just above the 5m longest job timeout, so a
	// mid-flight grading/identify call is never insta-cancelled and re-recorded as
	// a terminal failure. systemd's TimeoutStopSec must exceed this, or SIGKILL
	// wins the race. Override with ADAMARKER_SHUTDOWN_DRAIN (a Go duration string).
	ShutdownDrain time.Duration

	// Providers are vision-LLM endpoints detected from the environment. Since D11 v1
	// they are only used to seed the llm_providers table ONCE on an empty database;
	// day-to-day management happens in the app UI.
	Providers []Provider

	// SecretKeyFile is the machine-local master key encrypting stored credentials
	// (D16). Auto-generated on first boot.
	SecretKeyFile string

	// Local OCR (D24): the first identification rung, entirely offline. All three
	// are optional and never required — see LocalOCRConfigured.
	OCRModelPath       string // path to the PP-OCRv5 server rec .onnx model (PP-OCRv5_server_rec_infer.onnx)
	OCRKeysPath        string // path to the charset dict .txt (ppocrv5_dict.txt; the class count is validated against it)
	ONNXRuntimeLibPath string // path to the libonnxruntime shared library

	// Email (publish-email-regrade spec §3, D31). Email.Provider selects
	// file|smtp|postmark|none; see resolveEmailConfig for the dev/prod defaults.
	Email email.Config

	// RegradeWindow is how long a signed regrade token stays valid after send
	// (spec §4, D32). Default 336h (14 days).
	RegradeWindow time.Duration

	// RegradeMax is the turn budget for the regrade v2 chain (spec §4, D57):
	// turns 1..RegradeMax are system-adjudicated; turn RegradeMax+1 (consuming
	// result #RegradeMax's token) fires the TA handoff instead of another
	// adjudication round. Read at receipt time, so in-flight tokens carry their
	// own turn and a mid-term change of this value stays coherent. Default 3.
	RegradeMax int

	// InboundWebhookSecret is the path secret for
	// POST /webhooks/email/inbound/{secret} (spec §5, D33). Empty means the
	// webhook route 404s unconditionally — there is no "open" default.
	InboundWebhookSecret string

	// MonthlyBudgetUSD is the monthly global spend cap compared against
	// month-to-date grading cost (trust spec §3, D36). A decimal string, never
	// float64 (docs/DECISIONS.md D4) — empty means no cap configured.
	MonthlyBudgetUSD string

	// ReportFontPath is the path to a UTF-8 TTF (Noto Sans TC, `make
	// report-fonts`) used to embed CJK glyphs in the per-student result PDF
	// (spec §3, D42). Feature-gated like local OCR (D24) but with a single
	// knob, not three: unset means the report/attachment feature is off
	// entirely (D43) — publish without attachments still works either way.
	ReportFontPath string

	// TypstBinPath is the path to a Typst executable (typst-report spec
	// 2026-07-20). When set, the per-student result PDF is rendered by Typst
	// — LaTeX math in criterion names and problem comments typesets via the
	// mitex package instead of appearing as raw source — with the fpdf
	// renderer as the automatic fallback on any compile failure. Unset means
	// the fpdf renderer runs exactly as before. Requires ReportFontPath to be
	// set too (the attachments feature gate is unchanged).
	TypstBinPath string

	// TectonicBinPath is the path to a tectonic executable (latex-transcription-
	// export spec 2026-07-25 §6). When set, every exported .tex is compiled
	// before it ships, so a document that does not build is reported as failed
	// instead of handed over silently. Unset means the .tex still exports but is
	// marked `unverified` in the bundle manifest — never presented as checked.
	TectonicBinPath string

	// TranscribeProvider / TranscribeModel select the vision model used by the
	// LaTeX transcription export. They are deliberately separate from the
	// grading method's model: transcription is not grading, and the operator
	// must be able to change one without perturbing D5's reproducibility
	// contract for graded records. Empty provider falls back to the first
	// enabled provider; empty model falls back to TranscribeModelDefault.
	TranscribeProvider string
	TranscribeModel    string

	// AppBaseURL is the deployed app's absolute origin (e.g.
	// https://ada.csie.ntu.edu.tw), used to build absolute deep links in outbound
	// mail — currently the regrade v2 TA-notify handoff email's "open in app" link
	// (whole-branch review F4). Empty means no base URL is configured: callers must
	// drop the link line entirely rather than emit a bare relative path (dead in
	// any mail client). Any trailing slash is trimmed at load time so callers can
	// unconditionally concatenate AppBaseURL + "/regrades/{id}".
	AppBaseURL string

	// EmailLoginTrustRequestHost allows production email-login links to be built
	// from the incoming request host instead of AppBaseURL. This is only intended
	// for temporary HTTPS tunnels whose public hostname changes during testing.
	EmailLoginTrustRequestHost bool
}

// ReportFontConfigured reports whether the report PDF's font is available —
// simply whether ReportFontPath is non-empty. Unlike LocalOCRConfigured there
// is no "partial" state: this is one env var, not three.
func (c Config) ReportFontConfigured() bool {
	return c.ReportFontPath != ""
}

// LocalOCRConfigured reports the state of the three local-OCR env vars:
// configured is true only when all three are set (the engine can be built);
// partial is true when some but not all three are set (a caller should warn
// and leave the feature off, per D24 "no hard failure").
func (c Config) LocalOCRConfigured() (configured bool, partial bool) {
	n := 0
	if c.OCRModelPath != "" {
		n++
	}
	if c.OCRKeysPath != "" {
		n++
	}
	if c.ONNXRuntimeLibPath != "" {
		n++
	}
	switch n {
	case 3:
		return true, false
	case 0:
		return false, false
	default:
		return false, true
	}
}

// Env var names, kept in one place.
const (
	envEnv                = "ADAMARKER_ENV"
	envHTTPAddr           = "ADAMARKER_HTTP_ADDR"
	envDatabaseURL        = "ADAMARKER_DATABASE_URL"
	envHostedDomain       = "ADAMARKER_HOSTED_DOMAIN"
	envBlobDir            = "ADAMARKER_BLOB_DIR"
	envGoogleClientID     = "ADAMARKER_GOOGLE_CLIENT_ID"
	envGoogleClientSecret = "ADAMARKER_GOOGLE_CLIENT_SECRET"
	envOAuthRedirectURL   = "ADAMARKER_OAUTH_REDIRECT_URL"
	envBootstrapAdmin     = "ADAMARKER_BOOTSTRAP_ADMIN_EMAIL"
	envDevLogin           = "ADAMARKER_DEV_LOGIN"
	envAllowMultiWorkers  = "ADAMARKER_ALLOW_MULTIPLE_WORKERS"
	envShutdownDrain      = "ADAMARKER_SHUTDOWN_DRAIN"
	envProviders          = "ADAMARKER_PROVIDERS"
	envSecretKeyFile      = "ADAMARKER_SECRET_KEY_FILE"

	// Local OCR (D24) — all optional, see Config.LocalOCRConfigured.
	envOCRModel           = "ADAMARKER_OCR_MODEL"
	envOCRKeys            = "ADAMARKER_OCR_KEYS"
	envONNXRuntimeLibPath = "ADAMARKER_ONNXRUNTIME"

	// Email (publish-email-regrade spec §3, D31).
	envEmailProvider = "ADAMARKER_EMAIL_PROVIDER"
	envEmailFrom     = "ADAMARKER_EMAIL_FROM"
	envEmailReplyDom = "ADAMARKER_EMAIL_REPLY_DOMAIN"
	envSMTPHost      = "ADAMARKER_SMTP_HOST"
	envSMTPPort      = "ADAMARKER_SMTP_PORT"
	envSMTPUser      = "ADAMARKER_SMTP_USER"
	envSMTPPass      = "ADAMARKER_SMTP_PASS"
	envPostmarkToken = "ADAMARKER_POSTMARK_TOKEN"
	envEmailRate     = "ADAMARKER_EMAIL_RATE"

	// Regrade (spec §4-6, D32-D33, D57).
	envRegradeWindow        = "ADAMARKER_REGRADE_WINDOW"
	envRegradeMax           = "ADAMARKER_REGRADE_MAX"
	envInboundWebhookSecret = "ADAMARKER_INBOUND_WEBHOOK_SECRET"

	// Monthly budget cap (trust spec §3, D36).
	envMonthlyBudgetUSD = "ADAMARKER_MONTHLY_BUDGET_USD"

	// Report PDF font (spec §3, D42/D43) — see Config.ReportFontPath.
	envReportFont = "ADAMARKER_REPORT_FONT"
	envTypstBin   = "ADAMARKER_TYPST_BIN"

	// LaTeX transcription export (spec 2026-07-25) — see Config.TectonicBinPath.
	envTectonicBin        = "ADAMARKER_TECTONIC_BIN"
	envTranscribeProvider = "ADAMARKER_TRANSCRIBE_PROVIDER"
	envTranscribeModel    = "ADAMARKER_TRANSCRIBE_MODEL"

	// App base URL (whole-branch review F4) — see Config.AppBaseURL.
	envAppBaseURL                 = "ADAMARKER_APP_BASE_URL"
	envEmailLoginTrustRequestHost = "ADAMARKER_EMAIL_LOGIN_TRUST_REQUEST_HOST"
)

// Load resolves configuration from getenv, applying defaults and validating.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		Env:                        Environment(firstNonEmpty(getenv(envEnv), string(EnvDevelopment))),
		HTTPAddr:                   firstNonEmpty(getenv(envHTTPAddr), ":8080"),
		DatabaseURL:                getenv(envDatabaseURL),
		HostedDomain:               firstNonEmpty(getenv(envHostedDomain), "ntu.edu.tw"),
		BlobDir:                    firstNonEmpty(getenv(envBlobDir), "./data/blobs"),
		GoogleClientID:             getenv(envGoogleClientID),
		GoogleClientSecret:         getenv(envGoogleClientSecret),
		OAuthRedirectURL:           firstNonEmpty(getenv(envOAuthRedirectURL), "http://localhost:8080/auth/callback"),
		BootstrapAdminEmail:        strings.ToLower(strings.TrimSpace(getenv(envBootstrapAdmin))),
		DevLogin:                   getenv(envDevLogin) != "",
		AllowMultipleWorkers:       getenv(envAllowMultiWorkers) != "",
		SecretKeyFile:              firstNonEmpty(getenv(envSecretKeyFile), "./data/secret.key"),
		OCRModelPath:               getenv(envOCRModel),
		OCRKeysPath:                getenv(envOCRKeys),
		ONNXRuntimeLibPath:         getenv(envONNXRuntimeLibPath),
		ReportFontPath:             getenv(envReportFont),
		TypstBinPath:               getenv(envTypstBin),
		TectonicBinPath:            getenv(envTectonicBin),
		TranscribeProvider:         strings.TrimSpace(getenv(envTranscribeProvider)),
		TranscribeModel:            strings.TrimSpace(getenv(envTranscribeModel)),
		AppBaseURL:                 strings.TrimSuffix(strings.TrimSpace(getenv(envAppBaseURL)), "/"),
		EmailLoginTrustRequestHost: getenv(envEmailLoginTrustRequestHost) != "",
	}

	drain, err := parseShutdownDrain(getenv(envShutdownDrain))
	if err != nil {
		return Config{}, err
	}
	c.ShutdownDrain = drain

	providers, err := loadProviders(getenv)
	if err != nil {
		return Config{}, err
	}
	c.Providers = providers

	c.Email = resolveEmailConfig(getenv, c.Env, c.BlobDir)

	rate, err := parseEmailRate(getenv(envEmailRate))
	if err != nil {
		return Config{}, err
	}
	c.Email.Rate = rate

	regradeWindow, err := parseRegradeWindow(getenv(envRegradeWindow))
	if err != nil {
		return Config{}, err
	}
	c.RegradeWindow = regradeWindow

	regradeMax, err := parseRegradeMax(getenv(envRegradeMax))
	if err != nil {
		return Config{}, err
	}
	c.RegradeMax = regradeMax

	c.InboundWebhookSecret = getenv(envInboundWebhookSecret)

	budget, err := parseMonthlyBudgetUSD(getenv(envMonthlyBudgetUSD))
	if err != nil {
		return Config{}, err
	}
	c.MonthlyBudgetUSD = budget

	if err := validate(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// resolveEmailConfig builds the email.Config subset that Load can determine
// without validation (provider selection + the raw field pass-through).
// Rate is filled in separately by parseEmailRate since it needs error
// handling Load already threads through.
//
// Provider selection (spec §3, D31): an explicit ADAMARKER_EMAIL_PROVIDER
// always wins. Left unset, development defaults to "file" writing under
// "<BlobDir>/../outbox" (a sibling of the blob dir, per spec wording) so a
// fresh dev checkout can publish without any configuration; production
// defaults to "none" — never file, which would silently drop real student
// email into a local directory. The "none" default does NOT fail Load; the
// caller (main.go) is responsible for the loud startup warning per spec §3,
// since Load has no logger.
func resolveEmailConfig(getenv func(string) string, env Environment, blobDir string) email.Config {
	defaultProvider := "none"
	if env == EnvDevelopment {
		defaultProvider = "file"
	}
	cfg := email.Config{
		Provider:      firstNonEmpty(getenv(envEmailProvider), defaultProvider),
		From:          getenv(envEmailFrom),
		ReplyDomain:   getenv(envEmailReplyDom),
		SMTPHost:      getenv(envSMTPHost),
		SMTPPort:      firstNonEmpty(getenv(envSMTPPort), "587"),
		SMTPUser:      getenv(envSMTPUser),
		SMTPPass:      getenv(envSMTPPass),
		PostmarkToken: getenv(envPostmarkToken),
	}
	if cfg.Provider == "file" {
		cfg.OutboxDir = firstNonEmpty(getenv("ADAMARKER_EMAIL_OUTBOX_DIR"), filepath.Join(filepath.Dir(blobDir), "outbox"))
		// Development only: the file provider may parse simulated Postmark
		// inbound payloads so regrade replies can be exercised locally. Never
		// enabled in production — the file provider has no real webhook.
		cfg.DevInbound = env == EnvDevelopment
	}
	return cfg
}

// Email rates are sends/sec for the outbound queue. The worker has a fixed two-minute
// job budget; keeping limiter delay at or below one minute leaves at least one minute
// for provider IO and prevents an impossible low rate from snoozing in a tight loop.
const (
	defaultEmailRate = 1.0
	minEmailRate     = 1.0 / 60.0 // one message per minute
)

// parseEmailRate resolves ADAMARKER_EMAIL_RATE. Empty ⇒ the default; malformed
// or non-positive fails loudly rather than silently falling back — a bad rate
// could otherwise blow past a provider's throughput limit unnoticed.
func parseEmailRate(raw string) (float64, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultEmailRate, nil
	}
	rate, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s %q is not a valid number: %w", envEmailRate, raw, err)
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return 0, fmt.Errorf("config: %s must be positive, got %v", envEmailRate, rate)
	}
	if rate < minEmailRate {
		return 0, fmt.Errorf("config: %s must be at least %.6g sends/sec (one message per minute), got %v", envEmailRate, minEmailRate, rate)
	}
	return rate, nil
}

// defaultRegradeWindow is how long a signed regrade token stays valid after
// send (spec §4, D32): 14 days.
const defaultRegradeWindow = 336 * time.Hour

// parseRegradeWindow resolves ADAMARKER_REGRADE_WINDOW. Empty ⇒ the default; a
// malformed duration fails loudly (mirrors parseShutdownDrain).
func parseRegradeWindow(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultRegradeWindow, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("config: %s %q is not a valid duration (e.g. 336h): %w", envRegradeWindow, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: %s must be positive, got %v", envRegradeWindow, d)
	}
	return d, nil
}

// defaultRegradeMax is the rate cap on verified regrade requests per
// (student, assessment) — spec §5, D33.
const defaultRegradeMax = 3

// parseRegradeMax resolves ADAMARKER_REGRADE_MAX. Empty ⇒ the default;
// malformed or non-positive fails loudly.
func parseRegradeMax(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultRegradeMax, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("config: %s %q is not a valid integer: %w", envRegradeMax, raw, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("config: %s must be positive, got %d", envRegradeMax, n)
	}
	return n, nil
}

// parseMonthlyBudgetUSD resolves ADAMARKER_MONTHLY_BUDGET_USD. Empty ⇒ no cap
// configured (the trust-spec §3 "unconfigured behaves exactly as today"
// escape hatch). When set it must be a plain positive decimal string — never
// float64 on this path (docs/DECISIONS.md D4) — matching the numeric.go
// convention (D4). Accepts only plain decimals (optional digits, optional single
// dot, at least one digit overall) to reject scientific notation ("1e2") and hex
// ("0x1p0") that strconv.ParseFloat would accept but numeric.Num would reject.
func parseMonthlyBudgetUSD(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}

	// Validate plain decimal format: ^[0-9]+(\.[0-9]+)?$
	// Must have at least one digit; optionally a dot followed by more digits.
	if !isPlainDecimal(trimmed) {
		return "", fmt.Errorf("config: %s %q is not a valid decimal amount", envMonthlyBudgetUSD, trimmed)
	}

	// Parse and check positivity.
	v, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return "", fmt.Errorf("config: %s %q is not a valid decimal amount: %w", envMonthlyBudgetUSD, trimmed, err)
	}
	if v <= 0 {
		return "", fmt.Errorf("config: %s must be positive when set, got %q", envMonthlyBudgetUSD, trimmed)
	}
	return trimmed, nil
}

// isPlainDecimal reports whether s matches ^[0-9]+(\.[0-9]+)?$: one or more
// digits, optionally followed by a single dot and one or more digits.
func isPlainDecimal(s string) bool {
	if s == "" {
		return false
	}
	dotCount := 0
	digitSeen := false
	for i, ch := range s {
		switch {
		case ch >= '0' && ch <= '9':
			digitSeen = true
		case ch == '.':
			dotCount++
			if dotCount > 1 {
				return false // multiple dots
			}
			if i == 0 || i == len(s)-1 {
				return false // dot at start or end
			}
		default:
			return false // non-digit, non-dot
		}
	}
	return digitSeen // reject empty or dot-only strings
}

// defaultShutdownDrain is the F17 drain window default: just above the 5m
// longest job timeout so an in-flight grading/identify call finishes rather
// than being insta-cancelled on SIGTERM.
const defaultShutdownDrain = 5*time.Minute + 30*time.Second

// parseShutdownDrain resolves ADAMARKER_SHUTDOWN_DRAIN. Empty ⇒ the default; a
// malformed duration is a loud error (a silent fallback could shrink the window
// and re-introduce the very insta-cancel F17 fixes). A non-positive value is
// rejected — a zero/negative drain would skip the soft stop entirely.
func parseShutdownDrain(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultShutdownDrain, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("config: %s %q is not a valid duration (e.g. 5m30s): %w", envShutdownDrain, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: %s must be positive, got %v", envShutdownDrain, d)
	}
	return d, nil
}

// LoadProviders resolves the LLM provider table from environment variables
// alone: ADAMARKER_PROVIDERS with its per-provider
// ADAMARKER_PROVIDER_<NAME>_{KIND,BASE_URL,API_KEY}, or — when that list is
// empty — the auto-detected vendor keys (DEEPSEEK_API_KEY, QWEN_API_KEY,
// OPENROUTER_API_KEY, each with an optional <VENDOR>_BASE_URL override). See
// loadProviders below for the exact rules; this is a thin export of it.
//
// It touches no database and needs no ADAMARKER_DATABASE_URL, and it skips the
// rest of Load — including the production validation that DOES require a
// database URL. Offline tooling uses it to construct an LLM client without the
// server's app-managed registry (D11 v1), which is unreachable without a
// database.
//
// No configured keys is not an error: it returns an empty table and nil, and
// the caller decides whether it can proceed with nothing.
func LoadProviders(getenv func(string) string) ([]Provider, error) {
	return loadProviders(getenv)
}

// loadProviders resolves LLM providers. An explicit ADAMARKER_PROVIDERS list wins;
// otherwise well-known vendor keys (DEEPSEEK_API_KEY, QWEN_API_KEY) are auto-detected
// with their documented default base URLs (D11).
func loadProviders(getenv func(string) string) ([]Provider, error) {
	if list := getenv(envProviders); list != "" {
		var out []Provider
		for _, raw := range strings.Split(list, ",") {
			name := strings.ToLower(strings.TrimSpace(raw))
			if name == "" {
				continue
			}
			upper := strings.ToUpper(name)
			p := Provider{
				Name:    name,
				Kind:    ProviderKind(firstNonEmpty(getenv("ADAMARKER_PROVIDER_"+upper+"_KIND"), string(ProviderKindAnthropicCompat))),
				BaseURL: getenv("ADAMARKER_PROVIDER_" + upper + "_BASE_URL"),
				APIKey:  getenv("ADAMARKER_PROVIDER_" + upper + "_API_KEY"),
			}
			if p.BaseURL == "" || p.APIKey == "" {
				return nil, fmt.Errorf("config: provider %q listed in %s but ADAMARKER_PROVIDER_%s_BASE_URL/_API_KEY incomplete", name, envProviders, upper)
			}
			if !validProviderKind(p.Kind) {
				return nil, fmt.Errorf("config: provider %q has invalid kind %q (want anthropic-compat|openai-compat)", name, p.Kind)
			}
			out = append(out, p)
		}
		return out, nil
	}

	var out []Provider
	if key := getenv("DEEPSEEK_API_KEY"); key != "" {
		out = append(out, Provider{
			Name:    "deepseek",
			Kind:    ProviderKindAnthropicCompat,
			BaseURL: firstNonEmpty(getenv("DEEPSEEK_BASE_URL"), "https://api.deepseek.com/anthropic"),
			APIKey:  key,
		})
	}
	if key := getenv("QWEN_API_KEY"); key != "" {
		out = append(out, Provider{
			Name:    "qwen",
			Kind:    ProviderKindAnthropicCompat,
			BaseURL: firstNonEmpty(getenv("QWEN_BASE_URL"), "https://dashscope-intl.aliyuncs.com/apps/anthropic"),
			APIKey:  key,
		})
	}
	if key := getenv("OPENROUTER_API_KEY"); key != "" {
		out = append(out, Provider{
			Name:    "openrouter",
			Kind:    ProviderKindOpenAICompat,
			BaseURL: firstNonEmpty(getenv("OPENROUTER_BASE_URL"), "https://openrouter.ai/api/v1"),
			APIKey:  key,
		})
	}
	return out, nil
}

func firstNonEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

var errMissingDatabaseURL = errors.New("config: " + envDatabaseURL + " is required in production")

func validate(c Config) error {
	switch c.Env {
	case EnvDevelopment, EnvProduction:
	default:
		return fmt.Errorf("config: invalid %s %q (want development|production)", envEnv, c.Env)
	}
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: %s must not be empty", envHTTPAddr)
	}
	if c.Env == EnvProduction {
		if c.DatabaseURL == "" {
			return errMissingDatabaseURL
		}
		googleConfigured := c.GoogleClientID != "" || c.GoogleClientSecret != "" || !strings.HasPrefix(c.OAuthRedirectURL, "http://localhost")
		emailLoginConfigured := c.Email.Provider != "none" && (c.AppBaseURL != "" || c.EmailLoginTrustRequestHost)
		if !googleConfigured && !emailLoginConfigured {
			return fmt.Errorf("config: production requires either Google OAuth (%s/%s/%s) or email login (%s != none)",
				envGoogleClientID, envGoogleClientSecret, envOAuthRedirectURL, envEmailProvider)
		}
		if googleConfigured {
			if c.GoogleClientID == "" || c.GoogleClientSecret == "" {
				return fmt.Errorf("config: Google OAuth requires %s and %s", envGoogleClientID, envGoogleClientSecret)
			}
			// The redirect URL always has a localhost default, so "unset in production"
			// surfaces as the default still being present.
			if strings.HasPrefix(c.OAuthRedirectURL, "http://localhost") {
				return fmt.Errorf("config: %s must be set to the real callback URL when Google OAuth is configured", envOAuthRedirectURL)
			}
		}
		if !googleConfigured && c.Email.Provider != "none" && c.AppBaseURL == "" && !c.EmailLoginTrustRequestHost {
			return fmt.Errorf("config: %s is required in production when email login is the configured auth path unless %s is set for temporary tunnel testing", envAppBaseURL, envEmailLoginTrustRequestHost)
		}
		if c.DevLogin {
			return fmt.Errorf("config: %s must not be set in production", envDevLogin)
		}
	}
	return validateEmail(c)
}

// validateEmail enforces the publish-email-regrade spec §3 rules: the
// provider name is always checked (a typo must fail loudly in every
// environment, mirroring internal/email.New's own "no silent fallback to
// none" contract), while the field-completeness checks for smtp/postmark are
// production-only — mirrors internal/email.New's own field checks
// (belt-and-braces per D31: New already returns config-mediated errors, but
// config should fail at boot, before a queue worker discovers it at send
// time). "none" and "file" pass through untouched in production — file is
// dev-only in practice but not forbidden (an operator temporarily routing to
// disk is their call, not config's to block); "none" is the accepted
// default, with main.go responsible for the loud startup warning since Load
// has no logger.
func validateEmail(c Config) error {
	switch c.Email.Provider {
	case "file", "smtp", "postmark", "none":
	default:
		return fmt.Errorf("config: %s %q is invalid (want file|smtp|postmark|none)", envEmailProvider, c.Email.Provider)
	}
	if c.Env != EnvProduction {
		return nil
	}
	switch c.Email.Provider {
	case "smtp":
		var missing []string
		if c.Email.SMTPHost == "" {
			missing = append(missing, envSMTPHost)
		}
		if c.Email.SMTPUser == "" {
			missing = append(missing, envSMTPUser)
		}
		if c.Email.SMTPPass == "" {
			missing = append(missing, envSMTPPass)
		}
		if c.Email.From == "" {
			missing = append(missing, envEmailFrom)
		}
		if len(missing) > 0 {
			return fmt.Errorf("config: %s=smtp in production requires %s", envEmailProvider, strings.Join(missing, ", "))
		}
	case "postmark":
		var missing []string
		if c.Email.PostmarkToken == "" {
			missing = append(missing, envPostmarkToken)
		}
		if c.Email.From == "" {
			missing = append(missing, envEmailFrom)
		}
		if len(missing) > 0 {
			return fmt.Errorf("config: %s=postmark in production requires %s", envEmailProvider, strings.Join(missing, ", "))
		}
	}
	return nil
}
