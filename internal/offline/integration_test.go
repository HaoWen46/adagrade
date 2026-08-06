package offline

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/localocr"
	"github.com/HaoWen46/adagrade/internal/render"
)

// This is the only test in the package that runs the REAL collaborators: the
// PDFium renderer over the committed demo scan piles and the PP-OCRv5 recognizer
// over their identity boxes. Every other test substitutes both, which is what
// makes them fast and hermetic — and also what makes them unable to answer the
// one question an operator actually has: does this pipeline put the right pages
// in front of the right students?
//
// It is gated on the three local-OCR asset variables (the same idiom as
// internal/localocr's live tests) because the model is an 85 MB download and the
// ONNX runtime is a system library:
//
//	ADAMARKER_OCR_MODEL   -> data/ocr/PP-OCRv5_server_rec_infer.onnx  (make ocr-models)
//	ADAMARKER_OCR_KEYS    -> data/ocr/ppocrv5_dict.txt                (make ocr-models)
//	ADAMARKER_ONNXRUNTIME -> libonnxruntime.{dylib,so}                (>= 1.27)
//
// It calls no paid API: transcription goes to the same httptest openai-compat
// server the orchestrator tests use, so the only cost is CPU.

// ---------------------------------------------------------------------------
// Ground truth, derived from scripts/make-demo-data.py — never guessed.
// ---------------------------------------------------------------------------

// The demo piles are committed byte-identical: make-demo-data.py renders them
// with reportlab's invariant=1 and consumes a FIXED seed in a fixed order
// (SEED=46 for demo-scan-pile.pdf, a separate SEED2=47 for the messy pile, so
// adding artifacts never shifts either). The page ORDER is therefore a pure
// function of the seed, and the tables below were derived by replaying the
// generator's own shuffle rather than by reading the PDFs:
//
//	# clean pile — make_pile(random.Random(46)); the shuffle is the rng's
//	# first draw, so nothing before it has to be replayed.
//	rng = random.Random(46)
//	pages = [(sid, code) for sid, _ in STUDENTS for code in ("Q1", "Q2", "Q3", "Q4")]
//	rng.shuffle(pages)   # pages[i] is page i+1
//
//	# messy pile — make_messy_pile(random.Random(47)); make_roster_v2 /
//	# _mistakes / _big5 run before it but consume no rng draws.
//	rng2 = random.Random(47)
//	rng2.shuffle(pages)  # the 12-tuple literal in make_messy_pile, in source order
//
// The messy derivation is independently corroborated: internal/localocr's
// TestLive_RetryRescuesBoxEdgeArtifacts pins the same 12 pages to the same IDs
// (0-based there, 1-based here) and passes against the real recognizer.
//
// All of these are synthetic demo identities (invented names, @demo.example
// addresses), never real students — the same D14 justification the localocr live
// tests carry.

// demoCell is the (student, problem) a page was PRINTED with. Problem is the
// 1-based number in the "Q<n>" box.
type demoCell struct {
	StudentID string
	Problem   int
}

// cleanPileTruth is what each of demo-scan-pile.pdf's 40 pages carries: 10
// students x 4 problems, every (student, problem) exactly once, shuffled.
var cleanPileTruth = map[int]demoCell{
	1: {"B11902008", 3}, 2: {"B11902006", 2}, 3: {"B11902002", 2}, 4: {"B11902001", 4},
	5: {"B11902006", 4}, 6: {"B11902005", 4}, 7: {"B11902008", 1}, 8: {"B11902002", 3},
	9: {"B11902005", 3}, 10: {"B11902006", 3}, 11: {"B11902003", 1}, 12: {"B11902004", 2},
	13: {"B11902007", 4}, 14: {"B11902010", 1}, 15: {"B11902002", 4}, 16: {"B11902007", 1},
	17: {"B11902003", 4}, 18: {"B11902004", 4}, 19: {"B11902004", 1}, 20: {"B11902009", 4},
	21: {"B11902008", 2}, 22: {"B11902005", 1}, 23: {"B11902009", 3}, 24: {"B11902005", 2},
	25: {"B11902008", 4}, 26: {"B11902001", 1}, 27: {"B11902009", 1}, 28: {"B11902007", 3},
	29: {"B11902006", 1}, 30: {"B11902003", 3}, 31: {"B11902010", 3}, 32: {"B11902010", 4},
	33: {"B11902001", 2}, 34: {"B11902010", 2}, 35: {"B11902003", 2}, 36: {"B11902009", 2},
	37: {"B11902004", 3}, 38: {"B11902001", 3}, 39: {"B11902007", 2}, 40: {"B11902002", 1},
}

// messyPage is one page of demo-scan-pile-messy.pdf. StudentID is the student
// the page BELONGS to — the one whose name is in the name box — which is not
// always the one whose id is legible, and Problem is the printed "Q<n>".
type messyPage struct {
	demoCell
	IDBox messyIDBox
}

// messyIDBox is what the student-ID box holds, which is what decides whether a
// page can be identified at all.
type messyIDBox int

const (
	idPrinted   messyIDBox = iota // a legible roster id
	idUnreadable                  // empty box, or scribbled illegible
	idOffRoster                   // B99999999: a legible id belonging to nobody
	idBlank                       // the whole header is empty: no id, no name, no problem
)

// messyPileTruth is demo-scan-pile-messy.pdf's 12 pages after the SEED2
// shuffle. Three (student, problem) cells are contended by two pages each:
// (B11902001,1) by pages 2+10, (B11902002,2) by pages 8+11 — the generator's
// two deliberate duplicates — and (B11902005,1) by pages 7+1, because the
// scribbled-ID page reuses 皮向文's Q1 identity.
var messyPileTruth = map[int]messyPage{
	1:  {demoCell{"B11902005", 1}, idUnreadable}, // scribbled over the id box
	2:  {demoCell{"B11902001", 1}, idPrinted},    // duplicated by page 10
	3:  {demoCell{"", 4}, idOffRoster},           // B99999999, on no roster
	4:  {demoCell{"B11902004", 4}, idPrinted},
	5:  {demoCell{"", 0}, idBlank},
	6:  {demoCell{"B11902003", 3}, idPrinted},
	7:  {demoCell{"B11902005", 1}, idPrinted}, // same cell as page 1
	8:  {demoCell{"B11902002", 2}, idPrinted}, // duplicated by page 11
	9:  {demoCell{"B11902008", 3}, idUnreadable},
	10: {demoCell{"B11902001", 1}, idPrinted},
	11: {demoCell{"B11902002", 2}, idPrinted},
	12: {demoCell{"B11902006", 2}, idPrinted},
}

// The identity geometry of the demo answer sheet, from make-demo-data.py:
//
//	BOXES = [(0.05, 0.30, "Student ID"), (0.35, 0.60, "Name"), (0.65, 0.90, "Problem")]
//	BOX_TOP, BOX_BOT = 0.02, 0.08
//
// i.e. each box spans (x0f, x1f) horizontally and 0.02..0.08 of the page height
// measured from the TOP edge, which is the same convention Region uses. The
// padding matches what scripts/seed-demo-walkthrough.py seeds for the web
// walkthrough, so this run crops what the app would crop.
const demoIDRegionsJSON = `{"version":1,"regions":[
  {"kind":"student_id","x":0.05,"y":0.02,"w":0.25,"h":0.06,"padding":0.01},
  {"kind":"name",      "x":0.35,"y":0.02,"w":0.25,"h":0.06,"padding":0.01},
  {"kind":"problem_id","x":0.65,"y":0.02,"w":0.25,"h":0.06,"padding":0.01}
]}`

// Fixture paths, relative to this package directory.
const (
	demoRosterPath    = "../../data/demo/demo-roster.csv"
	demoCleanPilePath = "../../data/demo/demo-scan-pile.pdf"
	demoMessyPilePath = "../../data/demo/demo-scan-pile-messy.pdf"
)

// demoProblems is the exam's problem count: the pile is 4 problems per student.
const demoProblems = 4

// minCleanPileMatches is the acceptance bar for the 40-page clean pile.
//
// DO NOT LOWER IT. It is not a description of what the recognizer happens to
// score today; it is the threshold below which this mode should not be shipped,
// because the pages it fails to place are pages a human has to sort by hand.
// Every page of this pile carries a printed id, a printed name and a printed
// problem number in three known boxes — if the pipeline cannot place 38 of 40 of
// those, the answer is to fix the pipeline (or the thresholds, which is a design
// decision), not to move the line.
const minCleanPileMatches = 38

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// ocrAssetsOrSkip returns the three local-OCR asset paths, skipping the test
// unless all of them are configured. Mirrors internal/localocr's live tests: the
// model is an 85 MB download (make ocr-models) and the runtime is a system
// library, so a developer without them gets a skip, not a failure.
func ocrAssetsOrSkip(t *testing.T) (model, keys, lib string) {
	t.Helper()
	model = os.Getenv("ADAMARKER_OCR_MODEL")
	keys = os.Getenv("ADAMARKER_OCR_KEYS")
	lib = os.Getenv("ADAMARKER_ONNXRUNTIME")
	if model == "" || keys == "" || lib == "" {
		t.Skip("offline integration test skipped: set ADAMARKER_OCR_MODEL, ADAMARKER_OCR_KEYS and ADAMARKER_ONNXRUNTIME (make ocr-models prints the first two) to run it")
	}
	return model, keys, lib
}

// integrationDeps builds the real Deps once for the whole test: one ONNX
// session (85 MB of weights) and one PDFium worker pool, shared by the
// subtests. Getenv, Stdout and Stderr are per-subtest and filled in by the
// caller.
func integrationDeps(t *testing.T) Deps {
	t.Helper()
	model, keys, lib := ocrAssetsOrSkip(t)

	engine, err := localocr.New(localocr.Config{ModelPath: model, KeysPath: keys, ONNXRuntimeLibPath: lib})
	if err != nil {
		t.Fatalf("load the local OCR engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	renderer, err := render.NewPDFium()
	if err != nil {
		t.Fatalf("start the PDF renderer: %v", err)
	}
	t.Cleanup(func() { _ = renderer.Close() })

	return Deps{
		Renderer: renderer,
		OCR:      engine,
		// The dictionary MUST come from the engine that produced the lattices
		// (cmd/adamarker/offline.go says the same): a Charset from another keys
		// file indexes the wrong rows and scores nonsense.
		Charset: engine.Charset(),
	}
}

// demoOptions is a complete, valid run over one demo pile, in the mode the
// README recommends: explicit --id-regions rather than the --id-band fallback.
func demoOptions(t *testing.T, pile, out string) Options {
	t.Helper()
	return Options{
		Roster:      demoRosterPath,
		Scans:       []string{pile},
		Out:         out,
		IDRegions:   writeFile(t, t.TempDir(), "id-regions.json", demoIDRegionsJSON),
		IDBand:      DefaultIDBand,
		Problems:    demoProblems,
		DPI:         DefaultDPI,
		LongEdge:    DefaultLongEdge,
		JPEGQuality: DefaultJPEGQuality,
		MinScore:    DefaultMinScore,
		MinMargin:   DefaultMinMargin,
		ExamName:    "demo-exam",
		Concurrency: DefaultConcurrency,
	}
}

// reportRow is one parsed line of match-report.csv, read back through the file
// the operator actually opens rather than from the in-memory results.
type reportRow struct {
	Page      int
	StudentID string
	Problem   int
	Score     float64
	Margin    float64
	Status    string
	Reason    string
}

func (r reportRow) matched() bool { return r.Status != StatusUnmatched }

// readMatchReport parses <out>/match-report.csv by COLUMN NAME, so a future
// appended column cannot silently shift what this test asserts on.
func readMatchReport(t *testing.T, out string) []reportRow {
	t.Helper()
	f, err := os.Open(filepath.Join(out, matchCSVName))
	if err != nil {
		t.Fatalf("open the match report: %v", err)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("parse the match report: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("the match report is empty; it must always carry at least its header")
	}
	col := map[string]int{}
	for i, name := range records[0] {
		col[name] = i
	}
	num := func(rec []string, name string) float64 {
		v, err := strconv.ParseFloat(rec[col[name]], 64)
		if err != nil {
			t.Fatalf("column %s = %q, want a number: %v", name, rec[col[name]], err)
		}
		return v
	}
	rows := make([]reportRow, 0, len(records)-1)
	for _, rec := range records[1:] {
		problem := 0
		if cell := rec[col["problem"]]; cell != "" {
			problem, err = strconv.Atoi(cell)
			if err != nil {
				t.Fatalf("problem column = %q, want an integer: %v", cell, err)
			}
		}
		rows = append(rows, reportRow{
			Page:      int(num(rec, "page")),
			StudentID: rec[col["student_id"]],
			Problem:   problem,
			Score:     num(rec, "score"),
			Margin:    num(rec, "margin"),
			Status:    rec[col["status"]],
			Reason:    rec[col["reason"]],
		})
	}
	return rows
}

// stageElapsed pulls "elapsed_ms=N" off a run.log line.
var stageElapsed = regexp.MustCompile(`elapsed_ms=(\d+)`)

// logStages echoes run.log into the test output and derives the number the
// ledger asked for: the per-page cost of the read stage, which is the
// recognizer's inference cost at recMaxW=1280 (localocr/engine.go) on this
// machine. Nothing here is asserted — a timing assertion would fail on a slower
// CI box while telling nobody anything.
func logStages(t *testing.T, out string, pages, cropsPerPage int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, runLogName))
	if err != nil {
		t.Fatalf("read run.log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		t.Logf("run.log | %s", line)
		if !strings.HasPrefix(line, "read ") {
			continue
		}
		m := stageElapsed.FindStringSubmatch(line)
		if m == nil || pages == 0 {
			continue
		}
		ms, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		t.Logf("read stage: %d pages x %d crops in %s => %.0f ms/page, %.0f ms/crop (PP-OCRv5 server rec, recMaxW=1280)",
			pages, cropsPerPage, time.Duration(ms)*time.Millisecond,
			float64(ms)/float64(pages), float64(ms)/float64(pages*cropsPerPage))
	}
}

// ---------------------------------------------------------------------------
// The clean pile: the whole pipeline, end to end.
// ---------------------------------------------------------------------------

// TestIntegration_CleanPileMatchesEveryPageToItsPrintedIdentity is the test that
// justifies the mode. The 40-page pile is the well-behaved case — every page
// carries a printed id, name and problem number in the three configured boxes —
// so it is where "did the right page reach the right student" has an
// unambiguous answer, and it runs the FULL pipeline (mask, transcribe against a
// local httptest endpoint, bundle) rather than stopping at the match.
func TestIntegration_CleanPileMatchesEveryPageToItsPrintedIdentity(t *testing.T) {
	deps := integrationDeps(t)

	out := filepath.Join(t.TempDir(), "out")
	opts := demoOptions(t, demoCleanPilePath, out)
	opts.BaseURL = transcriptionServer(t, nil).URL
	opts.ProviderKind = ProviderKindOpenAICompat
	opts.APIKeyEnv = "OFFLINE_INTEGRATION_KEY"
	opts.Model = "test-model"

	var stdout, stderr bytes.Buffer
	deps.Stdout, deps.Stderr = &stdout, &stderr
	deps.Getenv = func(k string) string {
		if k == "OFFLINE_INTEGRATION_KEY" {
			return "sk-integration-test"
		}
		return ""
	}

	started := time.Now()
	summary, err := Run(context.Background(), opts, deps)
	if err != nil {
		t.Fatalf("Run: %v\nstderr:\n%s", err, stderr.String())
	}
	t.Logf("clean pile: %d pages in %s (matched %d = auto %d + forced %d, unmatched %d, transcribed %d, failed %d)",
		summary.Pages, time.Since(started).Round(time.Second), summary.Matched(),
		summary.Auto, summary.Forced, summary.UnmatchedTotal(), summary.Transcribed, summary.Failed)
	logStages(t, out, summary.Pages, 3) // three configured regions per page

	if summary.Pages != len(cleanPileTruth) {
		t.Fatalf("rendered %d pages, want %d: the committed pile changed and the ground-truth table is stale", summary.Pages, len(cleanPileTruth))
	}

	rows := readMatchReport(t, out)
	if len(rows) != len(cleanPileTruth) {
		t.Fatalf("match-report.csv has %d rows, want one per page (%d)", len(rows), len(cleanPileTruth))
	}

	// The two findings are counted separately because they are not the same
	// severity. A page nobody could place costs a human two minutes; a page
	// placed under the WRONG STUDENT is graded as someone else's work, and one
	// of those is a failure however good the totals look.
	matched, wrongStudent, wrongProblem := 0, 0, 0
	seen := map[int]bool{}
	for _, row := range rows {
		want, ok := cleanPileTruth[row.Page]
		if !ok {
			t.Fatalf("report names page %d, which is not in the 40-page pile", row.Page)
		}
		if seen[row.Page] {
			t.Fatalf("page %d has two rows in the report", row.Page)
		}
		seen[row.Page] = true

		if !row.matched() {
			t.Logf("page %2d unmatched (%s): score %.4f margin %.4f", row.Page, row.Reason, row.Score, row.Margin)
			continue
		}
		matched++
		if row.StudentID != want.StudentID {
			wrongStudent++
			t.Errorf("page %d was assigned to the wrong student (score %.4f margin %.4f, status %s): this is the failure mode the whole design exists to avoid",
				row.Page, row.Score, row.Margin, row.Status)
			continue
		}
		if row.Problem != want.Problem {
			wrongProblem++
			t.Errorf("page %d (student matched correctly) was filed under problem %d, want %d: the answer reaches the right student under the wrong question",
				row.Page, row.Problem, want.Problem)
		}
	}
	if wrongStudent > 0 {
		t.Errorf("%d of %d matched pages went to the wrong student; zero is the only acceptable count", wrongStudent, matched)
	}
	// The bar, and it does not move: see minCleanPileMatches.
	if matched < minCleanPileMatches {
		t.Errorf("matched %d of %d pages, want at least %d (auto %d, forced %d, unmatched %v)",
			matched, len(rows), minCleanPileMatches, summary.Auto, summary.Forced, summary.Unmatched)
	}
	if matched != summary.Matched() {
		t.Errorf("the report says %d matched pages and the summary says %d: the audit trail and the run disagree", matched, summary.Matched())
	}
	if wrongProblem == 0 && wrongStudent == 0 {
		t.Logf("all %d matched pages agree with the generator's ground truth (student and problem)", matched)
	}

	// Every matched page was masked, sent and transcribed: the httptest endpoint
	// answers every call, so anything less means a cell was dropped between the
	// match and the bundle.
	if summary.Transcribed != matched || summary.Failed != 0 {
		t.Errorf("transcribed/failed = %d/%d, want %d/0", summary.Transcribed, summary.Failed, matched)
	}
	masked, err := os.ReadDir(filepath.Join(out, maskedDirName))
	if err != nil || len(masked) != matched {
		t.Errorf("masked/ holds %d pages, want the %d matched ones (err %v)", len(masked), matched, err)
	}
	requireFile(t, filepath.Join(out, maskPreviewName))

	// The professor's bundle: one tree per problem, named {slug}-p{n} by
	// export.RootDir (ExamName "demo-exam" slugifies to itself). All four
	// problems are answered by this pile, so all four trees must exist.
	for q := 1; q <= demoProblems; q++ {
		dir := filepath.Join(out, bundleDirName, fmt.Sprintf("demo-exam-p%d", q))
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) == 0 {
			t.Errorf("bundle/ is missing problem %d's tree (%s): %v (%d entries)", q, dir, err, len(entries))
		}
	}
}

// ---------------------------------------------------------------------------
// The messy pile: the edge paths.
// ---------------------------------------------------------------------------

// TestIntegration_MessyPileHandlesTheEdgePaths runs the identification half
// (--stop-after match — nothing to send, nothing to spend) over the 12-page
// intake pile: duplicates, an unreadable id box, an off-roster id and a blank
// page. What is asserted here is what the DESIGN promises, not what the
// recognizer happened to score:
//
//   - a page whose student is on the roster is never assigned to a DIFFERENT
//     student;
//   - a cell that some page actually prints is won by one of the pages that
//     print it, so no interloper displaces a real page;
//   - the blank page, which carries no ink in any box, is not assigned to
//     anybody.
//
// The pages with NO correct answer — the scribbled id, the empty id, the
// off-roster B99999999 — are logged rather than pinned, because "unmatched" is
// not the design's promise for them: a forced matcher is allowed to place a page
// on the strength of a name and a problem number, and it is allowed to be wrong
// about a student who is not on the roster at all. That is the hazard the banner
// and the README name, and the assertions above are the ones that bound it.
func TestIntegration_MessyPileHandlesTheEdgePaths(t *testing.T) {
	deps := integrationDeps(t)

	out := filepath.Join(t.TempDir(), "out")
	opts := demoOptions(t, demoMessyPilePath, out)
	opts.StopAfter = StopAfterMatch

	var stdout, stderr bytes.Buffer
	deps.Stdout, deps.Stderr = &stdout, &stderr

	started := time.Now()
	summary, err := Run(context.Background(), opts, deps)
	if err != nil {
		t.Fatalf("Run: %v\nstderr:\n%s", err, stderr.String())
	}
	t.Logf("messy pile: %d pages in %s (matched %d = auto %d + forced %d, unmatched %v)",
		summary.Pages, time.Since(started).Round(time.Second), summary.Matched(), summary.Auto, summary.Forced, summary.Unmatched)
	logStages(t, out, summary.Pages, 3)

	rows := readMatchReport(t, out)
	if len(rows) != len(messyPileTruth) {
		t.Fatalf("match-report.csv has %d rows, want one per page (%d)", len(rows), len(messyPileTruth))
	}

	// Which page won which cell, so the no-displacement rule below can be
	// checked over the whole batch rather than page by page.
	wonBy := map[demoCell]int{}
	for _, row := range rows {
		page, ok := messyPileTruth[row.Page]
		if !ok {
			t.Fatalf("report names page %d, which is not in the 12-page messy pile", row.Page)
		}
		if !row.matched() {
			t.Logf("page %2d (%v) unmatched (%s): score %.4f margin %.4f", row.Page, page.IDBox, row.Reason, row.Score, row.Margin)
			continue
		}
		got := demoCell{row.StudentID, row.Problem}
		if prev, dup := wonBy[got]; dup {
			t.Errorf("pages %d and %d were both assigned to %v: one cell, two pages", prev, row.Page, got)
		}
		wonBy[got] = row.Page
		t.Logf("page %2d (%v) -> problem %d, %s, score %.4f margin %.4f", row.Page, page.IDBox, row.Problem, row.Status, row.Score, row.Margin)

		// The rule that matters: a page belonging to a roster student may be
		// filed under the wrong PROBLEM (its own cell may already be taken by its
		// duplicate twin) but never under another STUDENT.
		if page.StudentID != "" && row.StudentID != page.StudentID {
			t.Errorf("page %d belongs to a roster student and was assigned to a different one (status %s, score %.4f, margin %.4f)",
				row.Page, row.Status, row.Score, row.Margin)
		}
	}

	// The blank page has no ink in any of the three boxes, so every field is
	// unread, every cell scores exactly 0, and --min-score must reject it. This
	// one IS pinned: a run that places a blank page has placed it on nothing.
	for _, row := range rows {
		if messyPileTruth[row.Page].IDBox != idBlank {
			continue
		}
		if row.matched() {
			t.Errorf("page %d is blank and was assigned to %s problem %d (score %.4f): a page with no identity on it must not be placed",
				row.Page, row.StudentID, row.Problem, row.Score)
		}
		if row.Score != 0 {
			t.Errorf("blank page %d scored %.6f, want exactly 0: no field can be read off it", row.Page, row.Score)
		}
	}

	// No displacement: every cell that at least one page PRINTS is won by one of
	// the pages that print it. This is what keeps the off-roster page (and any
	// misread) from stealing a real page's slot — the failure it would otherwise
	// cause is silent, because the displaced page just shows up as "forced"
	// somewhere else.
	printedBy := map[demoCell][]int{}
	for page, truth := range messyPileTruth {
		if truth.StudentID == "" {
			continue
		}
		printedBy[truth.demoCell] = append(printedBy[truth.demoCell], page)
	}
	for cellID, pages := range printedBy {
		winner, taken := wonBy[cellID]
		if !taken {
			// Legal: every page printing this cell may have been rejected by the
			// thresholds (both pages of a contended cell can be unreadable).
			continue
		}
		if !containsInt(pages, winner) {
			t.Errorf("cell %v was won by page %d, which does not print it; the pages that do are %v", cellID, winner, pages)
		}
	}

	// The duplicates the generator planted: two pages claim one cell, so at most
	// one of them can hold it and the other is either set aside or filed under
	// another problem of the SAME student (asserted above, per page).
	for cellID, pages := range printedBy {
		if len(pages) < 2 {
			continue
		}
		t.Logf("contended cell %v is printed by pages %v; the report gives it to page %d", cellID, pages, wonBy[cellID])
	}
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// String makes the messy-pile logs readable without a lookup table.
func (b messyIDBox) String() string {
	switch b {
	case idPrinted:
		return "printed id"
	case idUnreadable:
		return "unreadable id"
	case idOffRoster:
		return "off-roster id"
	case idBlank:
		return "blank page"
	}
	return "unknown"
}
