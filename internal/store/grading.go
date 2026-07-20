package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

var (
	// ErrFinalSourcePublished requires the explicit unpublish escape hatch before
	// changing the source represented by a live batch.
	ErrFinalSourcePublished = errors.New("store: assessment is published; unpublish before changing the final source")
	ErrFinalRunInvalid      = errors.New("store: final run does not belong to this assessment")
	ErrFinalRunNotCompleted = errors.New("store: final run is not completed")
	// ErrFinalRunNotAssessmentScope (audit A4-minimal): RecomputeOfficials joins
	// strictly on grading_records.run_id = final_run_id, so a problem- or
	// answer-scoped run only ever supplies grades for its own slice — pinning one
	// as the final source would silently un-officialize every answer outside that
	// scope. Full supplemental-run layering (assessment run + corrective
	// problem-scoped overlay) is deferred (docs/PLAN_GAPS.md); only
	// assessment-scoped runs may become the final source.
	ErrFinalRunNotAssessmentScope = errors.New("store: final run must be scoped to the whole assessment")
	// ErrFinalRunNoSucceeded (audit A3): a completed run with zero succeeded
	// leaves never gets a spot-check sample (createSpotCheckSample only pools
	// state='succeeded' items), so SpotCheckOpen can never become true except by
	// admin waive — pinning it wedges publish behind an unreachable "review
	// spot-check" call to action.
	ErrFinalRunNoSucceeded = errors.New("store: final run has no succeeded grading records")
)

// SetAssessmentFinalSource atomically validates and records one final source,
// then re-derives every unpublished official in the same transaction. For a
// method source, runID is the immutable completed execution; method_id is
// derived server-side from that run's pinned method version.
func (s *Store) SetAssessmentFinalSource(ctx context.Context, assessmentID int64, kind string, runID int64) (db.Assessment, int64, error) {
	var out db.Assessment
	var moved int64
	err := s.WithTx(ctx, func(q *db.Queries) error {
		// Shared lock order for source/publish/retry: assessment, then run.
		current, err := q.GetAssessmentForUpdate(ctx, assessmentID)
		if err != nil {
			return err
		}
		if _, err := q.LatestNonSupersededBatch(ctx, assessmentID); err == nil {
			// Recovery exception (audit A6): a NULL current source means
			// nothing is pinned yet — a fresh assessment, an explicitly
			// cleared source, or 0035's fail-closed migration NULLing a
			// legacy pointer it could not reconstruct. Setting a source
			// from that state is safe even while published: published
			// answers are already immutable independent of this guard —
			// RecomputeOfficials (and every runtime recompute path) only
			// ever writes answers with published_at IS NULL — so this can
			// never rewrite a published grade. What stays blocked is
			// changing a source that is ALREADY SET while published: that
			// would rederive which run backs still-unpublished officials in
			// a live batch, a real behavior change that must go through the
			// explicit unpublish escape hatch instead of happening silently
			// underneath a publish in progress.
			if current.FinalSourceKind.Valid {
				return ErrFinalSourcePublished
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check live publish batch: %w", err)
		}

		params := db.SetAssessmentFinalSourceParams{ID: assessmentID}
		switch kind {
		case "":
			// Clearing the source leaves every nullable field unset.
		case "consensus":
			params.Kind = pgtype.Text{String: "consensus", Valid: true}
		case "method":
			run, err := q.GetRunForUpdate(ctx, runID)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && run.AssessmentID != assessmentID) {
				return ErrFinalRunInvalid
			}
			if err != nil {
				return err
			}
			if run.Status != "completed" {
				return ErrFinalRunNotCompleted
			}
			if run.ScopeKind != "assessment" {
				return ErrFinalRunNotAssessmentScope
			}
			succeeded, err := q.CountSucceededItemsForRun(ctx, run.ID)
			if err != nil {
				return fmt.Errorf("count succeeded leaves: %w", err)
			}
			if succeeded == 0 {
				return ErrFinalRunNoSucceeded
			}
			mv, err := q.GetMethodVersion(ctx, run.MethodVersionID)
			if err != nil {
				return fmt.Errorf("load final run method version: %w", err)
			}
			params.Kind = pgtype.Text{String: "method", Valid: true}
			params.MethodID = pgtype.Int8{Int64: mv.MethodID, Valid: true}
			params.RunID = pgtype.Int8{Int64: run.ID, Valid: true}
		default:
			return fmt.Errorf("unsupported final source kind %q", kind)
		}

		out, err = q.SetAssessmentFinalSource(ctx, params)
		if err != nil {
			return err
		}
		moved, err = recomputeOfficialsWithQueries(ctx, q, out)
		return err
	})
	return out, moved, err
}

// RecomputeOfficials re-derives answers.official_record_id for every
// unpublished answer of the assessment from its chosen final source
// (round-based grading, migration 0027): the source's latest current-rubric
// record where it decided (and the answer is unflagged), the latest human
// record as fallback, NULL (a hole) otherwise. With no source chosen,
// officials clear entirely. Returns how many pointers changed.
//
// Call after anything that can move the derivation: source selection, run or
// consensus completion, manual grading, flag toggles, new rubric versions,
// forced re-ingest. The query only ever touches unpublished answers, so a
// published batch is never rewritten (spec §2 lock).
func (s *Store) RecomputeOfficials(ctx context.Context, assessmentID int64) (int64, error) {
	a, err := s.Q.GetAssessment(ctx, assessmentID)
	if err != nil {
		return 0, err
	}
	return recomputeOfficialsWithQueries(ctx, s.Q, a)
}

func recomputeOfficialsWithQueries(ctx context.Context, q *db.Queries, a db.Assessment) (int64, error) {
	if !a.FinalSourceKind.Valid {
		return q.ClearUnpublishedOfficials(ctx, a.ID)
	}
	var finalRunID pgtype.Int8
	if a.FinalSourceKind.String == "method" {
		if !a.FinalRunID.Valid {
			return 0, errors.New("store: method final source has no pinned run")
		}
		finalRunID = a.FinalRunID
	} else if a.FinalSourceKind.String != "consensus" {
		return 0, fmt.Errorf("store: unsupported final source kind %q", a.FinalSourceKind.String)
	}
	return q.RecomputeOfficials(ctx, db.RecomputeOfficialsParams{
		AssessmentID: a.ID,
		FinalRunID:   finalRunID,
	})
}
