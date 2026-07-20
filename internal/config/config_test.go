package config

import (
	"path/filepath"
	"testing"
	"time"
)

// envMap turns a map into a getenv callback; missing keys return "".
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_AppliesDefaultsWhenEnvEmpty(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Env != EnvDevelopment {
		t.Errorf("Env: got %q want %q", got.Env, EnvDevelopment)
	}
	if got.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr: got %q want %q", got.HTTPAddr, ":8080")
	}
	if got.HostedDomain != "ntu.edu.tw" {
		t.Errorf("HostedDomain: got %q want %q", got.HostedDomain, "ntu.edu.tw")
	}
	if got.BlobDir != "./data/blobs" {
		t.Errorf("BlobDir: got %q want %q", got.BlobDir, "./data/blobs")
	}
}

func TestLoad_OverridesFromEnv(t *testing.T) {
	got, err := Load(envMap(map[string]string{
		envEnv:                "production",
		envHTTPAddr:           ":9000",
		envDatabaseURL:        "postgres://localhost/adamarker",
		envHostedDomain:       "example.edu",
		envBlobDir:            "/srv/adamarker/blobs",
		envGoogleClientID:     "client-id.apps.googleusercontent.com",
		envGoogleClientSecret: "client-secret",
		envOAuthRedirectURL:   "https://marker.example.edu/auth/callback",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Env != EnvProduction {
		t.Errorf("Env: got %q want %q", got.Env, EnvProduction)
	}
	if got.HTTPAddr != ":9000" {
		t.Errorf("HTTPAddr: got %q want %q", got.HTTPAddr, ":9000")
	}
	if got.DatabaseURL != "postgres://localhost/adamarker" {
		t.Errorf("DatabaseURL: got %q", got.DatabaseURL)
	}
	if got.HostedDomain != "example.edu" {
		t.Errorf("HostedDomain: got %q", got.HostedDomain)
	}
}

func TestLoad_RejectsUnknownEnvironment(t *testing.T) {
	_, err := Load(envMap(map[string]string{envEnv: "staging"}))
	if err == nil {
		t.Fatal("expected error for unknown environment")
	}
}

func TestLoad_RequiresDatabaseURLInProduction(t *testing.T) {
	_, err := Load(envMap(map[string]string{envEnv: "production"}))
	if err == nil {
		t.Fatal("expected error when production is missing a database URL")
	}
}

func TestLoad_AllowsMissingDatabaseURLInDevelopment(t *testing.T) {
	_, err := Load(envMap(map[string]string{envEnv: "development"}))
	if err != nil {
		t.Fatalf("development should not require a database URL, got: %v", err)
	}
}

// prodEnv is a minimal valid production environment; tests mutate copies of it.
func prodEnv() map[string]string {
	return map[string]string{
		envEnv:                "production",
		envDatabaseURL:        "postgres://localhost/adamarker",
		envGoogleClientID:     "client-id.apps.googleusercontent.com",
		envGoogleClientSecret: "client-secret",
		envOAuthRedirectURL:   "https://marker.example.edu/auth/callback",
	}
}

func TestLoad_ProductionRequiresSomeAuth(t *testing.T) {
	_, err := Load(envMap(map[string]string{
		envEnv:         "production",
		envDatabaseURL: "postgres://localhost/adamarker",
	}))
	if err == nil {
		t.Fatal("production should require Google OAuth or email login")
	}
	if _, err := Load(envMap(prodEnv())); err != nil {
		t.Fatalf("complete Google production env should load, got: %v", err)
	}
}

func TestLoad_ProductionAllowsEmailLoginWithoutGoogleOAuth(t *testing.T) {
	_, err := Load(envMap(map[string]string{
		envEnv:           "production",
		envDatabaseURL:   "postgres://localhost/adamarker",
		envEmailProvider: "file",
		envAppBaseURL:    "https://marker.example.edu",
	}))
	if err != nil {
		t.Fatalf("email-login production env should load without Google OAuth, got: %v", err)
	}
}

func TestLoad_ProductionEmailLoginRequiresAppBaseURL(t *testing.T) {
	_, err := Load(envMap(map[string]string{
		envEnv:           "production",
		envDatabaseURL:   "postgres://localhost/adamarker",
		envEmailProvider: "file",
	}))
	if err == nil {
		t.Fatal("production email login should require app base URL")
	}
}

func TestLoad_ProductionAllowsEmailLoginWithTrustedRequestHost(t *testing.T) {
	got, err := Load(envMap(map[string]string{
		envEnv:                        "production",
		envDatabaseURL:                "postgres://localhost/adamarker",
		envEmailProvider:              "file",
		envEmailLoginTrustRequestHost: "1",
	}))
	if err != nil {
		t.Fatalf("production email login with trusted request host should load, got: %v", err)
	}
	if !got.EmailLoginTrustRequestHost {
		t.Fatal("EmailLoginTrustRequestHost should be true")
	}
	if got.AppBaseURL != "" {
		t.Fatalf("AppBaseURL should stay empty, got %q", got.AppBaseURL)
	}
}

func TestLoad_DevelopmentDoesNotRequireGoogleOAuth(t *testing.T) {
	got, err := Load(envMap(map[string]string{envEnv: "development"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.OAuthRedirectURL != "http://localhost:8080/auth/callback" {
		t.Errorf("OAuthRedirectURL default: got %q", got.OAuthRedirectURL)
	}
}

func TestLoad_DevLoginOnlyHonoredInDevelopment(t *testing.T) {
	got, err := Load(envMap(map[string]string{envEnv: "development", envDevLogin: "1"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.DevLogin {
		t.Error("DevLogin should be true in development with ADAMARKER_DEV_LOGIN=1")
	}

	m := prodEnv()
	m[envDevLogin] = "1"
	if _, err := Load(envMap(m)); err == nil {
		t.Error("production must reject ADAMARKER_DEV_LOGIN (fail loudly, not ignore)")
	}
}

// TestLoad_AllowMultipleWorkers covers the D26 escape hatch: unset (or empty)
// keeps the single-worker-fleet guard fatal-by-default; any non-empty value
// downgrades it to a warning. It is honored in every environment (unlike
// DevLogin) — deliberate multi-instance experiments can happen in prod too.
func TestLoad_AllowMultipleWorkers(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AllowMultipleWorkers {
		t.Error("AllowMultipleWorkers should default to false when unset")
	}

	got, err = Load(envMap(map[string]string{envAllowMultiWorkers: "1"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.AllowMultipleWorkers {
		t.Error("AllowMultipleWorkers should be true with ADAMARKER_ALLOW_MULTIPLE_WORKERS=1")
	}

	// Honored in production too (not rejected like DevLogin).
	m := prodEnv()
	m[envAllowMultiWorkers] = "1"
	got, err = Load(envMap(m))
	if err != nil {
		t.Fatalf("production with ADAMARKER_ALLOW_MULTIPLE_WORKERS should load, got: %v", err)
	}
	if !got.AllowMultipleWorkers {
		t.Error("AllowMultipleWorkers should be honored in production")
	}
}

// TestLoad_ShutdownDrainDefault covers the F17 graceful-shutdown drain window:
// unset falls back to the 5m30s default (just above the 5m longest job timeout),
// a valid duration string overrides it, and a malformed one fails loudly rather
// than silently reverting to the default (a too-short drain would insta-cancel
// in-flight grading calls, the very bug F17 fixes).
func TestLoad_ShutdownDrainDefault(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ShutdownDrain != 5*time.Minute+30*time.Second {
		t.Errorf("ShutdownDrain default: got %v want 5m30s", got.ShutdownDrain)
	}
}

func TestLoad_ShutdownDrainOverride(t *testing.T) {
	got, err := Load(envMap(map[string]string{envShutdownDrain: "90s"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ShutdownDrain != 90*time.Second {
		t.Errorf("ShutdownDrain override: got %v want 90s", got.ShutdownDrain)
	}
}

func TestLoad_ShutdownDrainMalformedFailsLoudly(t *testing.T) {
	if _, err := Load(envMap(map[string]string{envShutdownDrain: "5 minutes"})); err == nil {
		t.Fatal("malformed ADAMARKER_SHUTDOWN_DRAIN must fail loudly, not fall back to the default")
	}
}

func TestLoad_BootstrapAdminEmailIsNormalized(t *testing.T) {
	got, err := Load(envMap(map[string]string{envBootstrapAdmin: "  Admin@NTU.edu.tw "}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.BootstrapAdminEmail != "admin@ntu.edu.tw" {
		t.Errorf("BootstrapAdminEmail: got %q want %q", got.BootstrapAdminEmail, "admin@ntu.edu.tw")
	}
}

func TestLoad_ProviderAutoDetectionFromVendorKeys(t *testing.T) {
	got, err := Load(envMap(map[string]string{
		"DEEPSEEK_API_KEY": "sk-deepseek",
		"QWEN_API_KEY":     "sk-qwen",
		"QWEN_BASE_URL":    "https://dashscope-intl.aliyuncs.com/apps/anthropic",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Providers) != 2 {
		t.Fatalf("Providers: got %d want 2 (%+v)", len(got.Providers), got.Providers)
	}
	byName := map[string]Provider{}
	for _, p := range got.Providers {
		byName[p.Name] = p
	}
	ds, ok := byName["deepseek"]
	if !ok || ds.APIKey != "sk-deepseek" || ds.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("deepseek provider wrong: %+v", ds)
	}
	qw, ok := byName["qwen"]
	if !ok || qw.APIKey != "sk-qwen" || qw.BaseURL != "https://dashscope-intl.aliyuncs.com/apps/anthropic" {
		t.Errorf("qwen provider wrong: %+v", qw)
	}
	for _, p := range got.Providers {
		if p.Kind != ProviderKindAnthropicCompat {
			t.Errorf("provider %s kind: got %q", p.Name, p.Kind)
		}
	}
}

func TestLoad_OpenRouterAutoDetection(t *testing.T) {
	got, err := Load(envMap(map[string]string{"OPENROUTER_API_KEY": "sk-or-abc"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("Providers: got %d want 1 (%+v)", len(got.Providers), got.Providers)
	}
	p := got.Providers[0]
	if p.Name != "openrouter" || p.Kind != ProviderKindOpenAICompat ||
		p.BaseURL != "https://openrouter.ai/api/v1" || p.APIKey != "sk-or-abc" {
		t.Errorf("openrouter provider wrong: %+v", p)
	}
}

func TestLoad_ExplicitProviderKind(t *testing.T) {
	got, err := Load(envMap(map[string]string{
		"ADAMARKER_PROVIDERS":                 "gateway",
		"ADAMARKER_PROVIDER_GATEWAY_BASE_URL": "https://llm.example.com/v1",
		"ADAMARKER_PROVIDER_GATEWAY_API_KEY":  "sk-x",
		"ADAMARKER_PROVIDER_GATEWAY_KIND":     "openai-compat",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Providers[0].Kind != ProviderKindOpenAICompat {
		t.Errorf("explicit kind: %+v", got.Providers[0])
	}

	if _, err := Load(envMap(map[string]string{
		"ADAMARKER_PROVIDERS":                 "gateway",
		"ADAMARKER_PROVIDER_GATEWAY_BASE_URL": "https://llm.example.com/v1",
		"ADAMARKER_PROVIDER_GATEWAY_API_KEY":  "sk-x",
		"ADAMARKER_PROVIDER_GATEWAY_KIND":     "soap",
	})); err == nil {
		t.Error("invalid kind must fail loudly")
	}
}

func TestLoad_ExplicitProvidersOverrideAutoDetection(t *testing.T) {
	got, err := Load(envMap(map[string]string{
		"ADAMARKER_PROVIDERS":                  "myvendor",
		"ADAMARKER_PROVIDER_MYVENDOR_BASE_URL": "https://llm.example.com/anthropic",
		"ADAMARKER_PROVIDER_MYVENDOR_API_KEY":  "sk-explicit",
		"DEEPSEEK_API_KEY":                     "sk-ignored", // explicit list wins
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Providers) != 1 || got.Providers[0].Name != "myvendor" ||
		got.Providers[0].APIKey != "sk-explicit" || got.Providers[0].BaseURL != "https://llm.example.com/anthropic" {
		t.Errorf("explicit provider wrong: %+v", got.Providers)
	}
}

func TestLoad_ExplicitProviderMissingKeyFails(t *testing.T) {
	_, err := Load(envMap(map[string]string{
		"ADAMARKER_PROVIDERS":                  "myvendor",
		"ADAMARKER_PROVIDER_MYVENDOR_BASE_URL": "https://llm.example.com/anthropic",
	}))
	if err == nil {
		t.Fatal("provider listed without an API key must fail loudly")
	}
}

// --- Local OCR (D24) -----------------------------------------------------
//
// All three env vars are optional and never required to Load successfully —
// local OCR is a pure add-on. LocalOCRConfigured reports whether the engine
// can be constructed (all three set) and whether the env is in a partial,
// warning-worthy state (some but not all three set).

func TestLoad_LocalOCRUnsetByDefault(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.OCRModelPath != "" || got.OCRKeysPath != "" || got.ONNXRuntimeLibPath != "" {
		t.Errorf("local OCR paths should default empty, got: model=%q keys=%q lib=%q",
			got.OCRModelPath, got.OCRKeysPath, got.ONNXRuntimeLibPath)
	}
	configured, partial := got.LocalOCRConfigured()
	if configured || partial {
		t.Errorf("LocalOCRConfigured() = (%v, %v), want (false, false) when unset", configured, partial)
	}
}

func TestLoad_LocalOCRFullyConfigured(t *testing.T) {
	got, err := Load(envMap(map[string]string{
		envOCRModel:           "/data/ocr/rec.onnx",
		envOCRKeys:            "/data/ocr/keys.txt",
		envONNXRuntimeLibPath: "/opt/homebrew/lib/libonnxruntime.dylib",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.OCRModelPath != "/data/ocr/rec.onnx" {
		t.Errorf("OCRModelPath: got %q", got.OCRModelPath)
	}
	if got.OCRKeysPath != "/data/ocr/keys.txt" {
		t.Errorf("OCRKeysPath: got %q", got.OCRKeysPath)
	}
	if got.ONNXRuntimeLibPath != "/opt/homebrew/lib/libonnxruntime.dylib" {
		t.Errorf("ONNXRuntimeLibPath: got %q", got.ONNXRuntimeLibPath)
	}
	configured, partial := got.LocalOCRConfigured()
	if !configured || partial {
		t.Errorf("LocalOCRConfigured() = (%v, %v), want (true, false) when all three set", configured, partial)
	}
}

// --- Report font (spec §3, D42/D43) ---------------------------------------
//
// ADAMARKER_REPORT_FONT is a single optional path to a UTF-8 TTF (Noto Sans
// TC). Unlike local OCR's three-var all-or-nothing gate, this is a single
// value: unset means the report-attachment feature is off entirely (D43),
// set means it's on. There is no "partial" state to represent.

func TestLoad_ReportFontUnsetByDefault(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ReportFontPath != "" {
		t.Errorf("ReportFontPath should default empty, got %q", got.ReportFontPath)
	}
	if got.ReportFontConfigured() {
		t.Error("ReportFontConfigured() should be false when ADAMARKER_REPORT_FONT is unset")
	}
}

func TestLoad_ReportFontPassThrough(t *testing.T) {
	got, err := Load(envMap(map[string]string{
		envReportFont: "./data/fonts/NotoSansTC-Regular.ttf",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ReportFontPath != "./data/fonts/NotoSansTC-Regular.ttf" {
		t.Errorf("ReportFontPath: got %q", got.ReportFontPath)
	}
	if !got.ReportFontConfigured() {
		t.Error("ReportFontConfigured() should be true when ADAMARKER_REPORT_FONT is set")
	}
}

// --- App base URL (whole-branch review F4) --------------------------------
//
// ADAMARKER_APP_BASE_URL is a single optional absolute URL (e.g.
// https://ada.csie.ntu.edu.tw) used to build the TA-notify handoff email's deep link.
// Unset ⇒ AppBaseURL is empty and the caller must drop the link line entirely (a bare
// "/regrades/42" path is dead in any mail client).

func TestLoad_AppBaseURLUnsetByDefault(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AppBaseURL != "" {
		t.Errorf("AppBaseURL should default empty, got %q", got.AppBaseURL)
	}
	if got.EmailLoginTrustRequestHost {
		t.Error("EmailLoginTrustRequestHost should default false")
	}
}

func TestLoad_AppBaseURLPassThrough(t *testing.T) {
	got, err := Load(envMap(map[string]string{
		envAppBaseURL: "https://ada.csie.ntu.edu.tw",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AppBaseURL != "https://ada.csie.ntu.edu.tw" {
		t.Errorf("AppBaseURL: got %q", got.AppBaseURL)
	}
}

// TestLoad_AppBaseURLTrailingSlashTrimmed: a trailing slash would double up when
// concatenated with the "/regrades/{id}" path — trim it so config, not every caller,
// owns the normalization.
func TestLoad_AppBaseURLTrailingSlashTrimmed(t *testing.T) {
	got, err := Load(envMap(map[string]string{
		envAppBaseURL: "https://ada.csie.ntu.edu.tw/",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AppBaseURL != "https://ada.csie.ntu.edu.tw" {
		t.Errorf("AppBaseURL should have its trailing slash trimmed, got %q", got.AppBaseURL)
	}
}

// --- Email (spec §3, D31) -------------------------------------------------
//
// ADAMARKER_EMAIL_PROVIDER selects file|smtp|postmark|none. Development
// defaults to file with OutboxDir under <BlobDir>/../outbox; production
// defaults to none with a loud startup warning (never a hard error — an
// unconfigured system must still boot, per trust-spec convention of
// "fail-closed only when configured").

func TestLoad_EmailProviderDefaultsToFileInDevelopment(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Email.Provider != "file" {
		t.Errorf("Email.Provider: got %q want %q", got.Email.Provider, "file")
	}
	if got.Email.OutboxDir == "" {
		t.Error("Email.OutboxDir must be set when the file provider is selected by default")
	}
	// Spec: "<blobdir>/../outbox/" — i.e. a sibling of BlobDir, not nested under it.
	wantDir := filepath.Join(filepath.Dir(got.BlobDir), "outbox")
	if got.Email.OutboxDir != wantDir {
		t.Errorf("Email.OutboxDir: got %q want %q (sibling of BlobDir %q)", got.Email.OutboxDir, wantDir, got.BlobDir)
	}
}

func TestLoad_EmailProviderDefaultsToNoneInProduction(t *testing.T) {
	got, err := Load(envMap(prodEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Email.Provider != "none" {
		t.Errorf("Email.Provider: got %q want %q (unset in production must default to none, not file)", got.Email.Provider, "none")
	}
}

func TestLoad_EmailProviderExplicitOverridesDefault(t *testing.T) {
	got, err := Load(envMap(map[string]string{envEmailProvider: "none"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Email.Provider != "none" {
		t.Errorf("Email.Provider: got %q want %q", got.Email.Provider, "none")
	}
}

func TestLoad_EmailProviderSMTPRequiresHostUserPassFromInProduction(t *testing.T) {
	base := prodEnv()
	base[envEmailProvider] = "smtp"
	base[envSMTPHost] = "smtp.example.edu"
	base[envSMTPUser] = "marker"
	base[envSMTPPass] = "s3cret"
	base[envEmailFrom] = "grader@example.edu"

	if _, err := Load(envMap(base)); err != nil {
		t.Fatalf("complete smtp production config should load, got: %v", err)
	}

	for _, missing := range []string{envSMTPHost, envSMTPUser, envSMTPPass, envEmailFrom} {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		delete(m, missing)
		if _, err := Load(envMap(m)); err == nil {
			t.Errorf("production smtp provider should require %s", missing)
		}
	}
}

func TestLoad_EmailProviderPostmarkRequiresTokenAndFromInProduction(t *testing.T) {
	base := prodEnv()
	base[envEmailProvider] = "postmark"
	base[envPostmarkToken] = "pm-token"
	base[envEmailFrom] = "grader@example.edu"

	if _, err := Load(envMap(base)); err != nil {
		t.Fatalf("complete postmark production config should load, got: %v", err)
	}

	for _, missing := range []string{envPostmarkToken, envEmailFrom} {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		delete(m, missing)
		if _, err := Load(envMap(m)); err == nil {
			t.Errorf("production postmark provider should require %s", missing)
		}
	}
}

func TestLoad_EmailProviderUnknownValueFailsLoudly(t *testing.T) {
	if _, err := Load(envMap(map[string]string{envEmailProvider: "carrier-pigeon"})); err == nil {
		t.Fatal("unknown ADAMARKER_EMAIL_PROVIDER must fail loudly, not silently fall back")
	}
}

func TestLoad_EmailFieldsPassThrough(t *testing.T) {
	got, err := Load(envMap(map[string]string{
		envEmailProvider: "smtp",
		envEmailFrom:     "grader@example.edu",
		envEmailReplyDom: "reply.example.edu",
		envSMTPHost:      "smtp.example.edu",
		envSMTPPort:      "465",
		envSMTPUser:      "marker",
		envSMTPPass:      "s3cret",
		envPostmarkToken: "pm-token",
		envEmailRate:     "2.5",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Email.From != "grader@example.edu" {
		t.Errorf("Email.From: got %q", got.Email.From)
	}
	if got.Email.ReplyDomain != "reply.example.edu" {
		t.Errorf("Email.ReplyDomain: got %q", got.Email.ReplyDomain)
	}
	if got.Email.SMTPHost != "smtp.example.edu" {
		t.Errorf("Email.SMTPHost: got %q", got.Email.SMTPHost)
	}
	if got.Email.SMTPPort != "465" {
		t.Errorf("Email.SMTPPort: got %q", got.Email.SMTPPort)
	}
	if got.Email.SMTPUser != "marker" {
		t.Errorf("Email.SMTPUser: got %q", got.Email.SMTPUser)
	}
	if got.Email.SMTPPass != "s3cret" {
		t.Errorf("Email.SMTPPass: got %q", got.Email.SMTPPass)
	}
	if got.Email.PostmarkToken != "pm-token" {
		t.Errorf("Email.PostmarkToken: got %q", got.Email.PostmarkToken)
	}
	if got.Email.Rate != 2.5 {
		t.Errorf("Email.Rate: got %v want 2.5", got.Email.Rate)
	}
}

func TestLoad_EmailRateDefaultsTo1(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Email.Rate != 1.0 {
		t.Errorf("Email.Rate default: got %v want 1.0", got.Email.Rate)
	}
}

func TestLoad_EmailRateMalformedFailsLoudly(t *testing.T) {
	if _, err := Load(envMap(map[string]string{envEmailRate: "fast"})); err == nil {
		t.Fatal("malformed ADAMARKER_EMAIL_RATE must fail loudly")
	}
}

func TestLoad_EmailRateMustBePositive(t *testing.T) {
	for _, bad := range []string{"0", "-1", "NaN", "+Inf"} {
		if _, err := Load(envMap(map[string]string{envEmailRate: bad})); err == nil {
			t.Errorf("ADAMARKER_EMAIL_RATE=%s should be rejected (must be positive)", bad)
		}
	}
}

func TestLoad_EmailRateMustFitWorkerTimeout(t *testing.T) {
	if _, err := Load(envMap(map[string]string{envEmailRate: "0.001"})); err == nil {
		t.Fatal("an email rate slower than one message/minute must fail instead of snoozing every job forever")
	}
	if got, err := Load(envMap(map[string]string{envEmailRate: "0.016666666666666666"})); err != nil {
		t.Fatalf("one message/minute should be accepted: %v", err)
	} else if got.Email.Rate < minEmailRate {
		t.Fatalf("accepted email rate %v below minimum %v", got.Email.Rate, minEmailRate)
	}
}

// --- Regrade (spec §4-5, D32-D33) -----------------------------------------

func TestLoad_RegradeWindowDefault(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RegradeWindow != 336*time.Hour {
		t.Errorf("RegradeWindow default: got %v want 336h (14d)", got.RegradeWindow)
	}
}

func TestLoad_RegradeWindowOverride(t *testing.T) {
	got, err := Load(envMap(map[string]string{envRegradeWindow: "72h"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RegradeWindow != 72*time.Hour {
		t.Errorf("RegradeWindow override: got %v want 72h", got.RegradeWindow)
	}
}

func TestLoad_RegradeWindowMalformedFailsLoudly(t *testing.T) {
	if _, err := Load(envMap(map[string]string{envRegradeWindow: "two weeks"})); err == nil {
		t.Fatal("malformed ADAMARKER_REGRADE_WINDOW must fail loudly")
	}
}

func TestLoad_RegradeMaxDefault(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RegradeMax != 3 {
		t.Errorf("RegradeMax default: got %d want 3", got.RegradeMax)
	}
}

func TestLoad_RegradeMaxOverride(t *testing.T) {
	got, err := Load(envMap(map[string]string{envRegradeMax: "5"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RegradeMax != 5 {
		t.Errorf("RegradeMax override: got %d want 5", got.RegradeMax)
	}
}

func TestLoad_RegradeMaxMalformedFailsLoudly(t *testing.T) {
	if _, err := Load(envMap(map[string]string{envRegradeMax: "many"})); err == nil {
		t.Fatal("malformed ADAMARKER_REGRADE_MAX must fail loudly")
	}
}

func TestLoad_RegradeMaxMustBePositive(t *testing.T) {
	for _, bad := range []string{"0", "-1"} {
		if _, err := Load(envMap(map[string]string{envRegradeMax: bad})); err == nil {
			t.Errorf("ADAMARKER_REGRADE_MAX=%s should be rejected (must be positive)", bad)
		}
	}
}

func TestLoad_InboundWebhookSecretPassThrough(t *testing.T) {
	got, err := Load(envMap(map[string]string{envInboundWebhookSecret: "top-secret"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.InboundWebhookSecret != "top-secret" {
		t.Errorf("InboundWebhookSecret: got %q", got.InboundWebhookSecret)
	}
}

// --- Monthly budget cap (trust spec §3, D36) ------------------------------
//
// Travels as a decimal string end-to-end (never float64), matching the
// points/money convention (docs/DECISIONS.md D4). Empty means "no cap".

func TestLoad_MonthlyBudgetUSDDefaultsEmpty(t *testing.T) {
	got, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MonthlyBudgetUSD != "" {
		t.Errorf("MonthlyBudgetUSD default: got %q want empty (no cap)", got.MonthlyBudgetUSD)
	}
}

func TestLoad_MonthlyBudgetUSDValidDecimalPassesThrough(t *testing.T) {
	got, err := Load(envMap(map[string]string{envMonthlyBudgetUSD: "150.50"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MonthlyBudgetUSD != "150.50" {
		t.Errorf("MonthlyBudgetUSD: got %q want %q", got.MonthlyBudgetUSD, "150.50")
	}
}

func TestLoad_MonthlyBudgetUSDMalformedFailsLoudly(t *testing.T) {
	for _, bad := range []string{"not-a-number", "NaN", "Inf", "1/2", "$150", "1e2", "0x1p0", ".", ""} {
		if bad == "" {
			continue // empty means unset, tested separately
		}
		if _, err := Load(envMap(map[string]string{envMonthlyBudgetUSD: bad})); err == nil {
			t.Errorf("ADAMARKER_MONTHLY_BUDGET_USD=%q should be rejected as malformed", bad)
		}
	}
}

func TestLoad_MonthlyBudgetUSDMustBePositive(t *testing.T) {
	for _, bad := range []string{"0", "-5"} {
		if _, err := Load(envMap(map[string]string{envMonthlyBudgetUSD: bad})); err == nil {
			t.Errorf("ADAMARKER_MONTHLY_BUDGET_USD=%s should be rejected (must be positive when set)", bad)
		}
	}
}

func TestLoad_LocalOCRPartialConfigDoesNotFailLoad(t *testing.T) {
	cases := []map[string]string{
		{envOCRModel: "/data/ocr/rec.onnx"},
		{envOCRKeys: "/data/ocr/keys.txt"},
		{envONNXRuntimeLibPath: "/opt/homebrew/lib/libonnxruntime.dylib"},
		{envOCRModel: "/data/ocr/rec.onnx", envOCRKeys: "/data/ocr/keys.txt"},
	}
	for _, m := range cases {
		got, err := Load(envMap(m))
		if err != nil {
			t.Fatalf("partial local OCR config must still load fine, got: %v (env=%+v)", err, m)
		}
		configured, partial := got.LocalOCRConfigured()
		if configured {
			t.Errorf("LocalOCRConfigured() configured=true for partial env %+v", m)
		}
		if !partial {
			t.Errorf("LocalOCRConfigured() partial=false for partial env %+v, want true", m)
		}
	}
}
