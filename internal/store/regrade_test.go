package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// mustPublishItem is a small helper bundling the publish-batch dance every v2 regrade
// test needs to get a real publish_item_id to file against.
func mustPublishItem(t *testing.T, ctx context.Context, s *store.Store, f fixture) int64 {
	t.Helper()
	_, items, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}
	return items[0].ID
}

// TestRegradeRequestsV2_RoundTrip covers the basic insert/list/get/resolve path with
// the v2 kind column, replacing v1's TestRegradeRequests_RoundTripAndRateCap (the
// count-based rate cap itself is retired, design doc §4: "This replaces v1's
// count-based rate cap").
func TestRegradeRequestsV2_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nplease regrade\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}
	if rr.Kind != "filed" {
		t.Fatalf("kind = %q, want filed", rr.Kind)
	}

	got, err := s.GetRegradeRequest(ctx, rr.ID)
	if err != nil || got.Status != "received" {
		t.Fatalf("GetRegradeRequest: got %+v err %v", got, err)
	}

	listed, err := s.ListRegradeRequests(ctx, store.ListRegradeRequestsFilters{AssessmentID: f.AssessmentID})
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListRegradeRequests: got %d err %v", len(listed), err)
	}
	filteredByKind, err := s.ListRegradeRequests(ctx, store.ListRegradeRequestsFilters{Kind: "filed"})
	if err != nil || len(filteredByKind) != 1 {
		t.Fatalf("ListRegradeRequests(kind=filed): got %d err %v", len(filteredByKind), err)
	}
	filteredByOtherKind, err := s.ListRegradeRequests(ctx, store.ListRegradeRequestsFilters{Kind: "unparsed"})
	if err != nil || len(filteredByOtherKind) != 0 {
		t.Fatalf("ListRegradeRequests(kind=unparsed): got %d err %v, want 0", len(filteredByOtherKind), err)
	}

	resolved, err := s.ResolveRegradeRequest(ctx, rr.ID, "resolved_upheld", 0, "grade confirmed correct")
	if err != nil {
		t.Fatalf("ResolveRegradeRequest: %v", err)
	}
	if resolved.Status != "resolved_upheld" || !resolved.ResolvedAt.Valid {
		t.Fatalf("unexpected resolved row: %+v", resolved)
	}
}

// TestListRegradeRequests_OnlyOpenAndUndeliveredResultFilters is the test-first
// coverage for the HCI-audit queue-correctness fix: the UI's "Actionable" tab must be
// expressible SERVER-SIDE (kind=filed + open status group) so pagination and the pager
// total are computed over the filtered set — the old client-side narrowing let a page
// render empty while live appeals sat on later pages. Two filter extensions:
//   - OnlyOpen: status IN (received, under_review) — combined with Kind by callers.
//   - OnlyUndeliveredResult: kind='filed' AND status IN (resolved_upheld,
//     resolved_regraded) AND result_sent_at IS NULL — the "resolved but the result
//     email never reached the student" recovery set (migration 0026). kind='filed' is
//     part of the predicate itself: a reminded unparsed row is also resolved_upheld
//     with a NULL result_sent_at, but nothing was ever owed to the student there.
//
// List and Count must agree under every combination (they share one WHERE by design).
func TestListRegradeRequests_OnlyOpenAndUndeliveredResultFilters(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	insert := func(kind, status string, turn int) db.RegradeRequest {
		t.Helper()
		p := store.InsertRegradeRequestV2Params{
			StudentID: f.StudentID, AssessmentID: f.AssessmentID,
			FromEmail: "student@example.test", Subject: "re: grade", Body: "body",
			Status: status, Kind: kind, Turn: turn,
		}
		if kind == "filed" {
			p.PublishItemID = itemID
		}
		rr, err := s.InsertRegradeRequestV2(ctx, p)
		if err != nil {
			t.Fatalf("InsertRegradeRequestV2(%s/%s): %v", kind, status, err)
		}
		return rr
	}

	insert("filed", "received", 1)                          // open
	insert("filed", "under_review", 2)                      // open
	delivered := insert("filed", "resolved_regraded", 3)    // resolved, result delivered
	undelivered := insert("filed", "resolved_upheld", 4)    // resolved, send FAILED
	insert("unparsed", "received", 0)                       // open, non-filed
	insert("unparsed", "resolved_upheld", 0)                // reminder sent — NOT undelivered
	if _, err := s.SetRegradeResultSentAt(ctx, delivered.ID); err != nil {
		t.Fatalf("SetRegradeResultSentAt: %v", err)
	}

	assertListAndCount := func(name string, flt store.ListRegradeRequestsFilters, wantIDs ...int64) {
		t.Helper()
		rows, err := s.ListRegradeRequests(ctx, flt)
		if err != nil {
			t.Fatalf("%s: ListRegradeRequests: %v", name, err)
		}
		gotIDs := make(map[int64]bool, len(rows))
		for _, r := range rows {
			gotIDs[r.ID] = true
		}
		if len(rows) != len(wantIDs) {
			t.Errorf("%s: got %d rows, want %d", name, len(rows), len(wantIDs))
		}
		for _, id := range wantIDs {
			if !gotIDs[id] {
				t.Errorf("%s: row %d missing from the filtered list", name, id)
			}
		}
		total, err := s.CountRegradeRequests(ctx, flt)
		if err != nil {
			t.Fatalf("%s: CountRegradeRequests: %v", name, err)
		}
		if total != int64(len(wantIDs)) {
			t.Errorf("%s: count = %d, want %d (list and pager total must agree)", name, total, len(wantIDs))
		}
	}

	all, err := s.ListRegradeRequests(ctx, store.ListRegradeRequestsFilters{})
	if err != nil || len(all) != 6 {
		t.Fatalf("unfiltered: got %d rows err %v, want 6", len(all), err)
	}
	openFiled := []int64{all[5].ID, all[4].ID} // turns 1 and 2, in some order
	assertListAndCount("OnlyOpen", store.ListRegradeRequestsFilters{OnlyOpen: true},
		openFiled[0], openFiled[1], all[1].ID) // + open unparsed
	assertListAndCount("OnlyOpen+Kind=filed",
		store.ListRegradeRequestsFilters{OnlyOpen: true, Kind: "filed"},
		openFiled[0], openFiled[1])
	assertListAndCount("OnlyUndeliveredResult",
		store.ListRegradeRequestsFilters{OnlyUndeliveredResult: true},
		undelivered.ID)
}

// TestInsertRegradeRequestV2_DuplicateMessageID_UniqueViolation covers F1's storage
// layer, unchanged by v2: two inserts carrying the same non-empty message_id collide
// on the partial unique index (migration 0020).
func TestInsertRegradeRequestV2_DuplicateMessageID_UniqueViolation(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	params := store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nplease regrade\n</p1>",
		Status: "received", Kind: "filed", Turn: 1, MessageID: "pm-webhook-delivery-1",
	}
	if _, err := s.InsertRegradeRequestV2(ctx, params); err != nil {
		t.Fatalf("first InsertRegradeRequestV2: %v", err)
	}

	params.Kind = "unparsed" // even a different kind must still collide on message_id
	_, err := s.InsertRegradeRequestV2(ctx, params)
	if err == nil {
		t.Fatal("expected a unique-violation error on duplicate message_id, got nil")
	}
	if !store.IsUniqueViolation(err) {
		t.Fatalf("expected store.IsUniqueViolation(err) to be true, got err=%v", err)
	}

	listed, err := s.ListRegradeRequests(ctx, store.ListRegradeRequestsFilters{AssessmentID: f.AssessmentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("duplicate message_id must not create a second row, got %d rows", len(listed))
	}
}

// TestFiledUniqueIndex_SecondFiledRowSameItemTurnViolates is the test-first coverage
// for the D57 race-killer (spec §4, migration 0025's partial unique index): two
// 'filed' rows for the same (publish_item_id, turn) must not both succeed -- the
// second insert collides on regrade_requests_filed_item_turn_uniq. This structurally
// replaces v1's count-based rate cap / TOCTOU-prone concurrent-double-reply handling.
func TestFiledUniqueIndex_SecondFiledRowSameItemTurnViolates(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	first, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nfirst reply\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("first InsertRegradeRequestV2 (filed): %v", err)
	}
	if !first.Turn.Valid || first.Turn.Int32 != 1 {
		t.Fatalf("unexpected turn on first filed row: %+v", first.Turn)
	}

	// A second 'filed' row for the SAME (publish_item_id, turn) -- the racing reply
	// that loses -- must violate the partial unique index.
	_, err = s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nsecond reply, racing\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err == nil {
		t.Fatal("expected a unique-violation error on a second filed row for the same (item, turn), got nil")
	}
	if !store.IsUniqueViolation(err) {
		t.Fatalf("expected store.IsUniqueViolation(err) to be true, got err=%v", err)
	}

	// The loser re-records as an addendum instead -- this must succeed (addendum rows
	// are NOT covered by the partial index).
	addendum, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nsecond reply, racing\n</p1>",
		Status: "received", Kind: "addendum", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2 (addendum after lost race): %v", err)
	}
	if addendum.Kind != "addendum" {
		t.Fatalf("kind = %q, want addendum", addendum.Kind)
	}

	// A different turn on the same item is a different slot -- must succeed as filed.
	secondTurn, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nturn 2 reply\n</p1>",
		Status: "received", Kind: "filed", Turn: 2,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2 (filed, different turn): %v", err)
	}
	if !secondTurn.Turn.Valid || secondTurn.Turn.Int32 != 2 {
		t.Fatalf("unexpected turn on second-turn filed row: %+v", secondTurn.Turn)
	}
}

// TestFiledUniqueIndex_AddendumAndUnparsedUnaffected covers the flip side of D57: many
// addendum and unparsed rows may pile up on the same (publish_item_id, turn) without
// ever colliding with each other or with the one filed row -- only kind='filed' is
// constrained.
func TestFiledUniqueIndex_AddendumAndUnparsedUnaffected(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	if _, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nfiled\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
	}); err != nil {
		t.Fatalf("InsertRegradeRequestV2 (filed): %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
			PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
			FromEmail: "student@example.test", Subject: "re: grade", Body: "thanks!",
			Status: "received", Kind: "addendum", Turn: 1,
		}); err != nil {
			t.Fatalf("InsertRegradeRequestV2 (addendum %d): %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
			PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
			FromEmail: "student@example.test", Subject: "re: grade", Body: "no tags here at all",
			Status: "received", Kind: "unparsed", Turn: 1,
		}); err != nil {
			t.Fatalf("InsertRegradeRequestV2 (unparsed %d): %v", i, err)
		}
	}

	listed, err := s.ListRegradeRequests(ctx, store.ListRegradeRequestsFilters{AssessmentID: f.AssessmentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 7 { // 1 filed + 3 addendum + 3 unparsed
		t.Fatalf("expected 7 rows, got %d", len(listed))
	}
}

// TestMarkRequestHandedOff covers the final-turn handoff state transition (spec §6).
func TestMarkRequestHandedOff(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nfinal reply\n</p1>",
		Status: "received", Kind: "filed", Turn: 4,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}

	handed, err := s.MarkRequestHandedOff(ctx, rr.ID)
	if err != nil {
		t.Fatalf("MarkRequestHandedOff: %v", err)
	}
	if handed.Kind != "handed_off" {
		t.Fatalf("kind = %q, want handed_off", handed.Kind)
	}
	if handed.Status != "received" {
		t.Fatalf("MarkRequestHandedOff must not touch status, got %q", handed.Status)
	}
}

// TestInsertRegradeRequestV2_UnparsedTokenHasNoPublishItem mirrors v1's equivalent:
// ladder rung 1 failure (token didn't even parse) leaves publish_item/student/
// assessment NULL.
func TestInsertRegradeRequestV2_UnparsedTokenHasNoPublishItem(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		FromEmail: "someone@example.test", Subject: "re: grade", Body: "huh?",
		Status: "received", Kind: "unparsed",
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}
	if rr.PublishItemID.Valid || rr.StudentID.Valid || rr.AssessmentID.Valid {
		t.Fatalf("expected NULL refs for unparsed token, got %+v", rr)
	}
	if rr.Turn.Valid {
		t.Fatalf("expected NULL turn for unset Turn field, got %+v", rr.Turn)
	}
}

// TestResolveRegradeRequestV2_AtomicStatusGuard covers F2, unchanged by v2: the UPDATE
// itself only matches rows still in received/under_review.
func TestResolveRegradeRequestV2_AtomicStatusGuard(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nplease regrade\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}

	first, err := s.ResolveRegradeRequest(ctx, rr.ID, "resolved_upheld", 0, "first resolution")
	if err != nil {
		t.Fatalf("first ResolveRegradeRequest: %v", err)
	}
	if first.Status != "resolved_upheld" {
		t.Fatalf("first resolve: got status %q", first.Status)
	}

	_, err = s.ResolveRegradeRequest(ctx, rr.ID, "resolved_regraded", 0, "second resolution")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second ResolveRegradeRequest: expected pgx.ErrNoRows, got %v", err)
	}

	got, err := s.GetRegradeRequest(ctx, rr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "resolved_upheld" || got.ResolutionNote != "first resolution" {
		t.Fatalf("row must be unchanged after failed second resolve, got %+v", got)
	}
}

// --- Sub-items (regrade_request_problems, spec §5 D59) ---

// TestInsertRequestProblems_MultipleAndUniqueness covers the multi-problem fan-out
// (spec §5 D59) and its UNIQUE(request_id, problem_id) guard: two sub-items for
// different problems on one request both succeed; a second sub-item for the SAME
// problem on the SAME request violates the unique constraint (the escape-hatch
// add/correct path, §5, must use an update path for an already-present problem, not
// a second insert).
func TestInsertRequestProblems_MultipleAndUniqueness(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	maxPoints, err := store.Num("10")
	if err != nil {
		t.Fatal(err)
	}
	problem2, err := s.Q.CreateProblem(ctx, db.CreateProblemParams{
		AssessmentID: f.AssessmentID, Number: 4, Title: "Problem 4",
		MaxPoints: maxPoints, Position: 2,
	})
	if err != nil {
		t.Fatalf("CreateProblem (second problem): %v", err)
	}

	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade",
		Body:   "<p1>\nbase case wrong\n</p1>\n<p4>\nexchange argument\n</p4>",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}

	subItems, err := s.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{
		{ProblemID: f.ProblemID, ComplaintText: "base case wrong"},
		{ProblemID: problem2.ID, ComplaintText: "exchange argument"},
	})
	if err != nil {
		t.Fatalf("InsertRequestProblems: %v", err)
	}
	if len(subItems) != 2 {
		t.Fatalf("expected 2 sub-items, got %d", len(subItems))
	}

	listed, err := s.ListRequestProblems(ctx, rr.ID)
	if err != nil {
		t.Fatalf("ListRequestProblems: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("ListRequestProblems: got %d, want 2", len(listed))
	}

	// A duplicate problem on the same request must violate UNIQUE(request_id, problem_id).
	_, err = s.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{
		{ProblemID: f.ProblemID, ComplaintText: "TA escape-hatch duplicate attempt"},
	})
	if err == nil {
		t.Fatal("expected a unique-violation error for a duplicate (request_id, problem_id), got nil")
	}
	if !store.IsUniqueViolation(err) {
		t.Fatalf("expected store.IsUniqueViolation(err) to be true, got err=%v", err)
	}
}

// TestInsertRequestProblems_AllOrNothing covers the transactional posture: if one
// problem in a multi-problem batch fails (e.g. a bad problem_id FK), none of the
// batch's sub-items should be persisted.
func TestInsertRequestProblems_AllOrNothing(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nok\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}

	_, err = s.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{
		{ProblemID: f.ProblemID, ComplaintText: "valid"},
		{ProblemID: 999999999, ComplaintText: "bad FK, should roll back the whole batch"},
	})
	if err == nil {
		t.Fatal("expected an FK-violation error inserting a nonexistent problem_id, got nil")
	}

	listed, err := s.ListRequestProblems(ctx, rr.ID)
	if err != nil {
		t.Fatalf("ListRequestProblems: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("expected the whole batch to roll back, got %d sub-items persisted", len(listed))
	}
}

// TestFileRegradeRequestV2_AtomicAcrossRequestAndSubItems (Finding 2, IMPORTANT): the
// kind='filed' request row and its sub-items must commit as ONE unit. Before the fix,
// the webhook path called InsertRegradeRequestV2 (commits alone) and then
// InsertRequestProblems (a SEPARATE WithTx) — a failure between the two strands a
// consumed (item, turn) slot with zero sub-items, permanently unreachable (the partial
// unique index treats the slot as taken, so no retry can ever file against it again).
// This proves FileRegradeRequestV2 rolls back the REQUEST ROW TOO when a sub-item insert
// fails (a bad problem_id FK here), leaving the (item, turn) slot free.
func TestFileRegradeRequestV2_AtomicAcrossRequestAndSubItems(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	_, _, err := s.FileRegradeRequestV2(ctx, store.FileRegradeRequestParams{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nok\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
		Problems: []store.RequestProblemInput{
			{ProblemID: f.ProblemID, ComplaintText: "valid"},
			{ProblemID: 999999999, ComplaintText: "bad FK, should roll back the WHOLE filing"},
		},
	})
	if err == nil {
		t.Fatal("expected an FK-violation error inserting a nonexistent problem_id, got nil")
	}

	// No request row must have survived — not "filed with zero sub-items".
	listed, err := s.ListRegradeRequests(ctx, store.ListRegradeRequestsFilters{AssessmentID: f.AssessmentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("expected the failed filing to leave NO regrade_requests row, got %d: %+v", len(listed), listed)
	}

	// The (item, turn) slot must be free: a subsequent legitimate filing at the same
	// turn must succeed (proves the partial unique index slot was never actually
	// consumed by the rolled-back attempt).
	rr, subs, err := s.FileRegradeRequestV2(ctx, store.FileRegradeRequestParams{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nok\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
		Problems: []store.RequestProblemInput{
			{ProblemID: f.ProblemID, ComplaintText: "valid, retried"},
		},
	})
	if err != nil {
		t.Fatalf("FileRegradeRequestV2 retry at the same (item,turn) after a rolled-back attempt: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub-item on the successful retry, got %d", len(subs))
	}

	listedSubs, err := s.ListRequestProblems(ctx, rr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listedSubs) != 1 {
		t.Fatalf("ListRequestProblems after successful filing: got %d, want 1", len(listedSubs))
	}
}

// TestFileRegradeRequestV2_HappyPath_ReturnsRequestAndSubItems is the straightforward
// success case: one filing call with two contested problems returns both the request row
// and every sub-item, all visible via the normal read paths.
func TestFileRegradeRequestV2_HappyPath_ReturnsRequestAndSubItems(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	maxPoints, err := store.Num("10")
	if err != nil {
		t.Fatal(err)
	}
	problem2, err := s.Q.CreateProblem(ctx, db.CreateProblemParams{
		AssessmentID: f.AssessmentID, Number: 4, Title: "Problem 4",
		MaxPoints: maxPoints, Position: 2,
	})
	if err != nil {
		t.Fatalf("CreateProblem (second problem): %v", err)
	}

	rr, subs, err := s.FileRegradeRequestV2(ctx, store.FileRegradeRequestParams{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade",
		Body:   "<p1>\nbase case wrong\n</p1>\n<p4>\nexchange argument\n</p4>",
		Status: "received", Kind: "filed", Turn: 1,
		Problems: []store.RequestProblemInput{
			{ProblemID: f.ProblemID, ComplaintText: "base case wrong"},
			{ProblemID: problem2.ID, ComplaintText: "exchange argument"},
		},
	})
	if err != nil {
		t.Fatalf("FileRegradeRequestV2: %v", err)
	}
	if rr.Kind != "filed" || !rr.Turn.Valid || rr.Turn.Int32 != 1 {
		t.Fatalf("unexpected request row: %+v", rr)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 sub-items returned, got %d", len(subs))
	}

	listed, err := s.ListRequestProblems(ctx, rr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("ListRequestProblems: got %d, want 2", len(listed))
	}
}

// TestFileRegradeRequestV2_SlotRace_LoserGetsUniqueViolation (Finding 2 + D57): the
// (publish_item_id, turn) WHERE kind='filed' partial unique index must still be the
// authority for the filing race under the new composed method — a second FileRegradeRequestV2
// call for the SAME (item, turn) must fail as IsUniqueViolation (the caller's existing
// "record an addendum instead" branch keys off exactly this), not silently succeed or
// deadlock, and must NOT leave a stray request row of its own behind.
func TestFileRegradeRequestV2_SlotRace_LoserGetsUniqueViolation(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	params := store.FileRegradeRequestParams{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nfirst\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
		Problems: []store.RequestProblemInput{{ProblemID: f.ProblemID, ComplaintText: "first"}},
	}
	if _, _, err := s.FileRegradeRequestV2(ctx, params); err != nil {
		t.Fatalf("first FileRegradeRequestV2: %v", err)
	}

	params.Body = "<p1>\nsecond, should lose the race\n</p1>"
	_, _, err := s.FileRegradeRequestV2(ctx, params)
	if err == nil {
		t.Fatal("expected the second filing at the same (item,turn) to fail, got nil")
	}
	if !store.IsUniqueViolation(err) {
		t.Fatalf("expected store.IsUniqueViolation(err) to be true, got err=%v", err)
	}

	listed, err := s.ListRegradeRequests(ctx, store.ListRegradeRequestsFilters{AssessmentID: f.AssessmentID, Kind: "filed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected exactly 1 filed row to survive the race, got %d", len(listed))
	}
}

// TestFileAndHandOffRegradeRequestV2_HappyPath_ReturnsHandedOffRow covers F5's normal
// path: the composed insert-filed-then-flip returns a row that is ALREADY kind
// 'handed_off' (not 'filed') — the caller never observes the intermediate filed state.
func TestFileAndHandOffRegradeRequestV2_HappyPath_ReturnsHandedOffRow(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	rr, subs, err := s.FileAndHandOffRegradeRequestV2(ctx, store.FileRegradeRequestParams{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nfinal reply\n</p1>",
		Status: "received", Kind: "filed", Turn: 4,
		Problems: []store.RequestProblemInput{{ProblemID: f.ProblemID, ComplaintText: "final reply"}},
	})
	if err != nil {
		t.Fatalf("FileAndHandOffRegradeRequestV2: %v", err)
	}
	if rr.Kind != "handed_off" {
		t.Fatalf("kind = %q, want handed_off (the flip must be visible in the returned row)", rr.Kind)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub-item, got %d", len(subs))
	}

	// No live 'filed' row must remain at this (item, turn) — the partial unique
	// index slot is freed by the flip within the same transaction.
	listedFiled, err := s.ListRegradeRequests(ctx, store.ListRegradeRequestsFilters{AssessmentID: f.AssessmentID, Kind: "filed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listedFiled) != 0 {
		t.Fatalf("expected NO surviving 'filed' row after the handoff flip, got %d: %+v", len(listedFiled), listedFiled)
	}
}

// TestFileAndHandOffRegradeRequestV2_FailureLeavesNoAdjudicableFiledRow is F5's core
// regression test: before this fix, a failure between the filed-insert and the flip
// (originally two separate calls) could leave a live kind='filed' row sitting at
// turn MAX+1 — which handleSendResult (no turn<=MAX guard) would treat as an ordinary
// adjudicable request and send an ordinary result email for, rather than the handoff
// having actually happened. This drives a failure INSIDE the same operation (a bad
// sub-item problem_id FK, exactly like TestFileRegradeRequestV2_AtomicAcrossRequestAndSubItems)
// and asserts the whole transaction — including the flip — rolls back: no request row
// survives at all, filed or handed_off, and the (item, turn) slot is left free for a
// legitimate retry.
func TestFileAndHandOffRegradeRequestV2_FailureLeavesNoAdjudicableFiledRow(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	_, _, err := s.FileAndHandOffRegradeRequestV2(ctx, store.FileRegradeRequestParams{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nok\n</p1>",
		Status: "received", Kind: "filed", Turn: 4,
		Problems: []store.RequestProblemInput{
			{ProblemID: f.ProblemID, ComplaintText: "valid"},
			{ProblemID: 999999999, ComplaintText: "bad FK, should roll back the WHOLE handoff"},
		},
	})
	if err == nil {
		t.Fatal("expected an FK-violation error inserting a nonexistent problem_id, got nil")
	}

	// No request row of ANY kind must have survived — not 'filed' (would be an
	// adjudicable-looking row at turn MAX+1) and not 'handed_off' (the flip never
	// should have committed either).
	listed, err := s.ListRegradeRequests(ctx, store.ListRegradeRequestsFilters{AssessmentID: f.AssessmentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("expected the failed handoff to leave NO regrade_requests row, got %d: %+v", len(listed), listed)
	}

	// The (item, turn) slot must be free: a legitimate retry at the same turn must
	// succeed and hand off cleanly.
	rr, _, err := s.FileAndHandOffRegradeRequestV2(ctx, store.FileRegradeRequestParams{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nok, retried\n</p1>",
		Status: "received", Kind: "filed", Turn: 4,
		Problems: []store.RequestProblemInput{{ProblemID: f.ProblemID, ComplaintText: "ok, retried"}},
	})
	if err != nil {
		t.Fatalf("FileAndHandOffRegradeRequestV2 retry at the same (item,turn) after a rolled-back attempt: %v", err)
	}
	if rr.Kind != "handed_off" {
		t.Fatalf("retry kind = %q, want handed_off", rr.Kind)
	}
}

// TestSetProblemVerdict_AndAIRecordAndError covers per-sub-item adjudication and AI
// linkage (spec §5): verdict/note/who/when round-trip; AI record links and clears a
// stale ai_error; ai_error persists and doesn't touch verdict/ai_record_id.
func TestSetProblemVerdict_AndAIRecordAndError(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)
	recordID := mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "6")

	ta, err := s.Q.CreateUser(ctx, db.CreateUserParams{
		Email: "ta-" + t.Name() + "@example.test", Role: "ta", Active: true,
	})
	if err != nil {
		t.Fatalf("CreateUser (ta): %v", err)
	}

	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nplease regrade\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}
	subItems, err := s.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{
		{ProblemID: f.ProblemID, ComplaintText: "please regrade"},
	})
	if err != nil {
		t.Fatalf("InsertRequestProblems: %v", err)
	}
	subItemID := subItems[0].ID

	if subItems[0].Verdict.Valid {
		t.Fatalf("expected no verdict yet, got %+v", subItems[0].Verdict)
	}

	// AI error persists and doesn't touch verdict/ai_record_id.
	failed, err := s.SetProblemAIError(ctx, subItemID, "AI unavailable — provider removed")
	if err != nil {
		t.Fatalf("SetProblemAIError: %v", err)
	}
	if !failed.AiError.Valid || failed.AiError.String != "AI unavailable — provider removed" {
		t.Fatalf("unexpected ai_error: %+v", failed.AiError)
	}
	if failed.Verdict.Valid || failed.AiRecordID.Valid {
		t.Fatalf("SetProblemAIError must only touch ai_error, got %+v", failed)
	}

	// AI record links and clears the stale error.
	linked, err := s.SetProblemAIRecord(ctx, subItemID, recordID)
	if err != nil {
		t.Fatalf("SetProblemAIRecord: %v", err)
	}
	if !linked.AiRecordID.Valid || linked.AiRecordID.Int64 != recordID {
		t.Fatalf("unexpected ai_record_id: %+v", linked.AiRecordID)
	}
	if linked.AiError.Valid {
		t.Fatalf("SetProblemAIRecord should clear ai_error, got %+v", linked.AiError)
	}

	// Verdict round-trips.
	verdicted, err := s.SetProblemVerdict(ctx, store.SetProblemVerdictParams{
		SubItemID: subItemID, Verdict: "regraded", Note: "rubric line 2 applied", VerdictBy: ta.ID,
	})
	if err != nil {
		t.Fatalf("SetProblemVerdict: %v", err)
	}
	if !verdicted.Verdict.Valid || verdicted.Verdict.String != "regraded" {
		t.Fatalf("unexpected verdict: %+v", verdicted.Verdict)
	}
	if verdicted.VerdictNote != "rubric line 2 applied" {
		t.Fatalf("unexpected verdict_note: %q", verdicted.VerdictNote)
	}
	if !verdicted.VerdictBy.Valid || verdicted.VerdictBy.Int64 != ta.ID {
		t.Fatalf("unexpected verdict_by: %+v", verdicted.VerdictBy)
	}
	if !verdicted.VerdictAt.Valid {
		t.Fatal("expected verdict_at to be set")
	}
	// Verdict must not disturb the AI linkage set earlier.
	if !verdicted.AiRecordID.Valid || verdicted.AiRecordID.Int64 != recordID {
		t.Fatalf("SetProblemVerdict must not touch ai_record_id, got %+v", verdicted.AiRecordID)
	}
}

// TestSetProblemVerdict_RejectsInvalidValue covers the database CHECK (migration
// 0025): verdict must be 'upheld' or 'regraded', nothing else.
func TestSetProblemVerdict_RejectsInvalidValue(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nx\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}
	subItems, err := s.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{
		{ProblemID: f.ProblemID, ComplaintText: "x"},
	})
	if err != nil {
		t.Fatalf("InsertRequestProblems: %v", err)
	}

	_, err = s.SetProblemVerdict(ctx, store.SetProblemVerdictParams{
		SubItemID: subItems[0].ID, Verdict: "denied", Note: "not a real outcome",
	})
	if err == nil {
		t.Fatal("expected a CHECK violation for an invalid verdict value, got nil")
	}
}

// TestAllProblemsVerdicted_TruthTable is the test-first coverage for
// AllProblemsVerdicted's truth table (task brief): zero sub-items is NOT
// all-verdicted (nothing to send); one verdicted + one pending is false; all
// verdicted is true; and it flips back to false if a new sub-item is added later
// (the TA escape-hatch, spec §5) even after the rest were already verdicted.
func TestAllProblemsVerdicted_TruthTable(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	maxPoints, err := store.Num("10")
	if err != nil {
		t.Fatal(err)
	}
	problem2, err := s.Q.CreateProblem(ctx, db.CreateProblemParams{
		AssessmentID: f.AssessmentID, Number: 4, Title: "Problem 4",
		MaxPoints: maxPoints, Position: 2,
	})
	if err != nil {
		t.Fatalf("CreateProblem (second problem): %v", err)
	}

	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade",
		Body:   "<p1>\nfirst\n</p1>\n<p4>\nsecond\n</p4>",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}

	// Zero sub-items: not all-verdicted.
	all, err := s.AllProblemsVerdicted(ctx, rr.ID)
	if err != nil {
		t.Fatalf("AllProblemsVerdicted (zero sub-items): %v", err)
	}
	if all {
		t.Fatal("a request with zero sub-items must not be considered all-verdicted")
	}

	subItems, err := s.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{
		{ProblemID: f.ProblemID, ComplaintText: "first"},
		{ProblemID: problem2.ID, ComplaintText: "second"},
	})
	if err != nil {
		t.Fatalf("InsertRequestProblems: %v", err)
	}

	// Two sub-items, neither verdicted: false.
	all, err = s.AllProblemsVerdicted(ctx, rr.ID)
	if err != nil {
		t.Fatalf("AllProblemsVerdicted (none verdicted): %v", err)
	}
	if all {
		t.Fatal("expected false with no sub-items verdicted")
	}

	// One verdicted, one pending: still false.
	if _, err := s.SetProblemVerdict(ctx, store.SetProblemVerdictParams{
		SubItemID: subItems[0].ID, Verdict: "upheld", Note: "confirmed",
	}); err != nil {
		t.Fatalf("SetProblemVerdict (first): %v", err)
	}
	all, err = s.AllProblemsVerdicted(ctx, rr.ID)
	if err != nil {
		t.Fatalf("AllProblemsVerdicted (one of two): %v", err)
	}
	if all {
		t.Fatal("expected false with one of two sub-items verdicted")
	}

	// Both verdicted: true.
	if _, err := s.SetProblemVerdict(ctx, store.SetProblemVerdictParams{
		SubItemID: subItems[1].ID, Verdict: "regraded", Note: "adjusted",
	}); err != nil {
		t.Fatalf("SetProblemVerdict (second): %v", err)
	}
	all, err = s.AllProblemsVerdicted(ctx, rr.ID)
	if err != nil {
		t.Fatalf("AllProblemsVerdicted (both verdicted): %v", err)
	}
	if !all {
		t.Fatal("expected true with both sub-items verdicted")
	}

	// A third sub-item added later (TA escape-hatch, unverdicted) flips it back to false.
	maxPoints3, err := store.Num("10")
	if err != nil {
		t.Fatal(err)
	}
	problem3, err := s.Q.CreateProblem(ctx, db.CreateProblemParams{
		AssessmentID: f.AssessmentID, Number: 7, Title: "Problem 7",
		MaxPoints: maxPoints3, Position: 3,
	})
	if err != nil {
		t.Fatalf("CreateProblem (third problem): %v", err)
	}
	if _, err := s.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{
		{ProblemID: problem3.ID, ComplaintText: "TA added this one manually"},
	}); err != nil {
		t.Fatalf("InsertRequestProblems (escape hatch): %v", err)
	}
	all, err = s.AllProblemsVerdicted(ctx, rr.ID)
	if err != nil {
		t.Fatalf("AllProblemsVerdicted (after escape-hatch add): %v", err)
	}
	if all {
		t.Fatal("expected false after adding a new unverdicted sub-item")
	}
}

// --- TA assignment (problem_ta_assignments, spec §6 D60) ---

// TestAssignProblemTA_UniquenessAndReplace covers the D60 uniqueness rule: at most
// one TA per problem, and assigning a new TA to an already-assigned problem REPLACES
// the row (ON CONFLICT DO UPDATE) rather than erroring or creating a second row.
func TestAssignProblemTA_UniquenessAndReplace(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	ta1, err := s.Q.CreateUser(ctx, db.CreateUserParams{Email: "ta1-" + t.Name() + "@example.test", Role: "ta", Active: true})
	if err != nil {
		t.Fatalf("CreateUser (ta1): %v", err)
	}
	ta2, err := s.Q.CreateUser(ctx, db.CreateUserParams{Email: "ta2-" + t.Name() + "@example.test", Role: "ta", Active: true})
	if err != nil {
		t.Fatalf("CreateUser (ta2): %v", err)
	}
	lecturer, err := s.Q.CreateUser(ctx, db.CreateUserParams{Email: "lecturer-" + t.Name() + "@example.test", Role: "lecturer", Active: true})
	if err != nil {
		t.Fatalf("CreateUser (lecturer): %v", err)
	}

	assigned, err := s.AssignProblemTA(ctx, f.ProblemID, ta1.ID, lecturer.ID)
	if err != nil {
		t.Fatalf("AssignProblemTA (first): %v", err)
	}
	if assigned.UserID != ta1.ID {
		t.Fatalf("user_id = %d, want %d", assigned.UserID, ta1.ID)
	}

	got, err := s.GetProblemTA(ctx, f.ProblemID)
	if err != nil {
		t.Fatalf("GetProblemTA: %v", err)
	}
	if got.UserID != ta1.ID {
		t.Fatalf("GetProblemTA user_id = %d, want %d", got.UserID, ta1.ID)
	}

	// Re-assigning replaces, not duplicates.
	replaced, err := s.AssignProblemTA(ctx, f.ProblemID, ta2.ID, lecturer.ID)
	if err != nil {
		t.Fatalf("AssignProblemTA (replace): %v", err)
	}
	if replaced.UserID != ta2.ID {
		t.Fatalf("user_id after replace = %d, want %d", replaced.UserID, ta2.ID)
	}
	if replaced.ID != assigned.ID {
		t.Fatalf("replace should update the same row (id %d), got a new id %d", assigned.ID, replaced.ID)
	}

	list, err := s.ListTAAssignments(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("ListTAAssignments: %v", err)
	}
	found := 0
	for _, row := range list {
		if row.ProblemID == f.ProblemID {
			found++
			if !row.UserID.Valid || row.UserID.Int64 != ta2.ID {
				t.Fatalf("ListTAAssignments row for problem = %+v, want ta2", row)
			}
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one assignment row for the problem, got %d", found)
	}
}

// TestAssignProblemTA_OneTAManyProblems covers the other half of D60: one TA may own
// many problems (the UNIQUE constraint is on problem_id alone, not composite).
func TestAssignProblemTA_OneTAManyProblems(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	maxPoints, err := store.Num("10")
	if err != nil {
		t.Fatal(err)
	}
	problem2, err := s.Q.CreateProblem(ctx, db.CreateProblemParams{
		AssessmentID: f.AssessmentID, Number: 4, Title: "Problem 4",
		MaxPoints: maxPoints, Position: 2,
	})
	if err != nil {
		t.Fatalf("CreateProblem (second problem): %v", err)
	}

	ta, err := s.Q.CreateUser(ctx, db.CreateUserParams{Email: "ta-" + t.Name() + "@example.test", Role: "ta", Active: true})
	if err != nil {
		t.Fatalf("CreateUser (ta): %v", err)
	}

	if _, err := s.AssignProblemTA(ctx, f.ProblemID, ta.ID, 0); err != nil {
		t.Fatalf("AssignProblemTA (problem 1): %v", err)
	}
	if _, err := s.AssignProblemTA(ctx, problem2.ID, ta.ID, 0); err != nil {
		t.Fatalf("AssignProblemTA (problem 2), same TA on a second problem: %v", err)
	}

	list, err := s.ListTAAssignments(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("ListTAAssignments: %v", err)
	}
	assignedToTA := 0
	unassigned := 0
	for _, row := range list {
		if row.UserID.Valid && row.UserID.Int64 == ta.ID {
			assignedToTA++
		}
		if !row.UserID.Valid {
			unassigned++
		}
	}
	if assignedToTA != 2 {
		t.Fatalf("expected the TA assigned to 2 problems, got %d", assignedToTA)
	}
}

// TestListTAAssignments_UnassignedProblemsAppearWithNullTA covers the "no TA
// assigned" visibility requirement (spec §6: lecturer-visible flag, publish preview
// warning) — a problem with no assignment row must still appear in the list, not be
// silently omitted.
func TestListTAAssignments_UnassignedProblemsAppearWithNullTA(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	list, err := s.ListTAAssignments(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("ListTAAssignments: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 problem (unassigned), got %d", len(list))
	}
	if list[0].UserID.Valid {
		t.Fatalf("expected NULL user_id for an unassigned problem, got %+v", list[0].UserID)
	}
}

// TestRemoveProblemTA covers unassignment (spec §6 D60 UI) and the no-op-when-absent
// case.
func TestRemoveProblemTA(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	ta, err := s.Q.CreateUser(ctx, db.CreateUserParams{Email: "ta-" + t.Name() + "@example.test", Role: "ta", Active: true})
	if err != nil {
		t.Fatalf("CreateUser (ta): %v", err)
	}
	if _, err := s.AssignProblemTA(ctx, f.ProblemID, ta.ID, 0); err != nil {
		t.Fatalf("AssignProblemTA: %v", err)
	}

	if err := s.RemoveProblemTA(ctx, f.ProblemID); err != nil {
		t.Fatalf("RemoveProblemTA: %v", err)
	}
	if _, err := s.GetProblemTA(ctx, f.ProblemID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetProblemTA after remove: expected pgx.ErrNoRows, got %v", err)
	}

	// Removing again (already absent) must not error.
	if err := s.RemoveProblemTA(ctx, f.ProblemID); err != nil {
		t.Fatalf("RemoveProblemTA (already absent) should be a no-op, got: %v", err)
	}
}

// TestInsertRegradeRequestV2_FiledRequiresSlot_GoGuard covers the Go-layer belt (review
// finding, filed-row slot invariant): InsertRegradeRequestV2 maps Turn/PublishItemID
// with `Valid: != 0`, so a caller passing Kind: "filed" with Turn 0 (or
// PublishItemID 0) would otherwise silently insert NULLs into the columns the partial
// unique index (publish_item_id, turn) WHERE kind='filed' (migration 0025) keys on --
// and Postgres treats NULLs as distinct, so two such rows would both succeed, defeating
// the D57 race-killer for a buggy caller. The Go layer must reject this before it ever
// reaches the database.
func TestInsertRegradeRequestV2_FiledRequiresSlot_GoGuard(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	t.Run("turn zero", func(t *testing.T) {
		_, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
			PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
			FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nplease regrade\n</p1>",
			Status: "received", Kind: "filed", Turn: 0,
		})
		if err == nil {
			t.Fatal("expected an error for Kind=filed with Turn=0, got nil")
		}
	})

	t.Run("publish item id zero", func(t *testing.T) {
		_, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
			PublishItemID: 0, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
			FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nplease regrade\n</p1>",
			Status: "received", Kind: "filed", Turn: 1,
		})
		if err == nil {
			t.Fatal("expected an error for Kind=filed with PublishItemID=0, got nil")
		}
	})

	t.Run("both zero", func(t *testing.T) {
		_, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
			PublishItemID: 0, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
			FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nplease regrade\n</p1>",
			Status: "received", Kind: "filed", Turn: 0,
		})
		if err == nil {
			t.Fatal("expected an error for Kind=filed with both PublishItemID=0 and Turn=0, got nil")
		}
	})

	// Sanity: the guard must not reject legitimate non-filed rows with zero
	// turn/publish_item_id (e.g. a token that never parsed).
	if _, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: 0, StudentID: 0, AssessmentID: 0,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "no tags here",
		Status: "received", Kind: "unparsed", Turn: 0,
	}); err != nil {
		t.Fatalf("InsertRegradeRequestV2 (unparsed, zero turn/item) should not be rejected by the filed guard: %v", err)
	}
}

// TestRegradeRequestsFiledNeedsSlotCheck_DB covers the DB-layer belt-and-suspenders
// (review finding): migration 0025's CHECK constraint
// regrade_requests_filed_needs_slot rejects kind='filed' rows with a NULL
// publish_item_id or turn directly at the database, independent of the Go guard in
// InsertRegradeRequestV2. This bypasses the Go guard by calling the sqlc layer (s.Q)
// directly, so it must be exercised even if the Go guard above already prevents the
// same caller mistake -- the CHECK is the actual self-enforcing invariant; the Go guard
// is just a clearer, earlier error for the common case.
func TestRegradeRequestsFiledNeedsSlotCheck_DB(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	t.Run("null turn", func(t *testing.T) {
		_, err := s.Q.InsertRegradeRequestV2(ctx, db.InsertRegradeRequestV2Params{
			PublishItemID: pgtype.Int8{Int64: itemID, Valid: true},
			StudentID:     pgtype.Int8{Int64: f.StudentID, Valid: true},
			AssessmentID:  pgtype.Int8{Int64: f.AssessmentID, Valid: true},
			FromEmail:     "student@example.test",
			Subject:       "re: grade",
			Body:          "<p1>\nplease regrade\n</p1>",
			Status:        "received",
			Kind:          "filed",
			Turn:          pgtype.Int4{Valid: false}, // NULL turn on a filed row
		})
		if err == nil {
			t.Fatal("expected the CHECK constraint to reject a filed row with NULL turn, got nil")
		}
	})

	t.Run("null publish item id", func(t *testing.T) {
		_, err := s.Q.InsertRegradeRequestV2(ctx, db.InsertRegradeRequestV2Params{
			PublishItemID: pgtype.Int8{Valid: false}, // NULL publish_item_id on a filed row
			StudentID:     pgtype.Int8{Int64: f.StudentID, Valid: true},
			AssessmentID:  pgtype.Int8{Int64: f.AssessmentID, Valid: true},
			FromEmail:     "student@example.test",
			Subject:       "re: grade",
			Body:          "<p1>\nplease regrade\n</p1>",
			Status:        "received",
			Kind:          "filed",
			Turn:          pgtype.Int4{Int32: 1, Valid: true},
		})
		if err == nil {
			t.Fatal("expected the CHECK constraint to reject a filed row with NULL publish_item_id, got nil")
		}
	})

	t.Run("both null", func(t *testing.T) {
		_, err := s.Q.InsertRegradeRequestV2(ctx, db.InsertRegradeRequestV2Params{
			PublishItemID: pgtype.Int8{Valid: false},
			StudentID:     pgtype.Int8{Valid: false},
			AssessmentID:  pgtype.Int8{Valid: false},
			FromEmail:     "student@example.test",
			Subject:       "re: grade",
			Body:          "<p1>\nplease regrade\n</p1>",
			Status:        "received",
			Kind:          "filed",
			Turn:          pgtype.Int4{Valid: false},
		})
		if err == nil {
			t.Fatal("expected the CHECK constraint to reject a filed row with both NULL, got nil")
		}
	})

	// Sanity: a legitimate non-filed row with both NULL must still be accepted by the
	// CHECK (it only constrains kind='filed').
	if _, err := s.Q.InsertRegradeRequestV2(ctx, db.InsertRegradeRequestV2Params{
		PublishItemID: pgtype.Int8{Valid: false},
		StudentID:     pgtype.Int8{Valid: false},
		AssessmentID:  pgtype.Int8{Valid: false},
		FromEmail:     "student@example.test",
		Subject:       "re: grade",
		Body:          "no tags here",
		Status:        "received",
		Kind:          "unparsed",
		Turn:          pgtype.Int4{Valid: false},
	}); err != nil {
		t.Fatalf("CHECK constraint should not reject an unparsed row with NULL publish_item_id/turn: %v", err)
	}
}
