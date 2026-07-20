package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// verifyFixture seeds one row referencing a real, existing blob in EVERY table
// verifyBlobRefs walks (submissions, upload_quarantine, answer_pages x2,
// scan_batches, scan_sources, scan_pages x4, direct_uploads) — 11 ref columns
// total, matching blobRefQueries exactly. blobs is rooted at blobDir so the
// caller controls the directory (needed to also exercise runVerifyBlobs, which
// re-derives its own blobstore.LocalDisk from cfg.BlobDir). Returns the pool and
// the seeded keys, in the same table.column order as blobRefQueries.
func verifyFixture(t *testing.T, blobDir string) (pool *pgxpool.Pool, blobs blobstore.Store, keys []string) {
	t.Helper()
	st := storetest.Fresh(t)
	ctx := context.Background()

	bs, err := blobstore.NewLocalDisk(blobDir)
	if err != nil {
		t.Fatalf("blobstore: %v", err)
	}

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	mustExecReturningID := func(sql string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := st.Pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
		return id
	}
	putBlob := func(key string) string {
		t.Helper()
		if _, _, err := bs.Put(ctx, key, strings.NewReader("fixture bytes for "+key)); err != nil {
			t.Fatalf("put %q: %v", key, err)
		}
		return key
	}

	// Roster + assessment/problem scaffolding shared by every ref below. IDs are
	// GENERATED ALWAYS AS IDENTITY (no explicit values allowed), so every insert
	// comes back through RETURNING id.
	studentID := mustExecReturningID(`INSERT INTO students (student_id, name, email) VALUES ('b01', 'Student One', 's1@x.edu') RETURNING id`)
	assessmentID := mustExecReturningID(`INSERT INTO assessments (kind, name) VALUES ('exam', 'Fixture Exam') RETURNING id`)
	problemID := mustExecReturningID(`INSERT INTO problems (assessment_id, number, max_points, position) VALUES ($1, 1, 10, 0) RETURNING id`, assessmentID)

	// submissions.source_ref
	subKey := putBlob("submissions/1/source.pdf")
	submissionID := mustExecReturningID(`INSERT INTO submissions (assessment_id, student_id, original_filename, source_ref, source_sha256, page_count, source_kind)
		VALUES ($1, $2, 'sub.pdf', $3, 'deadbeef', 1, 'pdf') RETURNING id`, assessmentID, studentID, subKey)

	// upload_quarantine.pdf_ref
	quarKey := putBlob("quarantine/1.pdf")
	mustExec(`INSERT INTO upload_quarantine (assessment_id, original_filename, pdf_ref, pdf_sha256, reason)
		VALUES ($1, 'q.pdf', $2, 'deadbeef', 'unknown_student')`, assessmentID, quarKey)

	// answer_pages.image_ref + masked_image_ref
	answerID := mustExecReturningID(`INSERT INTO answers (assessment_id, student_id, problem_id) VALUES ($1, $2, $3) RETURNING id`,
		assessmentID, studentID, problemID)
	imgKey := putBlob("answer_pages/1/image.jpg")
	maskKey := putBlob("answer_pages/1/masked.jpg")
	mustExec(`INSERT INTO answer_pages (answer_id, page_index, submission_id, pdf_page_index, image_ref, image_sha256, image_width, image_height, masked_image_ref)
		VALUES ($1, 0, $2, 0, $3, 'deadbeef', 100, 100, $4)`, answerID, submissionID, imgKey, maskKey)

	// scan_batches.source_ref
	batchSourceKey := putBlob("scan_batches/1.zip")
	batchID := mustExecReturningID(`INSERT INTO scan_batches (assessment_id, source_ref) VALUES ($1, $2) RETURNING id`, assessmentID, batchSourceKey)

	// scan_sources.source_ref
	scanSrcKey := putBlob("scan_sources/1/source.pdf")
	sourceID := mustExecReturningID(`INSERT INTO scan_sources (batch_id, original_filename, source_ref, source_sha256, source_kind)
		VALUES ($1, 'sf.pdf', $2, 'deadbeef', 'pdf') RETURNING id`, batchID, scanSrcKey)

	// scan_pages.image_ref + student_id_crop_ref + name_crop_ref + problem_crop_ref
	scanPageImgKey := putBlob("scan_pages/1/page.jpg")
	scanStudentIDCropKey := putBlob("scan_pages/1/student_id_crop.jpg")
	scanNameCropKey := putBlob("scan_pages/1/name_crop.jpg")
	scanProblemCropKey := putBlob("scan_pages/1/problem_crop.jpg")
	mustExec(`INSERT INTO scan_pages (source_id, batch_id, assessment_id, page_index, image_ref, student_id_crop_ref, name_crop_ref, problem_crop_ref)
		VALUES ($1, $2, $3, 0, $4, $5, $6, $7)`,
		sourceID, batchID, assessmentID, scanPageImgKey, scanStudentIDCropKey, scanNameCropKey, scanProblemCropKey)

	// direct_uploads.source_ref
	directKey := putBlob("direct_uploads/1.pdf")
	mustExec(`INSERT INTO direct_uploads (assessment_id, original_filename, source_ref, source_sha256)
		VALUES ($1, 'd.pdf', $2, 'deadbeef')`, assessmentID, directKey)

	return st.Pool, bs, []string{
		subKey, quarKey, imgKey, maskKey, batchSourceKey, scanSrcKey,
		scanPageImgKey, scanStudentIDCropKey, scanNameCropKey, scanProblemCropKey, directKey,
	}
}

func TestVerifyBlobRefs_AllPresent(t *testing.T) {
	pool, blobs, keys := verifyFixture(t, t.TempDir())
	if len(keys) != len(blobRefQueries) {
		t.Fatalf("fixture wrote %d keys, but blobRefQueries has %d columns — fixture is out of sync", len(keys), len(blobRefQueries))
	}

	missing, checked, err := verifyBlobRefs(context.Background(), pool, blobs)
	if err != nil {
		t.Fatalf("verifyBlobRefs: %v", err)
	}
	if checked != len(blobRefQueries) {
		t.Errorf("checked = %d, want %d (one ref per column)", checked, len(blobRefQueries))
	}
	if len(missing) != 0 {
		t.Errorf("missing = %+v, want none (every fixture blob was written)", missing)
	}
}

func TestVerifyBlobRefs_DetectsMissing(t *testing.T) {
	pool, blobs, keys := verifyFixture(t, t.TempDir())

	// Delete one blob out from under its DB row — the ref-integrity gap this
	// command exists to catch (e.g. a restore where the DB dump outpaced the blob
	// tarball).
	deleted := keys[2] // answer_pages.image_ref
	if err := blobs.Delete(context.Background(), deleted); err != nil {
		t.Fatalf("delete fixture blob: %v", err)
	}

	missing, _, err := verifyBlobRefs(context.Background(), pool, blobs)
	if err != nil {
		t.Fatalf("verifyBlobRefs: %v", err)
	}
	if len(missing) != 1 {
		t.Fatalf("missing = %+v, want exactly 1", missing)
	}
	if missing[0].table != "answer_pages" || missing[0].column != "image_ref" || missing[0].key != deleted {
		t.Errorf("missing[0] = %+v, want table=answer_pages column=image_ref key=%s", missing[0], deleted)
	}
}

func TestRunVerifyBlobs_ExitCodeAndReport(t *testing.T) {
	blobDir := t.TempDir()
	_, blobs, keys := verifyFixture(t, blobDir)

	cfg := config.Config{DatabaseURL: storetest.DSN(t), BlobDir: blobDir}

	// Clean run: exit 0, "OK" in the report.
	var buf bytes.Buffer
	if code := runVerifyBlobs(context.Background(), cfg, &buf, nil); code != 0 {
		t.Fatalf("clean run: exit code = %d, want 0; output:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "OK") {
		t.Errorf("clean run output missing OK marker: %s", buf.String())
	}

	// Delete a blob, expect exit 1 and the key (not PII — a content path) reported.
	deleted := keys[len(keys)-1]
	if err := blobs.Delete(context.Background(), deleted); err != nil {
		t.Fatalf("delete: %v", err)
	}
	buf.Reset()
	if code := runVerifyBlobs(context.Background(), cfg, &buf, nil); code != 1 {
		t.Fatalf("dirty run: exit code = %d, want 1; output:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), deleted) {
		t.Errorf("dirty run output missing deleted key %q: %s", deleted, buf.String())
	}
	if !strings.Contains(buf.String(), "FAIL") {
		t.Errorf("dirty run output missing FAIL marker: %s", buf.String())
	}
}

// TestRunVerifyBlobs_NoDatabaseURL guards the config error path: a missing DSN
// must fail loudly (exit 1) rather than panic or silently report clean.
func TestRunVerifyBlobs_NoDatabaseURL(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Config{DatabaseURL: "", BlobDir: t.TempDir()}
	if code := runVerifyBlobs(context.Background(), cfg, &buf, nil); code != 1 {
		t.Fatalf("no DSN: exit code = %d, want 1; output:\n%s", code, buf.String())
	}
}
