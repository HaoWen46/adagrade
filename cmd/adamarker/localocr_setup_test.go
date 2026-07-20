package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/scan"
)

// The local-OCR rung's absence must be LOUD (privacy audit 2026-07-12): the
// masking feature exists to keep student identity off cloud providers, yet a
// server booted without the local reader silently ships every ID/name crop to
// a cloud provider. Every setupLocalOCR path that ends WITHOUT a reader must
// WARN with (a) the cloud consequence and (b) the fix (make ocr-models +
// ADAMARKER_OCR_MODEL/_KEYS/_ONNXRUNTIME).

func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func assertLoudOCRWarn(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("want a WARN, got: %s", out)
	}
	if !strings.Contains(out, "cloud provider") {
		t.Errorf("warning must spell out the cloud consequence, got: %s", out)
	}
	if !strings.Contains(out, "make ocr-models") || !strings.Contains(out, "ADAMARKER_OCR_MODEL") {
		t.Errorf("warning must say how to fix it (make ocr-models + ADAMARKER_OCR_MODEL), got: %s", out)
	}
}

func TestSetupLocalOCR_UnconfiguredWarnsLoudly(t *testing.T) {
	logger, buf := captureLogger()
	scans := &scan.Service{}

	closeFn := setupLocalOCR(config.Config{}, scans, logger)
	defer closeFn()

	if scans.Local != nil {
		t.Fatal("no config: scans.Local must stay nil")
	}
	assertLoudOCRWarn(t, buf.String())
}

func TestSetupLocalOCR_PartialConfigWarnsLoudly(t *testing.T) {
	logger, buf := captureLogger()
	scans := &scan.Service{}

	closeFn := setupLocalOCR(config.Config{OCRModelPath: "/some/model.onnx"}, scans, logger)
	defer closeFn()

	if scans.Local != nil {
		t.Fatal("partial config: scans.Local must stay nil")
	}
	out := buf.String()
	assertLoudOCRWarn(t, out)
	if !strings.Contains(out, "set together") {
		t.Errorf("partial-config warning should say the three vars go together, got: %s", out)
	}
}

func TestSetupLocalOCR_UnusableModelWarnsLoudly(t *testing.T) {
	logger, buf := captureLogger()
	scans := &scan.Service{}

	// All three set, but the model file does not exist: construction fails and
	// the absence must still be loud, not a quiet fallthrough.
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys.txt")
	if err := os.WriteFile(keys, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		OCRModelPath:       filepath.Join(dir, "missing.onnx"),
		OCRKeysPath:        keys,
		ONNXRuntimeLibPath: filepath.Join(dir, "libonnxruntime.dylib"),
	}
	closeFn := setupLocalOCR(cfg, scans, logger)
	defer closeFn()

	if scans.Local != nil {
		t.Fatal("unusable model: scans.Local must stay nil")
	}
	assertLoudOCRWarn(t, buf.String())
}
