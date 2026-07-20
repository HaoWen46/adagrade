package report

import (
	"archive/zip"
	"bytes"
	"fmt"
)

// BuildZIP builds the ZIP-of-images fallback (spec §3, D45): per-problem
// page JPEGs at the chosen quality plus a grades.txt carrying the same
// breakdown the email body already has — "for mail-gateway or PDF-viewer
// trouble". Filenames: problem-<n>-page-<m>.jpg (1-based) + grades.txt.
func BuildZIP(in ReportInput) ([]byte, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	if len(in.Problems) == 0 {
		return nil, fmt.Errorf("report: BuildZIP: ReportInput has no problems")
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for problemIdx, p := range in.Problems {
		if len(p.Pages) == 0 {
			_ = zw.Close()
			return nil, fmt.Errorf("report: BuildZIP: problem %q has no pages", p.Label)
		}
		for pageIdx, pageJPEG := range p.Pages {
			resolved, err := resolvePageJPEG(pageJPEG, in.Quality)
			if err != nil {
				_ = zw.Close()
				return nil, err
			}
			w, err := zw.Create(pageLabel(problemIdx, pageIdx))
			if err != nil {
				_ = zw.Close()
				return nil, fmt.Errorf("report: BuildZIP: create zip entry: %w", err)
			}
			if _, err := w.Write(resolved); err != nil {
				_ = zw.Close()
				return nil, fmt.Errorf("report: BuildZIP: write zip entry: %w", err)
			}
		}
	}

	gw, err := zw.Create("grades.txt")
	if err != nil {
		_ = zw.Close()
		return nil, fmt.Errorf("report: BuildZIP: create grades.txt: %w", err)
	}
	if _, err := gw.Write([]byte(gradesText(in))); err != nil {
		_ = zw.Close()
		return nil, fmt.Errorf("report: BuildZIP: write grades.txt: %w", err)
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("report: BuildZIP: finalize zip: %w", err)
	}
	return buf.Bytes(), nil
}

// gradesText renders the same problem/criterion/total breakdown as the
// grade email's plain-text body (spec §3: "grades.txt (the text body's
// breakdown)"), scoped to this student's own content only — no PII beyond
// what the student already sees in the email itself.
func gradesText(in ReportInput) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\n", in.AssessmentName)
	fmt.Fprintf(&b, "%s (%s)\n\n", in.StudentName, in.StudentID)
	for _, p := range in.Problems {
		fmt.Fprintf(&b, "%s\n", p.Label)
		for _, c := range p.Criteria {
			fmt.Fprintf(&b, "  - %s: %s/%s", c.Name, c.Score, c.Max)
			if c.Comment != "" {
				fmt.Fprintf(&b, " — %s", c.Comment)
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "  Subtotal: %s/%s\n\n", p.Total, p.Max)
	}
	if in.Total != "" && in.Max != "" {
		fmt.Fprintf(&b, "Total: %s/%s\n", in.Total, in.Max)
	}
	return b.String()
}
