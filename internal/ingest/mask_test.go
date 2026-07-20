package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// ---- MaskFingerprint (pure) ----

func TestMaskFingerprint_StableAndSensitive(t *testing.T) {
	regs := []imaging.Region{
		{X: 0.05, Y: 0.02, W: 0.4, H: 0.08, Color: "#4a4a4a", Padding: 0.01},
		{X: 0.1, Y: 0.2, W: 0.3, H: 0.05, Color: "#000000", Padding: 0.02},
	}
	base := MaskFingerprint("abc123", 85, regs)

	// Stable: identical inputs → identical fingerprint.
	if again := MaskFingerprint("abc123", 85, regs); again != base {
		t.Errorf("fingerprint not stable: %q vs %q", base, again)
	}
	// Non-empty, hex-ish (sha256 hex is 64 chars).
	if len(base) != 64 {
		t.Errorf("fingerprint should be a 64-char sha256 hex, got %d: %q", len(base), base)
	}

	cases := []struct {
		name string
		sha  string
		qual int
		regs []imaging.Region
	}{
		{"different image sha", "def456", 85, regs},
		{"different quality", "abc123", 90, regs},
		{"different region coord", "abc123", 85, []imaging.Region{
			{X: 0.06, Y: 0.02, W: 0.4, H: 0.08, Color: "#4a4a4a", Padding: 0.01},
			{X: 0.1, Y: 0.2, W: 0.3, H: 0.05, Color: "#000000", Padding: 0.02},
		}},
		{"different color", "abc123", 85, []imaging.Region{
			{X: 0.05, Y: 0.02, W: 0.4, H: 0.08, Color: "#ffffff", Padding: 0.01},
			{X: 0.1, Y: 0.2, W: 0.3, H: 0.05, Color: "#000000", Padding: 0.02},
		}},
		{"different padding", "abc123", 85, []imaging.Region{
			{X: 0.05, Y: 0.02, W: 0.4, H: 0.08, Color: "#4a4a4a", Padding: 0.05},
			{X: 0.1, Y: 0.2, W: 0.3, H: 0.05, Color: "#000000", Padding: 0.02},
		}},
		{"fewer regions", "abc123", 85, regs[:1]},
		// Ordering is part of the fingerprint (regions have DB order).
		{"reordered regions", "abc123", 85, []imaging.Region{regs[1], regs[0]}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskFingerprint(tc.sha, tc.qual, tc.regs); got == base {
				t.Errorf("fingerprint should change for %q, but matched base", tc.name)
			}
		})
	}
}

func TestMaskFingerprint_EmptyRegions(t *testing.T) {
	// An empty region set still produces a stable, distinct fingerprint (a valid
	// masked artifact — a re-encoded copy — is produced for it).
	a := MaskFingerprint("abc123", 85, nil)
	b := MaskFingerprint("abc123", 85, nil)
	if a != b || a == "" {
		t.Errorf("empty-regions fingerprint should be stable and non-empty: %q %q", a, b)
	}
}

// ---- PlanMasks + MaskPage (DB) ----

// setupMasks ingests b01's whole-assessment PDF (2 pages) and defines one
// first-page mask region, returning the fixture.
func setupMasksFixture(t *testing.T) fx {
	t.Helper()
	f := setup(t)
	if res := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false); res.Status != "ingested" {
		t.Fatalf("ingest: %+v", res)
	}
	if _, err := f.st.Q.CreateMaskRegion(f.ctx, db.CreateMaskRegionParams{
		AssessmentID: f.aid, PageScope: "first",
		X: 0.05, Y: 0.02, W: 0.4, H: 0.08, Color: "#4a4a4a", Padding: 0.01,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPlanMasks_NoRegionsErrors(t *testing.T) {
	f := setup(t)
	f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false)
	if _, _, err := f.svc.PlanMasks(f.ctx, f.aid); err == nil {
		t.Fatal("PlanMasks with no regions should error")
	}
}

func TestPlanMasks_AllPagesNeedWorkThenNoneAfterMasking(t *testing.T) {
	f := setupMasksFixture(t)

	ids, skipped, err := f.svc.PlanMasks(f.ctx, f.aid)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(ids) != 2 || skipped != 0 {
		t.Fatalf("plan should need all 2 pages, 0 skipped: ids=%v skipped=%d", ids, skipped)
	}

	// Run each page's job body.
	for _, id := range ids {
		if err := f.svc.MaskPage(f.ctx, id, false); err != nil {
			t.Fatalf("MaskPage %d: %v", id, err)
		}
	}

	// A second plan now skips both (fingerprints match).
	ids2, skipped2, err := f.svc.PlanMasks(f.ctx, f.aid)
	if err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	if len(ids2) != 0 || skipped2 != 2 {
		t.Fatalf("re-plan should skip both up-to-date pages: ids=%v skipped=%d", ids2, skipped2)
	}
}

func TestMaskPage_SkipPathPreservesAcceptedReview(t *testing.T) {
	f := setupMasksFixture(t)
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	pg := pages[0]

	// First mask: writes the artifact + fingerprint, resets to pending.
	if err := f.svc.MaskPage(f.ctx, pg.ID, false); err != nil {
		t.Fatalf("first mask: %v", err)
	}
	// A reviewer accepts the mask.
	if _, err := f.st.Q.SetMaskReview(f.ctx, db.SetMaskReviewParams{
		ID: pg.ID, MaskReviewStatus: "accepted", MaskReviewedBy: pgtype.Int8{},
	}); err != nil {
		t.Fatal(err)
	}

	// Re-running MaskPage with unchanged inputs is a no-op: it must NOT reset the
	// accepted review to pending.
	if err := f.svc.MaskPage(f.ctx, pg.ID, false); err != nil {
		t.Fatalf("idempotent re-mask: %v", err)
	}
	got, _ := f.st.Q.GetAnswerPage(f.ctx, pg.ID)
	if got.MaskReviewStatus != "accepted" {
		t.Errorf("unchanged re-mask must preserve accepted review, got %q", got.MaskReviewStatus)
	}
}

func TestMaskPage_RegionChangeResetsToPending(t *testing.T) {
	f := setupMasksFixture(t)
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	pg := pages[0]

	if err := f.svc.MaskPage(f.ctx, pg.ID, false); err != nil {
		t.Fatalf("first mask: %v", err)
	}
	if _, err := f.st.Q.SetMaskReview(f.ctx, db.SetMaskReviewParams{
		ID: pg.ID, MaskReviewStatus: "accepted", MaskReviewedBy: pgtype.Int8{},
	}); err != nil {
		t.Fatal(err)
	}

	// Change the region set (delete + recreate with different coords), so the
	// fingerprint no longer matches → re-mask must run and reset review to pending.
	if err := f.st.Q.DeleteMaskRegions(f.ctx, f.aid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Q.CreateMaskRegion(f.ctx, db.CreateMaskRegionParams{
		AssessmentID: f.aid, PageScope: "first",
		X: 0.1, Y: 0.1, W: 0.5, H: 0.1, Color: "#000000", Padding: 0.02,
	}); err != nil {
		t.Fatal(err)
	}

	// Plan should now flag this page as needing work again.
	ids, _, err := f.svc.PlanMasks(f.ctx, f.aid)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id == pg.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("region change should re-flag page %d: plan=%v", pg.ID, ids)
	}

	if err := f.svc.MaskPage(f.ctx, pg.ID, false); err != nil {
		t.Fatalf("re-mask after region change: %v", err)
	}
	got, _ := f.st.Q.GetAnswerPage(f.ctx, pg.ID)
	if got.MaskReviewStatus != "pending" {
		t.Errorf("region change should reset review to pending, got %q", got.MaskReviewStatus)
	}
}

func TestMaskPage_NoRegionsIsTerminalError(t *testing.T) {
	f := setup(t)
	f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false)
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	if err := f.svc.MaskPage(f.ctx, pages[0].ID, false); err == nil {
		t.Fatal("MaskPage with no regions defined should error (terminal)")
	}
}

// ApplyMasks stays as a thin sequential wrapper over PlanMasks+MaskPage; its
// existing behavior contract (derives artifacts, resets review) is covered by
// TestApplyMasks_DerivesArtifactsAndResetsReview in ingest_test.go. Here we only
// assert the masked-key convention still holds through the new path.
func TestApplyMasks_ThroughNewPathWritesMaskedKey(t *testing.T) {
	f := setupMasksFixture(t)
	n, err := f.svc.ApplyMasks(f.ctx, f.aid)
	if err != nil || n != 2 {
		t.Fatalf("ApplyMasks: n=%d err=%v", n, err)
	}
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	for _, pg := range pages {
		if !pg.MaskedImageRef.Valid || !strings.Contains(pg.MaskedImageRef.String, "/masked/") {
			t.Errorf("page %d missing masked ref: %+v", pg.ID, pg.MaskedImageRef)
		}
		if !pg.MaskInputSha.Valid || pg.MaskInputSha.String == "" {
			t.Errorf("page %d should record mask_input_sha: %+v", pg.ID, pg.MaskInputSha)
		}
	}
}

// ---- terminal mask_error surface (D27 review, Finding 1) ----

// corruptPageOriginal overwrites a page's stored ORIGINAL image blob with bytes
// that jpeg.Decode rejects on every attempt — the deterministic failure the D27
// review flagged: no amount of River retries will ever mask this page.
func corruptPageOriginal(f fx, t *testing.T, pg db.ListPagesForAssessmentRow) {
	t.Helper()
	if _, _, err := f.svc.Blobs.Put(f.ctx, pg.ImageRef, bytes.NewReader([]byte("not a jpeg at all"))); err != nil {
		t.Fatal(err)
	}
}

func TestMaskPage_DeterministicFailureWritesMaskErrorOnFinalAttempt(t *testing.T) {
	f := setupMasksFixture(t)
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	pg := pages[0]
	corruptPageOriginal(f, t, pg)

	// A non-final attempt returns the error so the queue backs off, and records
	// NOTHING on the row (the failure might be transient from the queue's view).
	if err := f.svc.MaskPage(f.ctx, pg.ID, false); err == nil {
		t.Fatal("non-final failed mask should return an error for queue backoff")
	}
	mid, _ := f.st.Q.GetAnswerPage(f.ctx, pg.ID)
	if mid.MaskError.Valid {
		t.Errorf("non-final attempt must not record mask_error yet, got %q", mid.MaskError.String)
	}

	// The final attempt (worker passes job.Attempt >= max) records a static,
	// PII-free mask_error and returns nil — the page is terminally failed, not
	// retried into a discard with no recorded cause.
	if err := f.svc.MaskPage(f.ctx, pg.ID, true); err != nil {
		t.Fatalf("final failed mask should swallow the error and record mask_error, got %v", err)
	}
	got, _ := f.st.Q.GetAnswerPage(f.ctx, pg.ID)
	if !got.MaskError.Valid || got.MaskError.String != maskDecodeErrorReason {
		t.Fatalf("final attempt should record %q, got valid=%v %q", maskDecodeErrorReason, got.MaskError.Valid, got.MaskError.String)
	}
	// The page stays unmasked (correct — the run gate still blocks) but now the
	// cause is visible instead of silent.
	if got.MaskedImageRef.Valid {
		t.Error("a failed page must not carry a masked artifact")
	}
	// PII discipline: the recorded reason is the static category, carrying no
	// blob path or student content.
	if strings.Contains(got.MaskError.String, "/") || strings.Contains(got.MaskError.String, pg.ImageRef) {
		t.Errorf("mask_error must be a static PII-free reason, got %q", got.MaskError.String)
	}
}

func TestMaskPage_ReApplyReEnqueuesErroredPageThenSuccessClears(t *testing.T) {
	f := setupMasksFixture(t)
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	pg := pages[0]
	origRef := pg.ImageRef
	origBytes := func() []byte {
		rc, err := f.svc.Blobs.Get(f.ctx, origRef)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}()

	// Corrupt + final-attempt fail → mask_error recorded, page unmasked.
	corruptPageOriginal(f, t, pg)
	if err := f.svc.MaskPage(f.ctx, pg.ID, true); err != nil {
		t.Fatalf("final failed mask: %v", err)
	}

	// The "re-apply masks" retry path re-enqueues errored pages: PlanMasks must
	// still include this page (it has no masked artifact), regardless of mask_error.
	ids, _, err := f.svc.PlanMasks(f.ctx, f.aid)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id == pg.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("re-apply should re-enqueue the errored page %d: plan=%v", pg.ID, ids)
	}

	// Restore the original bytes (the operator fixed the source) and re-run: a
	// successful mask clears mask_error.
	if _, _, err := f.svc.Blobs.Put(f.ctx, origRef, bytes.NewReader(origBytes)); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.MaskPage(f.ctx, pg.ID, false); err != nil {
		t.Fatalf("recovery mask: %v", err)
	}
	got, _ := f.st.Q.GetAnswerPage(f.ctx, pg.ID)
	if got.MaskError.Valid {
		t.Errorf("a successful mask must clear mask_error, got %q", got.MaskError.String)
	}
	if !got.MaskedImageRef.Valid {
		t.Error("recovery mask should write a masked artifact")
	}
}

// ---- stale accepted masks (stale-mask fix 2026-07-11) ----

// A region edit after review acceptance must invalidate exactly the accepted
// pages whose mask inputs actually changed: the fixture's single 'first'-scope
// region only feeds page 0's fingerprint, so editing it leaves page 1 (whose
// region set is empty either way) untouched.
func TestInvalidateStaleMasks_ResetsOnlyAffectedAcceptedPages(t *testing.T) {
	f := setupMasksFixture(t)
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	if len(pages) != 2 {
		t.Fatalf("pages: got %d want 2", len(pages))
	}
	for _, pg := range pages {
		if err := f.svc.MaskPage(f.ctx, pg.ID, false); err != nil {
			t.Fatalf("MaskPage(%d): %v", pg.ID, err)
		}
		if _, err := f.st.Q.SetMaskReview(f.ctx, db.SetMaskReviewParams{
			ID: pg.ID, MaskReviewStatus: "accepted", MaskReviewedBy: pgtype.Int8{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	var page0, page1 int64
	for _, pg := range pages {
		if pg.PdfPageIndex == 0 {
			page0 = pg.ID
		} else {
			page1 = pg.ID
		}
	}

	// Inputs unchanged: nothing is stale, nothing resets.
	stale, err := StaleAcceptedMasks(f.ctx, f.st.Q, f.aid)
	if err != nil || len(stale) != 0 {
		t.Fatalf("unchanged regions should have no stale pages: %v %v", stale, err)
	}
	n, err := InvalidateStaleMasks(f.ctx, f.st.Q, f.aid)
	if err != nil || n != 0 {
		t.Fatalf("unchanged regions should reset nothing: n=%d err=%v", n, err)
	}

	// Move the first-page region: only page 0's fingerprint changes.
	if err := f.st.Q.DeleteMaskRegions(f.ctx, f.aid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Q.CreateMaskRegion(f.ctx, db.CreateMaskRegionParams{
		AssessmentID: f.aid, PageScope: "first",
		X: 0.2, Y: 0.2, W: 0.3, H: 0.1, Color: "#000000", Padding: 0.02,
	}); err != nil {
		t.Fatal(err)
	}

	stale, err = StaleAcceptedMasks(f.ctx, f.st.Q, f.aid)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != page0 {
		t.Fatalf("stale should be exactly page0 (%d): %v", page0, stale)
	}
	n, err = InvalidateStaleMasks(f.ctx, f.st.Q, f.aid)
	if err != nil || n != 1 {
		t.Fatalf("invalidate: n=%d err=%v", n, err)
	}
	got0, _ := f.st.Q.GetAnswerPage(f.ctx, page0)
	if got0.MaskReviewStatus != "pending" || got0.MaskReviewedAt.Valid {
		t.Errorf("page0 should be knocked back to pending with the stamp cleared: %q %v", got0.MaskReviewStatus, got0.MaskReviewedAt)
	}
	got1, _ := f.st.Q.GetAnswerPage(f.ctx, page1)
	if got1.MaskReviewStatus != "accepted" {
		t.Errorf("page1's inputs never changed — acceptance must survive, got %q", got1.MaskReviewStatus)
	}

	// Idempotent: the stale page is pending now, so a re-run resets nothing.
	n, err = InvalidateStaleMasks(f.ctx, f.st.Q, f.aid)
	if err != nil || n != 0 {
		t.Fatalf("second invalidate should be a no-op: n=%d err=%v", n, err)
	}
}

// An accepted page with NO recorded fingerprint (pre-0015 rows) can't be proven
// fresh, so reconciliation treats it as stale — the privacy-conservative
// direction (mirrors PlanMasks, which re-masks such pages).
func TestStaleAcceptedMasks_MissingFingerprintCountsStale(t *testing.T) {
	f := setupMasksFixture(t)
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	pg := pages[0]
	if err := f.svc.MaskPage(f.ctx, pg.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Q.SetMaskReview(f.ctx, db.SetMaskReviewParams{
		ID: pg.ID, MaskReviewStatus: "accepted", MaskReviewedBy: pgtype.Int8{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE answer_pages SET mask_input_sha = NULL WHERE id = $1`, pg.ID); err != nil {
		t.Fatal(err)
	}

	stale, err := StaleAcceptedMasks(f.ctx, f.st.Q, f.aid)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != pg.ID {
		t.Fatalf("fingerprint-less accepted page should be stale: %v", stale)
	}
}

// ---- shutdown-cancel is not a terminal mask_error (F17) ----

// getCanceledBlobs wraps a real blobstore.Store so Get on any key fails with
// context.Canceled — exactly what LocalDisk.Get returns when ctx is already done
// mid-flight during a hard-stop escalation. Put/Delete/Exists pass through
// unchanged so fixture setup (which writes the original blob) is unaffected.
type getCanceledBlobs struct{ blobstore.Store }

func (b getCanceledBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return nil, context.Canceled
}

// TestMaskPage_ShutdownCancellationNotTerminal is the F17 mask half: a SIGTERM
// that cancels Blobs.Get mid-flight surfaces as context.Canceled. Even on the FINAL
// attempt, MaskPage must NOT write mask_error (the D10 run gate would otherwise
// block forever on a false "image decode error" cause). It must return the error so
// River records a plain attempt and the worker snoozes it on shutdown, leaving the
// page to be re-masked cleanly on the next start.
func TestMaskPage_ShutdownCancellationNotTerminal(t *testing.T) {
	f := setupMasksFixture(t)
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	pg := pages[0]
	f.svc.Blobs = getCanceledBlobs{f.svc.Blobs}

	err := f.svc.MaskPage(f.ctx, pg.ID, true /* finalAttempt */)
	if err == nil {
		t.Fatal("cancellation should be RETURNED (so River records a plain attempt), got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("returned error should wrap context.Canceled, got %v", err)
	}
	got, _ := f.st.Q.GetAnswerPage(f.ctx, pg.ID)
	if got.MaskError.Valid {
		t.Errorf("interruption must NOT write mask_error, got %q", got.MaskError.String)
	}
	if got.MaskedImageRef.Valid {
		t.Error("interruption must NOT write a masked artifact")
	}
}
