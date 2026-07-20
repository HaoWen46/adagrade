package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

func TestUpsertModelPricing_RoundTripAndEdit(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)

	provider, err := s.Q.CreateProvider(ctx, db.CreateProviderParams{
		Name: "anthropic", Kind: "anthropic-compat", BaseUrl: "https://api.example.test",
		ApiKeyCiphertext: []byte("ct"), Models: []string{"claude-x"},
		RequestsPerSecond: 1, Burst: 2,
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	got, err := s.UpsertModelPricing(ctx, provider.ID, "claude-x", "3.00", "15.00")
	if err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}
	if store.NumStr(got.InputUsdPerMtok) != "3" || store.NumStr(got.OutputUsdPerMtok) != "15" {
		t.Fatalf("unexpected pricing: %+v", got)
	}

	// Editing is an upsert on (provider_id, model), not a new row.
	if _, err := s.UpsertModelPricing(ctx, provider.ID, "claude-x", "2.50", "12.00"); err != nil {
		t.Fatalf("UpsertModelPricing (edit): %v", err)
	}
	list, err := s.ListModelPricing(ctx, provider.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListModelPricing: got %d err %v", len(list), err)
	}
	if store.NumStr(list[0].InputUsdPerMtok) != "2.5" {
		t.Fatalf("edit did not take effect: %+v", list[0])
	}
}

func TestUpsertModelPricing_RejectsInvalidDecimal(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	provider, err := s.Q.CreateProvider(ctx, db.CreateProviderParams{
		Name: "p", Kind: "anthropic-compat", BaseUrl: "https://x.test",
		ApiKeyCiphertext: []byte("ct"), Models: []string{"m"}, RequestsPerSecond: 1, Burst: 2,
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := s.UpsertModelPricing(ctx, provider.ID, "m", "not-a-number", "1"); err == nil {
		t.Fatalf("expected error for invalid input price")
	}
}

func TestMonthToDateCost_SumsModelRecordsOnly(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	run := mustRun(t, s, f)

	zero, err := s.MonthToDateCost(ctx)
	if err != nil {
		t.Fatalf("MonthToDateCost (empty): %v", err)
	}
	if zero != "0" {
		t.Fatalf("expected 0 spend with no records, got %q", zero)
	}

	mustModelRecord(t, s, run.ID, f.AnswerID, f.RubricVersionID, f.MethodVersionID, "8", 1500, 400, "0.012")
	// A record with no pricing at insert time -> NULL cost_usd -> contributes 0.
	mustModelRecord(t, s, run.ID, f.AnswerID, f.RubricVersionID, f.MethodVersionID, "7", 1500, 400, "")

	total, err := s.MonthToDateCost(ctx)
	if err != nil {
		t.Fatalf("MonthToDateCost: %v", err)
	}
	if total != "0.012" {
		t.Fatalf("MonthToDateCost = %q, want 0.012 (NULL-cost record should contribute 0, not error)", total)
	}

	// A human record's cost must never be counted (source not in ('model','regrade_ai')).
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "9")
	total, err = s.MonthToDateCost(ctx)
	if err != nil {
		t.Fatalf("MonthToDateCost after human record: %v", err)
	}
	if total != "0.012" {
		t.Fatalf("MonthToDateCost changed after a human record was inserted: got %q", total)
	}

	// A regrade_ai record's cost (provider spend for the stricter AI re-grade, spec §8)
	// MUST count toward month-to-date — it is real provider spend, same as a run leaf.
	mustRegradeAIRecord(t, s, f.AnswerID, f.RubricVersionID, "0.034")
	total, err = s.MonthToDateCost(ctx)
	if err != nil {
		t.Fatalf("MonthToDateCost after regrade_ai record: %v", err)
	}
	if total != "0.046" {
		t.Fatalf("MonthToDateCost = %q, want 0.046 (0.012 model + 0.034 regrade_ai)", total)
	}
}

// TestMonthToDateCost_UTCMonthBoundary pins the boundary to genuine UTC, not the
// session TimeZone: a record stamped one second before UTC month-start must be
// excluded, and one stamped exactly at UTC month-start must be included. The query
// runs over a second connection whose session TimeZone is pinned to a non-UTC zone
// (America/New_York, -4 or -5 from UTC) via a `options=-c TimeZone=...` DSN param —
// applied at connection-open time so it holds for every connection pgx pulls from that
// pool, unlike a per-statement `SET TIME ZONE` which pgxpool could route to a
// different underlying connection on the next query. A naive
// `date_trunc('month', now() AT TIME ZONE 'UTC')` comparison strips the offset and
// gets it reinterpreted in that session zone, drifting the boundary by hours and
// misclassifying one of the two rows.
func TestMonthToDateCost_UTCMonthBoundary(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	run := mustRun(t, s, f)

	before := mustModelRecord(t, s, run.ID, f.AnswerID, f.RubricVersionID, f.MethodVersionID, "8", 100, 50, "1.000000")
	after := mustModelRecord(t, s, run.ID, f.AnswerID, f.RubricVersionID, f.MethodVersionID, "7", 100, 50, "2.000000")

	// Stamp `before` one second before the current UTC month's start, and `after`
	// exactly at the current UTC month's start.
	if _, err := s.Pool.Exec(ctx,
		`UPDATE grading_records SET created_at = (date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') - interval '1 second' WHERE id = $1`,
		before.ID); err != nil {
		t.Fatalf("stamp before-boundary record: %v", err)
	}
	if _, err := s.Pool.Exec(ctx,
		`UPDATE grading_records SET created_at = (date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') WHERE id = $1`,
		after.ID); err != nil {
		t.Fatalf("stamp at-boundary record: %v", err)
	}

	tzDSN := nonUTCSessionDSN(t)
	tzStore, err := store.New(ctx, tzDSN)
	if err != nil {
		t.Fatalf("connect with non-UTC session TimeZone: %v", err)
	}
	t.Cleanup(tzStore.Close)

	total, err := tzStore.MonthToDateCost(ctx)
	if err != nil {
		t.Fatalf("MonthToDateCost: %v", err)
	}
	if total != "2" {
		t.Fatalf("MonthToDateCost = %q, want 2 (only the at-or-after-UTC-month-start record counted)", total)
	}
}

// nonUTCSessionDSN returns the integration-test DSN with a `-c TimeZone=...` libpq
// connection option appended, so every connection opened against it starts in a
// non-UTC session TimeZone (America/New_York) rather than the cluster default (UTC).
func nonUTCSessionDSN(t *testing.T) string {
	t.Helper()
	dsn := storetest.DSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "options=-c%20TimeZone%3DAmerica%2FNew_York"
}

func TestRunCost_SumsTokensAndCostForOneRun(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	runA := mustRun(t, s, f)
	runB := mustRun(t, s, f)

	mustModelRecord(t, s, runA.ID, f.AnswerID, f.RubricVersionID, f.MethodVersionID, "8", 1500, 400, "0.012")
	mustModelRecord(t, s, runB.ID, f.AnswerID, f.RubricVersionID, f.MethodVersionID, "7", 999, 111, "0.5")

	costA, err := s.RunCost(ctx, runA.ID)
	if err != nil {
		t.Fatalf("RunCost(runA): %v", err)
	}
	if costA.TotalUSD != "0.012" || costA.InputTokens != 1500 || costA.OutputTokens != 400 {
		t.Fatalf("unexpected RunCost for runA: %+v", costA)
	}

	costB, err := s.RunCost(ctx, runB.ID)
	if err != nil {
		t.Fatalf("RunCost(runB): %v", err)
	}
	if costB.TotalUSD != "0.5" {
		t.Fatalf("unexpected RunCost for runB: %+v", costB)
	}
}
