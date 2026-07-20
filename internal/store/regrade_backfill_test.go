package store_test

import (
	"context"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// TestMigration0023_BackfillsTurnAndEscalated is the test-first coverage for
// migration 0023's backfill (spec §6 D48, task brief backfill note): turn = 1 + the
// count of EARLIER verified (non-rejected) requests for the same (student,
// assessment), ordered by id; escalated = turn > ADAMARKER_REGRADE_MAX's default (3).
//
// storetest.Fresh already runs every migration including 0023, so this test rolls
// back to 0022 (the schema just before turn/escalated/problem_id/ai_record_id exist),
// seeds regrade_requests rows by hand (three verified rows for one student, one
// rejected row that must NOT consume a turn, and one row for a second unrelated
// student whose numbering must start over at 1), then re-applies 0023 and asserts the
// backfilled values.
func TestMigration0023_BackfillsTurnAndEscalated(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)

	if err := store.MigrateDownTo(ctx, dsn, 22); err != nil {
		t.Fatalf("migrate down to 0022: %v", err)
	}

	assessmentID := mustRawAssessment(t, ctx, s)
	studentA := mustRawStudent(t, ctx, s, "backfill-student-a")
	studentB := mustRawStudent(t, ctx, s, "backfill-student-b")

	// Student A: 4 verified requests (ids ascending) interleaved with 1 rejected
	// request that must not consume a turn number. Expected turns: 1, 2, (rejected,
	// no turn), 3, 4 -- so the 4th verified request (turn 4) must end up escalated
	// (turn > MAX=3) once 0023 backfills escalated.
	verified1 := mustRawRegradeRequest(t, ctx, s, studentA, assessmentID, "received")
	verified2 := mustRawRegradeRequest(t, ctx, s, studentA, assessmentID, "resolved_upheld")
	rejected := mustRawRegradeRequest(t, ctx, s, studentA, assessmentID, "rejected_sender_mismatch")
	verified3 := mustRawRegradeRequest(t, ctx, s, studentA, assessmentID, "received")
	verified4 := mustRawRegradeRequest(t, ctx, s, studentA, assessmentID, "received")

	// Student B: a single verified request -- must start its own numbering at 1,
	// independent of student A's count (partitioned by (student_id, assessment_id)).
	studentBReq := mustRawRegradeRequest(t, ctx, s, studentB, assessmentID, "received")

	// A row with no student_id/assessment_id at all (ladder rung 1: token never
	// parsed) -- must be left with turn NULL, not swept into any partition.
	unparsed := mustRawUnparsedRegradeRequest(t, ctx, s)

	if err := store.MigrateUpTo(ctx, dsn, 23); err != nil {
		t.Fatalf("migrate up to 0023: %v", err)
	}

	cases := []struct {
		name          string
		id            int64
		wantTurn      int32
		wantTurnValid bool
		wantEscalated bool
	}{
		{"verified1", verified1, 1, true, false},
		{"verified2", verified2, 2, true, false},
		{"rejected", rejected, 0, false, false},
		{"verified3", verified3, 3, true, false},
		{"verified4", verified4, 4, true, true}, // turn 4 > MAX(3) -> escalated
		{"studentB verified", studentBReq, 1, true, false},
		{"unparsed", unparsed, 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var turnVal *int32
			var escalated bool
			if err := s.Pool.QueryRow(ctx,
				"SELECT turn, escalated FROM regrade_requests WHERE id = $1", tc.id,
			).Scan(&turnVal, &escalated); err != nil {
				t.Fatalf("query backfilled row: %v", err)
			}
			gotValid := turnVal != nil
			if gotValid != tc.wantTurnValid {
				t.Fatalf("turn valid = %v, want %v (turn=%v)", gotValid, tc.wantTurnValid, turnVal)
			}
			if gotValid && *turnVal != tc.wantTurn {
				t.Fatalf("turn = %d, want %d", *turnVal, tc.wantTurn)
			}
			if escalated != tc.wantEscalated {
				t.Fatalf("escalated = %v, want %v", escalated, tc.wantEscalated)
			}
		})
	}
}

func mustRawAssessment(t *testing.T, ctx context.Context, s *store.Store) int64 {
	t.Helper()
	var id int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO assessments (kind, name) VALUES ('exam', $1) RETURNING id`, t.Name()+"-assessment",
	).Scan(&id); err != nil {
		t.Fatalf("insert assessment: %v", err)
	}
	return id
}

func mustRawStudent(t *testing.T, ctx context.Context, s *store.Store, suffix string) int64 {
	t.Helper()
	var id int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO students (student_id, name, email) VALUES ($1, 'Test Student', 'student@example.test') RETURNING id`,
		t.Name()+"-"+suffix,
	).Scan(&id); err != nil {
		t.Fatalf("insert student: %v", err)
	}
	return id
}

// mustRawRegradeRequest inserts a regrade_requests row directly (pre-0023 schema
// shape: no turn/escalated/problem_id/ai_record_id columns yet) tied to a known
// student/assessment.
func mustRawRegradeRequest(t *testing.T, ctx context.Context, s *store.Store, studentID, assessmentID int64, status string) int64 {
	t.Helper()
	var id int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO regrade_requests (student_id, assessment_id, from_email, subject, body, status)
		 VALUES ($1, $2, 'student@example.test', 're: grade', 'please regrade', $3)
		 RETURNING id`,
		studentID, assessmentID, status,
	).Scan(&id); err != nil {
		t.Fatalf("insert regrade_request: %v", err)
	}
	return id
}

// mustRawUnparsedRegradeRequest inserts a rung-1-failure row: no student/assessment
// known at all (the token didn't parse).
func mustRawUnparsedRegradeRequest(t *testing.T, ctx context.Context, s *store.Store) int64 {
	t.Helper()
	var id int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO regrade_requests (from_email, subject, body, status)
		 VALUES ('someone@example.test', 're: grade', 'huh?', 'rejected_bad_token')
		 RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatalf("insert unparsed regrade_request: %v", err)
	}
	return id
}
