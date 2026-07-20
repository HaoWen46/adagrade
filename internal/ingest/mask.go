package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// isInterruption reports whether a failed mask/ingest call was a shutdown or
// timeout interruption rather than a deterministic verdict (F17). Duplicated
// (not shared/exported) from grading.Runner's isInterruption / scan's
// isInterruption — same 3-line body, kept local so this package has no new
// cross-package dependency for it. True when the returned error is (or wraps)
// context.Canceled/DeadlineExceeded, or the context itself is already done
// (catching an operation that swallowed the cause). Callers must treat this as
// "record a plain attempt, write no terminal state".
func isInterruption(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return ctx.Err() != nil
}

// maskQuality is the JPEG quality used for masked artifacts (spec §7: ~85). It is
// part of the mask fingerprint, so changing it re-masks every page.
const maskQuality = 85

// MaskFingerprint is the pure fingerprint of the INPUTS that produce a page's
// masked artifact: the original image SHA, the JPEG quality, and the canonical
// serialization of the region set that applies to the page. A per-page re-apply
// job compares this against the page's stored mask_input_sha; a match means the
// masked artifact is already up to date and the page can be skipped WITHOUT
// resetting its review status (D27, F2).
//
// Serialization is fully deterministic: every float field is formatted with fixed
// precision, and the regions are serialized in the ORDER they are passed (which is
// the stored DB order — ListMaskRegions ORDER BY id). Region ordering is therefore
// part of the fingerprint: the same rects in a different order produce a different
// fingerprint. This is intentional — the DB order is stable, and treating it as
// significant keeps the function a trivial pure fold with no sort dependency.
func MaskFingerprint(imageSHA string, quality int, regions []imaging.Region) string {
	var b strings.Builder
	b.WriteString("v1\n")
	b.WriteString(imageSHA)
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(quality))
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(len(regions)))
	b.WriteByte('\n')
	for _, r := range regions {
		b.WriteString(ff(r.X))
		b.WriteByte('|')
		b.WriteString(ff(r.Y))
		b.WriteByte('|')
		b.WriteString(ff(r.W))
		b.WriteByte('|')
		b.WriteString(ff(r.H))
		b.WriteByte('|')
		b.WriteString(r.Color)
		b.WriteByte('|')
		b.WriteString(ff(r.Padding))
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ff formats a normalized coordinate with fixed 6-decimal precision so the
// fingerprint is stable across equal values (0.1 always serializes identically).
func ff(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

// regionsForPage filters the assessment's mask regions down to the ones that apply
// to a page at pdfPageIndex, exactly as ApplyMasks did: a 'first'-scope region only
// applies to pdf page 0. The stored DB order is preserved (regions is already
// ORDER BY id from ListMaskRegions), which the fingerprint depends on.
func regionsForPage(regions []db.MaskRegion, pdfPageIndex int32) []imaging.Region {
	var regs []imaging.Region
	for _, r := range regions {
		if r.PageScope == "first" && pdfPageIndex != 0 {
			continue
		}
		regs = append(regs, imaging.Region{
			X: float64(r.X), Y: float64(r.Y), W: float64(r.W), H: float64(r.H),
			Color: r.Color, Padding: float64(r.Padding),
		})
	}
	return regs
}

// PlanMasks enumerates an assessment's pages and computes, per page, the
// fingerprint of its masking inputs WITHOUT any image I/O — returning the ids of
// pages that need (re-)masking and the count already up to date. A page needs work
// when its stored mask_input_sha differs from the freshly-computed fingerprint OR
// it has no masked artifact yet. It errors if no regions are defined (the handler
// keeps its 400), matching ApplyMasks. This is the cheap enumeration the queue
// wiring will use to fan out one MaskPage job per page that needs work (D27, F2).
func (s *Service) PlanMasks(ctx context.Context, assessmentID int64) (pageIDs []int64, skipped int, err error) {
	regions, err := s.Store.Q.ListMaskRegions(ctx, assessmentID)
	if err != nil {
		return nil, 0, err
	}
	if len(regions) == 0 {
		return nil, 0, errors.New("no mask regions defined for this assessment")
	}
	pages, err := s.Store.Q.ListPagesForAssessment(ctx, assessmentID)
	if err != nil {
		return nil, 0, err
	}
	for _, pg := range pages {
		regs := regionsForPage(regions, pg.PdfPageIndex)
		fp := MaskFingerprint(pg.ImageSha256, maskQuality, regs)
		if pg.MaskedImageRef.Valid && pg.MaskedImageRef.String != "" &&
			pg.MaskInputSha.Valid && pg.MaskInputSha.String == fp {
			skipped++
			continue
		}
		pageIDs = append(pageIDs, pg.ID)
	}
	return pageIDs, skipped, nil
}

// StaleAcceptedMasks returns the ids of review-ACCEPTED pages whose stored
// mask_input_sha no longer matches the fingerprint of the assessment's CURRENT
// region set (stale-mask fix 2026-07-11). Those pages pass the "masked +
// accepted" grading gates while their masked artifact was produced from OLD
// inputs — after a TA fixes a region that was leaking a student id, every run
// would keep sending the old identity-revealing images to providers. A page
// with no artifact or no recorded fingerprint can't be proven fresh and counts
// stale too (the privacy-conservative direction; mirrors PlanMasks' needs-work
// test exactly). No image I/O — fingerprints are computed from stored SHAs.
//
// It takes a *db.Queries rather than the Service store so callers can run it
// inside their own transaction (handlePutMaskRegions reconciles in the same tx
// that replaces the region set).
func StaleAcceptedMasks(ctx context.Context, q *db.Queries, assessmentID int64) ([]int64, error) {
	regions, err := q.ListMaskRegions(ctx, assessmentID)
	if err != nil {
		return nil, err
	}
	pages, err := q.ListAcceptedMaskPages(ctx, assessmentID)
	if err != nil {
		return nil, err
	}
	var stale []int64
	for _, pg := range pages {
		regs := regionsForPage(regions, pg.PdfPageIndex)
		fp := MaskFingerprint(pg.ImageSha256, maskQuality, regs)
		if pg.MaskedImageRef.Valid && pg.MaskedImageRef.String != "" &&
			pg.MaskInputSha.Valid && pg.MaskInputSha.String == fp {
			continue
		}
		stale = append(stale, pg.ID)
	}
	return stale, nil
}

// InvalidateStaleMasks knocks every stale-accepted page (StaleAcceptedMasks)
// back to review 'pending', clearing the reviewer stamp, and reports how many
// pages were reset. The grading gates (CountMaskBlockers*) then block until the
// ordinary Apply-masks flow re-masks the page (SetPageMasked re-resets review)
// and a reviewer re-accepts it. Pages whose inputs didn't change are never
// touched, so their acceptance survives — the same preserve-review contract
// MaskPage's skip path keeps (D27, F2). Idempotent: a reset page is no longer
// accepted, so a re-run finds nothing.
func InvalidateStaleMasks(ctx context.Context, q *db.Queries, assessmentID int64) (int64, error) {
	stale, err := StaleAcceptedMasks(ctx, q, assessmentID)
	if err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}
	return q.ResetStaleMaskReview(ctx, stale)
}

// maskDecodeErrorReason is the single static, PII-free reason category recorded on
// a page whose mask job exhausted its River attempts on a deterministic image
// failure (D27 review, F1). It is a fixed string — never a wrapped dynamic message
// that could carry a blob path or student content — so it is safe to surface in
// the API/UI and safe to log.
const maskDecodeErrorReason = "mask failed: image decode error"

// MaskPage is the per-page mask job body (D27, F2). It loads the page and its
// assessment's mask regions, filters them by page scope, computes the input
// fingerprint, and:
//
//   - if the page's stored mask_input_sha already equals the fingerprint AND it has
//     a masked artifact, it is a no-op (the page is up to date; its review status is
//     left untouched — an accepted mask stays accepted; any stale mask_error is
//     cleared);
//   - otherwise it reads the original image, masks it, stores the masked blob, and
//     records the masked ref + fingerprint via SetPageMasked, which resets the
//     review to pending (as ApplyMasks always has, D10) and clears mask_error.
//
// It is idempotent under River redelivery: a redelivered job re-computes the same
// fingerprint and takes the no-op path. Zero regions defined is terminal (error) —
// the assessment has nothing to mask.
//
// Terminal-error surface (D27 review, F1): a deterministic image failure (a corrupt
// stored page JPEG that jpeg.Decode rejects on every attempt) would otherwise burn
// all River attempts, get discarded, and leave the page masked=false with NO recorded
// cause — the D10 run gate blocks forever with nothing visible. When finalAttempt is
// true (the worker passes job.Attempt >= maxAttempts) and the mask fails, MaskPage
// records the static maskDecodeErrorReason and returns nil (the page is terminally
// failed, not retried); on any earlier attempt it returns the error so the queue backs
// off. The "re-apply masks" button re-enqueues the page (PlanMasks includes any page
// without a masked artifact regardless of mask_error), and a later successful mask
// clears mask_error.
func (s *Service) MaskPage(ctx context.Context, pageID int64, finalAttempt bool) error {
	pg, err := s.Store.Q.GetAnswerPage(ctx, pageID)
	if err != nil {
		return fmt.Errorf("ingest: mask page: load page %d: %w", pageID, err)
	}
	answer, err := s.Store.Q.GetAnswer(ctx, pg.AnswerID)
	if err != nil {
		return fmt.Errorf("ingest: mask page: load answer %d: %w", pg.AnswerID, err)
	}
	regions, err := s.Store.Q.ListMaskRegions(ctx, answer.AssessmentID)
	if err != nil {
		return err
	}
	if len(regions) == 0 {
		return errors.New("no mask regions defined for this assessment")
	}
	regs := regionsForPage(regions, pg.PdfPageIndex)
	fp := MaskFingerprint(pg.ImageSha256, maskQuality, regs)

	// Up-to-date fast path: same inputs and a masked artifact already present →
	// no-op, preserving review status (D27, F2). Clear any stale mask_error so a
	// page that was already up to date never lingers as "errored" (D27 review, F1).
	if pg.MaskedImageRef.Valid && pg.MaskedImageRef.String != "" &&
		pg.MaskInputSha.Valid && pg.MaskInputSha.String == fp {
		return s.clearMaskError(ctx, pg)
	}

	rc, err := s.Blobs.Get(ctx, pg.ImageRef)
	if err != nil {
		// Blob store trouble is infrastructure, not a deterministic decode failure:
		// return it so the queue retries (and on finalAttempt still surfaces, below).
		return s.maskFailure(ctx, pg.ID, finalAttempt, fmt.Errorf("ingest: mask page %d: original image missing: %w", pg.ID, err))
	}
	orig, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return s.maskFailure(ctx, pg.ID, finalAttempt, fmt.Errorf("ingest: mask page %d: reading original: %w", pg.ID, err))
	}
	m, err := imaging.Mask(orig, regs, maskQuality)
	if err != nil {
		// The deterministic case: a corrupt/undecodable stored JPEG fails identically
		// on every attempt. On the final attempt record the static reason and stop.
		return s.maskFailure(ctx, pg.ID, finalAttempt, fmt.Errorf("ingest: mask page %d: masking: %w", pg.ID, err))
	}
	key := fmt.Sprintf("answers/%d/masked/%d-%s.jpg", pg.AnswerID, pg.PageIndex, m.SHA256()[:8])
	if _, _, err := s.Blobs.Put(ctx, key, bytes.NewReader(m.JPEG())); err != nil {
		return s.maskFailure(ctx, pg.ID, finalAttempt, fmt.Errorf("ingest: mask page %d: storing masked: %w", pg.ID, err))
	}
	// SetPageMasked also clears mask_error (a successful mask is the recovery).
	if _, err := s.Store.Q.SetPageMasked(ctx, db.SetPageMaskedParams{
		ID:             pg.ID,
		MaskedImageRef: pgtype.Text{String: key, Valid: true},
		MaskInputSha:   pgtype.Text{String: fp, Valid: true},
	}); err != nil {
		return err
	}
	return nil
}

// maskFailure implements the retry/terminal split for a failed mask attempt (D27
// review, F1): on the final River attempt it records the static, PII-free reason
// on the page and returns nil (terminal — the queue must not keep retrying a
// deterministic failure); on any earlier attempt it returns cause so the queue
// backs off. The wrapped cause never reaches the DB — only maskDecodeErrorReason
// does — so no path or student content is persisted.
//
// F17: a shutdown/timeout that cancels Blobs.Get (or any step feeding cause) mid-flight
// surfaces as context.Canceled/DeadlineExceeded. This is an INTERRUPTION, not a
// deterministic decode failure — never write mask_error for it, not even on the final
// attempt. Checked BEFORE the finalAttempt terminal write below, on both cause and ctx,
// so a wrapped-away cancellation is still caught. Returning cause lets River record a
// plain errored attempt (the worker snoozes it on shutdown) instead of wedging the page
// with a false "image decode error".
func (s *Service) maskFailure(ctx context.Context, pageID int64, finalAttempt bool, cause error) error {
	if isInterruption(ctx, cause) {
		s.log().Warn("ingest: mask page interrupted (shutdown/timeout); not terminal", "page_id", pageID)
		return cause
	}
	if !finalAttempt {
		return cause
	}
	if err := s.Store.Q.SetPageMaskError(ctx, db.SetPageMaskErrorParams{
		ID:        pageID,
		MaskError: pgtype.Text{String: maskDecodeErrorReason, Valid: true},
	}); err != nil {
		return err
	}
	s.log().Warn("ingest: mask page terminal failure", "page_id", pageID)
	return nil
}

// clearMaskError sets mask_error back to NULL when it is currently set, so the
// skip fast path recovers a page that previously errored but is now up to date.
// A no-op when already clear (avoids a needless write on the common skip path).
func (s *Service) clearMaskError(ctx context.Context, pg db.AnswerPage) error {
	if !pg.MaskError.Valid {
		return nil
	}
	return s.Store.Q.SetPageMaskError(ctx, db.SetPageMaskErrorParams{ID: pg.ID, MaskError: pgtype.Text{}})
}
