package offline

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parseCSV reads the written report back with a real CSV reader, so quoting is
// checked the way a spreadsheet would check it.
func parseCSV(t *testing.T, data []byte) [][]string {
	t.Helper()
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		t.Fatalf("the report is not valid CSV: %v", err)
	}
	return rows
}

// reportFixture is the three-row shape every report has to survive: a clean
// automatic match, a page the solver moved, and a page nobody could place.
func reportFixture() []MatchResult {
	return []MatchResult{
		{
			Page:         Page{Index: 1, SourcePDF: "scans/exam-a.pdf", SourcePage: 1, JPEG: []byte("bytes")},
			StudentID:    "B10902066",
			StudentName:  "王小明",
			Problem:      2,
			Score:        0.876543,
			ScoreID:      0.400000,
			ScoreName:    0.270000,
			ScoreProblem: 0.206543,
			Margin:       0.512345,
			Method:       MethodLattice,
			Status:       StatusAuto,
		},
		{
			Page:         Page{Index: 2, SourcePDF: "scans/exam-a.pdf", SourcePage: 2},
			StudentID:    "B10902067",
			StudentName:  "Ada Lovelace",
			Problem:      1,
			Score:        0.5,
			ScoreID:      0.2,
			ScoreName:    0.15,
			ScoreProblem: 0.15,
			Margin:       0.04,
			Method:       MethodLattice,
			Status:       StatusForced,
		},
		{
			Page:         Page{Index: 3, SourcePDF: "scans/exam-b.pdf", SourcePage: 1},
			Score:        0.1375,
			ScoreID:      0.045,
			ScoreName:    0.03,
			ScoreProblem: 0.0625,
			Margin:       0,
			Method:       MethodLattice,
			Status:       StatusUnmatched,
			Reason:       ReasonLowScore,
		},
	}
}

func reportMeta() Meta {
	return Meta{
		Roster:      "roster.csv",
		Scans:       []string{"scans/exam-a.pdf", "scans/exam-b.pdf"},
		Problems:    3,
		MinScore:    DefaultMinScore,
		MinMargin:   DefaultMinMargin,
		Weights:     [3]float64{WeightStudentID, WeightName, WeightProblem},
		IDBand:      DefaultIDBand,
		IDRegions:   "",
		GeneratedAt: time.Date(2026, 7, 15, 9, 30, 0, 0, time.UTC),
	}
}

// TestWriteMatchCSV_Format pins the file byte for byte where it matters: the
// header (scripts index by column), the first data row on line 2 (no comment
// preamble), and the %.6f float format.
func TestWriteMatchCSV_Format(t *testing.T) {
	path := filepath.Join(t.TempDir(), "match-report.csv")
	if err := WriteMatchCSV(path, reportFixture()); err != nil {
		t.Fatalf("WriteMatchCSV: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(got) != 4 {
		t.Fatalf("got %d lines, want 4 (header + 3 rows): %q", len(got), got)
	}

	const wantHeader = "page,source_pdf,source_page,student_id,name,problem,score,score_id,score_name,score_problem,margin,method,status,reason"
	if got[0] != wantHeader {
		t.Errorf("header:\n got %q\nwant %q", got[0], wantHeader)
	}

	const wantRow1 = "1,scans/exam-a.pdf,1,B10902066,王小明,2,0.876543,0.400000,0.270000,0.206543,0.512345,lattice,auto,"
	if got[1] != wantRow1 {
		t.Errorf("row 1:\n got %q\nwant %q", got[1], wantRow1)
	}

	const wantRow2 = "2,scans/exam-a.pdf,2,B10902067,Ada Lovelace,1,0.500000,0.200000,0.150000,0.150000,0.040000,lattice,forced,"
	if got[2] != wantRow2 {
		t.Errorf("row 2:\n got %q\nwant %q", got[2], wantRow2)
	}

	// The unmatched row names nobody and claims no problem: the empty cells are
	// the point, since a "0" there would read as problem zero.
	const wantRow3 = "3,scans/exam-b.pdf,1,,,,0.137500,0.045000,0.030000,0.062500,0.000000,lattice,unmatched,low-score"
	if got[3] != wantRow3 {
		t.Errorf("row 3:\n got %q\nwant %q", got[3], wantRow3)
	}
}

// TestWriteMatchCSV_UnmatchedNamesNobody: even if a caller hands over a result
// that still carries a student, an unmatched row must not print one. The CSV is
// the audit trail, and a name beside "unmatched" would read as an assignment.
func TestWriteMatchCSV_UnmatchedNamesNobody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "match-report.csv")
	results := []MatchResult{{
		Page:        Page{Index: 1, SourcePDF: "a.pdf", SourcePage: 1},
		StudentID:   "B10902066",
		StudentName: "王小明",
		Problem:     2,
		Method:      MethodLattice,
		Status:      StatusUnmatched,
		Reason:      ReasonAmbiguous,
	}}
	if err := WriteMatchCSV(path, results); err != nil {
		t.Fatalf("WriteMatchCSV: %v", err)
	}
	data, _ := os.ReadFile(path)
	row := strings.Split(strings.TrimRight(string(data), "\n"), "\n")[1]
	if strings.Contains(row, "B10902066") || strings.Contains(row, "王小明") {
		t.Errorf("unmatched row leaked an identity: %q", row)
	}
	if !strings.HasPrefix(row, "1,a.pdf,1,,,,") {
		t.Errorf("row = %q, want empty student_id, name and problem cells", row)
	}
}

// TestWriteMatchCSV_QuotesSeparators: a name with a comma or a quote in it must
// not shift every later column.
func TestWriteMatchCSV_QuotesSeparators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "match-report.csv")
	results := []MatchResult{{
		Page:        Page{Index: 1, SourcePDF: "a,b.pdf", SourcePage: 1},
		StudentID:   "X1",
		StudentName: `Smith, "Bo"`,
		Problem:     1,
		Method:      MethodLattice,
		Status:      StatusAuto,
	}}
	if err := WriteMatchCSV(path, results); err != nil {
		t.Fatalf("WriteMatchCSV: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rows := parseCSV(t, data)
	if len(rows) != 2 {
		t.Fatalf("got %d records, want 2", len(rows))
	}
	if len(rows[1]) != len(rows[0]) {
		t.Fatalf("data row has %d fields, header has %d", len(rows[1]), len(rows[0]))
	}
	if rows[1][4] != `Smith, "Bo"` {
		t.Errorf("name round-tripped as %q", rows[1][4])
	}
	if rows[1][1] != "a,b.pdf" {
		t.Errorf("source_pdf round-tripped as %q", rows[1][1])
	}
}

// TestWriteMatchCSV_HeaderOnly: a run where nothing matched still writes a
// readable file rather than an empty one.
func TestWriteMatchCSV_HeaderOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "match-report.csv")
	if err := WriteMatchCSV(path, nil); err != nil {
		t.Fatalf("WriteMatchCSV: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "page,source_pdf"; !strings.HasPrefix(string(data), want) {
		t.Errorf("empty report = %q, want it to start with the header", string(data))
	}
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Errorf("empty report has %d lines, want 1 (the header)", n)
	}
}

func TestWriteMatchJSON_ShapeAndMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "match-report.json")
	if err := WriteMatchJSON(path, reportFixture(), reportMeta()); err != nil {
		t.Fatalf("WriteMatchJSON: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var doc struct {
		Meta struct {
			Roster      string     `json:"roster"`
			Scans       []string   `json:"scans"`
			Problems    int        `json:"problems"`
			MinScore    float64    `json:"min_score"`
			MinMargin   float64    `json:"min_margin"`
			Weights     [3]float64 `json:"weights"`
			IDBand      float64    `json:"id_band"`
			IDRegions   string     `json:"id_regions"`
			GeneratedAt time.Time  `json:"generated_at"`
		} `json:"meta"`
		Results []struct {
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
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}

	if len(doc.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(doc.Results))
	}
	first := doc.Results[0]
	if first.Page != 1 || first.SourcePDF != "scans/exam-a.pdf" || first.SourcePage != 1 {
		t.Errorf("result 0 page identity = %+v", first)
	}
	if first.StudentID != "B10902066" || first.Name != "王小明" || first.Problem != 2 {
		t.Errorf("result 0 assignment = %+v", first)
	}
	if first.Score != 0.876543 || first.Margin != 0.512345 {
		t.Errorf("result 0 scores = %v / %v, want full precision (not the CSV's %%.6f rounding)", first.Score, first.Margin)
	}
	if first.Method != MethodLattice || first.Status != StatusAuto || first.Reason != "" {
		t.Errorf("result 0 verdict = %q/%q/%q", first.Method, first.Status, first.Reason)
	}
	if last := doc.Results[2]; last.Status != StatusUnmatched || last.Reason != ReasonLowScore || last.StudentID != "" || last.Problem != 0 {
		t.Errorf("unmatched result = %+v", last)
	}

	m := doc.Meta
	if m.Roster != "roster.csv" || len(m.Scans) != 2 || m.Problems != 3 {
		t.Errorf("meta inputs = %+v", m)
	}
	if m.MinScore != DefaultMinScore || m.MinMargin != DefaultMinMargin {
		t.Errorf("meta thresholds = %v / %v", m.MinScore, m.MinMargin)
	}
	if m.Weights != [3]float64{WeightStudentID, WeightName, WeightProblem} {
		t.Errorf("meta weights = %v, want the package constants", m.Weights)
	}
	if m.IDBand != DefaultIDBand {
		t.Errorf("meta id_band = %v", m.IDBand)
	}
	if !m.GeneratedAt.Equal(time.Date(2026, 7, 15, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("meta generated_at = %v", m.GeneratedAt)
	}

	// The page image bytes must not travel into the report.
	if strings.Contains(string(data), "bytes") {
		t.Error("the JSON report carries page image bytes; it must carry only the page's identity")
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("the JSON report should end with a newline")
	}
}

func TestWriteMatchReports_Errors(t *testing.T) {
	dir := t.TempDir()
	// A path whose parent is a FILE cannot be written.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(blocker, "match-report.csv")

	if err := WriteMatchCSV(bad, reportFixture()); err == nil {
		t.Error("WriteMatchCSV: want an error for an unwritable path")
	} else if ExitCode(err) != ExitOutDir {
		t.Errorf("WriteMatchCSV: ExitCode = %d, want %d", ExitCode(err), ExitOutDir)
	}
	if err := WriteMatchJSON(bad, reportFixture(), reportMeta()); err == nil {
		t.Error("WriteMatchJSON: want an error for an unwritable path")
	} else if ExitCode(err) != ExitOutDir {
		t.Errorf("WriteMatchJSON: ExitCode = %d, want %d", ExitCode(err), ExitOutDir)
	}
}

// TestWriteMatchReports_PrivateModes — the match report is a list of student
// ids and names next to the pages they were assigned. It is the most directly
// readable identity artifact this mode writes, so it is 0600 like the rest.
func TestWriteMatchReports_PrivateModes(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "match-report.csv")
	jsonPath := filepath.Join(dir, "match-report.json")

	if err := WriteMatchCSV(csvPath, reportFixture()); err != nil {
		t.Fatalf("WriteMatchCSV: %v", err)
	}
	if err := WriteMatchJSON(jsonPath, reportFixture(), reportMeta()); err != nil {
		t.Fatalf("WriteMatchJSON: %v", err)
	}
	for _, path := range []string{csvPath, jsonPath} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", path, got)
		}
	}
}

// TestWriteMatchReports_ReassertTheModeOnRewrite — --force licenses writing
// over a previous run's directory, and os.WriteFile only applies its mode when
// it CREATES the file, so a looser report left behind by an earlier run (or by
// an operator) would otherwise keep its permissions.
func TestWriteMatchReports_ReassertTheModeOnRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "match-report.csv")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteMatchCSV(path, reportFixture()); err != nil {
		t.Fatalf("WriteMatchCSV: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("%s mode = %04o, want 0600", path, got)
	}
}
