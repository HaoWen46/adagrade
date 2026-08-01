package report

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registers the JPEG decoder for image.DecodeConfig
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// mitexVersion pins the Typst package that renders LaTeX math (the grading
// template mandates "LaTeX for math", prompt.go). Typst fetches it once into
// its local package cache; see .env.adamarker.example's ADAMARKER_TYPST_BIN
// note for offline pre-seeding.
const mitexVersion = "0.2.5"

// typstCompileTimeout is a hard cap on the compile subprocess, independent of
// the caller's deadline. Legitimate reports compile in well under a second;
// this exists because comment/criterion text is model/TA-derived and assumed
// hostile — a crafted LaTeX macro inside a math span can drive mitex's
// expander into unbounded recursion, and without a kill that hang (not a
// compile *error*, so the fpdf fallback would never run) would wedge the
// single-worker email queue forever. Well under the queue's 2-minute job
// timeout so this fires first and the fallback path actually gets reached.
// A var rather than config so tests can shrink it (MaxZipBytes precedent).
var typstCompileTimeout = 20 * time.Second

// BuildTypst renders the per-student result PDF with Typst instead of fpdf —
// same disclosure, but LaTeX math in criterion names and problem comments is
// typeset via mitex rather than shown as raw source (typst-report spec
// 2026-07-20). bin is the typst executable (config ADAMARKER_TYPST_BIN);
// fontDir, when non-empty, is added via --font-path (the report-fonts dir, so
// Noto Sans TC resolves without OS installation).
//
// Determinism: --creation-timestamp 0 pins the only run-varying input, so the
// same ReportInput yields byte-identical PDFs (the fpdf renderer's invariant).
// Sandbox: --root confines Typst's file access to the throwaway build dir —
// defense in depth on top of typstComment's escaping.
//
// PII rule: on failure the returned error carries the exit status only, never
// typst's stderr — compiler diagnostics quote source lines, and the source
// embeds grading comments.
func BuildTypst(ctx context.Context, bin, fontDir string, in ReportInput) ([]byte, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	if bin == "" {
		return nil, fmt.Errorf("report: BuildTypst called with no typst binary configured")
	}

	dir, err := os.MkdirTemp("", "adamarker-typst-*")
	if err != nil {
		return nil, fmt.Errorf("report: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	doc, err := typstDocument(dir, in)
	if err != nil {
		return nil, err
	}
	docPath := filepath.Join(dir, "doc.typ")
	if err := os.WriteFile(docPath, []byte(doc), 0o600); err != nil {
		return nil, fmt.Errorf("report: write doc.typ: %w", err)
	}

	outPath := filepath.Join(dir, "out.pdf")
	if err := runTypst(ctx, bin, fontDir, dir, docPath, outPath); err != nil {
		if errors.Is(err, ErrTypstCompileFailed) {
			return nil, fmt.Errorf("report: typst compile failed (%v) — run typst manually on a sample input to diagnose; stderr is suppressed because it can quote grading comments", err)
		}
		return nil, fmt.Errorf("report: %v — likely a pathological LaTeX macro in a comment; falling back to fpdf", err)
	}
	pdf, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("report: read compiled pdf: %w", err)
	}
	return pdf, nil
}

// ErrTypstCompileFailed marks "this source does not compile" as distinct from
// engine/timeout trouble, so the transcription gate can branch without string
// matching (the tectonic ErrCompileFailed convention).
var ErrTypstCompileFailed = errTypstCompileFailed{}

type errTypstCompileFailed struct{}

func (errTypstCompileFailed) Error() string { return "typst compile failed" }

// CompileTypstSource compiles a self-contained .typ source and returns the
// PDF bytes. Exported for the transcription bundle's secondary gate (spec
// 2026-07-30): same sandbox, determinism pin, runaway kill, and suppressed
// stderr as BuildTypst, without the report-specific document assembly.
func CompileTypstSource(ctx context.Context, bin, fontDir, src string) ([]byte, error) {
	if bin == "" {
		return nil, fmt.Errorf("report: CompileTypstSource called with no typst binary configured")
	}
	dir, err := os.MkdirTemp("", "adamarker-typst-*")
	if err != nil {
		return nil, fmt.Errorf("report: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	docPath := filepath.Join(dir, "doc.typ")
	if err := os.WriteFile(docPath, []byte(src), 0o600); err != nil {
		return nil, fmt.Errorf("report: write doc.typ: %w", err)
	}
	outPath := filepath.Join(dir, "out.pdf")
	if err := runTypst(ctx, bin, fontDir, dir, docPath, outPath); err != nil {
		return nil, err
	}
	pdf, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("report: read compiled pdf: %w", err)
	}
	return pdf, nil
}

// runTypst invokes the typst binary with the shared hardening: --root
// sandbox, pinned creation timestamp, hard timeout with SIGKILL, and
// stdout/stderr dropped (diagnostics quote source lines, which embed user
// text). A nonzero exit within the deadline wraps ErrTypstCompileFailed.
func runTypst(ctx context.Context, bin, fontDir, root, docPath, outPath string) error {
	args := []string{"compile", "--root", root, "--creation-timestamp", "0"}
	if fontDir != "" {
		args = append(args, "--font-path", fontDir)
	}
	args = append(args, docPath, outPath)

	runCtx, cancel := context.WithTimeout(ctx, typstCompileTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.WaitDelay = 5 * time.Second   // SIGKILL then give up if the child ignores cancellation
	cmd.Stdout, cmd.Stderr = nil, nil // diagnostics can quote user text — drop them
	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return fmt.Errorf("typst compile exceeded %s and was killed (%w)", typstCompileTimeout, runCtx.Err())
		}
		return fmt.Errorf("%w: exit status %v (stderr suppressed)", ErrTypstCompileFailed, err)
	}
	return nil
}

// typstDocument generates doc.typ and writes the (quality-processed) page
// JPEGs beside it. ALL user-controlled strings pass through typstComment —
// never interpolate them into markup directly (see the invariant there).
func typstDocument(dir string, in ReportInput) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "#import \"@preview/mitex:%s\": mi, mitex\n", mitexVersion)
	b.WriteString(`#set page(paper: "a4", margin: (x: 1.6cm, y: 1.8cm))
#set text(size: 10pt, lang: "zh", region: "TW", font: ("Libertinus Serif", "Noto Sans TC", "Songti TC", "Noto Sans CJK TC"))
#set par(justify: true)
`)
	b.WriteString("#heading(level: 1)[" + typstComment(in.AssessmentName) + "]\n")
	b.WriteString(quotedTypst(in.StudentName+"（"+in.StudentID+"）") + "\n")
	b.WriteString("#h(1fr) #text(weight: \"bold\")[" + quotedTypst("Total: "+in.Total+" / "+in.Max) + "]\n")

	for pi, p := range in.Problems {
		b.WriteString("\n#heading(level: 2)[" + typstComment(p.Label) + "]\n")
		for pj, pageJPEG := range p.Pages {
			resolved, err := resolvePageJPEG(pageJPEG, in.Quality)
			if err != nil {
				return "", fmt.Errorf("report: problem %d page %d: %w", pi+1, pj+1, err)
			}
			name := fmt.Sprintf("img-%d-%d.jpg", pi, pj)
			if err := os.WriteFile(filepath.Join(dir, name), resolved, 0o600); err != nil {
				return "", fmt.Errorf("report: write %s: %w", name, err)
			}
			// Sizing depends on orientation. A portrait scan at width:100% is
			// taller than the page and pushes to its own sheet, leaving the
			// previous one mostly blank — cap its height. A landscape scan at
			// width:100% is already short, so a fixed 21cm box would only
			// letterbox it into a wasteful giant square; let it size naturally.
			sizing := "width: 100%"
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(resolved)); err == nil && cfg.Height > cfg.Width {
				sizing = "width: 100%, height: 21cm, fit: \"contain\""
			}
			fmt.Fprintf(&b, "#align(center, image(%s, %s))\n", quotedTypstBare(name), sizing)
		}
		if len(p.Criteria) > 0 {
			b.WriteString("#table(columns: (1fr, auto), stroke: 0.4pt + luma(160), inset: 5pt,\n")
			b.WriteString("  table.header([*Criterion*], [*Score*]),\n")
			for _, c := range p.Criteria {
				b.WriteString("  [" + typstComment(c.Name) + "], [" + quotedTypst(c.Score+" / "+c.Max) + "],\n")
			}
			b.WriteString("  [*Problem total*], [" + quotedTypst(p.Total+" / "+p.Max) + "],\n")
			b.WriteString(")\n")
		}
		if p.Comment != "" {
			b.WriteString("#block(inset: (left: 6pt), stroke: (left: 2pt + luma(140)))[" + typstComment(p.Comment) + "]\n")
		}
	}
	return b.String(), nil
}

// quotedTypst renders an app-controlled value as a literal-string CONTENT
// token (`#"…"`, the shape typstComment emits) — valid at markup top level
// and inside content brackets, where a bare "…" would be smart-quoted text.
func quotedTypst(s string) string { return `#"` + escapeTypstString(s) + `"` }

// quotedTypstBare renders a string literal for EXPRESSION position (function
// arguments like image(…)).
func quotedTypstBare(s string) string { return `"` + escapeTypstString(s) + `"` }
