package offline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/localocr"
	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/roster"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// Run is the only place the stages meet, so these tests assert the things no
// single stage can: that the stage ORDER is right (a failure at stage N leaves
// exactly the artifacts stages 1..N-1 produce), that the artifacts that survive
// a failure are the ones needed to diagnose it, and that nothing student-shaped
// reaches run.log or stderr.

// --- fixtures --------------------------------------------------------------

// writeRosterCSV writes rows in the format LoadRoster accepts, so the
// orchestrator tests go through the real parse path rather than a hand-built
// []roster.Row.
func writeRosterCSV(t *testing.T, dir string, rows []roster.Row) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("student_id,name,email\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%s,%s,%s\n", r.StudentID, r.Name, r.Email)
	}
	return writeFile(t, dir, "roster.csv", b.String())
}

// scriptedReader is the local-OCR stand-in: the Nth crop gets the Nth scripted
// set of lines. Every orchestrator test runs in band mode (the --id-band
// fallback), where ReadIdentity reads exactly ONE crop per page and Run reads
// pages in order, so the call counter IS the page's position.
type scriptedReader struct {
	mu    sync.Mutex
	calls int
	lines func(call int) []localocr.LineLattice
	err   error
}

func (r *scriptedReader) ReadLattices(ctx context.Context, _ imaging.IDCrop) ([]localocr.LineLattice, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The real engine propagates ctx and surfaces a cancellation as a read
	// failure, which ReadIdentity types as an *OCRError. The stub does the same,
	// so a test can tell whether Run reports a Ctrl-C as a cancellation or as a
	// broken local OCR install.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	call := r.calls
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.lines(call), nil
}

// identityBandLines makes page `call` read one student's ID and one problem
// label off its band crop. Pages cycle through the roster and then through the
// problems, so an N-student M-problem fixture with N*M pages fills every cell
// exactly once.
func identityBandLines(t *testing.T, rows []roster.Row, problems int) func(int) []localocr.LineLattice {
	t.Helper()
	return func(call int) []localocr.LineLattice {
		row := rows[call%len(rows)]
		problem := call/len(rows) + 1
		if problem > problems {
			problem = problems
		}
		return []localocr.LineLattice{
			textLine(t, row.StudentID, 0.9),
			textLine(t, "Q"+strconv.Itoa(problem), 0.9),
		}
	}
}

// blankBandLines is a page whose identity band holds no ink at all: the field
// reads as UNREAD, every cell scores zero, and nothing can be matched.
func blankBandLines(int) []localocr.LineLattice { return nil }

type runFixture struct {
	opts   Options
	deps   Deps
	out    string
	rows   []roster.Row
	reader *scriptedReader
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	env    map[string]string
}

// newRunFixture builds a complete, valid run: a roster CSV on disk, one scan
// file the Fake renderer turns into `pages` pages, and a scripted reader that
// makes each page identify itself.
func newRunFixture(t *testing.T, students, pages, problems int) *runFixture {
	t.Helper()
	dir := t.TempDir()
	rows := fixtureRoster(t, students)
	f := &runFixture{
		out:    filepath.Join(dir, "out"),
		rows:   rows,
		reader: &scriptedReader{lines: identityBandLines(t, rows, problems)},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		env:    map[string]string{},
	}
	f.opts = Options{
		Roster:      writeRosterCSV(t, dir, rows),
		Scans:       []string{writeScan(t, dir, "scan.pdf")},
		Out:         f.out,
		IDBand:      DefaultIDBand,
		Problems:    problems,
		DPI:         DefaultDPI,
		LongEdge:    DefaultLongEdge,
		JPEGQuality: DefaultJPEGQuality,
		MinScore:    DefaultMinScore,
		MinMargin:   DefaultMinMargin,
		ExamName:    "Offline Exam",
		Concurrency: DefaultConcurrency,
	}
	f.deps = Deps{
		Renderer: render.NewFake(pages),
		OCR:      f.reader,
		Charset:  matchCharset(),
		Getenv:   func(k string) string { return f.env[k] },
		Stdout:   f.stdout,
		Stderr:   f.stderr,
	}
	return f
}

// useProvider points the fixture at a base URL, the route that needs no
// provider table.
func (f *runFixture) useProvider(baseURL string) {
	f.opts.BaseURL = baseURL
	f.opts.ProviderKind = ProviderKindOpenAICompat
	f.opts.APIKeyEnv = "OFFLINE_TEST_KEY"
	f.opts.Model = "test-model"
	f.env["OFFLINE_TEST_KEY"] = "sk-test"
}

func (f *runFixture) path(parts ...string) string {
	return filepath.Join(append([]string{f.out}, parts...)...)
}

// transcriptionServer answers every transcription call with one prose block.
// Run builds its provider through the REAL BuildProvider, so the only way to
// drive the whole pipeline is with a URL that answers — this one is localhost.
func transcriptionServer(t *testing.T, onCall func(call int)) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := calls
		calls++
		mu.Unlock()
		if onCall != nil {
			onCall(n)
		}
		body, _ := json.Marshal(map[string]any{
			"model": "test-model",
			"choices": []map[string]any{{
				"message": map[string]any{
					"content": nil,
					"tool_calls": []map[string]any{{
						"function": map[string]any{
							"name":      transcribe.ToolName,
							"arguments": docJSON(fmt.Sprintf("answer %d", n)),
						},
					}},
				},
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func requireFile(t *testing.T, path string) fs.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("want %s to exist: %v", path, err)
	}
	return info
}

func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want %s to be absent, stat err = %v", path, err)
	}
}

func requireMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	if got := requireFile(t, path).Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

func readRunLog(t *testing.T, out string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, runLogName))
	if err != nil {
		t.Fatalf("read run.log: %v", err)
	}
	return string(data)
}

// --- happy path ------------------------------------------------------------

// TestRun_HappyPathWritesTheWholeArtifactTree is the shape of a complete run:
// every stage's artifact exists, the counts add up, and the summary names the
// files the operator has to check.
func TestRun_HappyPathWritesTheWholeArtifactTree(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.useProvider(transcriptionServer(t, nil).URL)

	summary, err := Run(context.Background(), f.opts, f.deps)
	if err != nil {
		t.Fatalf("Run: %v\nstderr:\n%s", err, f.stderr.String())
	}

	if summary.Pages != 3 {
		t.Errorf("Pages = %d, want 3", summary.Pages)
	}
	if summary.Auto+summary.Forced != 3 {
		t.Errorf("matched = %d auto + %d forced, want 3 matched", summary.Auto, summary.Forced)
	}
	if len(summary.Unmatched) != 0 {
		t.Errorf("Unmatched = %v, want none", summary.Unmatched)
	}
	if summary.Transcribed != 3 || summary.Failed != 0 {
		t.Errorf("Transcribed/Failed = %d/%d, want 3/0", summary.Transcribed, summary.Failed)
	}

	for _, rel := range []string{
		runLogName,
		filepath.Join(pagesDirName, PageFilename(1)),
		filepath.Join(pagesDirName, PageFilename(3)),
		filepath.Join(cropsDirName, bandCropFilename(1)),
		matchCSVName,
		matchJSONName,
		filepath.Join(maskedDirName, PageFilename(1)),
		maskPreviewName,
		bundleDirName,
	} {
		requireFile(t, f.path(rel))
	}
	// Nothing went unmatched, so the directory that would advertise unmatched
	// pages must not exist at all: an empty unmatched/ reads as "we lost some".
	requireAbsent(t, f.path(unmatchedDirName))

	entries, err := os.ReadDir(f.path(bundleDirName))
	if err != nil || len(entries) == 0 {
		t.Fatalf("bundle/ should hold one tree per answered problem: %v (%d entries)", err, len(entries))
	}

	out := f.stdout.String()
	for _, want := range []string{matchCSVName, maskPreviewName, bundleDirName} {
		if !strings.Contains(out, want) {
			t.Errorf("summary should name %s:\n%s", want, out)
		}
	}
}

// TestRun_ArtifactModesArePrivate — every byte this mode writes is a student's
// handwriting, their identity, or a transcription of it. None of it may be
// world-readable on a shared machine.
func TestRun_ArtifactModesArePrivate(t *testing.T) {
	f := newRunFixture(t, 3, 4, 1) // 4 pages over 3 cells => one unmatched
	f.opts.StopAfter = StopAfterMask

	if _, err := Run(context.Background(), f.opts, f.deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, dir := range []string{"", pagesDirName, cropsDirName, unmatchedDirName, maskedDirName} {
		requireMode(t, f.path(dir), 0o700)
	}
	for _, file := range []string{
		runLogName, matchCSVName, matchJSONName, maskPreviewName,
		filepath.Join(pagesDirName, PageFilename(1)),
		filepath.Join(cropsDirName, bandCropFilename(1)),
		filepath.Join(maskedDirName, PageFilename(1)),
	} {
		requireMode(t, f.path(file), 0o600)
	}
	// Which page went unmatched is the solver's call, so the set-aside copy is
	// found rather than assumed.
	unmatched, err := os.ReadDir(f.path(unmatchedDirName))
	if err != nil || len(unmatched) == 0 {
		t.Fatalf("read unmatched/: %v (%d entries)", err, len(unmatched))
	}
	requireMode(t, f.path(unmatchedDirName, unmatched[0].Name()), 0o600)
}

// TestRun_RunLogRecordsEveryStageAndNoIdentity — run.log is the run's own
// record, and it gets pasted into bug reports. It carries page indices and
// counts; a student id or name in it is a privacy incident (CLAUDE.md).
func TestRun_RunLogRecordsEveryStageAndNoIdentity(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.useProvider(transcriptionServer(t, nil).URL)

	if _, err := Run(context.Background(), f.opts, f.deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	log := readRunLog(t, f.out)
	for _, stage := range []string{"inputs", "regions", "render", "read", "match", "report", "provider", "mask", "transcribe", "bundle"} {
		if !strings.Contains(log, stage+" ") {
			t.Errorf("run.log has no %q stage line:\n%s", stage, log)
		}
	}
	if !strings.Contains(log, "elapsed_ms=") {
		t.Errorf("run.log stage lines must carry elapsed_ms:\n%s", log)
	}
	// Every stage line also went to stderr, so the operator watches the run.
	if !strings.Contains(f.stderr.String(), "render ") {
		t.Errorf("stage lines must mirror to stderr:\n%s", f.stderr.String())
	}
	for _, row := range f.rows {
		for _, needle := range []string{row.StudentID, row.Name, row.Email} {
			if strings.Contains(log, needle) {
				t.Errorf("run.log names a student (%q):\n%s", needle, log)
			}
			if strings.Contains(f.stderr.String(), needle) {
				t.Errorf("stderr names a student (%q)", needle)
			}
		}
	}
}

// --- stop-after ------------------------------------------------------------

// TestRun_StopAfterMatch — the identification half of the run, with no API
// stage: no provider is needed, nothing is masked, and nothing is sent.
func TestRun_StopAfterMatch(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.opts.StopAfter = StopAfterMatch

	summary, err := Run(context.Background(), f.opts, f.deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Auto+summary.Forced != 3 {
		t.Errorf("matched = %d, want 3", summary.Auto+summary.Forced)
	}
	if summary.Transcribed != 0 || summary.Failed != 0 {
		t.Errorf("nothing should be transcribed: %d/%d", summary.Transcribed, summary.Failed)
	}

	requireFile(t, f.path(matchCSVName))
	requireAbsent(t, f.path(maskedDirName))
	requireAbsent(t, f.path(maskPreviewName))
	requireAbsent(t, f.path(bundleDirName))
}

// TestRun_StopAfterMask — masking and the preview run, so the operator can
// check what WOULD be sent, but nothing is sent and no provider is required.
func TestRun_StopAfterMask(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.opts.StopAfter = StopAfterMask

	summary, err := Run(context.Background(), f.opts, f.deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Transcribed != 0 {
		t.Errorf("nothing should be transcribed: %d", summary.Transcribed)
	}

	requireFile(t, f.path(maskedDirName, PageFilename(1)))
	requireFile(t, f.path(maskPreviewName))
	requireAbsent(t, f.path(bundleDirName))
}

// --- unmatched pages -------------------------------------------------------

// TestRun_UnmatchedPagesAreSetAsideAndNeverMasked — an unmatched page names
// nobody, so it has no cell to be transcribed into. It is copied out as the
// ORIGINAL for the operator to look at, and it must never reach the mask stage
// (which exists only to prepare pages for the API).
func TestRun_UnmatchedPagesAreSetAsideAndNeverMasked(t *testing.T) {
	f := newRunFixture(t, 3, 4, 1) // 4 pages, 3 cells: one page has no cell left
	f.opts.StopAfter = StopAfterMask

	summary, err := Run(context.Background(), f.opts, f.deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Unmatched[ReasonSurplus] != 1 {
		t.Fatalf("Unmatched = %v, want exactly one %q", summary.Unmatched, ReasonSurplus)
	}
	if summary.Auto+summary.Forced != 3 {
		t.Errorf("matched = %d, want 3", summary.Auto+summary.Forced)
	}

	unmatched, err := os.ReadDir(f.path(unmatchedDirName))
	if err != nil || len(unmatched) != 1 {
		t.Fatalf("unmatched/ should hold exactly the one page nobody could place: %v (%d)", err, len(unmatched))
	}
	// The same page must NOT have a masked twin: only matched pages are masked.
	masked, err := os.ReadDir(f.path(maskedDirName))
	if err != nil {
		t.Fatalf("read masked/: %v", err)
	}
	if len(masked) != 3 {
		t.Errorf("masked/ holds %d pages, want the 3 matched ones", len(masked))
	}
	requireAbsent(t, f.path(maskedDirName, unmatched[0].Name()))

	// The copy is the original raster, byte for byte — that is what makes it
	// useful to look at.
	original, err := os.ReadFile(f.path(pagesDirName, unmatched[0].Name()))
	if err != nil {
		t.Fatalf("read the rendered page: %v", err)
	}
	setAside, err := os.ReadFile(f.path(unmatchedDirName, unmatched[0].Name()))
	if err != nil {
		t.Fatalf("read the set-aside page: %v", err)
	}
	if !bytes.Equal(original, setAside) {
		t.Error("unmatched/ must hold the page as it was scanned")
	}
}

// --- zero matches ----------------------------------------------------------

// TestRun_ZeroMatchesWritesTheReportsBeforeFailing — exit 9 is a diagnosis, not
// a crash: the operator's next move is to open match-report.csv and see WHY
// nothing matched, so the reports must be on disk before Run returns.
func TestRun_ZeroMatchesWritesTheReportsBeforeFailing(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.reader.lines = blankBandLines

	summary, err := Run(context.Background(), f.opts, f.deps)
	if err == nil {
		t.Fatal("a run where nothing matched must fail")
	}
	if !errors.As(err, new(*NoMatchError)) {
		t.Fatalf("err = %T (%v), want *NoMatchError", err, err)
	}
	if got := ExitCode(err); got != ExitNoMatch {
		t.Errorf("ExitCode = %d, want %d", got, ExitNoMatch)
	}
	if summary.Auto+summary.Forced != 0 {
		t.Errorf("matched = %d, want 0", summary.Auto+summary.Forced)
	}

	requireFile(t, f.path(matchCSVName))
	requireFile(t, f.path(matchJSONName))
	requireFile(t, f.path(unmatchedDirName, PageFilename(1)))
	// Nothing was masked and nothing was sent.
	requireAbsent(t, f.path(maskedDirName))
	requireAbsent(t, f.path(bundleDirName))
}

// --- provider configuration ------------------------------------------------

// TestRun_ProviderMisconfigurationFailsBeforeMasking pins the ordering decision
// (see Run's doc comment): BuildProvider is pure configuration validation, so
// it runs BEFORE the mask stage. A typo'd --provider must not cost a full pass
// of masking and a preview sheet that nothing will ever be sent from.
func TestRun_ProviderMisconfigurationFailsBeforeMasking(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.opts.Provider = "not-configured"
	f.opts.Model = "test-model"

	_, err := Run(context.Background(), f.opts, f.deps)
	if !errors.As(err, new(*ProviderError)) {
		t.Fatalf("err = %T (%v), want *ProviderError", err, err)
	}
	if got := ExitCode(err); got != ExitProvider {
		t.Errorf("ExitCode = %d, want %d", got, ExitProvider)
	}

	// The identification half is complete and kept...
	requireFile(t, f.path(matchCSVName))
	// ...and not one masked page was written for a run that cannot send them.
	requireAbsent(t, f.path(maskedDirName))
	requireAbsent(t, f.path(maskPreviewName))
	requireAbsent(t, f.path(bundleDirName))
}

// --- failures during transcription -----------------------------------------

// TestRun_FailedCellsAreReportedLoudlyWithoutIdentity — a cell the model
// refused is the operator's problem to see, named by the page and problem it
// happened on and by nothing else.
func TestRun_FailedCellsAreReportedLoudlyWithoutIdentity(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.opts.Concurrency = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	f.useProvider(srv.URL)

	summary, err := Run(context.Background(), f.opts, f.deps)
	if !errors.As(err, new(*ProviderError)) {
		t.Fatalf("every call failing is a run failure: err = %T (%v)", err, err)
	}
	if summary.Failed != 3 || summary.Transcribed != 0 {
		t.Errorf("Failed/Transcribed = %d/%d, want 3/0", summary.Failed, summary.Transcribed)
	}

	log := readRunLog(t, f.out)
	if !strings.Contains(log, "page 1 problem 1: transcription failed") {
		t.Errorf("each failed cell needs its own line naming page and problem:\n%s", log)
	}
	for _, row := range f.rows {
		if strings.Contains(log, row.StudentID) || strings.Contains(log, row.Name) {
			t.Errorf("a failure line named a student:\n%s", log)
		}
	}
}

// TestRun_CancellationStopsPromptlyAndKeepsWhatWasWritten — Ctrl-C must stop
// the SPEND immediately, report itself as a cancellation (not as a dead
// provider), and leave everything already on disk intact.
func TestRun_CancellationStopsPromptlyAndKeepsWhatWasWritten(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.opts.Concurrency = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.useProvider(transcriptionServer(t, func(int) { cancel() }).URL)

	_, err := Run(ctx, f.opts, f.deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancellation", err)
	}

	requireFile(t, f.path(matchCSVName))
	requireFile(t, f.path(maskedDirName, PageFilename(1)))
	// A bundle assembled out of a cancelled batch would be mostly "transcription
	// failed" rows, indistinguishable from a run against a dead provider.
	requireAbsent(t, f.path(bundleDirName))
}

// --- input failures --------------------------------------------------------

// TestRun_InputFailuresKeepTheirExitCodes — the typed errors the early stages
// raise must reach the caller unchanged, because a wrapper script branches on
// them.
func TestRun_InputFailuresKeepTheirExitCodes(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*runFixture)
		want int
	}{
		{"missing roster", func(f *runFixture) { f.opts.Roster = filepath.Join(t.TempDir(), "nope.csv") }, ExitRoster},
		{"missing scan", func(f *runFixture) { f.opts.Scans = []string{filepath.Join(t.TempDir(), "nope.pdf")} }, ExitScan},
		{"bad id-regions", func(f *runFixture) { f.opts.IDRegions = filepath.Join(t.TempDir(), "nope.json") }, ExitRegions},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t, 3, 3, 1)
			tc.mut(f)
			_, err := Run(context.Background(), f.opts, f.deps)
			if got := ExitCode(err); got != tc.want {
				t.Fatalf("ExitCode = %d (err %v), want %d", got, err, tc.want)
			}
		})
	}
}

// TestRun_OCRFailureIsAnOCRError — the local reader is the one dependency this
// mode cannot substitute, so its failure has to say so with exit 6.
func TestRun_OCRFailureIsAnOCRError(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.reader.err = errors.New("no onnxruntime")

	_, err := Run(context.Background(), f.opts, f.deps)
	if got := ExitCode(err); got != ExitOCR {
		t.Fatalf("ExitCode = %d (err %v), want %d", got, err, ExitOCR)
	}
}

// TestRun_MissingDependenciesFailBeforeAnythingIsWritten — the three Deps that
// have no usable zero value. The Charset is the dangerous one: an empty
// dictionary scores nothing, which the matcher would report as an unreadable
// identity band (exit 9) and send the operator to re-scan pages that were fine.
func TestRun_MissingDependenciesFailBeforeAnythingIsWritten(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Deps)
	}{
		{"no renderer", func(d *Deps) { d.Renderer = nil }},
		{"no ocr", func(d *Deps) { d.OCR = nil }},
		{"zero charset", func(d *Deps) { d.Charset = localocr.Charset{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t, 3, 3, 1)
			tc.mut(&f.deps)

			_, err := Run(context.Background(), f.opts, f.deps)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := ExitCode(err); got != ExitFailure {
				t.Errorf("ExitCode = %d (err %v), want %d: a wiring bug has no operator remedy", got, err, ExitFailure)
			}
			requireAbsent(t, f.out)
		})
	}
}

// TestRun_CancelledCellsAreNotReportedAsFailures — wording, and it matters: a
// screenful of "transcription failed" after a Ctrl-C reads as a provider that
// refused the batch, and sends the operator to check a key that is fine.
func TestRun_CancelledCellsAreNotReportedAsFailures(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.opts.Concurrency = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.useProvider(transcriptionServer(t, func(int) { cancel() }).URL)

	if _, err := Run(ctx, f.opts, f.deps); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancellation", err)
	}

	log := readRunLog(t, f.out)
	if !strings.Contains(log, "the run was cancelled") {
		t.Errorf("cancelled cells should say so:\n%s", log)
	}
}

// TestRun_CancellationDuringTheReadStageIsNotAnOCRFailure — the read stage is
// the longest in the run, so it is where a Ctrl-C actually lands, and it hands
// ctx to the OCR engine: a cancellation comes back out of ReadIdentity typed as
// an *OCRError (exit 6). Reporting that would tell an operator who changed
// their mind to go reinstall onnxruntime.
func TestRun_CancellationDuringTheReadStageIsNotAnOCRFailure(t *testing.T) {
	f := newRunFixture(t, 3, 4, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scripted := f.reader.lines
	f.reader.lines = func(call int) []localocr.LineLattice {
		if call == 1 {
			cancel() // partway through the batch, with pages already read
		}
		return scripted(call)
	}

	_, err := Run(ctx, f.opts, f.deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if errors.As(err, new(*OCRError)) {
		t.Errorf("a cancelled run must not be reported as a local-OCR failure: %v", err)
	}
	if got := ExitCode(err); got != ExitFailure {
		t.Errorf("ExitCode = %d, want %d: a cancellation is nobody's misconfiguration", got, ExitFailure)
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("the message should say the run was cancelled: %v", err)
	}

	// What was read before the stop is still on disk, and nothing downstream ran.
	requireFile(t, f.path(pagesDirName, PageFilename(1)))
	requireFile(t, f.path(cropsDirName, bandCropFilename(1)))
	requireAbsent(t, f.path(matchCSVName))
	requireAbsent(t, f.path(maskedDirName))
}

// TestRun_CancellationBeforeRenderStopsAtTheRenderStage — the same guard one
// stage earlier. The renderer types its own cancellation as "cannot render page
// N" (*ScanError, exit 4), which would send the operator to re-export a scan
// that is fine.
func TestRun_CancellationBeforeRenderStopsAtTheRenderStage(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Run(ctx, f.opts, f.deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if errors.As(err, new(*ScanError)) {
		t.Errorf("a cancelled run must not be reported as a bad scan file: %v", err)
	}
	// The read stage never started.
	requireAbsent(t, f.path(cropsDirName))
}

// TestRun_UnknownStopAfterIsRejectedAtEntry — an out-of-band stage name from a
// programmatic caller (one that never went through ParseArgs) passes both stage
// gates and reaches the transcription stage with no provider built. It is
// refused before any work, not discovered by a nil dereference.
func TestRun_UnknownStopAfterIsRejectedAtEntry(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.opts.StopAfter = "matched" // a plausible typo for "match"

	_, err := Run(context.Background(), f.opts, f.deps)
	if got := ExitCode(err); got != ExitUsage {
		t.Fatalf("ExitCode = %d (err %v), want %d", got, err, ExitUsage)
	}
	if !strings.Contains(err.Error(), "--stop-after") {
		t.Errorf("the message should name the option: %v", err)
	}
	// Refused before the output directory was touched.
	requireAbsent(t, f.out)
}

// --- stale artifacts from a previous run -----------------------------------

// TestRun_ForceClearsThePreviousRunsArtifacts — --force means "this run replaces
// what is in this directory", and the README's recommended workflow relies on it
// (--stop-after match, read the report, re-run with --force). Most of the
// artifact tree is named after its CONTENT, so rewriting is not enough: a
// leftover unmatched/pNNNN.jpg claims a page went unplaced in a run whose report
// says it matched, a leftover overflow preview sheet claims pages were sent that
// were not, and a leftover bundle tree hands the professor an exam that no
// longer exists under that name. doc.go calls these artifacts the audit trail;
// an audit trail that mixes two runs is worse than none.
func TestRun_ForceClearsThePreviousRunsArtifacts(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.useProvider(transcriptionServer(t, nil).URL)
	f.opts.Force = true

	// A previous run's leavings, one per owned path. The names are all ones
	// this run will NOT produce: a page index past the batch, the other region
	// mode's crop, an overflow preview sheet, a bundle under an older
	// --exam-name.
	stale := []string{
		filepath.Join(pagesDirName, PageFilename(9999)),
		filepath.Join(cropsDirName, CropFilename(9999, KindStudentID)),
		filepath.Join(unmatchedDirName, PageFilename(9999)),
		filepath.Join(maskedDirName, PageFilename(9999)),
		"masked-preview-02.jpg",
		filepath.Join(bundleDirName, "older-exam-p1", "MANIFEST.csv"),
	}
	for _, rel := range stale {
		path := f.path(rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("a previous run"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}

	// Without --force the same directory is refused outright: clearing is what
	// --force licenses, not a replacement for it.
	noForce := f.opts
	noForce.Force = false
	if _, err := Run(context.Background(), noForce, f.deps); ExitCode(err) != ExitOutDir {
		t.Fatalf("a non-empty --out without --force: ExitCode = %d (err %v), want %d", ExitCode(err), err, ExitOutDir)
	}
	// ...and it is refused BEFORE anything is deleted.
	requireFile(t, f.path(stale[0]))

	if _, err := Run(context.Background(), f.opts, f.deps); err != nil {
		t.Fatalf("Run: %v\nstderr:\n%s", err, f.stderr.String())
	}

	for _, rel := range stale {
		requireAbsent(t, f.path(rel))
	}
	// The stale bundle's whole tree went, not just the file inside it.
	requireAbsent(t, f.path(bundleDirName, "older-exam-p1"))
	// And this run's own artifacts are there, so the clearing ran BEFORE each
	// stage rather than after it.
	for _, rel := range []string{
		filepath.Join(pagesDirName, PageFilename(1)),
		filepath.Join(cropsDirName, bandCropFilename(1)),
		filepath.Join(maskedDirName, PageFilename(1)),
		maskPreviewName,
		matchCSVName,
	} {
		requireFile(t, f.path(rel))
	}
}

// TestRun_ClearingIsScopedToTheRunsOwnArtifacts — an operator's --out is often a
// directory they keep other things in (the roster they matched against, notes, a
// previous run's zip). --force licenses replacing the artifacts this mode
// writes, and nothing else.
func TestRun_ClearingIsScopedToTheRunsOwnArtifacts(t *testing.T) {
	f := newRunFixture(t, 3, 3, 1)
	f.opts.Force = true
	f.opts.StopAfter = StopAfterMask

	if err := os.MkdirAll(f.out, 0o700); err != nil {
		t.Fatalf("mkdir out: %v", err)
	}
	keep := []string{"notes.txt", "roster-copy.csv", "masked-preview.txt", "pages.txt"}
	for _, name := range keep {
		writeFile(t, f.out, name, "the operator's own file")
	}
	if err := os.MkdirAll(filepath.Join(f.out, "my-notes"), 0o700); err != nil {
		t.Fatalf("mkdir my-notes: %v", err)
	}
	writeFile(t, filepath.Join(f.out, "my-notes"), "keep.txt", "mine")

	if _, err := Run(context.Background(), f.opts, f.deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, name := range keep {
		requireFile(t, f.path(name))
	}
	requireFile(t, f.path("my-notes", "keep.txt"))
}

// TestRun_MatchJSONRecordsTheRunsSettings — the JSON report is the only place a
// run's own settings survive. Six months later "why did this page match" is
// answerable only if the report says which thresholds, which render resolution
// and which model produced it.
func TestRun_MatchJSONRecordsTheRunsSettings(t *testing.T) {
	readMeta := func(t *testing.T, out string) Meta {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(out, matchJSONName))
		if err != nil {
			t.Fatalf("read the JSON report: %v", err)
		}
		var doc struct {
			Meta Meta `json:"meta"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("unmarshal the JSON report: %v", err)
		}
		return doc.Meta
	}

	t.Run("full run names the provider it sent to", func(t *testing.T) {
		f := newRunFixture(t, 3, 3, 1)
		srv := transcriptionServer(t, nil)
		f.useProvider(srv.URL)
		f.opts.ExamName = "midterm-1"

		if _, err := Run(context.Background(), f.opts, f.deps); err != nil {
			t.Fatalf("Run: %v", err)
		}

		m := readMeta(t, f.out)
		if m.DPI != DefaultDPI || m.LongEdge != DefaultLongEdge || m.JPEGQuality != DefaultJPEGQuality {
			t.Errorf("render settings = %d dpi / %d long edge / %d quality", m.DPI, m.LongEdge, m.JPEGQuality)
		}
		if m.ExamName != "midterm-1" {
			t.Errorf("exam_name = %q, want the run's --exam-name", m.ExamName)
		}
		// The route as the operator spelled it — the --base-url here, since no
		// provider table was involved.
		if m.Provider != srv.URL || m.Model != "test-model" {
			t.Errorf("provider/model = %q/%q, want %q/%q", m.Provider, m.Model, srv.URL, "test-model")
		}
		// Never the key itself, nor the name of the variable holding it.
		if strings.Contains(m.Provider, "sk-test") || strings.Contains(m.Provider, "OFFLINE_TEST_KEY") {
			t.Errorf("the report leaked the API key configuration: %q", m.Provider)
		}
	})

	t.Run("stop-after run names no provider", func(t *testing.T) {
		f := newRunFixture(t, 3, 3, 1)
		f.opts.StopAfter = StopAfterMatch

		if _, err := Run(context.Background(), f.opts, f.deps); err != nil {
			t.Fatalf("Run: %v", err)
		}

		// Empty is the fact worth recording: nothing was sent anywhere.
		if m := readMeta(t, f.out); m.Provider != "" || m.Model != "" {
			t.Errorf("provider/model = %q/%q on a --stop-after run, want both empty", m.Provider, m.Model)
		}
	})
}
