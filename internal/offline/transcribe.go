package offline

import (
	"context"
	"fmt"
	"sync"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// MatchedCell is one transcription call's input: a page the matcher placed, and
// the masked derivative that may be sent for it.
//
// It pairs a MatchResult with an imaging.MaskedImage rather than with a whole
// MaskedPage because MatchResult already carries the original Page — pairing
// with MaskedPage would store the same page bytes twice and invite the two
// copies to drift.
type MatchedCell struct {
	Result MatchResult
	Masked imaging.MaskedImage
}

// CellDoc is one cell's outcome: what was asked, what came back, and — when the
// call failed — why, so a partial run can still write a bundle and say what is
// missing from it.
//
// Result and Masked are the MatchedCell that produced this row, carried through
// unchanged: the bundle stage needs BOTH, the masked image for the export's
// pages (which keeps export's own privacy sweep honest) and Result.Page.JPEG
// for the images/ the professor actually reads.
//
// Confidence is the model's own "high"|"medium"|"low"|"illegible" verdict. It
// is kept rather than folded into the Doc because the bundle's manifest reports
// it, and an "illegible" answer with blocks reads differently from a confident
// one.
type CellDoc struct {
	Result     MatchResult
	Masked     imaging.MaskedImage
	Doc        transcribe.Doc
	Confidence string
	Err        error
}

// PairCells joins matched pages to their masked images, dropping the unmatched
// ones (an unmatched page names no student, so there is nothing to file its
// transcription under — it lives in the match report and nowhere else).
//
// The join key is the global page index, the one identifier that means the same
// thing in pages/, in masked/ and in the report. A matched page with no masked
// twin is a *ScanError (exit 4) rather than a skip: the masked image is the
// only thing that may be sent, so "carry on without it" has no safe meaning.
//
// The caller's order is preserved.
func PairCells(results []MatchResult, masked []MaskedPage) ([]MatchedCell, error) {
	byIndex := make(map[int]imaging.MaskedImage, len(masked))
	for _, mp := range masked {
		byIndex[mp.Page.Index] = mp.Masked
	}
	cells := make([]MatchedCell, 0, len(results))
	for _, r := range results {
		if r.Status == StatusUnmatched || r.Problem < 1 || r.StudentID == "" {
			continue
		}
		img, ok := byIndex[r.Page.Index]
		if !ok {
			return nil, newScanError(nil,
				"page %d (%s page %d) was matched but has no masked image: the mask stage must run over every page before transcription",
				r.Page.Index, r.Page.SourcePDF, r.Page.SourcePage)
		}
		cells = append(cells, MatchedCell{Result: r, Masked: img})
	}
	return cells, nil
}

// TranscribeCells transcribes every cell, at most concurrency calls at a time,
// and returns one CellDoc per cell IN CELL ORDER.
//
// The order is load-bearing, not cosmetic: the caller zips these back against
// its own cells by position, so returning them in completion order would file
// every answer under whichever page happened to finish in that slot. Results
// are therefore written into a pre-sized slice by index, never appended from a
// worker.
//
// The request mirrors the server's dedicated transcription call
// (internal/httpapi/transcription.go) field for field — same system prompt,
// same schema, same forced tool, same zero temperature — so the same page
// transcribed here and there produces the same text. The only difference is the
// user prompt's problem number, which this mode knows and the server's export
// path does not.
//
// Failure policy: one cell's failure is recorded in its CellDoc.Err and the
// rest continue, because a single refused page must not cost the other two
// hundred their transcription (and their spend). If EVERY cell failed, that is
// a run failure — a wrong key, a dead endpoint, a model that cannot see images
// — and it returns a *ProviderError (exit 8) wrapping a representative cause.
// The per-cell rows are returned alongside that error so a caller can still
// report what happened; nothing in them is usable for a bundle. Zero cells in
// means zero out and no error; the orchestrator decides what an empty batch
// means.
//
// PII: no message this function builds carries a student id, a name, or one
// byte of a provider response. A cell is identified by its page index and
// problem number, which is what an operator needs to go look at the page.
func TranscribeCells(ctx context.Context, p llm.Provider, model string, cells []MatchedCell, concurrency int) ([]CellDoc, error) {
	if len(cells) == 0 {
		return []CellDoc{}, nil
	}
	if concurrency < 1 {
		concurrency = 1
	}

	docs := make([]CellDoc, len(cells))
	for i, c := range cells {
		docs[i] = CellDoc{Result: c.Result, Masked: c.Masked}
	}

	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				doc, confidence, err := transcribeOne(ctx, p, model, cells[i])
				docs[i].Doc, docs[i].Confidence, docs[i].Err = doc, confidence, err
			}
		}()
	}
	for i := range cells {
		work <- i
	}
	close(work)
	wg.Wait()

	failed := 0
	var first error
	for _, d := range docs {
		if d.Err != nil {
			failed++
			if first == nil {
				first = d.Err
			}
		}
	}
	if failed == len(docs) {
		// One representative cause, not all of them: they are the same error N
		// times in every case this fires, and the list would be N page numbers
		// the operator already has in the report.
		return docs, newProviderError(first, "all %d transcription calls failed", failed)
	}
	return docs, nil
}

// transcribeOne is one provider call plus its parse. It never returns a
// half-filled Doc: a parse failure yields the error and nothing else, so a
// caller cannot mistake a partial transcription for a complete one.
func transcribeOne(ctx context.Context, p llm.Provider, model string, cell MatchedCell) (transcribe.Doc, string, error) {
	// Checked per cell rather than once up front: a cancellation during a long
	// batch must stop the SPEND, which means no call issued after it.
	if err := ctx.Err(); err != nil {
		return transcribe.Doc{}, "", newCellError(cell, err, "cancelled before the transcription call")
	}
	// Structurally unreachable through PairCells, which is the only supported
	// way to build a MatchedCell. It is checked anyway because the failure mode
	// is a request with no page in it, billed in full.
	if cell.Masked.IsZero() {
		return transcribe.Doc{}, "", newCellError(cell, nil, "has no masked image, so there is nothing that may be sent")
	}

	res, err := p.Grade(ctx, model, llm.Request{
		System: transcribe.SystemPrompt,
		Prompt: transcribe.UserPrompt(cell.Result.Problem),
		// The seal (D10/D19): Images accepts only imaging.ProviderImage, and the
		// only value here is the masked derivative. An original page does not
		// typecheck.
		Images:      []imaging.ProviderImage{cell.Masked},
		Schema:      transcribe.BuildSchema(),
		Temperature: 0,
		ToolName:    transcribe.ToolName,
	})
	if err != nil {
		return transcribe.Doc{}, "", newCellError(cell, err, "transcription call failed")
	}
	doc, confidence, err := transcribe.ParseResponse(res.JSON)
	if err != nil {
		// ParseResponse withholds the response body itself; nothing is added.
		return transcribe.Doc{}, "", newCellError(cell, err, "the model's reply did not match the transcription schema")
	}
	return doc, confidence, nil
}

// newCellError labels a per-cell failure with the page and problem, and NOTHING
// else. The student id and name are deliberately absent: these errors are
// printed to a terminal and pasted into bug reports (CLAUDE.md PII rule), and
// the page index is what the operator needs to open the page anyway.
//
// The cause is wrapped rather than flattened, so a cancelled run still unwraps
// to context.Canceled at the top of the pipeline.
func newCellError(cell MatchedCell, cause error, what string) error {
	if cause == nil {
		return fmt.Errorf("page %d (problem %d) %s", cell.Result.Page.Index, cell.Result.Problem, what)
	}
	return fmt.Errorf("page %d (problem %d) %s: %w", cell.Result.Page.Index, cell.Result.Problem, what, cause)
}
