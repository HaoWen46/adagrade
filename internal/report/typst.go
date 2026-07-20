package report

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// mitexVersion pins the Typst package that renders LaTeX math (the grading
// template mandates "LaTeX for math", prompt.go). Typst fetches it once into
// its local package cache; see .env.adamarker.example's ADAMARKER_TYPST_BIN
// note for offline pre-seeding.
const mitexVersion = "0.2.5"

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
func BuildTypst(bin, fontDir string, in ReportInput) ([]byte, error) {
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
	args := []string{"compile", "--root", dir, "--creation-timestamp", "0"}
	if fontDir != "" {
		args = append(args, "--font-path", fontDir)
	}
	args = append(args, docPath, outPath)
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = nil, nil // diagnostics can quote comment text — drop them
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("report: typst compile failed (%v) — run typst manually on a sample input to diagnose; stderr is suppressed because it can quote grading comments", err)
	}
	pdf, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("report: read compiled pdf: %w", err)
	}
	return pdf, nil
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
			// Height-capped so a portrait scan still fits under the running
			// header/heading instead of pushing to its own page and leaving
			// the previous page mostly blank.
			fmt.Fprintf(&b, "#align(center, image(%s, width: 100%%, height: 21cm, fit: \"contain\"))\n", quotedTypstBare(name))
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
