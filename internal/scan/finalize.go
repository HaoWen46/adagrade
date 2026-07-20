// This file owns assessment-wide finalize (design spec 2026-07-04 §7, D27,
// F1): the incremental, re-runnable pass that gates on missing (student,
// problem) cells, seeds identity mask regions onto every page (D66), and
// enqueues one scan.promote_page job per assigned-unpromoted page. PromotePage
// is that job's body: it drives a page's image through the existing ingest
// seam (D22) exactly as the old per-file PromoteFile did, so the supersede
// chain and graded/published guards are unchanged.
package scan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// FinalizeReport summarizes one Finalize call.
type FinalizeReport struct {
	Enqueued        int `json:"enqueued"`
	AlreadyPromoted int `json:"already_promoted"`
	MissingCells    int `json:"missing_cells"`
}

// ErrMissingUnacknowledged is Finalize's gate: the assessment has (student,
// problem) cells with neither a live assigned page nor a live submission, and
// the caller has not explicitly acknowledged proceeding anyway.
type ErrMissingUnacknowledged struct{ Count int }

func (e *ErrMissingUnacknowledged) Error() string {
	return fmt.Sprintf("scan: finalize: %d cell(s) missing and unacknowledged", e.Count)
}

// Finalize is assessment-wide and incremental: gate on missing cells (unless
// acked), seed identity mask regions, enqueue one scan.promote_page per
// assigned-unpromoted page. Safe to re-run; only new pages promote.
func (s *Service) Finalize(ctx context.Context, assessmentID int64, ackMissing bool, actor int64) (FinalizeReport, error) {
	missing, err := s.Store.Q.CountMissingCells(ctx, assessmentID)
	if err != nil {
		return FinalizeReport{}, fmt.Errorf("scan: finalize: count missing: %w", err)
	}
	if missing > 0 && !ackMissing {
		return FinalizeReport{}, &ErrMissingUnacknowledged{Count: int(missing)}
	}
	if err := s.seedMaskRegions(ctx, assessmentID); err != nil {
		return FinalizeReport{}, fmt.Errorf("scan: finalize: seed masks: %w", err)
	}
	report := FinalizeReport{MissingCells: int(missing)}
	err = s.Store.WithTxPgx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := q.ClearPromotionErrorsForAssessment(ctx, assessmentID); err != nil {
			return err
		}
		pages, err := q.ListAssignedUnpromotedPages(ctx, assessmentID)
		if err != nil {
			return err
		}
		items := make([]PromotePage, 0, len(pages))
		for _, p := range pages {
			items = append(items, PromotePage{PageID: p.ID, Force: p.ForcePromote, Actor: actor})
		}
		report.Enqueued = len(items)
		if len(items) > 0 && s.EnqueuePromotePages != nil {
			return s.EnqueuePromotePages(ctx, tx, items)
		}
		return nil
	})
	if err != nil {
		return FinalizeReport{}, err
	}
	// AlreadyPromoted = live assigned pages minus the ones just enqueued.
	assigned, err := s.Store.Q.ListLiveAssignedPagesForAssessment(ctx, assessmentID)
	// Best-effort reporting count, deliberately not gating: a failure here
	// must not fail a finalize whose enqueue already committed.
	if err == nil {
		for _, p := range assigned {
			if p.SubmissionID.Valid {
				report.AlreadyPromoted++
			}
		}
	}
	_ = s.Store.InsertAudit(ctx, actor, "scan.finalize", "assessment", itoa(assessmentID),
		map[string]any{"enqueued": report.Enqueued, "missing_cells": report.MissingCells})
	return report, nil
}

// seedMaskRegions copies the student_id and name id_regions into mask_regions
// with page_scope 'all' (idempotent by exact rect equality) — every page now
// carries identity at known coordinates, so masking hides it everywhere (D66).
// The problem_id region is NOT seeded (the grader may use it).
// Seeding is append-only: it dedupes by exact rect equality but never removes
// previously seeded rows, so editing an id-region after a finalize leaves the
// old mask rect in place alongside the new one. Draw regions final before
// finalizing; revisit if the adjust-and-refinalize workflow becomes real.
func (s *Service) seedMaskRegions(ctx context.Context, assessmentID int64) error {
	idRegions, err := s.Store.Q.ListIDRegions(ctx, assessmentID)
	if err != nil {
		return err
	}
	existing, err := s.Store.Q.ListMaskRegions(ctx, assessmentID)
	if err != nil {
		return err
	}
	for _, r := range idRegions {
		if r.Kind != "student_id" && r.Kind != "name" {
			continue
		}
		already := false
		for _, m := range existing {
			if m.PageScope == "all" && m.X == r.X && m.Y == r.Y && m.W == r.W && m.H == r.H {
				already = true
				break
			}
		}
		if already {
			continue
		}
		if _, err := s.Store.Q.CreateMaskRegion(ctx, db.CreateMaskRegionParams{
			AssessmentID: assessmentID, PageScope: "all",
			X: r.X, Y: r.Y, W: r.W, H: r.H,
			Color: r.Color, Padding: r.Padding,
		}); err != nil {
			return err
		}
	}
	return nil
}

// errPromotionSuperseded aborts a promote whose page was mutated (reassigned/
// unassigned/discarded) while the job was in flight. It is returned to the
// queue as a plain retryable failure — never stamped onto the page, which is
// fine under its new state: the retry re-reads the fresh row and promotes the
// corrected cell, or no-ops if the page is no longer eligible.
var errPromotionSuperseded = errors.New("scan: promote: page changed mid-flight; promotion aborted")

// PromotePage is the scan.promote_page worker body: page image -> per-problem
// image submission through the ingest seam (supersede chain, graded/published
// guards unchanged).
func (s *Service) PromotePage(ctx context.Context, pageID int64, force bool, actor int64, finalAttempt bool) error {
	page, err := s.Store.Q.GetScanPage(ctx, pageID)
	if err != nil {
		return fmt.Errorf("scan: promote: load page: %w", err)
	}
	if page.SubmissionID.Valid || !page.AssignedStudentID.Valid || page.DiscardedAt.Valid {
		return nil // idempotent / no longer eligible
	}

	// D65 at promote time: a live submission may already cover this cell — a
	// newer Submissions-tab upload that landed AFTER the page was assigned
	// (identify and manual assign both refuse covered cells, so this is the
	// only way in). Silently ingesting would supersede the newer upload and
	// install the stale scan image over it; park the page as a conflict
	// instead (same semantics as identify's resolveCell) so the TA
	// adjudicates. Runs on the locked row so a duplicate promote job or a
	// concurrent reassign is observed, not raced.
	done := false
	err = s.Store.WithTx(ctx, func(q *db.Queries) error {
		cur, err := q.GetScanPageForUpdate(ctx, pageID)
		if err != nil {
			return err
		}
		if cur.SubmissionID.Valid || !cur.AssignedStudentID.Valid || cur.DiscardedAt.Valid {
			done = true // mutated since the snapshot: idempotent no-op
			return nil
		}
		if cur.AssignedStudentID.Int64 != page.AssignedStudentID.Int64 ||
			cur.AssignedProblemID.Int64 != page.AssignedProblemID.Int64 {
			return errPromotionSuperseded // reassigned: retry under the new cell
		}
		incPage, incSub, err := s.cellIncumbentQ(ctx, q, page.AssessmentID,
			page.AssignedStudentID.Int64, page.AssignedProblemID.Int64, pageID)
		if err != nil {
			return err
		}
		if incPage == nil && incSub == nil {
			return nil // cell uncovered: proceed to ingest
		}
		done = true
		// Unassign first so the park mirrors identify's parks exactly (parked
		// pages hold no cell); the contested cell is captured on the park row.
		if err := q.UnassignScanPage(ctx, pageID); err != nil {
			return err
		}
		return s.parkAgainst(ctx, q, cur, page.AssignedStudentID.Int64, page.AssignedProblemID.Int64, incPage, incSub)
	})
	if err != nil {
		return fmt.Errorf("scan: promote: cell check: %w", err)
	}
	if done {
		return nil
	}

	student, err := s.Store.Q.GetStudent(ctx, page.AssignedStudentID.Int64)
	if err != nil {
		return fmt.Errorf("scan: promote: load student: %w", err)
	}
	img, err := s.readAll(ctx, page.ImageRef.String)
	if err != nil {
		if isInterruption(ctx, err) {
			return err
		}
		if finalAttempt {
			_ = s.setPromotionError(ctx, pageID, "promotion rejected: page image unreadable")
			return nil
		}
		return err
	}
	// The success-path link runs INSIDE ingest's transaction, conditional on
	// the page still carrying the exact assignment read above (the mirror of
	// SetScanPagePromotionError's race guard): a TA reassigning the page while
	// this job is in flight makes the link match nothing, which aborts the
	// whole ingest — the wrong-student submission never becomes visible.
	assignmentChanged := false
	res := s.Ingest.Ingest(ctx, page.AssessmentID, ingest.IngestInput{
		Filename: student.StudentID + ".jpg", Data: img,
		Kind: "image", TargetProblemID: page.AssignedProblemID.Int64,
		LinkInTx: func(q *db.Queries, submissionID int64) error {
			n, err := q.LinkScanPagePromotion(ctx, db.LinkScanPagePromotionParams{
				ID:                pageID,
				SubmissionID:      int8OrNull(submissionID),
				AssignedStudentID: page.AssignedStudentID,
				AssignedProblemID: page.AssignedProblemID,
			})
			if err != nil {
				return err
			}
			if n == 0 {
				assignmentChanged = true
				return errPromotionSuperseded
			}
			return nil
		},
	}, actor, force || page.ForcePromote)
	if assignmentChanged {
		s.log().Warn("scan: promote aborted: page mutated mid-flight; queue will retry", "page_id", pageID)
		return errPromotionSuperseded
	}
	// Status is one of ingested|rejected|quarantined (exhaustive in
	// internal/ingest today). A future status added there would fall into
	// default and be marked permanently non-retryable — update this switch
	// if ingest grows a new status.
	switch res.Status {
	case "ingested":
		return nil // linked inside the ingest transaction (LinkInTx above)
	default: // rejected | quarantined: business outcome, never retried
		_ = s.setPromotionError(ctx, pageID, "promotion rejected: "+res.Reason)
		return nil
	}
}
