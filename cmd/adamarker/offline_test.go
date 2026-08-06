package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/offline"
)

// The subcommand's own job is small — parse, build two dependencies, hand off —
// so these tests cover exactly that: the argv routing, and the two failures
// that happen before offline.Run is ever reached.

// clearOCREnv unsets the three local-OCR variables for the duration of a test.
// A developer machine with a working local reader must not make the
// "unconfigured" tests pass for the wrong reason (or, worse, load an 85MB model
// in a unit test).
func clearOCREnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{ocrModelEnv, ocrKeysEnv, ocrLibEnv} {
		t.Setenv(name, "")
	}
}

// stopAfterMatchArgs is a command line that parses cleanly and needs no
// provider, so the only thing left to fail is the local OCR engine.
func stopAfterMatchArgs(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	return []string{
		"--roster", filepath.Join(dir, "roster.csv"),
		"--out", filepath.Join(dir, "out"),
		"--problems", "1",
		"--stop-after", "match",
		filepath.Join(dir, "scan.pdf"),
	}
}

// --- argv dispatch ---------------------------------------------------------

// TestIsOfflineGrade_RoutesOnlyTheSubcommand — the dispatch runs before
// flag.Parse, so anything it swallows by mistake stops working entirely. In
// particular -verify-blobs (docs/OPERATIONS.md §5) must still reach the
// server's flag set.
func TestIsOfflineGrade_RoutesOnlyTheSubcommand(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"adamarker", "offline-grade"}, true},
		{[]string{"adamarker", "offline-grade", "--roster", "r.csv"}, true},
		{[]string{"adamarker"}, false},
		{[]string{"adamarker", "-verify-blobs"}, false},
		{[]string{"adamarker", "--verify-blobs"}, false},
		{[]string{"adamarker", "-verify-blobs", "offline-grade"}, false},
		{[]string{"adamarker", "offline"}, false},
		{[]string{}, false},
	}
	for _, tc := range tests {
		if got := isOfflineGrade(tc.args); got != tc.want {
			t.Errorf("isOfflineGrade(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// --- usage -----------------------------------------------------------------

// TestOfflineGrade_UnknownFlagPrintsUsageOnce — flag's own reason and the usage
// block are one message, printed to stderr exactly once, and the exit code is
// the stable 2.
func TestOfflineGrade_UnknownFlagPrintsUsageOnce(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := offlineGrade([]string{"--not-a-flag"}, &stdout, &stderr)

	if code != offline.ExitUsage {
		t.Errorf("exit = %d, want %d", code, offline.ExitUsage)
	}
	out := stderr.String()
	if !strings.Contains(out, "not-a-flag") {
		t.Errorf("stderr should name the offending flag:\n%s", out)
	}
	if n := strings.Count(out, "usage: adamarker offline-grade"); n != 1 {
		t.Errorf("usage block printed %d times, want exactly 1:\n%s", n, out)
	}
	if stdout.Len() != 0 {
		t.Errorf("a usage failure must not write to stdout:\n%s", stdout.String())
	}
}

// TestOfflineGrade_MissingRequiredFlagExitsTwo — a validation failure names the
// flag to pass and carries the same exit code.
func TestOfflineGrade_MissingRequiredFlagExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := offlineGrade(nil, &stdout, &stderr)

	if code != offline.ExitUsage {
		t.Errorf("exit = %d, want %d", code, offline.ExitUsage)
	}
	if !strings.Contains(stderr.String(), "--roster") {
		t.Errorf("stderr should name the missing flag:\n%s", stderr.String())
	}
}

// --- local OCR -------------------------------------------------------------

// TestOfflineGrade_UnconfiguredLocalOCRExitsSixWithTheRemedy — the local reader
// is what makes masking possible, so its absence stops the run with exit 6 and
// prints the same remedy the server's startup warning does.
func TestOfflineGrade_UnconfiguredLocalOCRExitsSixWithTheRemedy(t *testing.T) {
	clearOCREnv(t)
	var stdout, stderr bytes.Buffer

	code := offlineGrade(stopAfterMatchArgs(t), &stdout, &stderr)

	if code != offline.ExitOCR {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, offline.ExitOCR, stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{"make ocr-models", ocrModelEnv, ocrKeysEnv, ocrLibEnv} {
		if !strings.Contains(out, want) {
			t.Errorf("the remedy must mention %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "mask") {
		t.Errorf("the message should say why offline-grade needs it:\n%s", out)
	}
}

// TestOfflineGrade_PartiallyConfiguredLocalOCRNamesWhatIsMissing — the three
// variables go together, and an operator who set two of them needs to be told
// which one is left.
func TestOfflineGrade_PartiallyConfiguredLocalOCRNamesWhatIsMissing(t *testing.T) {
	clearOCREnv(t)
	t.Setenv(ocrModelEnv, filepath.Join(t.TempDir(), "model.onnx"))
	var stdout, stderr bytes.Buffer

	code := offlineGrade(stopAfterMatchArgs(t), &stdout, &stderr)

	if code != offline.ExitOCR {
		t.Fatalf("exit = %d, want %d", code, offline.ExitOCR)
	}
	out := stderr.String()
	if !strings.Contains(out, ocrKeysEnv) || !strings.Contains(out, ocrLibEnv) {
		t.Errorf("stderr should name the two unset variables:\n%s", out)
	}
}

// TestNewOfflineOCR_UnusableModelIsStillAnOCRError — all three variables set
// but pointing at nothing is exit 6 too, with the remedy attached: the operator
// has done half the setup, not none of it.
func TestNewOfflineOCR_UnusableModelIsStillAnOCRError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ocrModelEnv, filepath.Join(dir, "missing.onnx"))
	t.Setenv(ocrKeysEnv, filepath.Join(dir, "missing-keys.txt"))
	t.Setenv(ocrLibEnv, filepath.Join(dir, "missing-lib.dylib"))

	engine, err := newOfflineOCR()
	if err == nil {
		_ = engine.Close()
		t.Fatal("a model path that does not exist must fail")
	}
	if got := offline.ExitCode(err); got != offline.ExitOCR {
		t.Errorf("ExitCode = %d (err %v), want %d", got, err, offline.ExitOCR)
	}
	if !strings.Contains(err.Error(), "make ocr-models") {
		t.Errorf("the remedy must survive onto the load failure: %v", err)
	}
}
