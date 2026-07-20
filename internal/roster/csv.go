// Package roster parses and imports the student roster CSV (docs/DECISIONS.md D13):
// UTF-8 only (non-UTF-8 files — typically Excel's default Big5 "Save As CSV" — are
// rejected whole with an actionable message), required header student_id,name,email
// (any order/case, BOM tolerated, extra columns ignored), student_id as the
// identity/upsert key. Duplicate guards (roster-lifecycle plan 2026-07-10): exact
// duplicate student_ids, student_ids that collide under studentid.Normalize, and
// case-insensitive duplicate emails are all per-line errors. Any error rejects the
// whole import — a half-imported roster is worse than none.
package roster

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/HaoWen46/adagrade/internal/studentid"
)

// Row is one validated roster line.
type Row struct {
	StudentID string
	Name      string
	Email     string // normalized lowercase
	Line      int    // 1-based line in the file, for error reporting
}

// ParseError points at a specific line. Msg never contains cell contents beyond the
// student_id column (PII policy D14 — names/emails stay out of logs and errors).
type ParseError struct {
	Line int    `json:"line"`
	Msg  string `json:"msg"`
}

func (e ParseError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// errNotUTF8 is the single whole-file rejection for non-UTF-8 input. The Excel
// wording is load-bearing: "CSV UTF-8 (Comma delimited)" is the exact Save As
// entry that fixes the default Big5/CP950 export on Chinese-locale Windows.
const errNotUTF8 = "file is not valid UTF-8 — in Excel, use Save As → 'CSV UTF-8 (Comma delimited)'"

// Parse reads the CSV and returns validated rows plus every problem found.
func Parse(r io.Reader) ([]Row, []ParseError) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, []ParseError{{Line: 1, Msg: "cannot read file"}}
	}
	if !utf8.Valid(data) {
		return nil, []ParseError{{Line: lineOfFirstInvalidUTF8(data), Msg: errNotUTF8}}
	}

	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = -1 // header defines the width; extra columns are ignored
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		return nil, []ParseError{{Line: 1, Msg: "cannot read header row (empty file?)"}}
	}
	idx := map[string]int{}
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")))
		idx[h] = i
	}
	var missing []string
	for _, want := range []string{"student_id", "name", "email"} {
		if _, ok := idx[want]; !ok {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return nil, []ParseError{{Line: 1, Msg: "missing required column(s): " + strings.Join(missing, ", ")}}
	}

	var rows []Row
	var errs []ParseError
	seen := map[string]int{}      // exact student_id -> first line
	seenNorm := map[string]Row{}  // studentid.Normalize(student_id) -> first row
	seenEmail := map[string]Row{} // lowercase email -> first row
	line := 1
	for {
		rec, err := cr.Read()
		line++
		if err == io.EOF {
			break
		}
		if err != nil {
			errs = append(errs, ParseError{Line: line, Msg: "malformed CSV row"})
			continue
		}
		get := func(col string) string {
			i := idx[col]
			if i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		row := Row{
			StudentID: get("student_id"),
			Name:      get("name"),
			Email:     strings.ToLower(get("email")),
			Line:      line,
		}
		switch {
		case row.StudentID == "":
			errs = append(errs, ParseError{Line: line, Msg: "empty student_id"})
			continue
		case row.Name == "":
			errs = append(errs, ParseError{Line: line, Msg: fmt.Sprintf("empty name for student_id %s", row.StudentID)})
			continue
		case !strings.Contains(row.Email, "@"):
			errs = append(errs, ParseError{Line: line, Msg: fmt.Sprintf("invalid email for student_id %s", row.StudentID)})
			continue
		}
		if first, dup := seen[row.StudentID]; dup {
			errs = append(errs, ParseError{Line: line, Msg: fmt.Sprintf("duplicate student_id %s (first at line %d)", row.StudentID, first)})
			continue
		}
		// Duplicate guards past the exact-id check. A row can trip both (two
		// normalize-colliding ids sharing an email) — report both, keep neither.
		bad := false
		norm := studentid.Normalize(row.StudentID)
		if first, dup := seenNorm[norm]; dup {
			// Same id once case/width/punctuation noise is stripped — filename
			// ingest and scan matching fold ids this way, so they'd collide.
			errs = append(errs, ParseError{Line: line, Msg: fmt.Sprintf(
				"student_id %s is the same as student_id %s (line %d) after normalization (case/width/punctuation)",
				row.StudentID, first.StudentID, first.Line)})
			bad = true
		}
		if first, dup := seenEmail[row.Email]; dup {
			// Both ids and lines, never the address itself (D14).
			errs = append(errs, ParseError{Line: line, Msg: fmt.Sprintf(
				"duplicate email: student_id %s shares an email address with student_id %s (line %d)",
				row.StudentID, first.StudentID, first.Line)})
			bad = true
		}
		if bad {
			continue
		}
		seen[row.StudentID] = line
		seenNorm[norm] = row
		seenEmail[row.Email] = row
		rows = append(rows, row)
	}
	return rows, errs
}

// lineOfFirstInvalidUTF8 returns the 1-based line of the first invalid byte, so
// the operator can find the offending record (usually the first non-ASCII name).
func lineOfFirstInvalidUTF8(data []byte) int {
	line := 1
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			return line
		}
		if r == '\n' {
			line++
		}
		i += size
	}
	return 1
}
