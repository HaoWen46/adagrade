package queue

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/scan"
)

// unstartedPool builds a *pgxpool.Pool against a syntactically valid DSN without
// dialing — pgxpool.New is lazy, so this is safe to use for construction-only
// assertions (no Start/insert) with no live Postgres required.
func unstartedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("unstartedPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func checkInsertOpts(t *testing.T, name string, opts river.InsertOpts, wantQueue string, wantAttempts int) {
	t.Helper()
	if opts.Queue != wantQueue {
		t.Errorf("%s: InsertOpts().Queue = %q, want %q", name, opts.Queue, wantQueue)
	}
	if opts.MaxAttempts != wantAttempts {
		t.Errorf("%s: InsertOpts().MaxAttempts = %d, want %d", name, opts.MaxAttempts, wantAttempts)
	}
}

// TestArgsKindsAndInsertOpts locks in the job kinds + static insert opts for the
// scan-intake jobs (design spec 2026-07-04): scan.expand shares the CPU-bound
// "scan" queue with MaxAttempts 5; the page-level split/render/identify/promote
// jobs (Task 9) use NEW kind strings (so retired in-flight job shapes can't
// mis-decode) with MaxAttempts 3 each, split/render/promote on scanQueue and
// identify on llmQueue (shares provider rate limiting with grading leaves).
func TestArgsKindsAndInsertOpts(t *testing.T) {
	if got, want := (ScanExpandArgs{}).Kind(), "scan.expand"; got != want {
		t.Errorf("ScanExpandArgs.Kind() = %q, want %q", got, want)
	}
	checkInsertOpts(t, "ScanExpandArgs", ScanExpandArgs{}.InsertOpts(), scanQueue, scanExpandMaxAttempts)

	if got, want := (MaskPageArgs{}).Kind(), "mask.page"; got != want {
		t.Errorf("MaskPageArgs.Kind() = %q, want %q", got, want)
	}
	checkInsertOpts(t, "MaskPageArgs", MaskPageArgs{}.InsertOpts(), scanQueue, maskPageMaxAttempts)

	if got, want := (DirectIngestArgs{}).Kind(), "ingest.direct"; got != want {
		t.Errorf("DirectIngestArgs.Kind() = %q, want %q", got, want)
	}
	checkInsertOpts(t, "DirectIngestArgs", DirectIngestArgs{}.InsertOpts(), scanQueue, directIngestMaxAttempts)

	if got, want := (ScanSplitArgs{}).Kind(), "scan.split"; got != want {
		t.Errorf("ScanSplitArgs.Kind() = %q, want %q", got, want)
	}
	checkInsertOpts(t, "ScanSplitArgs", ScanSplitArgs{}.InsertOpts(), scanQueue, scanSplitMaxAttempts)

	if got, want := (ScanRenderPagesArgs{}).Kind(), "scan.render_pages"; got != want {
		t.Errorf("ScanRenderPagesArgs.Kind() = %q, want %q", got, want)
	}
	checkInsertOpts(t, "ScanRenderPagesArgs", ScanRenderPagesArgs{}.InsertOpts(), scanQueue, scanRenderMaxAttempts)

	if got, want := (ScanIdentifyPageArgs{}).Kind(), "scan.identify_page"; got != want {
		t.Errorf("ScanIdentifyPageArgs.Kind() = %q, want %q", got, want)
	}
	checkInsertOpts(t, "ScanIdentifyPageArgs", ScanIdentifyPageArgs{}.InsertOpts(), llmQueue, scanIdentifyMaxAttempts)

	if got, want := (ScanPromotePageArgs{}).Kind(), "scan.promote_page"; got != want {
		t.Errorf("ScanPromotePageArgs.Kind() = %q, want %q", got, want)
	}
	checkInsertOpts(t, "ScanPromotePageArgs", ScanPromotePageArgs{}.InsertOpts(), scanQueue, scanPromoteMaxAttempts)
}

// TestNew_ScansNil_SkipsScanWiring builds a client with only a Runner (the httpapi
// test-harness shape) against an unstarted pool. Deps.Scans stays optional so
// grading-only callers are unaffected: construction must succeed and the runner's
// closure must still be wired, with no attempt to touch a nil Service.
func TestNew_ScansNil_SkipsScanWiring(t *testing.T) {
	pool := unstartedPool(t)
	runner := &grading.Runner{}
	if _, err := New(pool, Deps{Runner: runner}, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	if runner.EnqueueLeaves == nil {
		t.Error("runner.EnqueueLeaves was not wired")
	}
}

// TestNew_ScansSet_InjectsEnqueueClosures builds a client with both deps set and
// checks that the Service's Enqueue* closures were all injected (mirrors the
// runner.EnqueueLeaves wiring) — the actual insert behavior needs a live
// Postgres tx and is exercised by integration tests, not here.
func TestNew_ScansSet_InjectsEnqueueClosures(t *testing.T) {
	pool := unstartedPool(t)
	runner := &grading.Runner{}
	scans := &scan.Service{}
	if _, err := New(pool, Deps{Runner: runner, Scans: scans}, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	if scans.EnqueueExpand == nil {
		t.Error("scans.EnqueueExpand was not wired")
	}
	if scans.EnqueueSplit == nil {
		t.Error("scans.EnqueueSplit was not wired")
	}
	if scans.EnqueueRenderPages == nil {
		t.Error("scans.EnqueueRenderPages was not wired")
	}
	if scans.EnqueueIdentifyPages == nil {
		t.Error("scans.EnqueueIdentifyPages was not wired")
	}
	if scans.EnqueuePromotePages == nil {
		t.Error("scans.EnqueuePromotePages was not wired")
	}
}

// TestNew_IngestNil_SkipsDirectIngestWiring mirrors TestNew_ScansNil_SkipsScanWiring
// for the new Deps.Ingest seam (D27, F1): API-only processes with no Ingest set must
// still construct cleanly with no attempt to touch a nil Service.
func TestNew_IngestNil_SkipsDirectIngestWiring(t *testing.T) {
	pool := unstartedPool(t)
	runner := &grading.Runner{}
	if _, err := New(pool, Deps{Runner: runner}, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// TestNew_IngestSet_InjectsEnqueueDirectIngest checks the ingest.Service's
// EnqueueDirectIngest closure is wired when Deps.Ingest is set, independent of
// whether Deps.Scans is set (masking/direct-upload don't require the scan pipeline).
func TestNew_IngestSet_InjectsEnqueueDirectIngest(t *testing.T) {
	pool := unstartedPool(t)
	runner := &grading.Runner{}
	ing := &ingest.Service{}
	if _, err := New(pool, Deps{Runner: runner, Ingest: ing}, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	if ing.EnqueueDirectIngest == nil {
		t.Error("ing.EnqueueDirectIngest was not wired")
	}
}

// TestClient_EnqueueMaskPages checks the non-tx mask-page enqueue helper exists and
// handles the empty-slice case without touching a live Postgres (mirrors
// EnqueueLeaves' shape; the actual insert behavior needs a live tx and is exercised
// by the ingest package's own tests plus integration tests).
func TestClient_EnqueueMaskPages_EmptyIsNoop(t *testing.T) {
	pool := unstartedPool(t)
	runner := &grading.Runner{}
	c, err := New(pool, Deps{Runner: runner}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.EnqueueMaskPages(context.Background(), nil); err != nil {
		t.Errorf("EnqueueMaskPages(nil): %v", err)
	}
}
