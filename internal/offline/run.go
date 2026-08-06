package offline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/localocr"
	"github.com/HaoWen46/adagrade/internal/render"
)

// The artifact tree. Named here rather than spelled at each call site because
// the banner, the final summary, the README and an operator's wrapper script
// all refer to these paths: they are a compatibility surface, not a detail.
//
//	<out>/run.log              one line per stage (no identity, ever)
//	<out>/pages/pNNNN.jpg      every scanned page, as rendered
//	<out>/crops/               what the local OCR actually looked at
//	<out>/match-report.csv     who each page was assigned to, and how confident
//	<out>/match-report.json    the same rows unrounded, plus the run's settings
//	<out>/unmatched/pNNNN.jpg  the pages nobody could place (originals)
//	<out>/masked/pNNNN.jpg     the only bytes that may leave this machine
//	<out>/masked-preview.jpg   what the model saw where identity used to be
//	<out>/bundle/              the professor's export, one tree per problem
const (
	runLogName       = "run.log"
	pagesDirName     = "pages"
	cropsDirName     = "crops"
	unmatchedDirName = "unmatched"
	maskedDirName    = "masked"
	bundleDirName    = "bundle"
	matchCSVName     = "match-report.csv"
	matchJSONName    = "match-report.json"
	maskPreviewName  = "masked-preview.jpg"
)

// reportedReasons is the order unmatched reasons are counted out in, so two
// runs with the same outcome print the same line.
var reportedReasons = []string{ReasonSurplus, ReasonLowScore, ReasonAmbiguous}

// cjkFontEnv names the bundled Traditional Chinese face. It is the same
// variable the server reads for report attachments; here it reaches the
// bundle's LaTeX preamble (WriteBundle documents the fallback when it is unset).
const cjkFontEnv = "ADAMARKER_REPORT_FONT"

// Deps are the collaborators Run cannot construct for itself: the two heavy,
// externally-provisioned ones (a PDF renderer and the local OCR engine), the
// dictionary that makes the OCR's lattices scorable, and the process's own
// environment and streams.
//
// They are injected rather than built inside Run so the whole pipeline is
// testable without an ONNX runtime, a model file, or a network — which is also
// why OCR is the narrow latticeReader (crop in, lattices out) instead of
// *localocr.Engine.
type Deps struct {
	Renderer render.Renderer
	OCR      latticeReader // *localocr.Engine satisfies this
	Charset  localocr.Charset
	Getenv   func(string) string
	Stderr   io.Writer // banner and per-stage progress
	Stdout   io.Writer // the final summary
}

// Summary is what the run did, in the terms an operator checks it in.
//
// Unmatched is keyed by reason (surplus, low-score, ambiguous) rather than
// being a single total, because the three mean different things: surplus says
// the batch has more pages than cells, low-score says the identity band was not
// read, and ambiguous says two students explain the same page.
//
// It is returned even when Run fails, so a caller can report how far the run
// got. The counts describe stages that COMPLETED; a failure mid-stage leaves
// that stage's counters at zero.
type Summary struct {
	Pages, Auto, Forced int
	Unmatched           map[string]int
	Transcribed, Failed int
	Artifacts           []string
}

// Matched is the number of pages that were assigned to a student.
func (s Summary) Matched() int { return s.Auto + s.Forced }

// UnmatchedTotal is the number of pages set aside, across all reasons.
func (s Summary) UnmatchedTotal() int {
	n := 0
	for _, c := range s.Unmatched {
		n += c
	}
	return n
}

// Run executes one offline grading run: scans in, artifacts out.
//
// The stages run in a fixed order and each one logs a line to <out>/run.log and
// to Stderr as it finishes. Failures are the typed errors of flags.go, so the
// caller turns them into exit codes with ExitCode and never matches on strings.
//
// Two orderings in here are decisions rather than consequences:
//
// ZERO MATCHES FAIL AFTER THE REPORTS ARE WRITTEN. A run that could not place a
// single page returns *NoMatchError (exit 9), but only once match-report.csv,
// match-report.json and unmatched/ are on disk. Exit 9 is a diagnosis and the
// operator's next move is to open those files and see WHY — returning before
// writing them would leave them with an exit code and an empty directory.
//
// BUILDPROVIDER RUNS BEFORE THE MASK STAGE, not at the transcription stage
// where the pipeline's narrative would put it. It is pure configuration
// validation — it parses a URL, reads one environment variable and constructs a
// client, with no network call and no side effect — so its position is free to
// choose, and the only thing that changes with it is how much work a typo'd
// --provider or an unexported API key throws away. Masking is the first stage
// whose entire output exists to be sent: a misconfigured provider means every
// masked page and the preview sheet were written for nothing. It is deliberately
// NOT hoisted above render/read/match, because those stages' artifacts are the
// operator's either way — `--stop-after match` produces exactly them — so a run
// that dies before them leaves nothing to look at, while one that dies here
// leaves a complete match report.
func Run(ctx context.Context, o Options, d Deps) (Summary, error) {
	stderr := writerOr(d.Stderr)
	stdout := writerOr(d.Stdout)
	getenv := d.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	summary := Summary{Unmatched: map[string]int{}}

	// Wiring bugs, not operator mistakes: they cannot be reached through the
	// CLI, and there is no remedy to print, so they stay unclassified (exit 1).
	if d.Renderer == nil {
		return summary, errors.New("offline: Run needs a Renderer")
	}
	if d.OCR == nil {
		return summary, errors.New("offline: Run needs a local OCR reader")
	}

	// 1. The warning comes first and unconditionally, before any work: it exists
	// to be read BEFORE the operator trusts the output, not after.
	PrintBanner(stderr, o.Out)

	// 2. Inputs. All three checks run before anything is written, so a typo
	// fails in the first second rather than after the render stage.
	started := time.Now()
	rows, err := LoadRoster(o.Roster)
	if err != nil {
		return summary, err
	}
	if err := ValidateScans(o.Scans); err != nil {
		return summary, err
	}
	if err := PrepareOutDir(o.Out, o.Force); err != nil {
		return summary, err
	}
	log, err := openRunLog(o.Out, stderr)
	if err != nil {
		return summary, err
	}
	defer log.Close()
	summary.Artifacts = append(summary.Artifacts, log.path)
	log.stage("inputs", started, "roster_rows=%d scans=%d", len(rows), len(o.Scans))

	// 3. Where identity lives on the page. One region set is resolved here and
	// used by BOTH the read stage and the mask stage — reading one rectangle and
	// masking another is the failure mode this single resolution rules out.
	started = time.Now()
	regions, source, err := resolveRegions(o)
	if err != nil {
		return summary, err
	}
	log.stage("regions", started, "source=%s regions=%d mask_regions=%d", source, len(regions.All()), len(regions.MaskRegions()))

	// 4. Render.
	started = time.Now()
	pages, err := RenderPages(ctx, d.Renderer, o.Scans, filepath.Join(o.Out, pagesDirName), o.DPI, o.LongEdge, o.JPEGQuality)
	if err != nil {
		return summary, err
	}
	summary.Pages = len(pages)
	log.stage("render", started, "pages=%d", len(pages))

	// 5. Read identity. Sequential on purpose: the engine serializes inference
	// behind its own mutex, so a worker pool here would buy nothing and would
	// break the "Nth crop is the Nth page" property the artifacts rely on.
	started = time.Now()
	cropsDir := filepath.Join(o.Out, cropsDirName)
	reads := make([]PageRead, 0, len(pages))
	for _, page := range pages {
		id, err := ReadIdentity(ctx, d.OCR, page, regions, cropsDir)
		if err != nil {
			return summary, err
		}
		reads = append(reads, PageRead{Page: page, ID: id})
	}
	log.stage("read", started, "pages=%d", len(reads))

	// 6. Match, globally over the whole batch.
	started = time.Now()
	results, err := MatchPages(reads, rows, o.Problems, d.Charset, o.MinScore, o.MinMargin)
	if err != nil {
		return summary, err
	}
	countMatches(&summary, results)
	log.stage("match", started, "auto=%d forced=%d unmatched=%d", summary.Auto, summary.Forced, summary.UnmatchedTotal())

	// 7. The audit trail. This is the whole audit trail — there are no database
	// rows to fall back on — so it is written before anything can fail again.
	started = time.Now()
	csvPath := filepath.Join(o.Out, matchCSVName)
	if err := WriteMatchCSV(csvPath, results); err != nil {
		return summary, err
	}
	jsonPath := filepath.Join(o.Out, matchJSONName)
	if err := WriteMatchJSON(jsonPath, results, runMeta(o)); err != nil {
		return summary, err
	}
	summary.Artifacts = append(summary.Artifacts, csvPath, jsonPath)
	unmatchedDir, unmatchedCount, err := writeUnmatched(o.Out, results)
	if err != nil {
		return summary, err
	}
	if unmatchedCount > 0 {
		summary.Artifacts = append(summary.Artifacts, dirPath(unmatchedDir))
	}
	log.stage("report", started, "rows=%d unmatched_pages=%d%s", len(results), unmatchedCount, reasonBreakdown(summary))

	// 8. Nothing matched: fail now, with the reports already on disk.
	if summary.Matched() == 0 {
		return summary, newNoMatchError(nil,
			"not one of the %d page(s) could be matched to a roster entry: read %s and the pages in %s, then re-run — the usual causes are an identity band the local OCR could not read (check the crops in %s), an --id-regions file describing a different layout, or thresholds set too high (--min-score %g, --min-margin %g)",
			len(results), csvPath, dirPath(filepath.Join(o.Out, unmatchedDirName)), dirPath(cropsDir), o.MinScore, o.MinMargin)
	}

	// 9. --stop-after match: the identification half, and nothing leaves the
	// machine.
	if o.StopAfter == StopAfterMatch {
		printSummary(stdout, summary)
		return summary, nil
	}

	// 10a. The provider check, hoisted ahead of masking (see the doc comment).
	// Skipped entirely for --stop-after mask, which needs no provider and where
	// ParseArgs therefore does not require one.
	var provider llm.Provider
	var model string
	if o.StopAfter == "" {
		started = time.Now()
		provider, model, err = BuildProvider(o, getenv)
		if err != nil {
			return summary, err
		}
		log.stage("provider", started, "name=%s model=%s", provider.Name(), model)
	}

	// 10b. Mask the MATCHED pages only. An unmatched page names nobody, so it
	// has no cell to be transcribed into and no reason to be prepared for the
	// API; it stays in unmatched/ as the original the operator has to look at.
	started = time.Now()
	masked, err := MaskPages(matchedPages(results), regions, o.JPEGQuality, filepath.Join(o.Out, maskedDirName))
	if err != nil {
		return summary, err
	}
	sheets, err := WriteContactSheet(masked, regions, filepath.Join(o.Out, maskPreviewName))
	if err != nil {
		return summary, err
	}
	summary.Artifacts = append(summary.Artifacts, sheets...)
	log.stage("mask", started, "pages=%d preview_sheets=%d", len(masked), len(sheets))

	// 11. --stop-after mask: the operator inspects masked-preview.jpg and
	// decides whether this is safe to send at all.
	if o.StopAfter == StopAfterMask {
		printSummary(stdout, summary)
		return summary, nil
	}

	// 12. Transcribe.
	cells, err := PairCells(results, masked)
	if err != nil {
		return summary, err
	}
	started = time.Now()
	docs, transcribeErr := TranscribeCells(ctx, provider, model, cells, o.Concurrency)

	// 13. Every failed cell gets its own line, named by page and problem and by
	// nothing else — these lines are pasted into bug reports.
	for _, doc := range docs {
		if doc.Err != nil {
			summary.Failed++
			log.line("page %d problem %d: transcription failed", doc.Result.Page.Index, doc.Result.Problem)
			continue
		}
		summary.Transcribed++
	}
	log.stage("transcribe", started, "cells=%d transcribed=%d failed=%d", len(cells), summary.Transcribed, summary.Failed)

	// Cancellation is checked BEFORE the transcription error, because a
	// cancelled batch fails every remaining cell and would otherwise be reported
	// as an unreachable provider (exit 8) when the operator simply pressed
	// Ctrl-C. No bundle is assembled from it either: a tree of "transcription
	// failed" rows is indistinguishable from a run against a dead endpoint, and
	// everything written so far stays on disk to be re-used or inspected.
	if cerr := ctx.Err(); cerr != nil {
		return summary, fmt.Errorf("offline: the run was cancelled during transcription; the artifacts written so far are in %s: %w", o.Out, cerr)
	}
	if transcribeErr != nil {
		return summary, transcribeErr
	}

	// 14. The professor's bundle.
	started = time.Now()
	bundleDir := filepath.Join(o.Out, bundleDirName)
	if err := WriteBundle(bundleDir, o.ExamName, docs, rows, o.Problems, getenv(cjkFontEnv)); err != nil {
		return summary, err
	}
	summary.Artifacts = append(summary.Artifacts, dirPath(bundleDir))
	log.stage("bundle", started, "problems=%d cells=%d", o.Problems, len(docs))

	printSummary(stdout, summary)
	return summary, nil
}

// writerOr defaults a nil stream to io.Discard, so a caller that only wants the
// Summary does not have to invent writers.
func writerOr(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// dirPath marks a directory in operator-facing text with a trailing separator,
// so "unmatched/" reads as a directory and not as a file someone forgot to give
// an extension.
func dirPath(p string) string { return p + string(filepath.Separator) }

// resolveRegions picks the identity geometry: the operator's file if they drew
// one, the top --id-band strip otherwise. The label is for run.log.
func resolveRegions(o Options) (RegionSet, string, error) {
	if o.IDRegions != "" {
		set, err := LoadRegions(o.IDRegions)
		return set, "id-regions", err
	}
	return BandRegions(o.IDBand), "id-band", nil
}

// runMeta is the settings block the JSON report carries, so a report read six
// months later still says which roster and which thresholds produced it.
func runMeta(o Options) Meta {
	return Meta{
		Roster:      o.Roster,
		Scans:       o.Scans,
		Problems:    o.Problems,
		MinScore:    o.MinScore,
		MinMargin:   o.MinMargin,
		Weights:     [3]float64{WeightStudentID, WeightName, WeightProblem},
		IDBand:      o.IDBand,
		IDRegions:   o.IDRegions,
		GeneratedAt: time.Now().UTC(),
	}
}

// countMatches tallies the match stage into the summary.
func countMatches(s *Summary, results []MatchResult) {
	for _, r := range results {
		switch r.Status {
		case StatusAuto:
			s.Auto++
		case StatusForced:
			s.Forced++
		default:
			s.Unmatched[r.Reason]++
		}
	}
}

// reasonBreakdown renders the per-reason counts for the report stage line, in a
// fixed order and only for reasons that actually occurred.
func reasonBreakdown(s Summary) string {
	out := ""
	for _, reason := range reportedReasons {
		if n := s.Unmatched[reason]; n > 0 {
			out += fmt.Sprintf(" %s=%d", reason, n)
		}
	}
	return out
}

// matchedPages is the pages the matcher placed, in page order.
func matchedPages(results []MatchResult) []Page {
	out := make([]Page, 0, len(results))
	for _, r := range results {
		if r.Status == StatusUnmatched {
			continue
		}
		out = append(out, r.Page)
	}
	return out
}

// writeUnmatched copies every unmatched page's ORIGINAL raster into
// <out>/unmatched/, and returns the directory and how many landed in it.
//
// The originals, not masked copies: these pages are for a human on this machine
// to look at and place by hand, and covering the identity band would remove the
// only thing that could tell them whose page it is. They never reach a
// provider — the mask stage runs over the matched pages alone.
//
// The directory is created only when there is something to put in it. An empty
// unmatched/ would read as "some pages were lost", which is a claim about the
// run that a successful one must not make.
func writeUnmatched(outDir string, results []MatchResult) (string, int, error) {
	dir := filepath.Join(outDir, unmatchedDirName)
	count := 0
	for _, r := range results {
		if r.Status != StatusUnmatched {
			continue
		}
		if count == 0 {
			if err := mkdirPrivate(dir); err != nil {
				return dir, 0, newOutDirError(err, "cannot create the unmatched-page directory %s", dir)
			}
		}
		path := filepath.Join(dir, PageFilename(r.Page.Index))
		if err := writePrivate(path, r.Page.JPEG); err != nil {
			return dir, count, newOutDirError(err, "cannot write unmatched page %s", path)
		}
		count++
	}
	return dir, count, nil
}

// printSummary writes the run's closing report to stdout: the counts, then the
// artifacts the operator has to open before trusting any of it.
func printSummary(w io.Writer, s Summary) {
	fmt.Fprintf(w, "\noffline-grade summary\n")
	fmt.Fprintf(w, "  pages         %d\n", s.Pages)
	fmt.Fprintf(w, "  matched       %d (auto %d, forced %d)\n", s.Matched(), s.Auto, s.Forced)
	fmt.Fprintf(w, "  unmatched     %d%s\n", s.UnmatchedTotal(), reasonBreakdown(s))
	fmt.Fprintf(w, "  transcribed   %d\n", s.Transcribed)
	fmt.Fprintf(w, "  failed        %d\n", s.Failed)
	fmt.Fprintf(w, "  artifacts:\n")
	for _, path := range s.Artifacts {
		fmt.Fprintf(w, "    %s\n", path)
	}
}

// ---------------------------------------------------------------------------
// run.log
// ---------------------------------------------------------------------------

// runLog is the run's own record: one line per stage, written to <out>/run.log
// and mirrored to stderr so the operator watches the same thing they can read
// back later.
//
// It is a file handle and a Fprintln, deliberately — no logging framework, no
// levels, no structured encoder. The file is small, is read by humans, and its
// one hard rule is a content rule rather than a formatting one: NOTHING
// identifying goes in it. Stages are counted in pages and cells, and a page is
// named by its global index, which is also what names it in pages/, in the
// report and in the crops.
type runLog struct {
	path   string
	file   *os.File
	stderr io.Writer
}

// openRunLog creates <out>/run.log at 0600, truncating a previous run's log
// (--force already licensed overwriting this directory). The mode is
// re-asserted after opening for the same reason mask.go re-asserts it:
// O_CREATE's mode applies only when the file did not already exist.
func openRunLog(outDir string, stderr io.Writer) (*runLog, error) {
	path := filepath.Join(outDir, runLogName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, privateFileMode)
	if err != nil {
		return nil, newOutDirError(err, "cannot create the run log %s", path)
	}
	if err := f.Chmod(privateFileMode); err != nil {
		_ = f.Close()
		return nil, newOutDirError(err, "cannot set the mode of the run log %s", path)
	}
	return &runLog{path: path, file: f, stderr: stderr}, nil
}

// line writes one raw line to both destinations. Write errors are dropped: a
// run must not fail because its log could not be appended to, and the operator
// is looking at the same line on stderr either way.
func (l *runLog) line(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintln(l.stderr, msg)
	fmt.Fprintln(l.file, msg)
}

// stage closes out one stage: its name, whatever it counted, and how long it
// took.
func (l *runLog) stage(name string, started time.Time, format string, a ...any) {
	l.line("%s %s elapsed_ms=%d", name, fmt.Sprintf(format, a...), time.Since(started).Milliseconds())
}

func (l *runLog) Close() error { return l.file.Close() }
