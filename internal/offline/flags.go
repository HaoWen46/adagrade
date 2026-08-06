package offline

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"strings"
)

// Options is the fully-resolved command line. Every field is validated by
// ParseArgs, so the pipeline can read it without re-checking ranges.
type Options struct {
	Roster    string   // --roster, required
	Scans     []string // --scans (repeatable) plus positional args, at least one required
	Out       string   // --out, required
	Force     bool     // --force: allow non-empty --out
	IDRegions string   // --id-regions JSON path, optional
	IDBand    float64  // --id-band, default 0.18, valid (0,0.9]
	Problems  int      // --problems, required, >=1

	DPI         int     // --dpi 250
	LongEdge    int     // --long-edge 2200
	JPEGQuality int     // --jpeg-quality 85
	MinScore    float64 // --min-score 0.15
	MinMargin   float64 // --min-margin 0.03
	StopAfter   string  // --stop-after "", "match", or "mask"

	Provider     string // --provider (name in config.LoadProviders table)
	ProviderKind string // --provider-kind anthropic-compat|openai-compat (with --base-url)
	BaseURL      string // --base-url (direct construction, bypasses table)
	Model        string // --model, required unless StopAfter != ""
	APIKeyEnv    string // --api-key-env (env var NAME holding the key, used with --base-url)

	ExamName    string // --exam-name, default "offline"
	Concurrency int    // --concurrency 2, >=1
}

// Defaults. Named so the usage text, the tests and the pipeline agree on one
// source of truth.
const (
	DefaultIDBand      = 0.18
	DefaultDPI         = 250
	DefaultLongEdge    = 2200
	DefaultJPEGQuality = 85
	DefaultMinScore    = 0.15
	DefaultMinMargin   = 0.03
	DefaultExamName    = "offline"
	DefaultConcurrency = 2
)

// maxIDBand caps --id-band: a band past 90% of the page is not an identity
// band, it is "mask the whole page", which would leave nothing to grade.
const maxIDBand = 0.9

// Stage names accepted by --stop-after. "" runs the whole pipeline.
const (
	StopAfterMatch = "match"
	StopAfterMask  = "mask"
)

// Provider kinds accepted by --provider-kind; they mirror config.ProviderKind.
const (
	ProviderKindAnthropicCompat = "anthropic-compat"
	ProviderKindOpenAICompat    = "openai-compat"
)

// stringList is the repeatable-flag adapter for --scans. It never splits on
// commas: a scan path may legitimately contain one, and repeating the flag (or
// listing paths positionally) is unambiguous.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, " ") }

func (s *stringList) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("empty path")
	}
	*s = append(*s, v)
	return nil
}

// ParseArgs parses args (without the program name) and validates them. Every
// rejection is a *UsageError (exit 2) naming the offending flag.
//
// The FlagSet's output is captured into a buffer that becomes the error
// message, so parsing prints nothing itself: the caller decides where usage
// goes and prints it exactly once, and tests stay quiet.
func ParseArgs(args []string) (Options, error) {
	var opts Options
	var scans stringList

	fs := flag.NewFlagSet("offline-grade", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)

	fs.StringVar(&opts.Roster, "roster", "", "path to the roster CSV (student_id,name,email) (required)")
	fs.Var(&scans, "scans", "scanned exam file (PDF or image); repeatable, and paths may also be given positionally after all flags")
	fs.StringVar(&opts.Out, "out", "", "directory to write artifacts into (required)")
	fs.BoolVar(&opts.Force, "force", false, "write into a non-empty --out directory")
	fs.StringVar(&opts.IDRegions, "id-regions", "", "path to an id-regions JSON file; without it the top --id-band strip is used")
	fs.Float64Var(&opts.IDBand, "id-band", DefaultIDBand, "height of the top identity band as a fraction of the page, used when --id-regions is absent")
	fs.IntVar(&opts.Problems, "problems", 0, "number of problems on the exam (required)")

	fs.IntVar(&opts.DPI, "dpi", DefaultDPI, "render resolution for PDF pages")
	fs.IntVar(&opts.LongEdge, "long-edge", DefaultLongEdge, "downscale each page so its long edge is at most this many pixels")
	fs.IntVar(&opts.JPEGQuality, "jpeg-quality", DefaultJPEGQuality, "JPEG quality for rendered and masked page images")
	fs.Float64Var(&opts.MinScore, "min-score", DefaultMinScore, "minimum match score for a page to be assigned")
	fs.Float64Var(&opts.MinMargin, "min-margin", DefaultMinMargin, "minimum score margin over the runner-up for a page to be assigned")
	fs.StringVar(&opts.StopAfter, "stop-after", "", "stop the run after a stage: match|mask (default: run everything)")

	fs.StringVar(&opts.Provider, "provider", "", "provider name from the environment provider table")
	fs.StringVar(&opts.ProviderKind, "provider-kind", "", "API dialect for --base-url: anthropic-compat|openai-compat")
	fs.StringVar(&opts.BaseURL, "base-url", "", "provider base URL, bypassing the provider table (needs --api-key-env)")
	fs.StringVar(&opts.Model, "model", "", "model id for transcription (required unless --stop-after is set)")
	fs.StringVar(&opts.APIKeyEnv, "api-key-env", "", "NAME of the environment variable holding the API key for --base-url")

	fs.StringVar(&opts.ExamName, "exam-name", DefaultExamName, "exam name recorded in the artifacts")
	fs.IntVar(&opts.Concurrency, "concurrency", DefaultConcurrency, "number of pages transcribed in parallel")

	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: adamarker offline-grade --roster ROSTER.csv --out DIR --problems N [flags] [SCAN...]\n\n"+
			"Grade scanned exams without the server: force-match every page to a roster\nentry, mask identity, transcribe, and write artifacts to --out.\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		// flag already wrote the reason (and the usage text) into out; that
		// text names the offending flag, so it IS the actionable message and
		// err must not be appended on top of it.
		msg := strings.TrimRight(out.String(), " \t\n")
		if msg == "" {
			return Options{}, newUsageError(err, "cannot parse arguments")
		}
		return Options{}, newFlagUsageError(err, msg)
	}

	// Positional scan paths follow the flags; blank ones are rejected rather
	// than dropped, so an empty shell variable fails loudly instead of
	// silently shrinking the input set.
	for _, arg := range fs.Args() {
		if strings.TrimSpace(arg) == "" {
			return Options{}, newUsageError(nil, "empty scan path given as a positional argument: every --scans value and positional path must be a real file path")
		}
	}
	opts.Scans = append([]string(scans), fs.Args()...)
	opts.Roster = strings.TrimSpace(opts.Roster)
	opts.Out = strings.TrimSpace(opts.Out)
	opts.StopAfter = strings.TrimSpace(opts.StopAfter)
	opts.Provider = strings.TrimSpace(opts.Provider)
	opts.ProviderKind = strings.TrimSpace(opts.ProviderKind)
	opts.BaseURL = strings.TrimSpace(opts.BaseURL)
	opts.Model = strings.TrimSpace(opts.Model)
	opts.APIKeyEnv = strings.TrimSpace(opts.APIKeyEnv)
	opts.ExamName = strings.TrimSpace(opts.ExamName)

	if err := validate(opts); err != nil {
		return Options{}, err
	}
	return opts, nil
}

// validate enforces every rule ParseArgs promises. Each message names the flag
// to change and what an acceptable value looks like.
func validate(o Options) error {
	if o.Roster == "" {
		return newUsageError(nil, "--roster is required: path to the roster CSV (header student_id,name,email)")
	}
	if len(o.Scans) == 0 {
		return newUsageError(nil, "--scans is required: pass at least one scanned exam file, by repeating --scans or listing paths after all flags")
	}
	if o.Out == "" {
		return newUsageError(nil, "--out is required: directory to write the run's artifacts into")
	}
	if o.Problems < 1 {
		return newUsageError(nil, "--problems must be at least 1, got %d: pass the number of problems on the exam", o.Problems)
	}
	if o.IDBand <= 0 || o.IDBand > maxIDBand {
		return newUsageError(nil, "--id-band must be in (0, %g], got %g: it is the identity band height as a fraction of the page", maxIDBand, o.IDBand)
	}
	if o.DPI <= 0 {
		return newUsageError(nil, "--dpi must be greater than 0, got %d", o.DPI)
	}
	if o.LongEdge <= 0 {
		return newUsageError(nil, "--long-edge must be greater than 0, got %d", o.LongEdge)
	}
	if o.JPEGQuality <= 0 {
		return newUsageError(nil, "--jpeg-quality must be greater than 0, got %d (85 is a good default)", o.JPEGQuality)
	}
	if o.MinScore < 0 || o.MinScore > 1 {
		return newUsageError(nil, "--min-score must be in [0, 1], got %g", o.MinScore)
	}
	if o.MinMargin < 0 || o.MinMargin > 1 {
		return newUsageError(nil, "--min-margin must be in [0, 1], got %g", o.MinMargin)
	}
	switch o.StopAfter {
	case "", StopAfterMatch, StopAfterMask:
	default:
		return newUsageError(nil, "--stop-after must be %q or %q (or omitted to run everything), got %q", StopAfterMatch, StopAfterMask, o.StopAfter)
	}
	if o.Concurrency < 1 {
		return newUsageError(nil, "--concurrency must be at least 1, got %d", o.Concurrency)
	}
	// Only reachable by passing it explicitly blank; the artifacts are named
	// after it, so take the operator at their word and refuse rather than
	// quietly substituting the default they overrode.
	if o.ExamName == "" {
		return newUsageError(nil, "--exam-name must not be empty: it names the exam in the artifacts (default %q)", DefaultExamName)
	}

	// Provider routing: the table lookup and the direct construction are two
	// different ways to reach the same client, never both at once.
	if o.Provider != "" && o.BaseURL != "" {
		return newUsageError(nil, "--provider and --base-url are mutually exclusive: use --provider to pick a configured provider, or --base-url (with --api-key-env) to build one directly")
	}
	if o.BaseURL != "" && o.APIKeyEnv == "" {
		return newUsageError(nil, "--base-url requires --api-key-env: pass the NAME of the environment variable holding the API key (the key itself must never be an argument)")
	}
	switch o.ProviderKind {
	case "", ProviderKindAnthropicCompat, ProviderKindOpenAICompat:
	default:
		return newUsageError(nil, "--provider-kind must be %q or %q, got %q", ProviderKindAnthropicCompat, ProviderKindOpenAICompat, o.ProviderKind)
	}
	if o.ProviderKind != "" && o.BaseURL == "" {
		return newUsageError(nil, "--provider-kind only applies to --base-url: drop it, or pass --base-url (the API dialect of a --provider comes from its configuration)")
	}

	// The API stage only runs when the pipeline is not cut short, so a
	// provider route and a model are required exactly then.
	if o.StopAfter == "" {
		if o.Provider == "" && o.BaseURL == "" {
			return newUsageError(nil, "a provider is required to transcribe: pass --provider NAME or --base-url URL (with --api-key-env), or --stop-after match|mask to skip the API stage")
		}
		if o.Model == "" {
			return newUsageError(nil, "--model is required to transcribe: pass the model id, or --stop-after match|mask to skip the API stage")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Error taxonomy and exit codes.
//
// Every stage of the pipeline fails with one of the types below so the CLI can
// turn a failure into a stable exit code without string matching. Later stages
// construct these; ExitCode is the single place that maps them.
// ---------------------------------------------------------------------------

// Exit codes. Stable: scripts branch on them.
const (
	ExitOK       = 0 // success
	ExitFailure  = 1 // anything unclassified
	ExitUsage    = 2 // bad arguments, or --help
	ExitRoster   = 3 // roster missing, unreadable, or unparseable
	ExitScan     = 4 // scan input missing, unreadable, or undecodable
	ExitOutDir   = 5 // output directory unusable
	ExitOCR      = 6 // local OCR unavailable or failed to load
	ExitRegions  = 7 // id-regions file invalid
	ExitProvider = 8 // provider unconfigured, or every transcription call failed
	ExitNoMatch  = 9 // zero pages matched
)

// offlineErr is the shared body of every typed error: an actionable message
// (what was wrong AND what to do) plus the underlying cause, which stays
// reachable through errors.Is/errors.As.
type offlineErr struct {
	Msg string
	Err error
	// causeInMsg marks a cause that Msg already states, so Error() must not
	// append it. The flag package writes its own reason into the buffer we
	// adopt as Msg; appending would print it twice, and for -h would trail the
	// help text with the "flag: help requested" sentinel.
	causeInMsg bool
}

func (e offlineErr) Error() string {
	switch {
	case e.Msg == "":
		if e.Err == nil {
			return ""
		}
		return e.Err.Error()
	case e.Err == nil || e.causeInMsg:
		return e.Msg
	}
	return e.Msg + ": " + e.Err.Error()
}

func (e offlineErr) Unwrap() error { return e.Err }

// The typed errors. Each is a distinct type purely so errors.As can tell them
// apart; the behaviour lives in offlineErr.
//
// Only POINTER values are classified: ExitCode matches *UsageError, not
// UsageError, and every constructor below returns a pointer. Later stages must
// return these the same way — a by-value RosterError would fall through to
// exit 1.
type (
	UsageError    struct{ offlineErr } // exit 2
	RosterError   struct{ offlineErr } // exit 3
	ScanError     struct{ offlineErr } // exit 4
	OutDirError   struct{ offlineErr } // exit 5
	OCRError      struct{ offlineErr } // exit 6
	RegionsError  struct{ offlineErr } // exit 7
	ProviderError struct{ offlineErr } // exit 8
	NoMatchError  struct{ offlineErr } // exit 9
)

func newOfflineErr(cause error, format string, a ...any) offlineErr {
	return offlineErr{Msg: fmt.Sprintf(format, a...), Err: cause}
}

func newUsageError(cause error, format string, a ...any) *UsageError {
	return &UsageError{newOfflineErr(cause, format, a...)}
}

// newFlagUsageError reports a failure the flag package already described in
// full: msg is flag's own output (the reason, then the usage block), so the
// cause is kept only for errors.Is/As and never re-appended to the text.
func newFlagUsageError(cause error, msg string) *UsageError {
	return &UsageError{offlineErr{Msg: msg, Err: cause, causeInMsg: true}}
}

func newRosterError(cause error, format string, a ...any) *RosterError {
	return &RosterError{newOfflineErr(cause, format, a...)}
}

func newScanError(cause error, format string, a ...any) *ScanError {
	return &ScanError{newOfflineErr(cause, format, a...)}
}

func newOutDirError(cause error, format string, a ...any) *OutDirError {
	return &OutDirError{newOfflineErr(cause, format, a...)}
}

// newOCRError is the local-OCR failure (constructed by the read stage).
func newOCRError(cause error, format string, a ...any) *OCRError {
	return &OCRError{newOfflineErr(cause, format, a...)}
}

// NewOCRError is the one typed error a CALLER has to be able to mint.
//
// The local OCR engine is constructed outside this package on purpose — Run
// takes the narrow latticeReader seam, so the pipeline's tests need no ONNX
// runtime, no model file and no dictionary — which means "there is no local
// reader at all" can only be detected by the command that was supposed to build
// one. Every other typed error is minted by the stage that detects it and stays
// unexported.
func NewOCRError(cause error, format string, a ...any) *OCRError {
	return newOCRError(cause, format, a...)
}

func newRegionsError(cause error, format string, a ...any) *RegionsError {
	return &RegionsError{newOfflineErr(cause, format, a...)}
}

// newProviderError is the transcription-stage failure (unconfigured provider,
// or every call failed).
func newProviderError(cause error, format string, a ...any) *ProviderError {
	return &ProviderError{newOfflineErr(cause, format, a...)}
}

// newNoMatchError is the match-stage failure: not one page could be assigned.
func newNoMatchError(cause error, format string, a ...any) *NoMatchError {
	return &NoMatchError{newOfflineErr(cause, format, a...)}
}

// ExitCode maps an error to the process exit status. It matches through wrapped
// errors (errors.As), so a stage may add context with fmt.Errorf("...: %w", …)
// without losing its classification. If a chain somehow carries two typed
// errors, the first match in the order below wins.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, new(*UsageError)):
		return ExitUsage
	case errors.As(err, new(*RosterError)):
		return ExitRoster
	case errors.As(err, new(*ScanError)):
		return ExitScan
	case errors.As(err, new(*OutDirError)):
		return ExitOutDir
	case errors.As(err, new(*OCRError)):
		return ExitOCR
	case errors.As(err, new(*RegionsError)):
		return ExitRegions
	case errors.As(err, new(*ProviderError)):
		return ExitProvider
	case errors.As(err, new(*NoMatchError)):
		return ExitNoMatch
	}
	return ExitFailure
}
