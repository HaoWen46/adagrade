package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// riverJobSeed describes an email.send river_job row to plant for one publish
// item before 0036's Up backfill runs, so the migration's job-matcher (A2-
// migration) has something concrete to match against.
type riverJobSeed struct {
	state   string
	attempt int16
}

func TestMigration0036_PreservesTerminalStateAndQuarantinesLegacyPending(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)
	f := mustFixture(t, s)

	var batchID int64
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO publish_batches (assessment_id) VALUES ($1) RETURNING id`,
		f.AssessmentID,
	).Scan(&batchID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		seedStatus string // pre-0036 email_status (must be a value the old check constraint allows)
		job        *riverJobSeed
		wantStatus string
	}{
		{"legacy pending, no job at all", "pending", nil, "uncertain"},
		{"legacy sent stays sent", "sent", nil, "sent"},
		{"legacy failed stays failed", "failed", nil, "failed"},
		{"legacy skipped stays skipped", "skipped", nil, "skipped"},
		// A2-migration: an unattempted, still-runnable queued job proves the
		// crash happened before River ever picked the job up — restarting
		// River finishes the original send, so the item is not quarantined.
		{"legacy pending, unattempted queued job", "pending", &riverJobSeed{state: "available", attempt: 0}, "pending"},
		{"legacy pending, unattempted retryable job", "pending", &riverJobSeed{state: "retryable", attempt: 0}, "pending"},
		// A job that has been attempted at least once cannot prove the
		// provider was never called (River marks attempt>0 before running
		// the worker), so this still fails closed to uncertain.
		{"legacy pending, attempted retryable job", "pending", &riverJobSeed{state: "retryable", attempt: 1}, "uncertain"},
		{"legacy pending, no matching job", "pending", nil, "uncertain"},
	}

	studentIDs := make([]int64, len(cases))
	for i := range cases {
		var id int64
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO students (student_id, name, email)
			VALUES ($1, 'Synthetic Student', $2) RETURNING id`,
			fmt.Sprintf("%s-student-%d", t.Name(), i),
			fmt.Sprintf("delivery-migration-%d@example.test", i),
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		studentIDs[i] = id
	}

	itemIDs := make([]int64, len(cases))
	for i, tc := range cases {
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO publish_items
				(batch_id, student_id, snapshot, recipient_email, email_status)
			VALUES ($1, $2, '{}', $3, $4) RETURNING id`,
			batchID, studentIDs[i], fmt.Sprintf("legacy-%d@example.test", i), tc.seedStatus,
		).Scan(&itemIDs[i]); err != nil {
			t.Fatal(err)
		}
	}

	// river_job lives in River's own schema (provisioned once by
	// storetest.Fresh), independent of goose — it is unaffected by the
	// MigrateDownTo/MigrateUpTo round trip below, so seeding it here (against
	// the real item IDs) exercises exactly what a real crash-mid-publish
	// would have left behind.
	for i, tc := range cases {
		if tc.job == nil {
			continue
		}
		args := fmt.Sprintf(`{"item_id": %d, "email_generation": 1}`, itemIDs[i])
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO river_job (kind, args, state, attempt, max_attempts, queue)
			VALUES ('email.send', $1::jsonb, $2, $3, 25, 'email')`,
			args, tc.job.state, tc.job.attempt,
		); err != nil {
			t.Fatalf("seed river_job for %q: %v", tc.name, err)
		}
	}

	// Recreate the actual deployed pre-0036 shape, then apply the migration under
	// test. The publish_items rows above are only fixture setup; their delivery
	// metadata is deliberately discarded by the down migration.
	if err := store.MigrateDownTo(ctx, dsn, 35); err != nil {
		t.Fatalf("migrate down to 0035: %v", err)
	}
	if err := store.MigrateUpTo(ctx, dsn, 36); err != nil {
		t.Fatalf("migrate up to 0036: %v", err)
	}

	for i, tc := range cases {
		var (
			status     string
			generation int32
			key        pgtype.UUID
			jobID      pgtype.Int8
			stateAt    pgtype.Timestamptz
		)
		if err := s.Pool.QueryRow(ctx, `
			SELECT email_status, email_generation, delivery_key,
			       delivery_job_id, delivery_state_at
			FROM publish_items WHERE id = $1`, itemIDs[i],
		).Scan(&status, &generation, &key, &jobID, &stateAt); err != nil {
			t.Fatal(err)
		}
		if status != tc.wantStatus {
			t.Errorf("%s: status migrated to %q, want %q", tc.name, status, tc.wantStatus)
		}
		if generation != 1 || !key.Valid || jobID.Valid || !stateAt.Valid {
			t.Errorf("%s: metadata = generation %d key %+v job %+v state_at %+v",
				tc.name, generation, key, jobID, stateAt)
		}
	}
}

// TestMigration0036_FreshDatabaseHasNoRiverJobTable proves the A2-migration
// job-matcher degrades safely on a genuinely fresh install: goose migrations
// run at app startup, before River creates its own schema (CLAUDE.md), so
// to_regclass('river_job') is NULL the very first time 0036 runs there.
// Without the to_regclass guard, referencing river_job in that state raises
// "relation river_job does not exist" and startup migration fails outright —
// this test deliberately skips River's migrator (unlike storetest.Fresh) so
// river_job truly does not exist when goose reaches 0036.
func TestMigration0036_FreshDatabaseHasNoRiverJobTable(t *testing.T) {
	ctx := context.Background()
	dsn := storetest.DSN(t)

	s, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	// Same cross-package serialization storetest.Fresh uses: this test drops
	// the schema the whole test database shares.
	lock, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock conn: %v", err)
	}
	if _, err := lock.Exec(ctx, "SELECT pg_advisory_lock(hashtext('adamarker-test-db'))"); err != nil {
		lock.Release()
		t.Fatalf("advisory lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = lock.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext('adamarker-test-db'))")
		lock.Release()
	})

	if _, err := s.Pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	var exists bool
	if err := s.Pool.QueryRow(ctx, "SELECT to_regclass('river_job') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatalf("check river_job before migrating: %v", err)
	}
	if exists {
		t.Fatalf("test setup invariant broken: river_job already exists before goose ran")
	}

	// Deliberately skip River's own migrator: on a real first boot, goose
	// runs first and river_job does not exist yet.
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("RunMigrations with no river_job table present: %v", err)
	}
}
