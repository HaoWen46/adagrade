package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// SpotCheck aliases the generated row type.
type SpotCheck = db.SpotCheck

// InsertSpotChecks creates one spot_checks row (verdict NULL, unchecked) per
// grading_record_id — the sample selected by the caller's stratified-sampling logic
// (trust spec §4; sampling itself is internal/grading/spotcheck.go's job in a later
// task, not this store method's). Idempotent: re-running with an overlapping sample
// is a no-op for records already present (ON CONFLICT DO NOTHING).
func (s *Store) InsertSpotChecks(ctx context.Context, runID int64, recordIDs []int64) error {
	for _, rid := range recordIDs {
		_, err := s.Q.InsertSpotCheck(ctx, db.InsertSpotCheckParams{RunID: runID, GradingRecordID: rid})
		// ON CONFLICT DO NOTHING yields zero rows (pgx.ErrNoRows) when the record was
		// already sampled for this run — that's the idempotent no-op, not a failure.
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("insert spot check (record %d): %w", rid, err)
		}
	}
	return nil
}

// ListSpotChecks returns a run's sample, joined with the graded record's answer,
// total, criterion scores, and confidence — everything the Runs detail "Spot-check"
// strip needs to render image+grade side by side (trust spec §4).
func (s *Store) ListSpotChecks(ctx context.Context, runID int64) ([]db.ListSpotChecksRow, error) {
	return s.Q.ListSpotChecks(ctx, runID)
}

// SetSpotCheckVerdict records a checker's agree/adjusted call on one sampled record.
func (s *Store) SetSpotCheckVerdict(ctx context.Context, spotCheckID int64, verdict, note string, checkerID int64) (SpotCheck, error) {
	return s.Q.SetSpotCheckVerdict(ctx, db.SetSpotCheckVerdictParams{
		ID:        spotCheckID,
		Verdict:   pgtype.Text{String: verdict, Valid: verdict != ""},
		Note:      note,
		CheckerID: pgtype.Int8{Int64: checkerID, Valid: checkerID != 0},
	})
}

// SpotCheckState reports the gate's current state for a run (trust spec §4): total
// sampled, how many have a verdict, and whether the run is waived (either via the
// admin override or the pre-existing-runs migration backfill). The gate is open —
// accept-official is allowed — when waived is true, OR total > 0 AND done == total.
// total == 0 (no sample exists yet, e.g. the run hasn't completed) is NOT open on its
// own; callers must check waived explicitly for that case.
func (s *Store) SpotCheckState(ctx context.Context, runID int64) (total, done int, waived bool, err error) {
	counts, err := s.Q.SpotCheckCounts(ctx, runID)
	if err != nil {
		return 0, 0, false, fmt.Errorf("spot check counts: %w", err)
	}
	waived, err = s.Q.IsRunWaived(ctx, runID)
	if err != nil {
		return 0, 0, false, fmt.Errorf("is run waived: %w", err)
	}
	return int(counts.Total), int(counts.Done), waived, nil
}

// WaiveSpotCheck is the admin-only override (POST /api/runs/{id}/spot-check/waive,
// D37) — also used verbatim by the 0019 migration backfill (reason="migration",
// actorID=0). Idempotent: re-waiving updates the reason/actor.
func (s *Store) WaiveSpotCheck(ctx context.Context, runID, actorID int64, reason string) error {
	return s.Q.WaiveSpotCheck(ctx, db.WaiveSpotCheckParams{
		RunID:    runID,
		Reason:   reason,
		WaivedBy: pgtype.Int8{Int64: actorID, Valid: actorID != 0},
	})
}

// SpotCheckAgreementRate reports agreed/total among already-verdicted samples — shown
// on the accept-official confirm dialog (trust spec §4).
func (s *Store) SpotCheckAgreementRate(ctx context.Context, runID int64) (agreed, total int, err error) {
	row, err := s.Q.SpotCheckAgreementRate(ctx, runID)
	if err != nil {
		return 0, 0, err
	}
	return int(row.Agreed), int(row.Total), nil
}
