package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/HaoWen46/adagrade/internal/localocr"
	"github.com/HaoWen46/adagrade/internal/offline"
	"github.com/HaoWen46/adagrade/internal/render"
)

// offlineGradeCommand is the argv[1] that routes to the offline pipeline
// instead of the server. It is dispatched BEFORE flag.Parse (see main): the
// subcommand has its own flag set with its own names, and letting the server's
// FlagSet see them first would reject every one of them.
const offlineGradeCommand = "offline-grade"

// isOfflineGrade reports whether args (os.Args, program name included) select
// the offline subcommand.
//
// It matches argv[1] EXACTLY and nothing else, so every existing invocation —
// bare `adamarker`, `adamarker -verify-blobs` — still falls through to the
// server's own flag parsing untouched.
func isOfflineGrade(args []string) bool {
	return len(args) > 1 && args[1] == offlineGradeCommand
}

// runOfflineGrade is the subcommand's entry point: args are everything after
// "offline-grade", and the return value is the process exit status.
func runOfflineGrade(args []string) int {
	return offlineGrade(args, os.Stdout, os.Stderr)
}

// offlineGrade is runOfflineGrade with its streams injected, so the CLI's own
// behaviour (which failures print what, and which exit code they carry) is
// testable without capturing the process's file descriptors.
//
// The only output this function produces is the error line for a failure: on
// success offline.Run owns stdout and stderr completely — it prints the
// fallback-mode banner, the per-stage progress and the closing summary — and
// anything added here would be a second voice describing the same run.
func offlineGrade(args []string, stdout, stderr io.Writer) int {
	opts, err := offline.ParseArgs(args)
	if err != nil {
		// A *UsageError's message already carries flag's own reason and the
		// usage block; printing it once is the whole report.
		fmt.Fprintln(stderr, err)
		return offline.ExitCode(err)
	}

	// The local reader is checked and built BEFORE the renderer: it is the
	// cheaper of the two (three environment variables and a file open, against a
	// PDFium worker pool), and it is the one an operator is likely not to have
	// installed yet.
	engine, err := newOfflineOCR()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return offline.ExitCode(err)
	}
	defer func() { _ = engine.Close() }()

	renderer, err := render.NewPDFium()
	if err != nil {
		// Not an input the operator can fix by editing a file, so it stays the
		// honest unclassified failure rather than borrowing the scan-input code.
		fmt.Fprintf(stderr, "cannot start the PDF renderer: %v\n", err)
		return offline.ExitFailure
	}
	defer func() { _ = renderer.Close() }()

	_, err = offline.Run(context.Background(), opts, offline.Deps{
		Renderer: renderer,
		OCR:      engine,
		// The dictionary MUST come from the engine that produced the lattices;
		// a Charset built from a different keys file scores nonsense, because
		// the classes it returns index the wrong rows of the lattice.
		Charset: engine.Charset(),
		Getenv:  os.Getenv,
		Stderr:  stderr,
		Stdout:  stdout,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
	}
	return offline.ExitCode(err)
}

// Environment variables the local reader is provisioned through (D24). They are
// read with os.Getenv directly rather than through config.Load, which would
// drag in a database URL and a whole server configuration that this mode has no
// use for and no way to satisfy.
const (
	ocrModelEnv = "ADAMARKER_OCR_MODEL"
	ocrKeysEnv  = "ADAMARKER_OCR_KEYS"
	ocrLibEnv   = "ADAMARKER_ONNXRUNTIME"
)

// offlineOCRNeed states why this mode cannot fall back to the cloud rung the
// server has. Reading identity locally is the entire reason the pages that DO
// leave the machine can be masked first, so an absent local reader is a hard
// failure here rather than the server's loud warning.
const offlineOCRNeed = "offline-grade needs the local OCR reader: it reads the identity band on this machine, which is what lets it mask every page before anything is sent to a provider"

// newOfflineOCR builds the local OCR engine from the environment. Every failure
// is an *OCRError (exit 6) carrying localOCRFix — the same remedy string the
// server's startup warning prints, so an operator who has seen one recognizes
// the other.
func newOfflineOCR() (*localocr.Engine, error) {
	model := strings.TrimSpace(os.Getenv(ocrModelEnv))
	keys := strings.TrimSpace(os.Getenv(ocrKeysEnv))
	lib := strings.TrimSpace(os.Getenv(ocrLibEnv))

	if missing := missingOCREnv(model, keys, lib); len(missing) > 0 {
		return nil, offline.NewOCRError(nil, "%s, but %s %s not set: %s",
			offlineOCRNeed, strings.Join(missing, ", "), plural(len(missing), "is", "are"), localOCRFix)
	}

	engine, err := localocr.New(localocr.Config{
		ModelPath:          model,
		KeysPath:           keys,
		ONNXRuntimeLibPath: lib,
	})
	if err != nil {
		// localocr's message names the offending path; the remedy is appended
		// because "model file not found" alone does not say how to get one.
		return nil, offline.NewOCRError(err, "%s, and it failed to load: %s", offlineOCRNeed, localOCRFix)
	}
	return engine, nil
}

// missingOCREnv names the unset variables, in the order .env.adamarker.example
// documents them, so the operator sets the ones they are actually missing.
func missingOCREnv(model, keys, lib string) []string {
	var missing []string
	for _, v := range []struct{ name, value string }{
		{ocrModelEnv, model}, {ocrKeysEnv, keys}, {ocrLibEnv, lib},
	} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	return missing
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
