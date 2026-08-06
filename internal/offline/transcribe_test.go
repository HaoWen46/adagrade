package offline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// Synthetic identity, never real roster data (D14). These strings double as
// PII needles: no error this stage produces may contain either of them.
const (
	fixtureStudentID = "AB07"
	fixtureName      = "Test Alpha"
)

// docJSON is a well-formed transcription response carrying one prose block.
func docJSON(text string) string {
	return fmt.Sprintf(`{"blocks":[{"kind":"prose","text":%q,"items":[]}],"confidence":"high"}`, text)
}

// transcribeCell builds one matched cell over a distinct masked page, so a
// response can be attributed to a cell by its image SHA alone.
func transcribeCell(t *testing.T, pageIndex, problem, w int) MatchedCell {
	t.Helper()
	// Width varies per cell purely to make every masked JPEG (and therefore
	// every SHA) distinct.
	masked, err := imaging.Mask(bandPageJPEG(t, w, 60), BandRegions(0.18).MaskRegions(), 85)
	if err != nil {
		t.Fatalf("mask fixture page: %v", err)
	}
	return MatchedCell{
		Result: MatchResult{
			Page:        Page{Index: pageIndex, SourcePDF: "scan.pdf", SourcePage: pageIndex},
			StudentID:   fixtureStudentID,
			StudentName: fixtureName,
			Problem:     problem,
			Method:      MethodLattice,
			Status:      StatusAuto,
		},
		Masked: masked,
	}
}

func transcribeCells(t *testing.T, n int) []MatchedCell {
	t.Helper()
	cells := make([]MatchedCell, n)
	for i := range cells {
		cells[i] = transcribeCell(t, i+1, i%3+1, 120+i)
	}
	return cells
}

// stubProvider is the llm.Provider the concurrency tests need: fake.
// ScriptedProvider replies by CALL POSITION, and under a worker pool call
// position is exactly the thing that is nondeterministic. This one replies by
// request content, and records the model it was handed.
type stubProvider struct {
	reply func(model string, req llm.Request) (llm.Result, error)

	mu     sync.Mutex
	calls  int
	models []string
}

func (s *stubProvider) Name() string { return "stub" }

func (s *stubProvider) Grade(ctx context.Context, model string, req llm.Request) (llm.Result, error) {
	s.mu.Lock()
	s.calls++
	s.models = append(s.models, model)
	s.mu.Unlock()
	return s.reply(model, req)
}

func (s *stubProvider) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// shaIndex maps each cell's masked-image SHA to its position, so a stub can
// answer "which cell is this request for" without relying on call order.
func shaIndex(cells []MatchedCell) map[string]int {
	idx := make(map[string]int, len(cells))
	for i, c := range cells {
		idx[c.Masked.SHA256()] = i
	}
	return idx
}

// --- request shape ---------------------------------------------------------

// TestTranscribeCells_MirrorsTheWebTranscriptionRequest — the offline path must
// ask for the same thing the server asks for, or the two produce different
// transcriptions of the same page and the bundle stops being comparable.
func TestTranscribeCells_MirrorsTheWebTranscriptionRequest(t *testing.T) {
	cell := transcribeCell(t, 4, 3, 120)
	p := &fake.ScriptedProvider{Steps: []fake.JSONStep{{JSON: docJSON("hello")}}}

	if _, err := TranscribeCells(context.Background(), p, "vision-1", []MatchedCell{cell}, 1); err != nil {
		t.Fatalf("TranscribeCells: %v", err)
	}
	if len(p.Calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(p.Calls))
	}
	req := p.Calls[0]
	if req.System != transcribe.SystemPrompt {
		t.Error("System is not transcribe.SystemPrompt")
	}
	if want := transcribe.UserPrompt(3); req.Prompt != want {
		t.Errorf("Prompt = %q, want UserPrompt(3)", req.Prompt)
	}
	if req.ToolName != transcribe.ToolName {
		t.Errorf("ToolName = %q, want %q", req.ToolName, transcribe.ToolName)
	}
	if string(req.Schema) != string(transcribe.BuildSchema()) {
		t.Error("Schema is not transcribe.BuildSchema()")
	}
	if req.Temperature != 0 {
		t.Errorf("Temperature = %v, want 0 (deterministic default)", req.Temperature)
	}
	if len(req.Images) != 1 {
		t.Fatalf("Images = %d, want 1", len(req.Images))
	}
	if got := req.Images[0].SHA256(); got != cell.Masked.SHA256() {
		t.Error("the image sent is not this cell's MASKED image")
	}
	// The seal, restated where it matters: the request carries the masked
	// derivative, and the original page bytes are still only on disk.
	if _, ok := req.Images[0].(imaging.MaskedImage); !ok {
		t.Errorf("Images[0] = %T, want imaging.MaskedImage", req.Images[0])
	}
}

// TestTranscribeCells_PassesTheModelThrough — the model id comes from --model
// and is the caller's, not the provider's, business.
func TestTranscribeCells_PassesTheModelThrough(t *testing.T) {
	stub := &stubProvider{reply: func(model string, _ llm.Request) (llm.Result, error) {
		return llm.Result{JSON: []byte(docJSON("x")), Model: model}, nil
	}}
	if _, err := TranscribeCells(context.Background(), stub, "vision-9", transcribeCells(t, 2), 2); err != nil {
		t.Fatalf("TranscribeCells: %v", err)
	}
	for _, m := range stub.models {
		if m != "vision-9" {
			t.Errorf("model = %q, want %q", m, "vision-9")
		}
	}
}

// --- ordering --------------------------------------------------------------

// TestTranscribeCells_OrderIsCellOrderNotCompletionOrder is the one that keeps
// a student's answer attached to that student: the results are zipped back
// against the cells by POSITION, so a pool that returned them in completion
// order would file every answer under the wrong page.
//
// The stub finishes in reverse, so completion order is exactly wrong.
func TestTranscribeCells_OrderIsCellOrderNotCompletionOrder(t *testing.T) {
	cells := transcribeCells(t, 12)
	idx := shaIndex(cells)
	stub := &stubProvider{reply: func(_ string, req llm.Request) (llm.Result, error) {
		i := idx[req.Images[0].SHA256()]
		time.Sleep(time.Duration(len(cells)-i) * 2 * time.Millisecond)
		return llm.Result{JSON: []byte(docJSON(fmt.Sprintf("cell-%d", i)))}, nil
	}}

	docs, err := TranscribeCells(context.Background(), stub, "vision-1", cells, 4)
	if err != nil {
		t.Fatalf("TranscribeCells: %v", err)
	}
	if len(docs) != len(cells) {
		t.Fatalf("docs = %d, want %d", len(docs), len(cells))
	}
	for i, d := range docs {
		if d.Err != nil {
			t.Fatalf("docs[%d].Err = %v", i, d.Err)
		}
		if d.Result.Page.Index != cells[i].Result.Page.Index {
			t.Errorf("docs[%d] is page %d, want page %d", i, d.Result.Page.Index, cells[i].Result.Page.Index)
		}
		if len(d.Doc.Blocks) != 1 {
			t.Fatalf("docs[%d] has %d blocks, want 1", i, len(d.Doc.Blocks))
		}
		if want := fmt.Sprintf("cell-%d", i); d.Doc.Blocks[0].Text != want {
			t.Errorf("docs[%d] carries %q, want %q — a response was filed against the wrong cell",
				i, d.Doc.Blocks[0].Text, want)
		}
		if d.Masked.SHA256() != cells[i].Masked.SHA256() {
			t.Errorf("docs[%d] carries the wrong masked image", i)
		}
		if d.Confidence != "high" {
			t.Errorf("docs[%d].Confidence = %q, want %q", i, d.Confidence, "high")
		}
	}
}

// TestTranscribeCells_ConcurrencyIsBounded — --concurrency is a spend and
// rate-limit control, so exceeding it is a real failure, not a detail.
func TestTranscribeCells_ConcurrencyIsBounded(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	stub := &stubProvider{reply: func(_ string, _ llm.Request) (llm.Result, error) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return llm.Result{JSON: []byte(docJSON("x"))}, nil
	}}
	if _, err := TranscribeCells(context.Background(), stub, "m", transcribeCells(t, 10), 3); err != nil {
		t.Fatalf("TranscribeCells: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > 3 {
		t.Errorf("peak concurrency = %d, want at most 3", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d: the calls did not run in parallel at all", peak)
	}
}

// --- failures --------------------------------------------------------------

// TestTranscribeCells_PerCellFailureIsRecordedAndTheRestContinue — one refused
// page must not cost the other 200 their transcription (and their spend).
func TestTranscribeCells_PerCellFailureIsRecordedAndTheRestContinue(t *testing.T) {
	cells := transcribeCells(t, 2)
	p := &fake.ScriptedProvider{Steps: []fake.JSONStep{
		{Err: errors.New("upstream refused")},
		{JSON: docJSON("second cell")},
	}}
	// Concurrency 1: the scripted provider replies by call position, so the
	// script only means anything in a serial run.
	docs, err := TranscribeCells(context.Background(), p, "vision-1", cells, 1)
	if err != nil {
		t.Fatalf("TranscribeCells returned an error for a PARTIAL failure: %v", err)
	}
	if docs[0].Err == nil {
		t.Fatal("docs[0].Err = nil, want the recorded failure")
	}
	if docs[1].Err != nil {
		t.Fatalf("docs[1].Err = %v, want nil", docs[1].Err)
	}
	if len(docs[1].Doc.Blocks) != 1 {
		t.Errorf("docs[1] has %d blocks, want 1", len(docs[1].Doc.Blocks))
	}
	// A failed cell carries no transcription, and its error names the page and
	// the problem — never the student.
	msg := docs[0].Err.Error()
	if !strings.Contains(msg, "page 1") {
		t.Errorf("error %q does not name the page", msg)
	}
	for _, needle := range []string{fixtureStudentID, fixtureName} {
		if strings.Contains(msg, needle) {
			t.Errorf("error %q names a student (PII rule)", msg)
		}
	}
}

// TestTranscribeCells_MalformedResponseNeverQuotesTheAnswer — the response body
// IS the student's writing, so a parse failure must describe the shape and
// withhold the content.
func TestTranscribeCells_MalformedResponseNeverQuotesTheAnswer(t *testing.T) {
	const secret = "the student wrote this sentence"
	p := &fake.ScriptedProvider{Steps: []fake.JSONStep{
		{JSON: fmt.Sprintf(`{"blocks":[{"kind":"nonsense","text":%q,"items":[]}],"confidence":"high"}`, secret)},
	}}
	docs, err := TranscribeCells(context.Background(), p, "vision-1", transcribeCells(t, 1), 1)
	assertErrorType[*ProviderError](t, err) // the only cell failed
	if docs[0].Err == nil {
		t.Fatal("docs[0].Err = nil, want a parse failure")
	}
	for _, msg := range []string{docs[0].Err.Error(), err.Error()} {
		if strings.Contains(msg, secret) {
			t.Errorf("error %q quotes the transcribed answer", msg)
		}
	}
}

// TestTranscribeCells_EveryCellFailedIsAProviderError — a wrong key or a dead
// endpoint fails every call, and that is a run failure (exit 8), not a bundle
// full of empty answers.
func TestTranscribeCells_EveryCellFailedIsAProviderError(t *testing.T) {
	p := &fake.ScriptedProvider{Steps: []fake.JSONStep{{Err: errors.New("401 unauthorized")}}}
	docs, err := TranscribeCells(context.Background(), p, "vision-1", transcribeCells(t, 3), 2)
	assertErrorType[*ProviderError](t, err, "3")
	if code := ExitCode(err); code != ExitProvider {
		t.Errorf("ExitCode = %d, want %d", code, ExitProvider)
	}
	if !strings.Contains(err.Error(), "401 unauthorized") {
		t.Errorf("error %q does not carry a representative cause", err.Error())
	}
	for i, d := range docs {
		if d.Err == nil {
			t.Errorf("docs[%d].Err = nil, want the recorded failure", i)
		}
	}
}

// TestTranscribeCells_NoCellsIsNotAFailure — a --stop-after run, or a batch
// where nothing matched, has nothing to transcribe. The orchestrator decides
// what that means.
func TestTranscribeCells_NoCellsIsNotAFailure(t *testing.T) {
	p := &fake.ScriptedProvider{Steps: []fake.JSONStep{{JSON: docJSON("x")}}}
	docs, err := TranscribeCells(context.Background(), p, "vision-1", nil, 4)
	if err != nil {
		t.Fatalf("TranscribeCells: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("docs = %d, want 0", len(docs))
	}
	if p.CallCount() != 0 {
		t.Errorf("provider calls = %d, want 0", p.CallCount())
	}
}

// TestTranscribeCells_CancelledContextStopsIssuingCalls — Ctrl-C must stop
// SPENDING, which means no call after the cancellation, not just a discarded
// result.
func TestTranscribeCells_CancelledContextStopsIssuingCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &stubProvider{reply: func(_ string, _ llm.Request) (llm.Result, error) {
		cancel() // the operator hits Ctrl-C during the first call
		return llm.Result{}, context.Canceled
	}}

	docs, err := TranscribeCells(ctx, stub, "vision-1", transcribeCells(t, 6), 1)
	assertErrorType[*ProviderError](t, err)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not unwrap to context.Canceled", err)
	}
	if got := stub.callCount(); got > 2 {
		t.Errorf("provider calls = %d after cancellation, want at most 2", got)
	}
	for i, d := range docs {
		if d.Err == nil {
			t.Errorf("docs[%d].Err = nil, want a recorded failure", i)
		}
	}
}

// TestTranscribeCells_AnUnmaskedCellNeverReachesTheProvider — the zero
// MaskedImage is what a bug in the pairing would produce, and sending it would
// be a request with no page at all.
func TestTranscribeCells_AnUnmaskedCellNeverReachesTheProvider(t *testing.T) {
	cells := transcribeCells(t, 1)
	cells[0].Masked = imaging.MaskedImage{}
	p := &fake.ScriptedProvider{Steps: []fake.JSONStep{{JSON: docJSON("x")}}}

	docs, err := TranscribeCells(context.Background(), p, "vision-1", cells, 1)
	assertErrorType[*ProviderError](t, err)
	if p.CallCount() != 0 {
		t.Errorf("provider calls = %d, want 0", p.CallCount())
	}
	if docs[0].Err == nil {
		t.Error("docs[0].Err = nil, want the unmasked-cell refusal")
	}
}

// --- PairCells -------------------------------------------------------------

// TestPairCells_JoinsOnThePageIndex — the join is by global page index, the one
// identifier that means the same thing in pages/, masked/ and the report.
func TestPairCells_JoinsOnThePageIndex(t *testing.T) {
	masked := maskedFixture(t, 3, bandPageJPEG, 200, 100, BandRegions(0.18))
	results := []MatchResult{
		{Page: masked[2].Page, StudentID: "AB03", Problem: 1, Status: StatusAuto},
		{Page: masked[0].Page, StudentID: "AB01", Problem: 2, Status: StatusForced},
		{Page: masked[1].Page, Status: StatusUnmatched, Reason: ReasonLowScore},
	}
	cells, err := PairCells(results, masked)
	if err != nil {
		t.Fatalf("PairCells: %v", err)
	}
	if len(cells) != 2 {
		t.Fatalf("cells = %d, want 2 (the unmatched page is not transcribed)", len(cells))
	}
	if cells[0].Result.Page.Index != 3 || cells[1].Result.Page.Index != 1 {
		t.Errorf("cells are %d,%d; want the caller's order 3,1",
			cells[0].Result.Page.Index, cells[1].Result.Page.Index)
	}
	if cells[0].Masked.SHA256() != masked[2].Masked.SHA256() {
		t.Error("cells[0] carries the wrong page's masked image")
	}
	if cells[1].Masked.SHA256() != masked[0].Masked.SHA256() {
		t.Error("cells[1] carries the wrong page's masked image")
	}
}

// TestPairCells_MatchedPageWithNoMaskedTwinIsFatal — the missing image is the
// privacy gate having skipped a page, which must never become "send the
// original instead".
func TestPairCells_MatchedPageWithNoMaskedTwinIsFatal(t *testing.T) {
	masked := maskedFixture(t, 2, bandPageJPEG, 200, 100, BandRegions(0.18))
	results := []MatchResult{{Page: Page{Index: 9, SourcePDF: "scan.pdf", SourcePage: 9}, StudentID: "AB01", Problem: 1, Status: StatusAuto}}
	_, err := PairCells(results, masked)
	assertErrorType[*ScanError](t, err, "page 9")
	if strings.Contains(err.Error(), "AB01") {
		t.Errorf("error %q names a student (PII rule)", err.Error())
	}
}
