package store_test

import (
	"context"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// TestMigration0025_BackfillsKindToFiled is the test-first coverage for migration
// 0025's kind backfill (spec §9, task brief, and the review-finding follow-up fix to
// that backfill): every pre-existing regrade_requests row (v1 had no
// addendum/unparsed/handed_off concept) becomes kind='filed' by default, EXCEPT rows
// that never had a real (publish_item_id, turn) slot -- those are corrected to
// 'unparsed' instead, because 'filed' with a NULL slot column is no longer even
// insertable once regrade_requests_filed_needs_slot (migration 0025's CHECK) exists.
// v1 allowed publish_item_id NULL (token never parsed, 0017) and turn NULL (rejected
// before a turn was ever assigned, 0023's backfill) independently, so this covers a
// row with both set (real verified reply -> filed), a row with turn NULL only
// (rejected-before-a-turn -> unparsed), and a row with neither set (token never parsed
// -> unparsed).
//
// storetest.Fresh already runs every migration including 0025, so this test rolls
// back to 0024 (the schema just before kind exists), seeds regrade_requests rows by
// hand covering a spread of v1 statuses and slot-completeness, then re-applies 0025
// and asserts each row's backfilled kind.
func TestMigration0025_BackfillsKindToFiled(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)

	if err := store.MigrateDownTo(ctx, dsn, 24); err != nil {
		t.Fatalf("migrate down to 0024: %v", err)
	}

	assessmentID := mustRawAssessment(t, ctx, s)
	studentID := mustRawStudent(t, ctx, s, "backfill-kind-student")
	itemID := mustRawPublishItem(t, ctx, s, assessmentID, studentID)

	// A real verified-and-slotted v1 reply: publish_item_id and turn both set, as a
	// genuine filed reply always would be in practice -- this is the only case that
	// should backfill to 'filed'.
	received := mustRawRegradeRequestWithSlot(t, ctx, s, studentID, assessmentID, "received", itemID, 1)
	resolved := mustRawRegradeRequestWithSlot(t, ctx, s, studentID, assessmentID, "resolved_upheld", itemID, 2)
	// Rejected before a turn was ever assigned (0023's backfill: rejected rows keep
	// turn NULL) -- has a publish_item_id but no turn, so it must NOT become 'filed'.
	rejected := mustRawRegradeRequest(t, ctx, s, studentID, assessmentID, "rejected_sender_mismatch")
	// Token never parsed at all -- no student/assessment/publish_item_id/turn.
	unparsedToken := mustRawUnparsedRegradeRequest(t, ctx, s)

	if err := store.MigrateUpTo(ctx, dsn, 25); err != nil {
		t.Fatalf("migrate up to 0025: %v", err)
	}

	for _, tc := range []struct {
		name     string
		id       int64
		wantKind string
	}{
		{"received", received, "filed"},
		{"resolved", resolved, "filed"},
		{"rejected", rejected, "unparsed"},
		{"unparsedToken", unparsedToken, "unparsed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var kind string
			if err := s.Pool.QueryRow(ctx,
				"SELECT kind FROM regrade_requests WHERE id = $1", tc.id,
			).Scan(&kind); err != nil {
				t.Fatalf("query backfilled kind: %v", err)
			}
			if kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}

// mustRawPublishItem inserts a minimal publish_batches + publish_items row directly
// (raw SQL, not store.CreatePublishBatch, since this test operates below migration
// 0025 where the store's higher-level helpers assume the current schema) so
// TestMigration0025_BackfillsKindToFiled has a real publish_item_id to give a
// "genuinely filed" fixture row.
func mustRawPublishItem(t *testing.T, ctx context.Context, s *store.Store, assessmentID, studentID int64) int64 {
	t.Helper()
	var batchID int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO publish_batches (assessment_id) VALUES ($1) RETURNING id`, assessmentID,
	).Scan(&batchID); err != nil {
		t.Fatalf("insert publish_batches: %v", err)
	}
	var itemID int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO publish_items (batch_id, student_id, snapshot, recipient_email)
		 VALUES ($1, $2, '{}', 'student@example.test') RETURNING id`,
		batchID, studentID,
	).Scan(&itemID); err != nil {
		t.Fatalf("insert publish_items: %v", err)
	}
	return itemID
}

// mustRawRegradeRequestWithSlot inserts a regrade_requests row directly (pre-0025
// schema shape, but AFTER 0023 so turn already exists) with a real
// (publish_item_id, turn) slot set -- modeling a genuine v1 verified-and-filed reply,
// as opposed to mustRawRegradeRequest's slot-less rows (used by the 0022->0023 turn
// backfill test, where turn/publish_item_id linkage isn't the thing under test).
func mustRawRegradeRequestWithSlot(t *testing.T, ctx context.Context, s *store.Store, studentID, assessmentID int64, status string, publishItemID int64, turn int32) int64 {
	t.Helper()
	var id int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO regrade_requests (student_id, assessment_id, from_email, subject, body, status, publish_item_id, turn)
		 VALUES ($1, $2, 'student@example.test', 're: grade', 'please regrade', $3, $4, $5)
		 RETURNING id`,
		studentID, assessmentID, status, publishItemID, turn,
	).Scan(&id); err != nil {
		t.Fatalf("insert regrade_request with slot: %v", err)
	}
	return id
}
