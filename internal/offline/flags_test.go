package offline

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// baseArgs is the smallest argument list that must parse cleanly: every
// required flag, the API route configured, nothing optional. Cases build on it
// by appending overrides (later flag wins) or by dropping a flag pair.
func baseArgs(extra ...string) []string {
	args := []string{
		"--roster", "roster.csv",
		"--out", "out",
		"--problems", "3",
		"--scans", "scan.pdf",
		"--provider", "deepseek",
		"--model", "some-model",
	}
	return append(args, extra...)
}

// argsWithout drops "--flag value" from a "--flag value"-only argument list.
func argsWithout(args []string, flagName string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flagName {
			i++ // skip the value too
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func TestParseArgsDefaults(t *testing.T) {
	opts, err := ParseArgs(baseArgs())
	if err != nil {
		t.Fatalf("ParseArgs(baseArgs()) error = %v, want nil", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Roster", opts.Roster, "roster.csv"},
		{"Out", opts.Out, "out"},
		{"Problems", opts.Problems, 3},
		{"Force", opts.Force, false},
		{"IDRegions", opts.IDRegions, ""},
		{"IDBand", opts.IDBand, 0.18},
		{"DPI", opts.DPI, 250},
		{"LongEdge", opts.LongEdge, 2200},
		{"JPEGQuality", opts.JPEGQuality, 85},
		{"MinScore", opts.MinScore, 0.15},
		{"MinMargin", opts.MinMargin, 0.03},
		{"StopAfter", opts.StopAfter, ""},
		{"Provider", opts.Provider, "deepseek"},
		{"ProviderKind", opts.ProviderKind, ""},
		{"BaseURL", opts.BaseURL, ""},
		{"Model", opts.Model, "some-model"},
		{"APIKeyEnv", opts.APIKeyEnv, ""},
		{"ExamName", opts.ExamName, "offline"},
		{"Concurrency", opts.Concurrency, 2},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
	if want := []string{"scan.pdf"}; !slices.Equal(opts.Scans, want) {
		t.Errorf("Scans = %#v, want %#v", opts.Scans, want)
	}
}

func TestParseArgsEveryFlagRoundTrips(t *testing.T) {
	opts, err := ParseArgs([]string{
		"--roster", "/tmp/r.csv",
		"--out", "/tmp/o",
		"--force",
		"--problems", "7",
		"--scans", "a.pdf",
		"--scans", "b.pdf",
		"--id-regions", "regions.json",
		"--id-band", "0.25",
		"--dpi", "300",
		"--long-edge", "3000",
		"--jpeg-quality", "70",
		"--min-score", "0.4",
		"--min-margin", "0.09",
		"--provider-kind", "openai-compat",
		"--base-url", "https://example.invalid/v1",
		"--api-key-env", "SOME_KEY",
		"--model", "m-1",
		"--exam-name", "midterm",
		"--concurrency", "4",
		"c.pdf", "d.pdf", // positionals must follow every flag (stdlib flag rule)
	})
	if err != nil {
		t.Fatalf("ParseArgs error = %v, want nil", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Roster", opts.Roster, "/tmp/r.csv"},
		{"Out", opts.Out, "/tmp/o"},
		{"Force", opts.Force, true},
		{"Problems", opts.Problems, 7},
		{"IDRegions", opts.IDRegions, "regions.json"},
		{"IDBand", opts.IDBand, 0.25},
		{"DPI", opts.DPI, 300},
		{"LongEdge", opts.LongEdge, 3000},
		{"JPEGQuality", opts.JPEGQuality, 70},
		{"MinScore", opts.MinScore, 0.4},
		{"MinMargin", opts.MinMargin, 0.09},
		{"ProviderKind", opts.ProviderKind, "openai-compat"},
		{"BaseURL", opts.BaseURL, "https://example.invalid/v1"},
		{"APIKeyEnv", opts.APIKeyEnv, "SOME_KEY"},
		{"Model", opts.Model, "m-1"},
		{"ExamName", opts.ExamName, "midterm"},
		{"Concurrency", opts.Concurrency, 4},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
	want := []string{"a.pdf", "b.pdf", "c.pdf", "d.pdf"}
	if !slices.Equal(opts.Scans, want) {
		t.Errorf("Scans = %#v, want %#v (repeated --scans then positionals)", opts.Scans, want)
	}
}

func TestParseArgsScansFromPositionalsOnly(t *testing.T) {
	args := append(argsWithout(baseArgs(), "--scans"), "one.pdf", "two.pdf")
	opts, err := ParseArgs(args)
	if err != nil {
		t.Fatalf("ParseArgs error = %v, want nil", err)
	}
	if want := []string{"one.pdf", "two.pdf"}; !slices.Equal(opts.Scans, want) {
		t.Errorf("Scans = %#v, want %#v", opts.Scans, want)
	}
}

func TestParseArgsStopAfterNeedsNoProviderRoute(t *testing.T) {
	for _, stage := range []string{"match", "mask"} {
		args := argsWithout(argsWithout(baseArgs(), "--provider"), "--model")
		opts, err := ParseArgs(append(args, "--stop-after", stage))
		if err != nil {
			t.Fatalf("--stop-after %s: error = %v, want nil", stage, err)
		}
		if opts.StopAfter != stage {
			t.Errorf("StopAfter = %q, want %q", opts.StopAfter, stage)
		}
	}
}

func TestParseArgsValidationErrors(t *testing.T) {
	// want lists substrings the message must contain — always the offending
	// flag, so the operator is told which one to fix.
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"missing roster", argsWithout(baseArgs(), "--roster"), []string{"--roster"}},
		{"blank roster", baseArgs("--roster", "  "), []string{"--roster"}},
		{"missing out", argsWithout(baseArgs(), "--out"), []string{"--out"}},
		{"missing scans", argsWithout(baseArgs(), "--scans"), []string{"--scans"}},
		{"missing problems", argsWithout(baseArgs(), "--problems"), []string{"--problems"}},
		{"zero problems", baseArgs("--problems", "0"), []string{"--problems"}},
		{"id-band zero", baseArgs("--id-band", "0"), []string{"--id-band"}},
		{"id-band too big", baseArgs("--id-band", "0.95"), []string{"--id-band"}},
		{"dpi zero", baseArgs("--dpi", "0"), []string{"--dpi"}},
		{"long-edge negative", baseArgs("--long-edge", "-1"), []string{"--long-edge"}},
		{"jpeg-quality zero", baseArgs("--jpeg-quality", "0"), []string{"--jpeg-quality"}},
		{"min-score above one", baseArgs("--min-score", "1.5"), []string{"--min-score"}},
		{"min-margin negative", baseArgs("--min-margin", "-0.1"), []string{"--min-margin"}},
		{"stop-after unknown", baseArgs("--stop-after", "grade"), []string{"--stop-after", "match", "mask"}},
		{"concurrency zero", baseArgs("--concurrency", "0"), []string{"--concurrency"}},
		{
			"provider and base-url", baseArgs("--base-url", "https://example.invalid/v1", "--api-key-env", "K"),
			[]string{"--provider", "--base-url"},
		},
		{
			"base-url without api-key-env",
			append(argsWithout(baseArgs(), "--provider"), "--base-url", "https://example.invalid/v1"),
			[]string{"--api-key-env"},
		},
		{"provider-kind unknown", baseArgs("--provider-kind", "grpc"), []string{"--provider-kind"}},
		{"provider-kind without base-url", baseArgs("--provider-kind", "openai-compat"), []string{"--provider-kind", "--base-url"}},
		{
			"no provider route", argsWithout(baseArgs(), "--provider"),
			[]string{"--provider", "--base-url", "--stop-after"},
		},
		{"no model", argsWithout(baseArgs(), "--model"), []string{"--model"}},
		{"unknown flag", baseArgs("--bogus"), []string{"bogus"}},
		// An empty path would otherwise reach the filesystem and produce a
		// message with a hole where the path should be.
		{"blank --scans value", baseArgs("--scans", " "), []string{"scans"}},
		{"blank positional scan", append(baseArgs(), ""), []string{"--scans"}},
		{"blank exam-name", baseArgs("--exam-name", "  "), []string{"--exam-name"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseArgs(tc.args)
			if err == nil {
				t.Fatalf("ParseArgs(%v) error = nil, want *UsageError", tc.args)
			}
			var ue *UsageError
			if !errors.As(err, &ue) {
				t.Fatalf("error type = %T, want *UsageError", err)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Errorf("ExitCode = %d, want %d", got, ExitUsage)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

func TestParseArgsHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		_, err := ParseArgs([]string{arg})
		var ue *UsageError
		if !errors.As(err, &ue) {
			t.Fatalf("%s: error type = %T, want *UsageError", arg, err)
		}
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%s: error does not wrap flag.ErrHelp", arg)
		}
		if got := ExitCode(err); got != ExitUsage {
			t.Errorf("%s: ExitCode = %d, want %d", arg, got, ExitUsage)
		}
		// The message IS the usage text: the caller prints it and exits 2.
		for _, want := range []string{"offline-grade", "-roster", "-problems", "-scans"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: usage text %q does not mention %q", arg, err.Error(), want)
			}
		}
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"unclassified", errors.New("boom"), 1},
		{"usage", &UsageError{}, ExitUsage},
		{"roster", &RosterError{}, ExitRoster},
		{"scan", &ScanError{}, ExitScan},
		{"outdir", &OutDirError{}, ExitOutDir},
		{"ocr", &OCRError{}, ExitOCR},
		{"regions", &RegionsError{}, ExitRegions},
		{"provider", &ProviderError{}, ExitProvider},
		{"nomatch", &NoMatchError{}, ExitNoMatch},
		{"wrapped roster", fmt.Errorf("stage 1: %w", &RosterError{}), ExitRoster},
		{"double wrapped provider", fmt.Errorf("a: %w", fmt.Errorf("b: %w", &ProviderError{})), ExitProvider},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestErrorConstructors pins the whole taxonomy: every constructor produces the
// type whose exit code it owns, formats its message, and keeps the cause
// reachable. Later stages construct these, so they must all work from day one.
func TestErrorConstructors(t *testing.T) {
	cause := errors.New("root cause")
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"usage", newUsageError(cause, "bad flag %s", "--x"), ExitUsage},
		{"roster", newRosterError(cause, "bad roster %s", "r.csv"), ExitRoster},
		{"scan", newScanError(cause, "bad scan %s", "s.pdf"), ExitScan},
		{"outdir", newOutDirError(cause, "bad out %s", "o"), ExitOutDir},
		{"ocr", newOCRError(cause, "bad ocr %s", "model"), ExitOCR},
		{"regions", newRegionsError(cause, "bad regions %s", "r.json"), ExitRegions},
		{"provider", newProviderError(cause, "bad provider %s", "p"), ExitProvider},
		{"nomatch", newNoMatchError(cause, "no match for %d pages", 4), ExitNoMatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode = %d, want %d", got, tc.want)
			}
			if !errors.Is(tc.err, cause) {
				t.Errorf("errors.Is(err, cause) = false, want true")
			}
			if msg := tc.err.Error(); !strings.Contains(msg, "root cause") || strings.Contains(msg, "%") {
				t.Errorf("Error() = %q, want the formatted message plus the cause", msg)
			}
		})
	}
}

func TestTypedErrorsWrapAndExplain(t *testing.T) {
	cause := errors.New("underlying cause")
	err := newRosterError(cause, "cannot read roster %s", "/tmp/r.csv")
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(err, cause) = false, want true (typed errors must wrap)")
	}
	msg := err.Error()
	for _, want := range []string{"/tmp/r.csv", "underlying cause"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
	// A nil cause must not leak an empty ": " tail.
	if got := newRosterError(nil, "plain message").Error(); got != "plain message" {
		t.Errorf("Error() with nil cause = %q, want %q", got, "plain message")
	}
}
