// This file is the heart of page-level scan intake (design spec 2026-07-04
// §6): one page's three identity crops (student ID, name, problem number) are
// OCR'd — local rung first (D24), cloud rung second (D19) — then resolved
// against the roster/problem list and placed: auto-assigned when both the
// student and the problem resolve cleanly (D64 both-or-orphan), parked when
// the target cell is already occupied (D65 never overwrite), or left an
// orphan with a best-effort pre-fill proposal otherwise. OCR text is written
// only to scan_pages columns — never logged, never put in an error string
// (D14).
package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/ocr"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// pageIdentitySchema is the strict structured-output contract for page
// identification: the only shape a provider may return.
// additionalProperties:false + required forces all six fields (design spec §5).
var pageIdentitySchema = []byte(`{"type":"object","additionalProperties":false,"properties":{"student_id":{"type":"string"},"name":{"type":"string"},"problem":{"type":"string"},"student_id_legible":{"type":"boolean"},"name_legible":{"type":"boolean"},"problem_legible":{"type":"boolean"}},"required":["student_id","name","problem","student_id_legible","name_legible","problem_legible"]}`)

const (
	pageIDSystemPrompt = "You read three tightly-cropped header boxes from one exam page, in order: (1) student ID box, (2) name box (often Chinese), (3) problem number box (like Q1 / P3 / 問2). Transcribe exactly what is written in each. Do not guess, translate, or infer. For any box that is blank or unreadable, set its legible flag to false and return an empty string."
	pageIDUserPrompt   = "Return the student ID, name, and problem reference exactly as written in these three boxes, in order."
	pageIDMaxTokens    = 256
)

// pageIdentityOut is the strict OCR schema (DisallowUnknownFields on parse).
type pageIdentityOut struct {
	StudentID        string `json:"student_id"`
	Name             string `json:"name"`
	Problem          string `json:"problem"`
	StudentIDLegible bool   `json:"student_id_legible"`
	NameLegible      bool   `json:"name_legible"`
	ProblemLegible   bool   `json:"problem_legible"`
}

// IdentifyPage is the scan.identify_page worker body. It tries the local-OCR
// rung first when installed (D24), and falls back to the batch's cloud OCR
// provider (D19) only when the local rung produced nothing fully
// auto-assignable. It parses the strict identity schema on the cloud path
// (one re-ask on malformed output), records the OCR fields, and places the
// page (auto-assign / park / orphan) via placeAuto (§6, D64, D65). It skips
// silently when the page has no crops, is already assigned/parked/discarded/
// promoted (render redelivery re-enqueues identify for unidentified pages —
// this guard makes that safe), or the batch has OCR disabled and no local
// reader exists (that IS the D24 ladder's last rung; a human assigns from
// the crop).
func (s *Service) IdentifyPage(ctx context.Context, pageID int64, finalAttempt bool) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil {
		return fmt.Errorf("scan: identify: load page: %w", err)
	}
	if page.AssignedStudentID.Valid || page.DiscardedAt.Valid ||
		page.SubmissionID.Valid || page.ParkedReason.Valid ||
		!page.StudentIDCropRef.Valid || !page.NameCropRef.Valid || !page.ProblemCropRef.Valid {
		return nil
	}
	batch, err := s.Store.Q.GetScanBatch(ctx, page.BatchID)
	if err != nil {
		return fmt.Errorf("scan: identify: load batch: %w", err)
	}

	crops, err := s.loadCrops(ctx, page)
	if err != nil {
		return s.setPageError(ctx, pageID, "crop unreadable")
	}

	var localOut pageIdentityOut
	localEngine := ""
	if s.Local != nil {
		var done bool
		localOut, done = s.identifyLocal(ctx, page, crops)
		if done {
			return nil
		}
		localEngine = engineLocal
	}
	if !batch.OcrEnabled {
		// The D24 ladder ends here: no cloud rung will ever run for this
		// batch. If a local rung ran and produced partial reads (an ID but
		// no name, a name but no valid problem, etc.), stamp the page
		// identified WITH those partial reads via placeAuto — same call
		// identifyLocal's own success path makes — so it surfaces as an
		// orphan with whatever pre-fill proposal the reads support, instead
		// of sitting unidentified ("processing" forever, invisible to the
		// OrphanQueue). When there's no local rung at all, localOut is the
		// zero value and localEngine is "" (textOrNull leaves ocr_engine
		// NULL), so placeAuto records a vacuous (illegible/empty)
		// identification — same as the no-local-reader case in pages.go.
		return s.placeAuto(ctx, page, localOut, localEngine)
	}

	out, engine, err := s.identifyCloud(ctx, batch, crops, finalAttempt)
	// F17: a shutdown/timeout that cancels the provider call mid-flight surfaces
	// as context.Canceled/DeadlineExceeded. This is an INTERRUPTION, not an OCR
	// verdict — never write the terminal error column, not even on the final
	// attempt. Return the error so the queue records a plain attempt instead of
	// wedging the page with a bogus error.
	if isInterruption(ctx, err) {
		s.log().Warn("scan: identify interrupted (shutdown/timeout); not terminal", "page_id", pageID)
		return err
	}
	var unavailable *llm.ProviderUnavailableError
	var retry retryableError
	switch {
	case err == nil:
		return s.placeAuto(ctx, page, out, engine)
	case errors.As(err, &unavailable):
		return s.setPageError(ctx, pageID, "OCR provider unavailable")
	case errors.As(err, &retry):
		if finalAttempt {
			return s.setPageError(ctx, pageID, "OCR failed after retries")
		}
		s.log().Warn("scan: identify retryable failure", "page_id", pageID)
		return err // queue backoff retries
	default:
		// Malformed-past-reask and other non-retryable failures are terminal.
		return s.setPageError(ctx, pageID, "OCR produced no usable identity")
	}
}

// loadCrops reads and gates the page's three identity crops in order
// (student_id, name, problem). A gate failure (missing bytes, wrong key
// shape) is a caller bug, not a transient condition — the caller treats any
// error here as terminal ("crop unreadable").
func (s *Service) loadCrops(ctx context.Context, page db.ScanPage) ([3]imaging.IDCrop, error) {
	var crops [3]imaging.IDCrop
	refs := [3]string{page.StudentIDCropRef.String, page.NameCropRef.String, page.ProblemCropRef.String}
	for i, ref := range refs {
		raw, err := s.readAll(ctx, ref)
		if err != nil {
			return crops, fmt.Errorf("scan: identify: read crop: %w", err)
		}
		crop, err := imaging.LoadIDCrop(ref, raw)
		if err != nil {
			return crops, fmt.Errorf("scan: identify: load crop: %w", err)
		}
		crops[i] = crop
	}
	return crops, nil
}

// identifyLocal runs the local-OCR rung (D24) over all three crops: ReadLines
// per crop, filter to confident lines, PickID/PickName for the id/name
// crops, and the highest-confidence line's text for the problem crop (parsed
// with ParseProblemRef). It writes nothing itself — local succeeds ONLY when
// the resulting resolution is a full auto-assign (student agreement AND a
// valid problem), in which case it calls placeAuto directly and reports
// done=true. ANY other outcome — a ReadLines error, a gate failure, no
// confident lines, or a partial/ambiguous resolution — reports done=false so
// IdentifyPage falls through to the cloud rung when OCR is enabled; every
// such case is logged at Warn with page_id ONLY (never OCR text, D14).
//
// The partial pageIdentityOut it built (whatever legible reads it managed,
// even when nothing resolved) is always returned alongside done, so a caller
// whose cloud rung is unavailable (OCR disabled) can still surface those
// partial reads via placeAuto instead of discarding them — see IdentifyPage.
// It is the zero value only on the ReadLines/roster/problem-list error paths,
// where nothing was read at all.
func (s *Service) identifyLocal(ctx context.Context, page db.ScanPage, crops [3]imaging.IDCrop) (out pageIdentityOut, done bool) {
	var lines [3][]ocr.Line
	for i, crop := range crops {
		ls, err := s.Local.ReadLines(ctx, crop)
		if err != nil {
			s.log().Warn("scan: local OCR read failed, falling back", "page_id", page.ID)
			return out, false
		}
		lines[i] = ls
	}

	confident := func(ls []ocr.Line) []ocr.Line {
		out := make([]ocr.Line, 0, len(ls))
		for _, ln := range ls {
			if ln.Confidence >= localOCRMinConfidence {
				out = append(out, ln)
			}
		}
		return out
	}
	idLines := confident(lines[0])
	nameLines := confident(lines[1])
	problemLines := confident(lines[2])

	id := PickID(idLines)
	name := PickName(nameLines)
	problem := bestLine(problemLines)

	out = pageIdentityOut{
		StudentID:        id,
		Name:             name,
		Problem:          problem,
		StudentIDLegible: id != "",
		NameLegible:      name != "",
		ProblemLegible:   problem != "",
	}

	roster, err := s.roster(ctx)
	if err != nil {
		s.log().Warn("scan: local OCR roster load failed, falling back", "page_id", page.ID)
		return pageIdentityOut{}, false
	}
	problems, err := s.Store.Q.ListProblems(ctx, page.AssessmentID)
	if err != nil {
		s.log().Warn("scan: local OCR problem list failed, falling back", "page_id", page.ID)
		return pageIdentityOut{}, false
	}

	m := MatchStudent(out.StudentID, out.Name, roster)
	problemID := resolveProblemID(out.Problem, problems)
	if m.AgreedID == 0 || problemID == 0 {
		return out, false // not a full auto-assignable resolution; fall through
	}

	if err := s.placeAuto(ctx, page, out, engineLocal); err != nil {
		s.log().Warn("scan: local OCR placement failed, falling back", "page_id", page.ID)
		return out, false
	}
	return out, true
}

// bestLine returns the highest-confidence line's text among confident lines
// (ties broken by encounter order), or "" when there are none.
func bestLine(lines []ocr.Line) string {
	best := -1.0
	text := ""
	for _, ln := range lines {
		if ln.Confidence > best {
			best = ln.Confidence
			text = ln.Text
		}
	}
	return text
}

// resolveProblemID parses ocrProblem and looks up the matching problem's DB
// id in problems, returning 0 when the reference doesn't parse or no problem
// with that number exists.
func resolveProblemID(ocrProblem string, problems []db.Problem) int64 {
	n, ok := ParseProblemRef(ocrProblem)
	if !ok {
		return 0
	}
	for _, p := range problems {
		if int(p.Number) == n {
			return p.ID
		}
	}
	return 0
}

// identifyCloud does the provider call + strict parse (one re-ask on
// malformed output). All returned errors are classified by the caller
// (IdentifyPage). engine is the resolved provider name, returned so the
// caller can record it as ocr_engine.
func (s *Service) identifyCloud(ctx context.Context, batch db.ScanBatch, crops [3]imaging.IDCrop, finalAttempt bool) (pageIdentityOut, string, error) {
	var out pageIdentityOut
	provider, limiter, err := s.Providers.Provider(ctx, batch.OcrProvider.String)
	if err != nil {
		var unavailable *llm.ProviderUnavailableError
		if errors.As(err, &unavailable) {
			return out, "", err // terminal: a missing/disabled provider won't heal by retrying
		}
		return out, "", retryableError{err}
	}

	prompt := pageIDUserPrompt
	for attempt := 0; attempt < 2; attempt++ {
		if limiter != nil {
			if err := limiter.Wait(ctx); err != nil {
				return out, "", retryableError{err}
			}
		}
		result, err := provider.Grade(ctx, batch.OcrModel.String, llm.Request{
			System:         pageIDSystemPrompt,
			Prompt:         prompt,
			Images:         []imaging.ProviderImage{crops[0], crops[1], crops[2]},
			Schema:         pageIdentitySchema,
			Temperature:    0,
			MaxTokens:      pageIDMaxTokens,
			ToolName:       "submit_identity",
			ReasoningLevel: "off",
		})
		if err != nil {
			var rle *llm.RateLimitError
			if errors.As(err, &rle) {
				return out, "", retryableError{err}
			}
			var unavailable *llm.ProviderUnavailableError
			if errors.As(err, &unavailable) {
				return out, "", err
			}
			return out, "", retryableError{err} // transport
		}
		if parseErr := strictUnmarshalIdentity(result.JSON, &out); parseErr == nil {
			return out, provider.Name(), nil
		} else if attempt == 0 {
			// One re-ask, mirroring the runner (no PII in the note).
			prompt = pageIDUserPrompt + "\n\nYour previous response did not match the required schema. Return ONLY the six fields student_id, name, problem, student_id_legible, name_legible, problem_legible."
			continue
		}
	}
	return out, "", errors.New("scan: page identity output malformed after re-ask")
}

// strictUnmarshalIdentity decodes exactly the page identity schema, rejecting
// unknown fields so a rubric-shaped fabrication or a wrong-shape reply is
// caught (mirrors the strict-parse stance of the grading path).
func strictUnmarshalIdentity(raw []byte, out *pageIdentityOut) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

// placeAuto applies one identified page's resolution in a single transaction
// (§6, D64, D65): it records the OCR fields + roster/problem-match proposal
// (SetScanPageIdentified, stamping identified_at — the orphan/processing
// boundary), and then, only when BOTH the student and the problem resolved
// cleanly (D64 both-or-orphan), attempts to place the page into that cell:
// an empty cell auto-assigns (assigned_by left NULL); an occupied cell parks
// instead of ever overwriting the incumbent (D65) — "duplicate" when the
// occupying page's image matches this one's, "conflict" otherwise, and
// "conflict" (parked_against NULL) when the cell is occupied by a live
// submission with no backing page. If AssignScanPage loses the race to the
// partial unique index (23505 — a concurrent identify won the same cell
// first), the incumbent lookup is re-run once outside the failed tx and this
// page is parked instead of erroring.
func (s *Service) placeAuto(ctx context.Context, page db.ScanPage, out pageIdentityOut, engine string) error {
	roster, err := s.roster(ctx)
	if err != nil {
		return retryableError{err}
	}
	problems, err := s.Store.Q.ListProblems(ctx, page.AssessmentID)
	if err != nil {
		return retryableError{err}
	}

	ocrID, ocrName, ocrProblem := "", "", ""
	if out.StudentIDLegible {
		ocrID = out.StudentID
	}
	if out.NameLegible {
		ocrName = out.Name
	}
	if out.ProblemLegible {
		ocrProblem = out.Problem
	}

	m := MatchStudent(ocrID, ocrName, roster)
	problemID := resolveProblemID(ocrProblem, problems)
	identifiedParams := db.SetScanPageIdentifiedParams{
		ID:                  page.ID,
		OcrStudentID:        textOrNull(out.StudentID),
		OcrName:             textOrNull(out.Name),
		OcrProblem:          textOrNull(out.Problem),
		OcrStudentIDLegible: boolOf(out.StudentIDLegible),
		OcrNameLegible:      boolOf(out.NameLegible),
		OcrProblemLegible:   boolOf(out.ProblemLegible),
		OcrEngine:           textOrNull(engine),
		ProposedStudentID:   int8OrNull(m.ProposedID),
		ProposedProblemID:   int8OrNull(problemID),
		ProposalSource:      textOrNull(m.ProposalSource),
	}

	err = s.Store.WithTx(ctx, func(q *db.Queries) error {
		if err := q.SetScanPageIdentified(ctx, identifiedParams); err != nil {
			return err
		}
		if m.AgreedID == 0 || problemID == 0 {
			return nil // orphan
		}

		incPage, incSub, err := s.cellIncumbentQ(ctx, q, page.AssessmentID, m.AgreedID, problemID, page.ID)
		if err != nil {
			return err
		}
		return s.resolveCell(ctx, q, page, m.AgreedID, problemID, incPage, incSub)
	})
	if err != nil && store.IsUniqueViolation(err) {
		// Lost the race for this cell to a concurrent identify that assigned
		// first: AssignScanPage's own attempt above rolled back with the tx, so
		// SetScanPageIdentified must be re-applied, then this page parks
		// against whatever now occupies the cell — it must NOT retry
		// AssignScanPage again (that just repeats the same race).
		return s.Store.WithTx(ctx, func(q *db.Queries) error {
			if err := q.SetScanPageIdentified(ctx, identifiedParams); err != nil {
				return err
			}
			incPage, incSub, err := s.cellIncumbentQ(ctx, q, page.AssessmentID, m.AgreedID, problemID, page.ID)
			if err != nil {
				return err
			}
			return s.parkAgainst(ctx, q, page, m.AgreedID, problemID, incPage, incSub)
		})
	}
	return err
}

// resolveCell parks page against whatever already occupies the cell (D65), or
// assigns it into an empty cell (auto-assign, assigned_by left NULL).
func (s *Service) resolveCell(ctx context.Context, q *db.Queries, page db.ScanPage, studentID, problemID int64, incPage *db.ScanPage, incSub *db.Submission) error {
	if incPage != nil || incSub != nil {
		return s.parkAgainst(ctx, q, page, studentID, problemID, incPage, incSub)
	}
	return q.AssignScanPage(ctx, db.AssignScanPageParams{
		ID: page.ID, AssignedStudentID: int8OrNull(studentID),
		AssignedProblemID: int8OrNull(problemID), AssignedBy: int8OrNull(0), // NULL = auto
	})
}

// parkAgainst parks page against the cell's incumbent (D65): "duplicate" when
// the occupying page's image matches this one's, "conflict" otherwise, and
// "conflict" with parked_against NULL when the cell is occupied by a live
// submission with no backing page (content can't be compared cheaply).
// studentID/problemID name the CONTESTED cell, captured on the park row so
// ResolveConflict's replace still targets the cell where the fight happened
// even after the incumbent moves (0031).
func (s *Service) parkAgainst(ctx context.Context, q *db.Queries, page db.ScanPage, studentID, problemID int64, incPage *db.ScanPage, incSub *db.Submission) error {
	switch {
	case incPage != nil:
		reason := "conflict"
		if incPage.ImageSha256.Valid && page.ImageSha256.Valid &&
			incPage.ImageSha256.String == page.ImageSha256.String {
			reason = "duplicate"
		}
		return q.ParkScanPage(ctx, db.ParkScanPageParams{
			ID: page.ID, ParkedReason: textOrNull(reason), ParkedAgainst: int8OrNull(incPage.ID),
			ParkStudentID: int8OrNull(studentID), ParkProblemID: int8OrNull(problemID),
		})
	case incSub != nil:
		return q.ParkScanPage(ctx, db.ParkScanPageParams{
			ID: page.ID, ParkedReason: textOrNull("conflict"), ParkedAgainst: int8OrNull(0),
			ParkStudentID: int8OrNull(studentID), ParkProblemID: int8OrNull(problemID),
		})
	default:
		// The incumbent vanished between the failed tx and this re-check (a
		// transient lost race, not a real inconsistency): nothing to park
		// against right now, so surface this as retryable — the next attempt
		// re-reads the now-empty cell and auto-assigns it.
		return retryableError{fmt.Errorf("scan: identify: unique violation but cell has no incumbent")}
	}
}

// cellIncumbentQ reports what already occupies a (student, problem) cell: a
// live assigned page (possibly promoted), or — when no page occupies it — a
// live submission with no backing page (a Submissions-tab upload). Either
// one blocks auto-assignment per D65. It must run
// against the SAME *db.Queries as the surrounding transaction so the
// occupancy check and the subsequent park/assign write are consistent.
func (s *Service) cellIncumbentQ(ctx context.Context, q *db.Queries, assessmentID, studentID, problemID, excludePageID int64) (page *db.ScanPage, sub *db.Submission, err error) {
	p, err := q.LivePageForCell(ctx, db.LivePageForCellParams{
		AssessmentID:      assessmentID,
		AssignedStudentID: int8OrNull(studentID),
		AssignedProblemID: int8OrNull(problemID),
		ID:                excludePageID,
	})
	switch {
	case err == nil:
		return &p, nil, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to the submission check
	default:
		return nil, nil, err
	}

	s2, err := q.GetActiveSubmissionForProblem(ctx, db.GetActiveSubmissionForProblemParams{
		AssessmentID: assessmentID, StudentID: studentID, ProblemID: int8OrNull(problemID),
	})
	switch {
	case err == nil:
		return nil, &s2, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to the whole-assessment submission check
	default:
		return nil, nil, err
	}

	s3, err := q.GetActiveSubmission(ctx, db.GetActiveSubmissionParams{
		AssessmentID: assessmentID, StudentID: studentID,
	})
	switch {
	case err == nil:
		return nil, &s3, nil
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil, nil
	default:
		return nil, nil, err
	}
}
