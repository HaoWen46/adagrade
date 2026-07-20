package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/publish"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// Phase 6 publish routes (spec §7). Preview/publish/history/resend are lecturer+;
// unpublish is admin-only (registered with requireRole in api.go). All state changes
// are audit-logged; no PII (bodies, names, emails) is logged — counts and ids only.

func (s *Server) handlePublishPreview(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	pv, err := s.publish.GetPreview(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "preview failed")
		return
	}
	warnings, err := s.publishPreviewWarnings(r.Context(), aid, pv)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "preview failed")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		publish.Preview
		Warnings []WorkflowWarning `json:"warnings"`
	}{Preview: pv, Warnings: warnings})
}

// publishPreviewWarningCodes is the publish-scoped subset of the standing
// assessment-wide warnings (workflow-guards plan 2026-07-10): hazards that make
// publishing right now risky — pages that would grade incomplete, results still in
// flight or of doubtful provenance. Grading-input hazards the coverage gate already
// fronts for (mask_errors, no_rubric_problems, ...) stay off the publish dialog.
var publishPreviewWarningCodes = map[string]bool{
	"stranded_scan_pages":     true,
	"unidentified_scan_pages": true,
	"quarantined_uploads":     true,
	"run_in_progress":         true,
	"mixed_method_versions":   true,
	"adjusted_spot_checks":    true,
	"final_source_no_records": true,
	// stale_masks is a grading-INPUT hazard, but unlike mask_errors nothing
	// else fronts for it at publish time: the stale pages were graded, so the
	// coverage gate passes while every published grade came from outdated
	// (possibly identity-revealing) images (stale-mask fix 2026-07-11).
	"stale_masks": true,
	// text_render_loss: same shape as stale_masks — the affected pages graded
	// fine as far as the coverage gate can tell, but the images were missing
	// text the PDF actually contains (pdfium CJK fix 2026-07-12).
	"text_render_loss": true,
}

// publishPreviewWarnings composes the publish dialog's warnings[]: the publish-scoped
// subset of the assessment-wide list, plus the publish-only codes derived from the
// preview itself (skipped_students) and from the email configuration. Warn-only —
// nothing here touches Publishable() or the publish gates. Counts only, no PII.
func (s *Server) publishPreviewWarnings(ctx context.Context, aid int64, pv publish.Preview) ([]WorkflowWarning, error) {
	all, err := s.workflowWarnings(ctx, aid)
	if err != nil {
		return nil, err
	}
	out := make([]WorkflowWarning, 0, len(all)+3)
	for _, w := range all {
		if publishPreviewWarningCodes[w.Code] {
			out = append(out, w)
		}
	}
	// Students who will receive NO email because every problem is no_submission
	// (publish records them skipped) — informational by design, escalated to warning
	// when anyone is actually affected: a genuinely-submitted student in this list
	// means their pages are stranded in intake.
	if n := len(pv.Skipped); n > 0 {
		out = append(out, WorkflowWarning{Code: "skipped_students", Severity: "warning", Count: int64(n)})
	}
	// Duplicate active emails (roster-lifecycle plan 2026-07-10): two ACTIVE students
	// sharing a mailbox means one of them reads the other's grades — a danger at the
	// exact moment of publishing. Computed directly from the S0 query (count of
	// emails, never the values) rather than through the assessment-wide list's
	// publish-scoped subset, so this warning is self-contained here.
	dupEmails, err := s.store.Q.CountDuplicateActiveEmails(ctx)
	if err != nil {
		return nil, err
	}
	if dupEmails > 0 {
		out = append(out, WorkflowWarning{Code: "duplicate_emails", Severity: "danger", Count: dupEmails})
	}
	out = append(out, emailConfigWarnings(s.cfg.Email.Provider, s.cfg.Email.ReplyDomain, s.cfg.Env)...)
	// No regrade deadline while a reply channel is advertised (demo-polish plan
	// 2026-07-10, Task D): a configured reply domain puts a regrade Reply-To on every
	// grade email (the sender's HasReplyTo condition), and the inbound webhook only
	// enforces assessments.regrade_deadline when set — unset means replies are
	// accepted for as long as tokens live. Info, not warning: an open window can be
	// deliberate; the fix is one field on the Regrade rounds tab.
	if s.cfg.Email.ReplyDomain != "" {
		a, err := s.store.Q.GetAssessment(ctx, aid)
		if err != nil {
			return nil, err
		}
		if !a.RegradeDeadline.Valid {
			out = append(out, WorkflowWarning{Code: "no_regrade_deadline", Severity: "info"})
		}
	}
	return out, nil
}

// emailConfigWarnings derives the email-delivery hazards from static config
// (workflow-guards plan 2026-07-10) — pure so the production branch is unit-testable
// (the HTTP harness can't run production: dev-login is development-only).
//   - email_file_provider: provider "file" writes .eml files to a local outbox — fine
//     in development (info), a silent no-delivery disaster in production (danger).
//   - email_replyto_dead: smtp with a reply domain advertises a regrade Reply-To no
//     webhook can receive, so students' regrade replies are silently lost (danger;
//     main.go warns at startup — this surfaces it at the moment of publishing).
func emailConfigWarnings(provider, replyDomain string, env config.Environment) []WorkflowWarning {
	var out []WorkflowWarning
	if provider == "file" {
		sev := "info"
		if env == config.EnvProduction {
			sev = "danger"
		}
		out = append(out, WorkflowWarning{Code: "email_file_provider", Severity: sev})
	}
	if provider == "smtp" && replyDomain != "" {
		out = append(out, WorkflowWarning{Code: "email_replyto_dead", Severity: "danger"})
	}
	return out
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	var body struct {
		Note       string `json:"note"`
		ResendAll  bool   `json:"resend_all"`
		Attachment string `json:"attachment"` // "none" (default) | "compressed" | "original" — spec §3 D44
		Zip        bool   `json:"zip"`        // spec §3 D45 ZIP-of-images fallback
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	me, _ := currentUser(r.Context())

	res, err := s.publish.Publish(r.Context(), aid, body.Note, body.ResendAll, me.ID, body.Attachment, body.Zip)
	var invalidAttachment publish.ErrInvalidAttachment
	if errors.As(err, &invalidAttachment) {
		apiError(w, http.StatusBadRequest, invalidAttachment.Error())
		return
	}
	if errors.Is(err, publish.ErrReportFontUnconfigured) {
		apiError(w, http.StatusBadRequest, "report PDF attachments are not available: ADAMARKER_REPORT_FONT is not configured")
		return
	}
	if errors.Is(err, publish.ErrAlreadyPublished) {
		// A non-superseded batch already exists (spec §2 single-live-batch invariant):
		// refuse rather than create a second live batch and double-send email. Re-publish
		// flow is unpublish -> publish.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "already published — unpublish first",
		})
		return
	}
	if errors.Is(err, publish.ErrGradingStateChanged) {
		apiError(w, http.StatusConflict, "grading source changed while preparing results; refresh and try again")
		return
	}
	if errors.Is(err, publish.ErrNoFinalSource) {
		// 0027: no chosen source ⇒ no derived officials ⇒ nothing to publish.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "choose the exam's final grading source before publishing",
		})
		return
	}
	var scGate publish.ErrSpotCheckGate
	if errors.As(err, &scGate) {
		// Trust spec §4: the exact run selected as the method source must clear
		// its spot-check sample before results go out.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "spot-check gate not cleared for the selected final run",
			"run_id": scGate.RunID,
			"total":  scGate.Total,
			"done":   scGate.Done,
		})
		return
	}
	var gate publish.ErrCoverageGate
	if errors.As(err, &gate) {
		// 409 with the blockers list (spec §2 — the preview enumerates blockers instead
		// of failing opaquely; publish echoes them on refusal).
		blockers := gate.Blockers
		if blockers == nil {
			blockers = []db.PublishBlockersRow{}
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    "coverage gate not satisfied: every answer must have an official grade or be no_submission before publishing",
			"blockers": blockers,
		})
		return
	}
	if errors.Is(err, publish.ErrNothingToPublish) {
		// The copy must match the actual situation (A5 follow-up): this error now
		// also fires on a FIRST publish with zero eligible students, where there is
		// no "last publish" and resend_all cannot help — the changed-only re-publish
		// suggestion would be flatly wrong there. Distinguish by whether any batch
		// has ever existed (same hasPriorBatch notion the service's selection uses);
		// on a re-publish, resend_all having ALREADY been set means the empty result
		// is an empty eligible population, not an empty diff, so don't re-suggest it.
		msg := "nothing to publish: no eligible students have grades to send"
		if batches, lerr := s.publish.ListBatches(r.Context(), aid); lerr == nil && len(batches) > 0 {
			if body.ResendAll {
				msg = "nothing to publish: no eligible students remain to re-send to"
			} else {
				msg = "no changed students since the last publish; use resend_all to re-send to everyone"
			}
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": msg})
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "publish failed")
		return
	}
	s.audit(r, "publish.create", "assessment", strconv.FormatInt(aid, 10), map[string]any{
		"batch_id": res.BatchID, "items": res.ItemsCreated,
		"enqueued": res.Enqueued, "skipped": res.Skipped,
		"skipped_withdrawn": res.SkippedWithdrawn, "resend_all": body.ResendAll,
	})
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	me, _ := currentUser(r.Context())
	batchID, err := s.publish.Unpublish(r.Context(), aid, me.ID)
	if errors.Is(err, store.ErrNotLatestBatch) {
		apiError(w, http.StatusConflict, "not the latest batch — a newer publish exists; refresh and unpublish that instead")
		return
	}
	if errors.Is(err, store.ErrPublishDeliveryInProgress) {
		apiError(w, http.StatusConflict, "an email is currently crossing the provider delivery boundary; wait for it to finish before unpublishing")
		return
	}
	if err != nil {
		// No non-superseded batch to unpublish surfaces as pgx.ErrNoRows from the store.
		apiError(w, http.StatusNotFound, "nothing to unpublish for this assessment")
		return
	}
	s.audit(r, "publish.unpublish", "assessment", strconv.FormatInt(aid, 10), map[string]any{"batch_id": batchID})
	writeJSON(w, http.StatusOK, map[string]any{"unpublished_batch_id": batchID})
}

func (s *Server) handlePublishBatches(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	batches, err := s.publish.ListBatches(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list batches failed")
		return
	}
	type itemJSON struct {
		ID                int64   `json:"id"`
		StudentID         string  `json:"student_id"`
		StudentName       string  `json:"student_name"`
		RecipientEmail    string  `json:"recipient_email"`
		EmailStatus       string  `json:"email_status"`
		ProviderMessageID *string `json:"provider_message_id,omitempty"`
		Error             *string `json:"error,omitempty"`
		// Warning is true when Error carries a non-terminal report-attachment warning
		// (spec §3 15MB guard) rather than a real send failure — the item's
		// email_status is still "sent" in that case; see the warningPrefix doc in
		// internal/publish/sender.go. Lets the UI style it distinctly without
		// string-matching the prefix itself.
		Warning bool `json:"warning"`
	}
	type batchJSON struct {
		ID         int64      `json:"id"`
		Note       string     `json:"note"`
		ResendAll  bool       `json:"resend_all"`
		CreatedAt  *string    `json:"created_at,omitempty"`
		Superseded bool       `json:"superseded"`
		Attachment string     `json:"attachment"` // "none" | "compressed" | "original" (spec §3 D44)
		Zip        bool       `json:"zip"`        // spec §3 D45
		Items      []itemJSON `json:"items"`
		// ItemsCount/SentCount/FailedCount/UncertainCount/SkippedCount (B5, additive):
		// the same per-status breakdown a summary view derives from Items today, plus
		// Skipped — which a collapsed ITEMS/SENT/FAILED/UNCERTAIN row left effectively
		// invisible (audit B5: reads as lost email rather than never-mailed
		// no_submission/none-provider items).
		ItemsCount     int64 `json:"items_count"`
		SentCount      int64 `json:"sent_count"`
		FailedCount    int64 `json:"failed_count"`
		UncertainCount int64 `json:"uncertain_count"`
		SkippedCount   int64 `json:"skipped_count"`
	}
	out := make([]batchJSON, 0, len(batches))
	for _, b := range batches {
		items, err := s.publish.ListItems(r.Context(), b.ID)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "list items failed")
			return
		}
		counts, err := s.store.PublishBatchItemCounts(r.Context(), b.ID)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "list items failed")
			return
		}
		ij := make([]itemJSON, 0, len(items))
		for _, it := range items {
			x := itemJSON{
				ID: it.ID, StudentID: it.StudentExternalID, StudentName: it.StudentName,
				RecipientEmail: it.RecipientEmail, EmailStatus: it.EmailStatus,
			}
			if it.ProviderMessageID.Valid {
				x.ProviderMessageID = &it.ProviderMessageID.String
			}
			if it.Error.Valid {
				x.Error = &it.Error.String
				x.Warning = strings.HasPrefix(it.Error.String, publish.WarningPrefix)
			}
			ij = append(ij, x)
		}
		bj := batchJSON{
			ID: b.ID, Note: b.Note, ResendAll: b.ResendAll,
			Superseded: b.SupersededAt.Valid, Attachment: b.Attachment, Zip: b.Zip, Items: ij,
			ItemsCount: counts.Items, SentCount: counts.Sent, FailedCount: counts.Failed,
			UncertainCount: counts.Uncertain, SkippedCount: counts.Skipped,
		}
		if b.CreatedAt.Valid {
			ts := b.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
			bj.CreatedAt = &ts
		}
		out = append(out, bj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": out})
}

func (s *Server) handleResendFailed(w http.ResponseWriter, r *http.Request) {
	batchID, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid batch id")
		return
	}
	if _, err := s.publish.GetBatch(r.Context(), batchID); err != nil {
		apiError(w, http.StatusNotFound, "no such publish batch")
		return
	}
	n, skippedWithdrawn, err := s.publish.ResendFailed(r.Context(), batchID)
	if errors.Is(err, store.ErrPublishBatchSuperseded) {
		apiError(w, http.StatusConflict, "this batch was unpublished (superseded); re-publish instead of resending it")
		return
	}
	if errors.Is(err, publish.ErrEmailDisabled) {
		apiError(w, http.StatusConflict, "email delivery is disabled; configure a real provider before resending")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "resend failed")
		return
	}
	s.audit(r, "publish.resend_failed", "publish_batch", strconv.FormatInt(batchID, 10), map[string]any{
		"reenqueued": n, "skipped_withdrawn": skippedWithdrawn,
	})
	writeJSON(w, http.StatusOK, map[string]any{"reenqueued": n, "skipped_withdrawn": skippedWithdrawn})
}

// handleResendItem is the individual-student resend (spec §4 D46): re-enqueues one
// publish_item's send job regardless of its current status ("student says they never
// got it"), reusing the parent batch's attachment/zip settings. lecturer+, audited
// publish.resend_item.
//
// Resend is a LIVE-batch-only action (I1). If the parent batch was unpublished
// (superseded), SendItem's unpublish guard would skip the re-enqueued send, so a resend
// there is a silent no-op that first downgrades a "sent" item to "pending" and wedges it
// there forever. We 409 that case instead — corrections to a superseded batch go through
// re-publish (unpublish -> publish), not individual resend. The row already carries the
// superseded flag (GetPublishItemForResend joins pb.superseded_at), so no extra lookup.
func (s *Server) handleResendItem(w http.ResponseWriter, r *http.Request) {
	itemID, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid publish item id")
		return
	}
	var body struct {
		AcknowledgeDuplicateRisk bool `json:"acknowledge_duplicate_risk"`
	}
	// Preserve the original body-less endpoint for ordinary terminal states while
	// accepting an explicit acknowledgement for uncertain provider outcomes.
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	item, err := s.publish.GetItemForResend(r.Context(), itemID)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such publish item")
		return
	}
	if item.BatchSuperseded {
		apiError(w, http.StatusConflict, "this item's batch was unpublished (superseded); a superseded item cannot be individually resent — re-publish instead")
		return
	}
	// Withdrawn guard (roster-lifecycle plan 2026-07-10, locked semantics (d)): a
	// withdrawn student receives no further grade email — refuse loudly instead of
	// silently re-sending to someone who left the course; reinstating re-enables it.
	stu, err := s.store.Q.GetStudent(r.Context(), item.StudentID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "student lookup failed")
		return
	}
	if stu.WithdrawnAt.Valid {
		apiError(w, http.StatusConflict, "student is withdrawn — reinstate to resend")
		return
	}
	me, _ := currentUser(r.Context())
	if err := s.publish.ResendItem(r.Context(), itemID, body.AcknowledgeDuplicateRisk, me.ID); err != nil {
		switch {
		case errors.Is(err, publish.ErrDeliveryInProgress):
			apiError(w, http.StatusConflict, "this email delivery is already in progress; wait for it to finish before resending")
		case errors.Is(err, publish.ErrUncertainNeedsAcknowledgement):
			apiError(w, http.StatusConflict, "provider acceptance is uncertain; acknowledge duplicate-delivery risk before resending")
		case errors.Is(err, publish.ErrRecipientWithdrawn):
			apiError(w, http.StatusConflict, "student is withdrawn — reinstate to resend")
		case errors.Is(err, publish.ErrEmailDisabled):
			apiError(w, http.StatusConflict, "email delivery is disabled; configure a real provider before resending")
		case errors.Is(err, store.ErrPublishBatchSuperseded):
			apiError(w, http.StatusConflict, "this item's batch was unpublished (superseded); re-publish instead")
		case errors.Is(err, publish.ErrResendNotAllowed):
			apiError(w, http.StatusConflict, "this email cannot be resent from its current delivery state; refresh and try again")
		default:
			apiError(w, http.StatusInternalServerError, "resend failed")
		}
		return
	}
	s.audit(r, "publish.resend_item", "publish_item", strconv.FormatInt(itemID, 10), map[string]any{
		"batch_id": item.BatchID, "acknowledged_duplicate_risk": body.AcknowledgeDuplicateRisk,
	})
	writeJSON(w, http.StatusOK, map[string]any{"resent_item_id": itemID})
}
