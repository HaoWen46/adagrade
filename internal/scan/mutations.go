// This file owns the manual, human-driven page mutations (design spec 2026-07-04
// §6): assigning/unassigning a page to a (student, problem) cell by hand,
// discarding/undiscarding a page, retrying a wedged page's next stage, and
// resolving a parked-page conflict raised by identify's D65 never-overwrite
// rule. Manual assignment NEVER overwrites an occupied cell (D65) — it always
// reports *ErrCellOccupied and leaves both pages untouched; only
// ResolveConflict, given an explicit human decision, may vacate a cell.
// Promoted pages (submission_id set to a live submission) block every mutation
// except ResolveConflict's replace path, which is the one place allowed to
// touch promoted state (with an explicit retract).
package scan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// ErrCellOccupied is the manual-assign 409: the target cell already holds a live
// page or submission. IncumbentPageID==0 means the incumbent is a submission.
type ErrCellOccupied struct {
	IncumbentPageID       int64
	IncumbentSubmissionID int64
	Duplicate             bool // content-identical to the incumbent page
}

func (e *ErrCellOccupied) Error() string {
	if e.IncumbentPageID != 0 {
		return fmt.Sprintf("scan: cell occupied by page %d", e.IncumbentPageID)
	}
	return fmt.Sprintf("scan: cell occupied by submission %d", e.IncumbentSubmissionID)
}

// ErrPagePromoted blocks mutating a page whose submission already exists.
type ErrPagePromoted struct{ PageID int64 }

func (e *ErrPagePromoted) Error() string {
	return fmt.Sprintf("scan: page %d is already promoted", e.PageID)
}

// ErrConflictStale is ResolveConflict's replace-path 409: the incumbent no
// longer occupies the contested cell recorded at park time, and the cell is
// not recoverable (a third page/submission took it, or a pre-0031 park row
// recorded no cell). The fight this park recorded is over — the TA
// re-adjudicates (or just assigns the parked page manually). Fixed
// vocabulary, no PII.
type ErrConflictStale struct{ PageID int64 }

func (e *ErrConflictStale) Error() string {
	return fmt.Sprintf("scan: page %d: conflict is stale — the incumbent moved; assign the page manually", e.PageID)
}

// ErrInvalidInput marks operator-input precondition failures (bad student/
// problem, non-parked page) so the HTTP layer can map them to 400 instead of
// a generic 500. Wrapped messages are fixed vocabulary — no PII.
var ErrInvalidInput = errors.New("invalid input")

// isRetractGuardFailure reports whether err is one of RetractSubmission's two
// fixed-vocabulary operator-fault guards (graded-without-force, published-
// block) — the ONLY RetractSubmission failures ResolveConflict's replace path
// maps to ErrInvalidInput (400). Any other failure (e.g. "guard check
// failed: %w" wrapping a live pgx/driver error from the guard-check queries)
// is a server-side fault, not something the operator did wrong, and must
// propagate unwrapped so the HTTP layer's static 500 fallback handles it
// instead of falsely reporting it as caller error.
func isRetractGuardFailure(err error) bool {
	return errors.Is(err, ingest.ErrRetractionNeedsForce) || errors.Is(err, ingest.ErrRetractionBlocked)
}

// livePromotedQ reports whether the page's linked submission is still live.
// A submission_id pointing at a retracted/superseded submission is a stale
// link (e.g. a crash between ResolveConflict's retract and its follow-up tx)
// and must NOT count as promoted — otherwise the page is stuck behind
// ErrPagePromoted with no recovery path.
func livePromotedQ(ctx context.Context, q *db.Queries, page db.ScanPage) (bool, error) {
	if !page.SubmissionID.Valid {
		return false, nil
	}
	sub, err := q.GetSubmission(ctx, page.SubmissionID.Int64)
	if err != nil {
		return false, err
	}
	return !sub.RetractedAt.Valid && !sub.SupersededBy.Valid, nil
}

// healStaleLinkQ clears a page's submission_id when it points at a
// retracted/superseded submission. Called at the top of a mutating
// transaction once livePromotedQ has already confirmed the link is stale, so
// touching a stuck page (any write mutation) heals it going forward.
func healStaleLinkQ(ctx context.Context, q *db.Queries, page db.ScanPage) error {
	if !page.SubmissionID.Valid {
		return nil
	}
	return q.SetScanPageSubmission(ctx, db.SetScanPageSubmissionParams{
		ID: page.ID, SubmissionID: pgtype.Int8{},
	})
}

// lockUnpromotedPageQ re-reads the page FOR UPDATE inside a mutating
// transaction and re-applies the promoted guard on the locked row. The
// caller's pre-tx snapshot check races an in-flight promote job's link (the
// UI keeps assignment mutations enabled during promotion); the promote's
// conditional link (LinkScanPagePromotion) and this re-check are the two
// halves of that handshake — whichever transaction commits second sees the
// other's write, so a lost race surfaces as a clean *ErrPagePromoted instead
// of silently mutating a just-promoted page. A stale link (retracted/
// superseded submission) is healed exactly like the pre-tx path would.
func lockUnpromotedPageQ(ctx context.Context, q *db.Queries, pageID int64) (db.ScanPage, error) {
	cur, err := q.GetScanPageForUpdate(ctx, pageID)
	if err != nil {
		return db.ScanPage{}, err
	}
	if live, err := livePromotedQ(ctx, q, cur); err != nil {
		return db.ScanPage{}, err
	} else if live {
		return db.ScanPage{}, &ErrPagePromoted{PageID: pageID}
	}
	if err := healStaleLinkQ(ctx, q, cur); err != nil {
		return db.ScanPage{}, err
	}
	return cur, nil
}

// AssignPage manually assigns a page to a (student, problem) cell.
// studentID/problemID are DB ids. Occupied cells return *ErrCellOccupied —
// the UI resolves via ResolveConflict; manual assignment NEVER overwrites.
func (s *Service) AssignPage(ctx context.Context, pageID, studentID, problemID, actor int64) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil {
		return fmt.Errorf("scan: assign: load page: %w", err)
	}
	if live, err := livePromotedQ(ctx, s.Store.Q, page); err != nil {
		return err
	} else if live {
		return &ErrPagePromoted{PageID: pageID}
	}
	student, err := s.Store.Q.GetStudent(ctx, studentID)
	if err != nil {
		return fmt.Errorf("scan: no such student: %w", ErrInvalidInput)
	}
	if student.WithdrawnAt.Valid {
		return fmt.Errorf("scan: student is withdrawn: %w", ErrInvalidInput)
	}
	problem, err := s.Store.Q.GetProblem(ctx, problemID)
	if err != nil || problem.AssessmentID != page.AssessmentID {
		return fmt.Errorf("scan: problem does not belong to this assessment: %w", ErrInvalidInput)
	}

	err = s.Store.WithTx(ctx, func(q *db.Queries) error {
		if _, err := lockUnpromotedPageQ(ctx, q, pageID); err != nil {
			return err
		}
		incPage, incSub, err := s.cellIncumbentQ(ctx, q, page.AssessmentID, studentID, problemID, pageID)
		if err != nil {
			return err
		}
		if incPage != nil {
			return &ErrCellOccupied{
				IncumbentPageID: incPage.ID,
				Duplicate: incPage.ImageSha256.Valid && page.ImageSha256.Valid &&
					incPage.ImageSha256.String == page.ImageSha256.String,
			}
		}
		if incSub != nil {
			return &ErrCellOccupied{IncumbentSubmissionID: incSub.ID}
		}
		return q.AssignScanPage(ctx, db.AssignScanPageParams{
			ID: pageID, AssignedStudentID: int8OrNull(studentID),
			AssignedProblemID: int8OrNull(problemID), AssignedBy: int8OrNull(actor),
		})
	})
	if err != nil {
		return err
	}

	_ = s.Store.InsertAudit(ctx, actor, "scan.page.assign", "scan_page", itoa(pageID), map[string]any{
		"student_id": studentID, "problem_id": problemID,
	})
	return nil
}

// UnassignPage clears a page's manual/auto assignment, returning it to an
// orphan. Blocked on a promoted page (ErrPagePromoted): retract the
// submission first via ResolveConflict/ingest, never silently through here.
func (s *Service) UnassignPage(ctx context.Context, pageID, actor int64) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil {
		return fmt.Errorf("scan: unassign: load page: %w", err)
	}
	if live, err := livePromotedQ(ctx, s.Store.Q, page); err != nil {
		return err
	} else if live {
		return &ErrPagePromoted{PageID: pageID}
	}
	err = s.Store.WithTx(ctx, func(q *db.Queries) error {
		if _, err := lockUnpromotedPageQ(ctx, q, pageID); err != nil {
			return err
		}
		return q.UnassignScanPage(ctx, pageID)
	})
	if err != nil {
		return err
	}
	_ = s.Store.InsertAudit(ctx, actor, "scan.page.unassign", "scan_page", itoa(pageID), map[string]any{})
	return nil
}

// DiscardPage marks a page as discarded (blank pages, misfeeds, duplicates a
// human doesn't want kept). Blocked on a promoted page.
func (s *Service) DiscardPage(ctx context.Context, pageID int64, reason string, actor int64) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil {
		return fmt.Errorf("scan: discard: load page: %w", err)
	}
	if live, err := livePromotedQ(ctx, s.Store.Q, page); err != nil {
		return err
	} else if live {
		return &ErrPagePromoted{PageID: pageID}
	}
	err = s.Store.WithTx(ctx, func(q *db.Queries) error {
		if err := healStaleLinkQ(ctx, q, page); err != nil {
			return err
		}
		return q.DiscardScanPage(ctx, db.DiscardScanPageParams{
			ID: pageID, DiscardReason: textOrNull(reason),
		})
	})
	if err != nil {
		return err
	}
	_ = s.Store.InsertAudit(ctx, actor, "scan.page.discard", "scan_page", itoa(pageID), map[string]any{})
	return nil
}

// UndiscardPage reverses DiscardPage. Blocked on a promoted page (a promoted
// page can never have been discarded in the first place, but the guard keeps
// every mutation's short-circuit uniform).
func (s *Service) UndiscardPage(ctx context.Context, pageID, actor int64) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil {
		return fmt.Errorf("scan: undiscard: load page: %w", err)
	}
	if live, err := livePromotedQ(ctx, s.Store.Q, page); err != nil {
		return err
	} else if live {
		return &ErrPagePromoted{PageID: pageID}
	}
	err = s.Store.WithTx(ctx, func(q *db.Queries) error {
		if err := healStaleLinkQ(ctx, q, page); err != nil {
			return err
		}
		return q.UndiscardScanPage(ctx, pageID)
	})
	if err != nil {
		return err
	}
	_ = s.Store.InsertAudit(ctx, actor, "scan.page.undiscard", "scan_page", itoa(pageID), map[string]any{})
	return nil
}

// RetryPage clears the error and re-enqueues the right stage: identify when
// crops exist, else a render chunk for just this page.
func (s *Service) RetryPage(ctx context.Context, pageID, actor int64) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil {
		return fmt.Errorf("scan: retry: load page: %w", err)
	}
	if live, err := livePromotedQ(ctx, s.Store.Q, page); err != nil {
		return err
	} else if live {
		return &ErrPagePromoted{PageID: pageID}
	}
	hasCrops := page.StudentIDCropRef.Valid && page.NameCropRef.Valid && page.ProblemCropRef.Valid

	err = s.Store.WithTxPgx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := healStaleLinkQ(ctx, q, page); err != nil {
			return err
		}
		if err := q.ClearScanPageError(ctx, pageID); err != nil {
			return err
		}
		if hasCrops {
			if s.EnqueueIdentifyPages != nil {
				return s.EnqueueIdentifyPages(ctx, tx, []int64{pageID})
			}
			return nil
		}
		if s.EnqueueRenderPages != nil {
			return s.EnqueueRenderPages(ctx, tx, page.SourceID, []int64{pageID})
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = s.Store.InsertAudit(ctx, actor, "scan.page.retry", "scan_page", itoa(pageID), map[string]any{})
	return nil
}

// RetryErroredPages is RetryPage's batch-level bulk form — the recovery path
// for a batch whose OCR provider terminal-errored every page (the per-page
// retry re-runs with the batch's stored provider and can only re-fail). When
// provider is non-empty it is validated against the live provider source (the
// same lookup identify itself performs, so exists+enabled) and the batch's
// ocr_provider/ocr_model are repointed first; then every errored page of the
// batch has its error cleared and the right stage re-enqueued (identify when
// crops exist, else a render chunk per source) in one transaction. Pages whose
// submission is still live are skipped (promoted state is immutable outside
// ResolveConflict). Returns how many pages were retried.
func (s *Service) RetryErroredPages(ctx context.Context, batchID int64, provider, model string, actor int64) (int, error) {
	batch, err := s.Store.Q.GetScanBatch(ctx, batchID)
	if err != nil {
		return 0, fmt.Errorf("scan: retry errored: load batch: %w", err)
	}
	if provider != "" {
		if _, _, err := s.Providers.Provider(ctx, provider); err != nil {
			var unavailable *llm.ProviderUnavailableError
			if errors.As(err, &unavailable) {
				return 0, fmt.Errorf("scan: retry errored: %w: %w", ErrInvalidInput, err)
			}
			return 0, fmt.Errorf("scan: retry errored: provider lookup: %w", err)
		}
		if err := s.Store.Q.SetScanBatchOCR(ctx, db.SetScanBatchOCRParams{
			ID: batch.ID, OcrProvider: textOrNull(provider), OcrModel: textOrNull(model),
		}); err != nil {
			return 0, fmt.Errorf("scan: retry errored: switch provider: %w", err)
		}
	}
	retry, err := s.erroredMutablePages(ctx, batchID)
	if err != nil {
		return 0, err
	}
	if len(retry) == 0 {
		return 0, nil
	}
	err = s.Store.WithTxPgx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var identifyIDs []int64
		renderBySource := map[int64][]int64{}
		var sourceOrder []int64
		for _, page := range retry {
			if err := healStaleLinkQ(ctx, q, page); err != nil {
				return err
			}
			if err := q.ClearScanPageError(ctx, page.ID); err != nil {
				return err
			}
			if page.StudentIDCropRef.Valid && page.NameCropRef.Valid && page.ProblemCropRef.Valid {
				identifyIDs = append(identifyIDs, page.ID)
				continue
			}
			if _, seen := renderBySource[page.SourceID]; !seen {
				sourceOrder = append(sourceOrder, page.SourceID)
			}
			renderBySource[page.SourceID] = append(renderBySource[page.SourceID], page.ID)
		}
		if len(identifyIDs) > 0 && s.EnqueueIdentifyPages != nil {
			if err := s.EnqueueIdentifyPages(ctx, tx, identifyIDs); err != nil {
				return err
			}
		}
		if s.EnqueueRenderPages != nil {
			for _, srcID := range sourceOrder {
				if err := s.EnqueueRenderPages(ctx, tx, srcID, renderBySource[srcID]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	detail := map[string]any{"retried": len(retry)}
	if provider != "" {
		detail["ocr_provider"] = provider
	}
	_ = s.Store.InsertAudit(ctx, actor, "scan.batch.retry_errored", "scan_batch", itoa(batchID), detail)
	return len(retry), nil
}

// DiscardErroredPages bulk-discards every errored page of a batch (same
// semantics as DiscardPage with a fixed "errored" reason — bulk write-off of
// a run of misfeeds/failures a human doesn't want kept). Pages whose
// submission is still live are skipped. Returns how many were discarded.
func (s *Service) DiscardErroredPages(ctx context.Context, batchID, actor int64) (int, error) {
	if _, err := s.Store.Q.GetScanBatch(ctx, batchID); err != nil {
		return 0, fmt.Errorf("scan: discard errored: load batch: %w", err)
	}
	discard, err := s.erroredMutablePages(ctx, batchID)
	if err != nil {
		return 0, err
	}
	if len(discard) == 0 {
		return 0, nil
	}
	err = s.Store.WithTx(ctx, func(q *db.Queries) error {
		for _, page := range discard {
			if err := healStaleLinkQ(ctx, q, page); err != nil {
				return err
			}
			if err := q.DiscardScanPage(ctx, db.DiscardScanPageParams{
				ID: page.ID, DiscardReason: textOrNull("errored"),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	_ = s.Store.InsertAudit(ctx, actor, "scan.batch.discard_errored", "scan_batch", itoa(batchID), map[string]any{
		"discarded": len(discard),
	})
	return len(discard), nil
}

// erroredMutablePages lists a batch's errored pages minus any whose linked
// submission is still live — the shared skip guard of both bulk recovery
// actions above (mirrors the per-page mutations' ErrPagePromoted check).
func (s *Service) erroredMutablePages(ctx context.Context, batchID int64) ([]db.ScanPage, error) {
	pages, err := s.Store.Q.ListErroredPagesForBatch(ctx, batchID)
	if err != nil {
		return nil, fmt.Errorf("scan: list errored pages: %w", err)
	}
	out := make([]db.ScanPage, 0, len(pages))
	for _, page := range pages {
		live, err := livePromotedQ(ctx, s.Store.Q, page)
		if err != nil {
			return nil, err
		}
		if live {
			continue
		}
		out = append(out, page)
	}
	return out, nil
}

// ResolveConflict resolves a parked page. action "keep" discards the parked
// page; action "replace" takes the cell: an unpromoted incumbent page is
// unassigned (back to orphan), a promoted one has its submission retracted
// (force applies ingest's graded guard) before the parked page is assigned;
// force also marks the page force_promote so the next finalize can re-ingest
// over grading records.
func (s *Service) ResolveConflict(ctx context.Context, pageID int64, action string, force bool, actor int64) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil {
		return fmt.Errorf("scan: resolve conflict: load page: %w", err)
	}
	if live, err := livePromotedQ(ctx, s.Store.Q, page); err != nil {
		return err
	} else if live {
		return &ErrPagePromoted{PageID: pageID}
	}
	if !page.ParkedReason.Valid {
		return fmt.Errorf("scan: page is not parked: %w", ErrInvalidInput)
	}

	switch action {
	case "keep":
		if err := s.Store.Q.DiscardScanPage(ctx, db.DiscardScanPageParams{
			ID: pageID, DiscardReason: textOrNull("conflict: kept incumbent"),
		}); err != nil {
			return err
		}
		_ = s.Store.InsertAudit(ctx, actor, "scan.page.resolve_conflict", "scan_page", itoa(pageID), map[string]any{
			"action": "keep",
		})
		return nil

	case "replace":
		if !page.ParkedAgainst.Valid {
			// This is the "conflict with a live submission, no backing page"
			// case (D65): there is nothing to unassign/retract via Replace —
			// the operator must retract the submission directly from the
			// Submissions tab instead. That's an operator-input precondition
			// failure, not a server fault, so it maps to 400 like every other
			// ErrInvalidInput case.
			return fmt.Errorf("scan: parked page has no incumbent to replace: %w", ErrInvalidInput)
		}
		incPage, err := s.Store.Q.GetScanPage(ctx, page.ParkedAgainst.Int64)
		if err != nil {
			return fmt.Errorf("scan: resolve conflict: load incumbent: %w", err)
		}

		// "Replace" means "put the parked page where the fight happened": the
		// CONTESTED cell captured at park time (0031). Park rows predating
		// those columns fall back to the incumbent's current cell — identical
		// whenever the incumbent hasn't moved; if such a legacy incumbent
		// holds no cell at all, the fight is unrecoverable.
		cellStudent, cellProblem := page.ParkStudentID, page.ParkProblemID
		if !cellStudent.Valid || !cellProblem.Valid {
			if !incPage.AssignedStudentID.Valid || !incPage.AssignedProblemID.Valid {
				return &ErrConflictStale{PageID: pageID}
			}
			cellStudent, cellProblem = incPage.AssignedStudentID, incPage.AssignedProblemID
		}
		incumbentHoldsCell := incPage.AssignedStudentID.Valid && incPage.AssignedProblemID.Valid &&
			incPage.AssignedStudentID.Int64 == cellStudent.Int64 &&
			incPage.AssignedProblemID.Int64 == cellProblem.Int64

		if !incumbentHoldsCell {
			// The incumbent was reassigned/unassigned after the park — its
			// current placement is somebody's deliberate decision, so replace
			// must never evict it. Take the contested cell when it is free;
			// when something else covers it by now, the recorded conflict is
			// stale (409) and the TA re-adjudicates against the new occupant.
			err = s.Store.WithTx(ctx, func(q *db.Queries) error {
				curInc, curSub, err := s.cellIncumbentQ(ctx, q, page.AssessmentID, cellStudent.Int64, cellProblem.Int64, pageID)
				if err != nil {
					return err
				}
				if curInc != nil || curSub != nil {
					return &ErrConflictStale{PageID: pageID}
				}
				return assignReplacedQ(ctx, q, pageID, cellStudent, cellProblem, actor, force)
			})
			if err != nil {
				return err
			}
			_ = s.Store.InsertAudit(ctx, actor, "scan.page.resolve_conflict", "scan_page", itoa(pageID), map[string]any{
				"action": "replace", "incumbent_page_id": incPage.ID, "incumbent_moved": true, "force": force,
			})
			return nil
		}

		incLive, err := livePromotedQ(ctx, s.Store.Q, incPage)
		if err != nil {
			return err
		}
		if incLive {
			if err := s.Ingest.RetractSubmission(ctx, incPage.SubmissionID.Int64, actor, force); err != nil &&
				!errors.Is(err, ingest.ErrAlreadyRetracted) {
				if isRetractGuardFailure(err) {
					// RetractSubmission's two guard sentinels (graded-without-
					// force, published-block) are fixed vocabulary, no PII —
					// map them to ErrInvalidInput so the HTTP layer returns
					// 400 instead of the generic 500 catch-all. Any OTHER
					// failure (e.g. a driver error from the guard-check
					// queries themselves) is NOT an operator-fault input
					// problem, so it propagates unwrapped to the static 500.
					return fmt.Errorf("scan: resolve conflict: retract incumbent: %w: %w", ErrInvalidInput, err)
				}
				return fmt.Errorf("scan: resolve conflict: retract incumbent: %w", err)
			}
		}
		// This tolerates a crash between the retract above and the follow-up
		// tx below on a prior run: the incumbent's submission_id may still be
		// Valid but stale (pointing at a submission that's already retracted),
		// and must still be cleared so the incumbent returns to orphan cleanly.

		err = s.Store.WithTx(ctx, func(q *db.Queries) error {
			// Re-verify on the LOCKED row that the incumbent still holds the
			// contested cell: an unpromoted incumbent can be reassigned by a
			// concurrent mutation between the snapshot above and this
			// transaction. (A crash after the retract above leaves at worst a
			// stale link, which self-heals on the next write mutation.)
			cur, err := q.GetScanPageForUpdate(ctx, incPage.ID)
			if err != nil {
				return err
			}
			if !cur.AssignedStudentID.Valid || cur.AssignedStudentID.Int64 != cellStudent.Int64 ||
				!cur.AssignedProblemID.Valid || cur.AssignedProblemID.Int64 != cellProblem.Int64 {
				return &ErrConflictStale{PageID: pageID}
			}
			if err := q.UnassignScanPage(ctx, incPage.ID); err != nil {
				return err
			}
			if cur.SubmissionID.Valid {
				// The retracted submission id must not linger on the incumbent —
				// otherwise it would still read as "promoted" even though its
				// submission is gone. Clearing it returns the incumbent to orphan
				// cleanly.
				if err := q.SetScanPageSubmission(ctx, db.SetScanPageSubmissionParams{
					ID: incPage.ID, SubmissionID: pgtype.Int8{},
				}); err != nil {
					return err
				}
			}
			return assignReplacedQ(ctx, q, pageID, cellStudent, cellProblem, actor, force)
		})
		if err != nil {
			return err
		}
		_ = s.Store.InsertAudit(ctx, actor, "scan.page.resolve_conflict", "scan_page", itoa(pageID), map[string]any{
			"action": "replace", "incumbent_page_id": incPage.ID, "force": force,
		})
		return nil

	default:
		return fmt.Errorf("scan: unknown resolve-conflict action %q: %w", action, ErrInvalidInput)
	}
}

// assignReplacedQ finishes a replace: the resolved parked page takes the
// contested cell (AssignScanPage clears its parked state and the recorded
// park cell), and force additionally marks it force_promote so the next
// finalize can re-ingest over grading records.
func assignReplacedQ(ctx context.Context, q *db.Queries, pageID int64, cellStudent, cellProblem pgtype.Int8, actor int64, force bool) error {
	if err := q.AssignScanPage(ctx, db.AssignScanPageParams{
		ID: pageID, AssignedStudentID: cellStudent, AssignedProblemID: cellProblem,
		AssignedBy: int8OrNull(actor),
	}); err != nil {
		return err
	}
	if force {
		return q.SetScanPageForcePromote(ctx, db.SetScanPageForcePromoteParams{
			ID: pageID, ForcePromote: true,
		})
	}
	return nil
}
