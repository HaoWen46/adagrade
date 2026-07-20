package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/store"
)

// blobRefQuery names one SELECT that enumerates a single blob-ref column from the
// schema. table/column are for the report only; sql must return one text value per
// row (NULLs already filtered) — the blob store key.
//
// This list must stay in sync with every *_ref TEXT column across migrations/*.sql
// (grep: `grep -nE '_ref\s+TEXT' migrations/*.sql`). As of this writing (migration
// 0029, page-level scan intake) that's: submissions.source_ref,
// upload_quarantine.pdf_ref, answer_pages.image_ref, answer_pages.masked_image_ref,
// scan_batches.source_ref, scan_sources.source_ref, scan_pages.image_ref,
// scan_pages.student_id_crop_ref, scan_pages.name_crop_ref,
// scan_pages.problem_crop_ref, direct_uploads.source_ref. Publish/regrade/pricing/
// spot-check migrations (0016-0019) carry no blob refs — they're metadata/
// snapshot-only.
type blobRefQuery struct {
	table, column, sql string
}

var blobRefQueries = []blobRefQuery{
	{"submissions", "source_ref", `SELECT source_ref FROM submissions WHERE source_ref IS NOT NULL`},
	{"upload_quarantine", "pdf_ref", `SELECT pdf_ref FROM upload_quarantine WHERE pdf_ref IS NOT NULL`},
	{"answer_pages", "image_ref", `SELECT image_ref FROM answer_pages WHERE image_ref IS NOT NULL`},
	{"answer_pages", "masked_image_ref", `SELECT masked_image_ref FROM answer_pages WHERE masked_image_ref IS NOT NULL`},
	{"scan_batches", "source_ref", `SELECT source_ref FROM scan_batches WHERE source_ref IS NOT NULL`},
	{"scan_sources", "source_ref", `SELECT source_ref FROM scan_sources WHERE source_ref IS NOT NULL`},
	{"scan_pages", "image_ref", `SELECT image_ref FROM scan_pages WHERE image_ref IS NOT NULL`},
	{"scan_pages", "student_id_crop_ref", `SELECT student_id_crop_ref FROM scan_pages WHERE student_id_crop_ref IS NOT NULL`},
	{"scan_pages", "name_crop_ref", `SELECT name_crop_ref FROM scan_pages WHERE name_crop_ref IS NOT NULL`},
	{"scan_pages", "problem_crop_ref", `SELECT problem_crop_ref FROM scan_pages WHERE problem_crop_ref IS NOT NULL`},
	{"direct_uploads", "source_ref", `SELECT source_ref FROM direct_uploads WHERE source_ref IS NOT NULL`},
}

// missingRef is one blob reference whose key has no matching file in the blob store.
type missingRef struct {
	table, column, key string
}

// verifyBlobRefs walks every blob-ref column in blobRefQueries, checks each
// referenced key against blobs.Exists, and returns the ones that are missing. It
// does not touch the HTTP server or River workers — a plain read-only DB + blob-store
// pass, safe to run against a live database.
func verifyBlobRefs(ctx context.Context, pool *pgxpool.Pool, blobs blobstore.Store) ([]missingRef, int, error) {
	var missing []missingRef
	checked := 0
	for _, q := range blobRefQueries {
		rows, err := pool.Query(ctx, q.sql)
		if err != nil {
			return nil, checked, fmt.Errorf("verify-blobs: query %s.%s: %w", q.table, q.column, err)
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				var key string
				if err := rows.Scan(&key); err != nil {
					return fmt.Errorf("verify-blobs: scan %s.%s: %w", q.table, q.column, err)
				}
				checked++
				ok, err := blobs.Exists(ctx, key)
				if err != nil {
					return fmt.Errorf("verify-blobs: exists check for %s.%s=%q: %w", q.table, q.column, key, err)
				}
				if !ok {
					missing = append(missing, missingRef{table: q.table, column: q.column, key: key})
				}
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, checked, err
		}
	}
	return missing, checked, nil
}

// maxReportedMissingKeys caps how many missing keys -verify-blobs prints; the count
// is always exact, only the listed keys are truncated (Task U3: "first 20 keys").
const maxReportedMissingKeys = 20

// runVerifyBlobs implements `adamarker -verify-blobs` (Task U3, docs/OPERATIONS.md
// §5 step 5): connects to the configured DB and blob store, walks every blob
// reference, and reports what's missing. Keys are content paths, not student PII
// (e.g. "assessments/3/submissions/7.pdf"), so printing them is fine — see
// blobstore.Store's key contract. It deliberately does NOT run migrations, start
// River workers, or bind the HTTP server: this is a read-only, single-shot check
// meant to run standalone (including against a live production DB mid-restore).
func runVerifyBlobs(ctx context.Context, cfg config.Config, out io.Writer, logger *slog.Logger) int {
	if cfg.DatabaseURL == "" {
		fmt.Fprintln(out, "verify-blobs: ADAMARKER_DATABASE_URL is not set")
		return 1
	}
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(out, "verify-blobs: connect: %v\n", err)
		return 1
	}
	defer st.Close()

	blobs, err := blobstore.NewLocalDisk(cfg.BlobDir)
	if err != nil {
		fmt.Fprintf(out, "verify-blobs: open blob store %q: %v\n", cfg.BlobDir, err)
		return 1
	}

	missing, checked, err := verifyBlobRefs(ctx, st.Pool, blobs)
	if err != nil {
		fmt.Fprintf(out, "verify-blobs: %v\n", err)
		return 1
	}

	fmt.Fprintf(out, "verify-blobs: checked %d blob reference(s) across %d column(s)\n", checked, len(blobRefQueries))
	if len(missing) == 0 {
		fmt.Fprintln(out, "verify-blobs: OK — no missing blobs")
		return 0
	}

	fmt.Fprintf(out, "verify-blobs: FAIL — %d missing blob(s)\n", len(missing))
	shown := missing
	if len(shown) > maxReportedMissingKeys {
		shown = shown[:maxReportedMissingKeys]
	}
	for _, m := range shown {
		fmt.Fprintf(out, "  missing: %s.%s = %s\n", m.table, m.column, m.key)
	}
	if len(missing) > len(shown) {
		fmt.Fprintf(out, "  ... and %d more\n", len(missing)-len(shown))
	}
	if logger != nil {
		logger.Warn("verify-blobs: missing blob references", "count", len(missing))
	}
	return 1
}
