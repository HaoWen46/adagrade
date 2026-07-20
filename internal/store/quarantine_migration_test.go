package store_test

import (
	"context"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// Migration 0034 widens the long-lived quarantine reason CHECK for image intake.
// Exercise the deployed upgrade shape (0033 -> 0034), then its data-safe downgrade
// and re-upgrade rather than relying only on a fresh-schema test.
func TestMigration0034_QuarantineAllowsInvalidImage(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)

	if err := store.MigrateDownTo(ctx, dsn, 33); err != nil {
		t.Fatalf("migrate down to 0033: %v", err)
	}
	// At schema 0033 the generated latest-schema CreateAssessment RETURNING list
	// includes columns introduced later. Seed with version-appropriate SQL so this
	// historical migration test remains valid as new columns are added.
	var assessmentID int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO assessments (kind, name) VALUES ('exam', 'Migration fixture') RETURNING id`,
	).Scan(&assessmentID); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUpTo(ctx, dsn, 34); err != nil {
		t.Fatalf("migrate up to 0034: %v", err)
	}

	var id int64
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO upload_quarantine
			(assessment_id, original_filename, pdf_ref, pdf_sha256, reason)
		VALUES ($1, 'broken.png', 'quarantine/broken.png', 'broken', 'invalid_image')
		RETURNING id`, assessmentID).Scan(&id); err != nil {
		t.Fatalf("insert invalid_image after 0034: %v", err)
	}

	if err := store.MigrateDownTo(ctx, dsn, 33); err != nil {
		t.Fatalf("migrate down to 0033 with invalid_image row: %v", err)
	}
	var reason string
	if err := s.Pool.QueryRow(ctx, "SELECT reason FROM upload_quarantine WHERE id = $1", id).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "invalid_pdf" {
		t.Fatalf("0034 down reason = %q, want invalid_pdf", reason)
	}
	if err := store.MigrateUpTo(ctx, dsn, 34); err != nil {
		t.Fatalf("re-apply 0034: %v", err)
	}
}
