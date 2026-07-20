// Package ingest owns the submission pipeline (spec §7/§8, DECISIONS D1/D10/D13/D22):
// upload → roster match (filename <student_id>.<ext>, else quarantine) → decode or
// render pages straight from the source PDF/image → map onto the whole assessment
// positionally, or a single target problem for per-problem/image submissions →
// store originals; and the masking pass that derives the ONLY artifacts a vision
// provider may ever see.
package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/studentid"
)

// FlagImageSuperseded marks answers whose graded image was replaced by a forced
// re-upload (D1); records keep the SHAs they actually graded.
const FlagImageSuperseded = "image_superseded"

// ErrAlreadyRetracted is returned by RetractSubmission when the submission is
// already retracted. Callers like scan.Reassign treat this as already-done
// (idempotent retry after a partial failure) rather than a real error, so they
// match on this sentinel via errors.Is instead of the error string.
var ErrAlreadyRetracted = errors.New("submission is already retracted")

// Business-rule sentinels are exported so HTTP callers can map them to caller-fault
// statuses without string matching.
var (
	ErrRetractionNeedsForce      = errors.New("student already has grading records; retraction requires force")
	ErrRetractionBlocked         = errors.New("student has published answers; retraction is not allowed")
	ErrQuarantineNotFound        = errors.New("no such quarantine entry")
	ErrQuarantineAlreadyResolved = errors.New("quarantine entry already resolved")
	ErrQuarantineNotAssignable   = errors.New("this quarantine entry cannot be assigned; upload a readable replacement or dismiss it")
)

// MaxPDFBytes bounds one uploaded file.
const MaxPDFBytes = 50 << 20

// ErrAmbiguousStudentID reports an external student id that exact-matched no
// roster row but matched MORE than one active student under studentid.Normalize.
// The caller cannot safely pick one, so it quarantines (filename ingest) or
// rejects with a 400 (quarantine resolve, orphan manual assign) and a human
// decides. The message is the exact wire copy the handlers return.
var ErrAmbiguousStudentID = errors.New("ambiguous student id")

// ResolveStudentByExternalID is the ONE exact-then-normalized roster lookup
// (roster-lifecycle plan 2026-07-10, fix 4) shared by filename ingest,
// quarantine resolve, and the scan orphan manual assign. An exact student_id
// match always wins — including a withdrawn student, so every caller's
// existing withdraw guard and message stay intact. On an exact miss the
// ACTIVE roster ids are scanned for equality under studentid.Normalize
// (NFKC fold → upper → strip non-alphanumeric): exactly one hit resolves to
// that student, zero hits return pgx.ErrNoRows exactly as the exact lookup
// would, and several hits return ErrAmbiguousStudentID.
func ResolveStudentByExternalID(ctx context.Context, q *db.Queries, externalID string) (db.Student, error) {
	student, err := q.GetStudentByExternalID(ctx, externalID)
	if err == nil {
		return student, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.Student{}, err
	}
	want := studentid.Normalize(externalID)
	if want == "" {
		// Nothing left after normalization (e.g. punctuation-only) — never
		// match the equally-degenerate normalization of some roster id.
		return db.Student{}, pgx.ErrNoRows
	}
	ids, err := q.ListActiveStudentIDs(ctx)
	if err != nil {
		return db.Student{}, err
	}
	match := ""
	for _, id := range ids {
		if studentid.Normalize(id) != want {
			continue
		}
		if match != "" {
			return db.Student{}, ErrAmbiguousStudentID
		}
		match = id
	}
	if match == "" {
		return db.Student{}, pgx.ErrNoRows
	}
	return q.GetStudentByExternalID(ctx, match)
}

// Service wires the pipeline's seams together.
type Service struct {
	Store    *store.Store
	Blobs    blobstore.Store
	Renderer render.Renderer
	Opts     render.Options
	Log      *slog.Logger

	// EnqueueDirectIngest enqueues the ingest job for staged bulk direct uploads
	// (D27, F1) inside the same tx as their row insert, so the rows and their jobs
	// commit atomically. nil in tests skips the enqueue.
	EnqueueDirectIngest func(ctx context.Context, tx pgx.Tx, ids []int64) error
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// FileResult reports one uploaded file's outcome. Statuses: ingested, quarantined,
// rejected (rejected = roster-matched but blocked, e.g. graded without force).
type FileResult struct {
	Filename     string `json:"filename"`
	StudentID    string `json:"student_id,omitempty"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	Pages        int    `json:"pages,omitempty"`
	MappedPages  int    `json:"mapped_pages,omitempty"`
	SubmissionID int64  `json:"submission_id,omitempty"`
}

// IngestInput is one file to run through the pipeline. Kind selects the source
// decode path ("pdf" renders every page with the Renderer seam; "image" decodes a
// single raster image via NormalizeImage). TargetProblemID, when non-zero, scopes
// the submission to that one problem instead of the whole-assessment positional
// mapping (spec §8, D22): every rendered page maps onto that problem's answer, and
// the guards/supersede chain are scoped to (assessment, student, problem) rather
// than (assessment, student).
type IngestInput struct {
	Filename             string // still carries "<student_id>.<ext>" for roster match
	Data                 []byte
	Kind                 string // "pdf" | "image"
	TargetProblemID      int64  // 0 = whole-assessment positional mapping
	ExistingQuarantineID int64  // non-zero only when retrying an existing open entry

	// LinkInTx, when set, runs INSIDE the same transaction that creates the
	// submission, after every row write, receiving the new submission's id. A
	// non-nil error aborts the entire ingest — the submission (and everything
	// it superseded/deleted) never becomes visible. Scan promotion uses it to
	// link scan_pages.submission_id conditionally on the page's assignment
	// being unchanged, so a promote racing a concurrent reassign rolls back
	// atomically instead of leaving a live submission under the wrong student.
	LinkInTx func(q *db.Queries, submissionID int64) error
}

// IngestFile runs the whole per-file pipeline for a PDF upload. force permits
// replacing an already-graded student's submission (flagging affected answers);
// published answers can never be replaced (D1). It is a thin wrapper over Ingest.
func (s *Service) IngestFile(ctx context.Context, assessmentID int64, filename string, pdf []byte, uploadedBy int64, force bool) FileResult {
	return s.Ingest(ctx, assessmentID, IngestInput{Filename: filename, Data: pdf, Kind: "pdf"}, uploadedBy, force)
}

// Ingest runs the whole per-file pipeline (spec §7/§8, D1/D10/D13/D22): upload →
// roster match (filename <student_id>.<ext>, else quarantine) → decode/render pages
// → map onto problem(s) → guards → store originals, in one transaction.
func (s *Service) Ingest(ctx context.Context, assessmentID int64, in IngestInput, uploadedBy int64, force bool) FileResult {
	filename := in.Filename
	data := in.Data
	res := FileResult{Filename: filename, Status: "rejected"}
	if len(data) == 0 || len(data) > MaxPDFBytes {
		res.Reason = "file empty or too large"
		return res
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	// Roster match by filename convention <student_id>.<ext> (D13): exact match
	// first, then a unique studentid.Normalize hit over the active roster; zero
	// or ambiguous normalized hits quarantine exactly as an unknown id does.
	externalID := strings.TrimSuffix(filename, filepath.Ext(filename))
	externalID = strings.TrimSpace(externalID)
	student, err := ResolveStudentByExternalID(ctx, s.Store.Q, externalID)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrAmbiguousStudentID) {
		return s.quarantineInput(ctx, assessmentID, in, sha, "unknown_student", uploadedBy)
	}
	if err != nil {
		res.Reason = "roster lookup failed"
		return res
	}
	if student.StudentID != externalID {
		// The normalized fallback fired — counts only, never the id (D14).
		s.log().Debug("filename roster match used normalized student-id fallback", "assessment_id", assessmentID)
	}
	res.StudentID = student.StudentID
	// A withdrawn student is off the roster for grading purposes: reject the direct
	// upload before any writes (MaterializeAnswers also excludes them, so their
	// answers would never exist to attach pages to).
	if student.WithdrawnAt.Valid {
		res.Reason = "student is withdrawn; reinstate before uploading"
		return res
	}

	targetProblemID := in.TargetProblemID
	scoped := targetProblemID != 0

	var pageCount int
	var rendered []renderedPage
	var sourceKind string
	var sourceExt string

	if in.Kind == "image" {
		sourceKind = "image"
		page, err := NormalizeImage(data, s.Opts)
		if err != nil {
			return s.quarantineInput(ctx, assessmentID, in, sha, "invalid_image", uploadedBy)
		}
		sourceExt = sniffImageExt(data)
		pageCount = 1
		if scoped {
			problem, err := s.Store.Q.GetProblem(ctx, targetProblemID)
			if err != nil {
				res.Reason = "target problem not found"
				return res
			}
			rendered = []renderedPage{{problem: problem, pdfPage: 0, page: page}}
		} else {
			problems, err := s.Store.Q.ListProblems(ctx, assessmentID)
			if err != nil || len(problems) == 0 {
				res.Reason = "assessment has no problems to map onto"
				return res
			}
			rendered = []renderedPage{{problem: problems[0], pdfPage: 0, page: page}}
		}
	} else {
		sourceKind = "pdf"
		sourceExt = "pdf"
		// Open ONCE per file (F3): PageCount + every RenderPage reuse this single
		// document load instead of re-parsing the whole PDF N+1 times. Close frees
		// the pooled worker; do it as soon as rendering is done (BEFORE the DB tx),
		// so we never hold a scarce pool slot across the transaction. All rendering
		// happens first (as before), then Close, then the tx section below.
		doc, derr := s.Renderer.Open(ctx, data)
		if derr != nil {
			return s.quarantineInput(ctx, assessmentID, in, sha, "invalid_pdf", uploadedBy)
		}
		pageCount = doc.PageCount()
		if pageCount == 0 {
			_ = doc.Close()
			return s.quarantineInput(ctx, assessmentID, in, sha, "invalid_pdf", uploadedBy)
		}
		res.Pages = pageCount

		if scoped {
			problem, err := s.Store.Q.GetProblem(ctx, targetProblemID)
			if err != nil {
				_ = doc.Close()
				res.Reason = "target problem not found"
				return res
			}
			// Per-problem batches: every page of the file maps onto the SAME
			// problem/answer, ordered page_index 0..n-1 (spec §8, D22).
			for i := 0; i < pageCount; i++ {
				raster, pg, err := doc.RenderPageImage(ctx, i, s.Opts)
				if err != nil {
					_ = doc.Close()
					s.log().Warn("render failed", "assessment_id", assessmentID, "pdf_page", i, "err", err)
					res.Reason = fmt.Sprintf("rendering page %d failed", i+1)
					return res
				}
				rendered = append(rendered, renderedPage{problem: problem, pdfPage: i, page: pg,
					textLossRuns: probeTextLoss(ctx, doc, i, raster)})
			}
		} else {
			// Render + map pages BEFORE touching rows, so a render failure leaves the
			// old submission fully intact.
			problems, err := s.Store.Q.ListProblems(ctx, assessmentID)
			if err != nil || len(problems) == 0 {
				_ = doc.Close()
				res.Reason = "assessment has no problems to map onto"
				return res
			}
			// The positional mapper can represent a short/incomplete paper (the
			// reconciliation view flags its missing problem cells), but it cannot
			// represent pages beyond the configured problem list. The former
			// min(pageCount, len(problems)) loop silently discarded those trailing
			// pages; reject them before rendering, blob writes, or DB writes.
			if pageCount > len(problems) {
				_ = doc.Close()
				res.Reason = wholeAssessmentExtraPageReason(pageCount, len(problems))
				return res
			}
			for i := 0; i < pageCount; i++ {
				raster, pg, err := doc.RenderPageImage(ctx, i, s.Opts)
				if err != nil {
					_ = doc.Close()
					s.log().Warn("render failed", "assessment_id", assessmentID, "pdf_page", i, "err", err)
					res.Reason = fmt.Sprintf("rendering page %d failed", i+1)
					return res
				}
				rendered = append(rendered, renderedPage{problem: problems[i], pdfPage: i, page: pg,
					textLossRuns: probeTextLoss(ctx, doc, i, raster)})
			}
		}
		// Rendering done — return the pooled worker before any DB work.
		_ = doc.Close()
	}
	res.Pages = pageCount

	// Re-upload guards (D1), scoped to the target problem when set (D22). These are
	// keyed on the SCOPE (assessment, or assessment+problem), NOT on whether a prior
	// submission exists: a live whole-assessment submission can already cover a
	// problem positionally, so a per-problem upload must honor that problem's
	// published/graded state even when no per-problem submission row exists yet, and
	// vice versa (spec §8; whole and per-problem submissions coexist).
	var published, records int64
	if scoped {
		published, err = s.Store.Q.CountPublishedForStudentProblem(ctx, db.CountPublishedForStudentProblemParams{
			AssessmentID: assessmentID, StudentID: student.ID, ProblemID: targetProblemID,
		})
	} else {
		published, err = s.Store.Q.CountPublishedForStudentAssessment(ctx, db.CountPublishedForStudentAssessmentParams{AssessmentID: assessmentID, StudentID: student.ID})
	}
	if err != nil {
		res.Reason = "guard check failed"
		return res
	}
	if published > 0 {
		res.Reason = "student has published answers; re-upload is not allowed"
		return res
	}
	if scoped {
		records, err = s.Store.Q.CountRecordsForStudentProblem(ctx, db.CountRecordsForStudentProblemParams{
			AssessmentID: assessmentID, StudentID: student.ID, ProblemID: targetProblemID,
		})
	} else {
		records, err = s.Store.Q.CountRecordsForStudentAssessment(ctx, db.CountRecordsForStudentAssessmentParams{AssessmentID: assessmentID, StudentID: student.ID})
	}
	if err != nil {
		res.Reason = "guard check failed"
		return res
	}
	graded := records > 0
	if graded && !force {
		res.Reason = "student already has grading records; re-upload requires force"
		return res
	}

	// Collect the live submissions this upload will supersede. The set is scoped:
	// a per-problem upload supersedes only a live per-problem submission for the
	// same problem (the whole-assessment submission, if any, stays live and keeps
	// owning the other problems' pages); a whole-assessment upload supersedes ALL
	// live submissions for the student — whole AND per-problem (spec §8).
	var toSupersede []db.Submission
	if scoped {
		prev, perr := s.Store.Q.GetActiveSubmissionForProblem(ctx, db.GetActiveSubmissionForProblemParams{
			AssessmentID: assessmentID, StudentID: student.ID, ProblemID: pgtype.Int8{Int64: targetProblemID, Valid: true},
		})
		if perr == nil {
			toSupersede = append(toSupersede, prev)
		} else if !errors.Is(perr, pgx.ErrNoRows) {
			res.Reason = "submission lookup failed"
			return res
		}
	} else {
		live, lerr := s.Store.Q.ListLiveSubmissionsForStudent(ctx, db.ListLiveSubmissionsForStudentParams{
			AssessmentID: assessmentID, StudentID: student.ID,
		})
		if lerr != nil {
			res.Reason = "submission lookup failed"
			return res
		}
		toSupersede = live
	}

	// Store the untouched source bytes (content-addressed per student).
	sourceKey := fmt.Sprintf("assessments/%d/students/%d/%s.%s", assessmentID, student.ID, sha[:16], sourceExt)
	if _, _, err := s.Blobs.Put(ctx, sourceKey, bytes.NewReader(data)); err != nil {
		res.Reason = "storing source failed"
		return res
	}

	// F15: store ALL page images BEFORE the transaction — no Blobs.Put inside an open
	// tx, so the tx holds a pool connection only for the (short) row work, never
	// across a per-page JPEG fsync. The page key is content-addressed on the image
	// SHA (assessments/{aid}/pages/{sha16}.jpg) instead of embedding the answer id,
	// so it needs no answer id (which only exists mid-tx) and is idempotent: identical
	// bytes → identical key → a harmless re-Put. Old rows stored under the previous
	// answers/{aid}/pages/... keys are absolute refs in answer_pages.image_ref and stay
	// readable — this only changes NEW writes.
	//
	// Failure semantics preserved: a page-store failure returns here, BEFORE any row
	// is touched, so the previous submission stays fully intact — exactly the property
	// the pre-F15 code got from doing the Puts as the first statements inside the tx
	// (a Put error rolled the tx back before any supersede/insert). An orphaned page
	// blob from a subsequent tx failure is harmless (content-addressed, unreferenced;
	// page blobs are never GC'd here anyway).
	pageKeys := make([]string, len(rendered))
	for i, rp := range rendered {
		pageKeys[i] = fmt.Sprintf("assessments/%d/pages/%s.jpg", assessmentID, rp.page.SHA256[:16])
		if _, _, err := s.Blobs.Put(ctx, pageKeys[i], bytes.NewReader(rp.page.JPEG)); err != nil {
			res.Reason = "storing page image failed"
			return res
		}
	}

	// Single transaction: materialize answers, supersede old submissions, write rows.
	// NO blob I/O happens inside this tx (F15) — every Put is above.
	err = s.Store.WithTx(ctx, func(q *db.Queries) error {
		if err := q.MaterializeAnswers(ctx, assessmentID); err != nil {
			return err
		}
		// The active-unique partial indexes allow only one live submission per scope,
		// and the new row's id doesn't exist yet — point each old row at itself to
		// free the slot, then fix the pointers to the new submission below.
		for _, prev := range toSupersede {
			if err := q.SupersedeSubmission(ctx, db.SupersedeSubmissionParams{ID: prev.ID, SupersededBy: pgtype.Int8{Int64: prev.ID, Valid: true}}); err != nil {
				return err
			}
		}
		sub, err := q.CreateSubmission(ctx, db.CreateSubmissionParams{
			AssessmentID: assessmentID, StudentID: student.ID,
			OriginalFilename: filename, SourceRef: sourceKey, SourceSha256: sha,
			SourceKind: sourceKind,
			PageCount:  int32(pageCount),
			UploadedBy: pgtype.Int8{Int64: uploadedBy, Valid: uploadedBy != 0},
			ProblemID:  pgtype.Int8{Int64: targetProblemID, Valid: scoped},
		})
		if err != nil {
			return err
		}
		for _, prev := range toSupersede {
			if err := q.SupersedeSubmission(ctx, db.SupersedeSubmissionParams{ID: prev.ID, SupersededBy: pgtype.Int8{Int64: sub.ID, Valid: true}}); err != nil {
				return err
			}
			if err := q.DeletePagesBySubmission(ctx, prev.ID); err != nil {
				return err
			}
		}
		if scoped {
			// A live whole-assessment submission may still own this problem's answer
			// pages positionally (it is NOT in toSupersede). Clear the whole answer so
			// the per-problem promotion supersedes only that problem's prior pages and
			// the new pages don't collide on UNIQUE(answer_id, page_index) (spec §8).
			ans, err := q.EnsureAnswer(ctx, db.EnsureAnswerParams{AssessmentID: assessmentID, StudentID: student.ID, ProblemID: targetProblemID})
			if err != nil {
				return err
			}
			if err := q.DeletePagesForAnswer(ctx, ans.ID); err != nil {
				return err
			}
		}
		if graded {
			if scoped {
				if err := q.AddFlagForStudentProblem(ctx, db.AddFlagForStudentProblemParams{
					AssessmentID: assessmentID, StudentID: student.ID, ProblemID: targetProblemID, Flag: FlagImageSuperseded,
				}); err != nil {
					return err
				}
			} else {
				if err := q.AddFlagForStudentAssessment(ctx, db.AddFlagForStudentAssessmentParams{
					AssessmentID: assessmentID, StudentID: student.ID, Flag: FlagImageSuperseded,
				}); err != nil {
					return err
				}
			}
		}
		for pageIdx, rp := range rendered {
			ans, err := q.EnsureAnswer(ctx, db.EnsureAnswerParams{AssessmentID: assessmentID, StudentID: student.ID, ProblemID: rp.problem.ID})
			if err != nil {
				return err
			}
			answerPageIndex := 0
			if scoped {
				// Per-problem multi-page files append onto the SAME answer,
				// ordered page_index 0..n-1 (spec §8).
				answerPageIndex = pageIdx
			}
			// The image was already stored above (F15); this loop does only row work.
			if _, err := q.CreateAnswerPage(ctx, db.CreateAnswerPageParams{
				AnswerID: ans.ID, PageIndex: int32(answerPageIndex),
				SubmissionID: sub.ID, PdfPageIndex: int32(rp.pdfPage),
				ImageRef: pageKeys[pageIdx], ImageSha256: rp.page.SHA256,
				ImageWidth: int32(rp.page.Width), ImageHeight: int32(rp.page.Height),
				TextLossRuns: int32(rp.textLossRuns),
			}); err != nil {
				return err
			}
		}
		if in.LinkInTx != nil {
			if err := in.LinkInTx(q, sub.ID); err != nil {
				return err
			}
		}
		res.SubmissionID = sub.ID
		return nil
	})
	if err != nil {
		s.log().Warn("ingest tx failed", "assessment_id", assessmentID, "err", err)
		res.Reason = "ingest transaction failed"
		res.SubmissionID = 0
		return res
	}
	res.Status = "ingested"
	res.MappedPages = len(rendered)
	res.Reason = ""
	return res
}

func wholeAssessmentExtraPageReason(pageCount, problemCount int) string {
	pageWord := "pages"
	if pageCount == 1 {
		pageWord = "page"
	}
	problemWord := "problems"
	if problemCount == 1 {
		problemWord = "problem"
	}
	return fmt.Sprintf(
		"PDF has %d %s but this assessment has %d %s; correct the PDF so it has exactly one page per problem, then re-upload",
		pageCount, pageWord, problemCount, problemWord,
	)
}

type renderedPage struct {
	problem db.Problem
	pdfPage int
	page    render.Page
	// textLossRuns > 0: the PDF's text layer contained runs that rendered as
	// nothing (render.ProbeTextLoss — pdfium drops non-embedded CID/CJK
	// glyphs), i.e. content the AI would grade without ever seeing.
	textLossRuns int
}

// probeTextLoss wraps render.ProbeTextLoss for the ingest loops: a probe error
// never fails an upload (the render itself succeeded), it just forfeits the
// warning for that page.
func probeTextLoss(ctx context.Context, doc render.Document, pageIndex int, raster image.Image) int {
	rep, err := render.ProbeTextLoss(ctx, doc, pageIndex, raster)
	if err != nil {
		return 0
	}
	return rep.SuspectRuns
}

// RetractSubmission unassigns a live submission (§7 unassignment tombstone, §8
// per-problem scoping): sets retracted_at and deletes its pages. It is blocked
// without force if the student's scope (the whole assessment, or the submission's
// problem when problem_id is set) has grading records, and blocked ALWAYS if
// published — the same guards Ingest applies before a re-upload, since retraction
// is symmetric with supersession (D1). When records exist and force=true, the
// affected answers are flagged image_superseded, scoped the same way.
func (s *Service) RetractSubmission(ctx context.Context, submissionID int64, actor int64, force bool) error {
	sub, err := s.Store.Q.GetSubmission(ctx, submissionID)
	if err != nil {
		return fmt.Errorf("no such submission")
	}
	if sub.SupersededBy.Valid {
		return errors.New("submission is not active (already superseded)")
	}
	if sub.RetractedAt.Valid {
		return ErrAlreadyRetracted
	}
	scoped := sub.ProblemID.Valid

	var published, records int64
	if scoped {
		published, err = s.Store.Q.CountPublishedForStudentProblem(ctx, db.CountPublishedForStudentProblemParams{
			AssessmentID: sub.AssessmentID, StudentID: sub.StudentID, ProblemID: sub.ProblemID.Int64,
		})
	} else {
		published, err = s.Store.Q.CountPublishedForStudentAssessment(ctx, db.CountPublishedForStudentAssessmentParams{
			AssessmentID: sub.AssessmentID, StudentID: sub.StudentID,
		})
	}
	if err != nil {
		return fmt.Errorf("guard check failed: %w", err)
	}
	if published > 0 {
		return ErrRetractionBlocked
	}

	if scoped {
		records, err = s.Store.Q.CountRecordsForStudentProblem(ctx, db.CountRecordsForStudentProblemParams{
			AssessmentID: sub.AssessmentID, StudentID: sub.StudentID, ProblemID: sub.ProblemID.Int64,
		})
	} else {
		records, err = s.Store.Q.CountRecordsForStudentAssessment(ctx, db.CountRecordsForStudentAssessmentParams{
			AssessmentID: sub.AssessmentID, StudentID: sub.StudentID,
		})
	}
	if err != nil {
		return fmt.Errorf("guard check failed: %w", err)
	}
	graded := records > 0
	if graded && !force {
		return ErrRetractionNeedsForce
	}

	return s.Store.WithTx(ctx, func(q *db.Queries) error {
		if err := q.RetractSubmission(ctx, submissionID); err != nil {
			return err
		}
		if err := q.DeletePagesBySubmission(ctx, submissionID); err != nil {
			return err
		}
		if graded {
			if scoped {
				return q.AddFlagForStudentProblem(ctx, db.AddFlagForStudentProblemParams{
					AssessmentID: sub.AssessmentID, StudentID: sub.StudentID, ProblemID: sub.ProblemID.Int64, Flag: FlagImageSuperseded,
				})
			}
			return q.AddFlagForStudentAssessment(ctx, db.AddFlagForStudentAssessmentParams{
				AssessmentID: sub.AssessmentID, StudentID: sub.StudentID, Flag: FlagImageSuperseded,
			})
		}
		return nil
	})
}

func (s *Service) quarantineInput(ctx context.Context, assessmentID int64, in IngestInput, sha, reason string, uploadedBy int64) FileResult {
	// Assignment retries an already-durable quarantine entry. If decoding reveals
	// a more specific unreadable reason, update that row in place instead of
	// storing the same bytes and appending a duplicate entry.
	if in.ExistingQuarantineID != 0 {
		n, err := s.Store.Q.ReclassifyQuarantine(ctx, db.ReclassifyQuarantineParams{
			ID: in.ExistingQuarantineID, Reason: reason,
		})
		if err != nil || n != 1 {
			return FileResult{Filename: in.Filename, Status: "rejected", Reason: "quarantine record failed"}
		}
		return FileResult{Filename: in.Filename, Status: "quarantined", Reason: reason}
	}

	ext := "pdf"
	if in.Kind == "image" {
		ext = sniffImageExt(in.Data)
	}
	key := fmt.Sprintf("assessments/%d/quarantine/%s.%s", assessmentID, sha[:16], ext)
	if _, _, err := s.Blobs.Put(ctx, key, bytes.NewReader(in.Data)); err != nil {
		return FileResult{Filename: in.Filename, Status: "rejected", Reason: "storing quarantined file failed"}
	}
	if _, err := s.Store.Q.CreateQuarantine(ctx, db.CreateQuarantineParams{
		AssessmentID: assessmentID, OriginalFilename: in.Filename,
		PdfRef: key, PdfSha256: sha, Reason: reason,
		UploadedBy: pgtype.Int8{Int64: uploadedBy, Valid: uploadedBy != 0},
	}); err != nil {
		return FileResult{Filename: in.Filename, Status: "rejected", Reason: "quarantine record failed"}
	}
	return FileResult{Filename: in.Filename, Status: "quarantined", Reason: reason}
}

// AssignQuarantine resolves a quarantined upload to a student and runs the normal
// ingest pipeline on its stored bytes.
func (s *Service) AssignQuarantine(ctx context.Context, quarantineID int64, studentExternalID string, actor int64, force bool) (FileResult, error) {
	qr, err := s.Store.Q.GetQuarantine(ctx, quarantineID)
	if err != nil {
		return FileResult{}, ErrQuarantineNotFound
	}
	if qr.ResolvedAt.Valid {
		return FileResult{}, ErrQuarantineAlreadyResolved
	}
	// Assignment repairs roster matching only. Re-running bytes already known to
	// be unreadable cannot repair them; it used to create a second quarantine row
	// and leave the original open. Reject before any blob read or write instead.
	if qr.Reason != "unknown_student" && qr.Reason != "duplicate_in_batch" {
		return FileResult{}, ErrQuarantineNotAssignable
	}
	// Same exact-then-normalized lookup as filename ingest; an ambiguous id is
	// surfaced verbatim (the handler returns it as a 400) rather than folded
	// into "no such student".
	student, err := ResolveStudentByExternalID(ctx, s.Store.Q, studentExternalID)
	if errors.Is(err, ErrAmbiguousStudentID) {
		return FileResult{}, ErrAmbiguousStudentID
	}
	if err != nil {
		return FileResult{}, fmt.Errorf("no such student on the roster")
	}
	rc, err := s.Blobs.Get(ctx, qr.PdfRef)
	if err != nil {
		return FileResult{}, fmt.Errorf("quarantined file missing from storage")
	}
	defer rc.Close()
	pdf, err := io.ReadAll(io.LimitReader(rc, MaxPDFBytes+1))
	if err != nil {
		return FileResult{}, fmt.Errorf("reading quarantined file failed")
	}

	kind := "pdf"
	ext := ".pdf"
	switch strings.ToLower(filepath.Ext(qr.OriginalFilename)) {
	case ".png", ".jpg", ".jpeg":
		kind = "image"
		ext = strings.ToLower(filepath.Ext(qr.OriginalFilename))
	}
	res := s.Ingest(ctx, qr.AssessmentID, IngestInput{
		Filename: student.StudentID + ext, Data: pdf, Kind: kind,
		ExistingQuarantineID: quarantineID,
	}, actor, force)
	if res.Status == "ingested" {
		if err := s.Store.Q.ResolveQuarantine(ctx, db.ResolveQuarantineParams{
			ID: quarantineID, ResolvedStudentID: pgtype.Int8{Int64: student.ID, Valid: true},
		}); err != nil {
			return res, err
		}
	}
	return res, nil
}

// DismissQuarantine removes an intentionally ignored entry from the open review
// queue without deleting its row or blob, preserving the audit/reference trail.
func (s *Service) DismissQuarantine(ctx context.Context, quarantineID int64) error {
	qr, err := s.Store.Q.GetQuarantine(ctx, quarantineID)
	if err != nil {
		return ErrQuarantineNotFound
	}
	if qr.ResolvedAt.Valid {
		return ErrQuarantineAlreadyResolved
	}
	n, err := s.Store.Q.DismissQuarantine(ctx, quarantineID)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrQuarantineAlreadyResolved
	}
	return nil
}

// ApplyMasks derives the masked artifact for every page of the assessment that is
// not already up to date, resetting each re-masked page's review to pending (D10).
//
// Deprecated: in favor of the queue path (PlanMasks + one MaskPage job per page,
// D27/F2) — at 200×9 scale re-masking ~1800 pages synchronously in one HTTP request
// cannot complete. This thin sequential wrapper is retained so the existing handler
// and tests keep working until the River worker wave wires MaskPage; it plans the
// work-list and runs each page's MaskPage job body in order, returning the number
// of pages actually (re-)masked (already-up-to-date pages are skipped and not
// counted). It errors if no regions are defined (the handler keeps its 400).
func (s *Service) ApplyMasks(ctx context.Context, assessmentID int64) (int, error) {
	pageIDs, _, err := s.PlanMasks(ctx, assessmentID)
	if err != nil {
		return 0, err
	}
	masked := 0
	for _, id := range pageIDs {
		// This synchronous wrapper has no River retry loop, so a failure is not a
		// "final attempt" that should be swallowed into mask_error — surface it (the
		// existing contract) rather than silently counting the page as masked.
		if err := s.MaskPage(ctx, id, false); err != nil {
			return masked, err
		}
		masked++
	}
	return masked, nil
}
