package transcribe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// compileTimeout is a hard cap on the compile subprocess, independent of the
// caller's deadline — the same defense D70 needed for Typst
// (internal/report/typst.go).
//
// Deliberately looser than D70's 20s, because the threat model differs. There,
// the timeout was the ONLY thing standing between a mitex macro bomb and a
// wedged queue. Here the math allow-list makes a macro bomb unconstructible
// before the compiler ever sees the source, so this is defense in depth against
// a pathological-but-legal construct (deeply nested \frac, an enormous matrix).
//
// It must also accommodate tectonic itself: a warm-cache compile of a realistic
// answer measures ~12s wall against ~1.5s CPU, because the engine revalidates
// its support bundle over the network. A 25s cap killed real, correct documents
// in testing. Cold cache is minutes — see cacheDir; hosts should pre-seed.
//
// A var rather than config so tests can shrink it (typstCompileTimeout precedent).
var compileTimeout = 120 * time.Second

// DefaultCacheDir returns the platform-correct location for tectonic's support
// bundle. Getting this wrong is not a small mistake: because Compile sandboxes
// HOME into a throwaway directory, a wrong or empty cache path makes the engine
// re-download its entire bundle on every single answer — minutes each, instead
// of seconds, and a hard network dependency on a job that should be offline
// after first use. Measured on macOS: ~12s warm versus ~5min cold.
//
// tectonic itself reports this via `tectonic -X show user-cache-dir`; this
// mirrors that resolution so callers get a correct default without shelling out.
func DefaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches", "Tectonic")
	}
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "Tectonic")
	}
	return filepath.Join(home, ".cache", "Tectonic")
}

// Compile renders a .tex source to PDF bytes using tectonic, and reports
// whether the document is actually compilable. bin is the tectonic executable
// (config ADAMARKER_TECTONIC_BIN); when empty, Compile reports ErrNoEngine so
// callers can mark the export "unverified" rather than silently claiming it was
// checked. cacheDir must persist across compiles — see DefaultCacheDir; it is
// the same operational shape as D70's mitex package cache (fetched once per
// machine, pre-seedable on air-gapped hosts).
//
// Hardening, mirroring the Typst renderer:
//   - --untrusted disables shell-escape and any other privileged TeX feature,
//     so \write18 cannot execute even if a future allow-list change let it
//     through the validator.
//   - The build runs in a throwaway temp dir; the source is written there and
//     nothing outside it is reachable.
//   - SOURCE_DATE_EPOCH=0 pins the only run-varying input, so identical source
//     yields byte-identical PDFs (the fpdf/Typst invariant).
//   - stderr is dropped: TeX diagnostics quote the offending source lines, and
//     the source embeds student answer text.
func Compile(ctx context.Context, bin, cacheDir, tex string) ([]byte, error) {
	if bin == "" {
		return nil, ErrNoEngine
	}

	dir, err := os.MkdirTemp("", "adamarker-tex-*")
	if err != nil {
		return nil, fmt.Errorf("transcribe: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	srcPath := filepath.Join(dir, "doc.tex")
	if err := os.WriteFile(srcPath, []byte(tex), 0o600); err != nil {
		return nil, fmt.Errorf("transcribe: write doc.tex: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, compileTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin,
		"-X", "compile",
		"--untrusted",
		"--outdir", dir,
		srcPath,
	)
	cmd.Dir = dir
	// A minimal environment: no inherited TEXINPUTS/TEXMFHOME that could point
	// the engine at files outside the sandbox. HOME stays inside the sandbox;
	// only the support-bundle cache is allowed to persist, and only if the
	// caller nominated a directory for it.
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"SOURCE_DATE_EPOCH=0",
		"HOME=" + dir,
	}
	if cacheDir != "" {
		cmd.Env = append(cmd.Env, "TECTONIC_CACHE_DIR="+cacheDir)
	}
	cmd.WaitDelay = 5 * time.Second // SIGKILL then give up if the child ignores cancellation
	cmd.Stdout, cmd.Stderr = nil, nil

	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return nil, fmt.Errorf("transcribe: tectonic exceeded %s and was killed (%w)", compileTimeout, runCtx.Err())
		}
		return nil, fmt.Errorf("%w: exit status %v (stderr suppressed — TeX diagnostics quote source lines, which embed student answer text)", ErrCompileFailed, err)
	}

	pdf, err := os.ReadFile(filepath.Join(dir, "doc.pdf"))
	if err != nil {
		return nil, fmt.Errorf("transcribe: read compiled pdf: %w", err)
	}
	return pdf, nil
}

// Sentinel errors so callers can distinguish "no engine configured" (export
// proceeds, marked unverified) from "this document does not compile" (export
// proceeds, marked failed) without string matching.
var (
	ErrNoEngine      = errNoEngine{}
	ErrCompileFailed = errCompileFailed{}
)

type errNoEngine struct{}

func (errNoEngine) Error() string {
	return "transcribe: no TeX engine configured (ADAMARKER_TECTONIC_BIN unset); .tex exported without compile verification"
}

type errCompileFailed struct{}

func (errCompileFailed) Error() string { return "transcribe: tex compile failed" }
