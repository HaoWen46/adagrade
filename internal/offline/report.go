package offline

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// matchCSVHeader is the report's column order, and it is a compatibility
// surface: an operator's spreadsheet formula and a wrapper script both index by
// position. Columns may be APPENDED; existing ones must not move or be renamed.
var matchCSVHeader = []string{
	"page", "source_pdf", "source_page",
	"student_id", "name", "problem",
	"score", "score_id", "score_name", "score_problem", "margin",
	"method", "status", "reason",
}

// scoreFormat is the float format for every score column. Six decimals is well
// past the precision the scores mean anything at, and a FIXED format matters
// more than the digits: %v would print 0.5 for one row and 0.13750000000000001
// for the next, and a column that changes width row to row is unreadable and
// diffs badly. The JSON report carries the unrounded values.
const scoreFormat = "%.6f"

// Meta is what the CSV cannot carry: the run's inputs and settings. It is the
// difference between a report you can reproduce and a list of numbers — an
// operator looking at a match six months later needs to know which roster, which
// thresholds and which identity regions produced it.
type Meta struct {
	Roster      string     `json:"roster"`
	Scans       []string   `json:"scans"`
	Problems    int        `json:"problems"`
	MinScore    float64    `json:"min_score"`
	MinMargin   float64    `json:"min_margin"`
	Weights     [3]float64 `json:"weights"` // student_id, name, problem — in that order
	IDBand      float64    `json:"id_band"`
	IDRegions   string     `json:"id_regions"` // path, empty when the band fallback was used
	GeneratedAt time.Time  `json:"generated_at"`
}

// matchJSON is one result as the JSON report writes it: the same field names as
// the CSV columns, so the two files can be read with one mental model.
//
// It exists as a separate type rather than tags on MatchResult because
// MatchResult embeds the whole Page, image bytes included. Serializing that
// would put a megabyte of base64 per page into the report — and, worse, put
// page pixels in a file whose whole purpose is to be small enough to read.
type matchJSON struct {
	Page         int     `json:"page"`
	SourcePDF    string  `json:"source_pdf"`
	SourcePage   int     `json:"source_page"`
	StudentID    string  `json:"student_id"`
	Name         string  `json:"name"`
	Problem      int     `json:"problem"`
	Score        float64 `json:"score"`
	ScoreID      float64 `json:"score_id"`
	ScoreName    float64 `json:"score_name"`
	ScoreProblem float64 `json:"score_problem"`
	Margin       float64 `json:"margin"`
	Method       string  `json:"method"`
	Status       string  `json:"status"`
	Reason       string  `json:"reason"`
}

// matchReport is the JSON document: settings first, then rows.
type matchReport struct {
	Meta    Meta        `json:"meta"`
	Results []matchJSON `json:"results"`
}

// assignment returns the identity columns as the report must print them.
//
// An unmatched row names NOBODY, even if the caller's result still carries the
// student it came closest to. The report is the only audit trail this mode
// produces, and a student id sitting on an "unmatched" row would be read as an
// assignment by every human and every script that ever opens the file. The
// scores stay — they say how close it was — but the identity does not.
func (r MatchResult) assignment() (studentID, name string, problem int) {
	if r.Status == StatusUnmatched {
		return "", "", 0
	}
	return r.StudentID, r.StudentName, r.Problem
}

// WriteMatchCSV writes the match report: one header row, then one row per page
// in page order. The first line of the body is data — no comment preamble, no
// blank line — so the file opens directly in a spreadsheet and parses with any
// CSV reader.
//
// A run where nothing matched still writes the header, because an empty file is
// indistinguishable from a crashed one.
func WriteMatchCSV(path string, results []MatchResult) error {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(matchCSVHeader); err != nil {
		return newOutDirError(err, "cannot format the match report header")
	}
	for _, r := range results {
		studentID, name, problem := r.assignment()
		problemCell := ""
		if problem > 0 {
			problemCell = strconv.Itoa(problem)
		}
		record := []string{
			strconv.Itoa(r.Page.Index),
			r.Page.SourcePDF,
			strconv.Itoa(r.Page.SourcePage),
			studentID,
			name,
			problemCell,
			fmt.Sprintf(scoreFormat, r.Score),
			fmt.Sprintf(scoreFormat, r.ScoreID),
			fmt.Sprintf(scoreFormat, r.ScoreName),
			fmt.Sprintf(scoreFormat, r.ScoreProblem),
			fmt.Sprintf(scoreFormat, r.Margin),
			r.Method,
			r.Status,
			r.Reason,
		}
		if err := w.Write(record); err != nil {
			return newOutDirError(err, "cannot format the match report row for page %d", r.Page.Index)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return newOutDirError(err, "cannot format the match report")
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return newOutDirError(err, "cannot write the match report %s", path)
	}
	return nil
}

// WriteMatchJSON writes the machine-readable twin of the CSV: the same rows
// with unrounded scores, plus the run settings the CSV has no room for.
func WriteMatchJSON(path string, results []MatchResult, meta Meta) error {
	doc := matchReport{Meta: meta, Results: make([]matchJSON, 0, len(results))}
	for _, r := range results {
		studentID, name, problem := r.assignment()
		doc.Results = append(doc.Results, matchJSON{
			Page:         r.Page.Index,
			SourcePDF:    r.Page.SourcePDF,
			SourcePage:   r.Page.SourcePage,
			StudentID:    studentID,
			Name:         name,
			Problem:      problem,
			Score:        r.Score,
			ScoreID:      r.ScoreID,
			ScoreName:    r.ScoreName,
			ScoreProblem: r.ScoreProblem,
			Margin:       r.Margin,
			Method:       r.Method,
			Status:       r.Status,
			Reason:       r.Reason,
		})
	}
	// Indented and newline-terminated: this file is read by people as often as
	// by programs, and it diffs between runs.
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return newOutDirError(err, "cannot format the match report JSON")
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return newOutDirError(err, "cannot write the match report %s", path)
	}
	return nil
}
