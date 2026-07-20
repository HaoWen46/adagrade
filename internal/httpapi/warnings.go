package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/HaoWen46/adagrade/internal/ingest"
)

// WorkflowWarning is the shared warning shape (workflow-guards plan 2026-07-10):
// one derived-state hazard, identified by a fixed code the frontend maps to
// copy + a fix-it link. Detail is a machine-neutral supplement (e.g. the
// stranded-page breakdown) and must NEVER carry student names/ids/content.
type WorkflowWarning struct {
	Code     string `json:"code"`
	Severity string `json:"severity"` // "info" | "warning" | "danger"
	Count    int64  `json:"count,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// handleWorkflowWarnings is GET /api/assessments/{id}/workflow-warnings: the
// standing hazard list for an assessment (any signed-in role — it shows the
// same derived state the tabs already show, counts only, no PII). Statuses are
// derived, never stored (D2); every code is recomputed from cheap COUNT queries
// on each call.
func (s *Server) handleWorkflowWarnings(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	if _, err := s.store.Q.GetAssessment(r.Context(), aid); err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	warnings, err := s.workflowWarnings(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "warnings check failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"warnings": warnings})
}

// workflowWarnings computes the assessment-wide warning list in workflow order
// (roster → intake → masking → grading → review → publish). Codes with a zero
// count are omitted; the empty case is an empty (non-nil) slice so the JSON is
// always an array.
func (s *Server) workflowWarnings(ctx context.Context, aid int64) ([]WorkflowWarning, error) {
	out := make([]WorkflowWarning, 0, 8)

	// Roster (roster-lifecycle plan 2026-07-10). Same-named active students can
	// never auto-assign by OCR'd name, so Identify always needs manual
	// confirmation for their pages.
	dupNames, err := s.store.Q.CountStudentsWithDuplicateNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("duplicate names count: %w", err)
	}
	if dupNames > 0 {
		out = append(out, WorkflowWarning{Code: "duplicate_student_names", Severity: "info", Count: dupNames})
	}

	// Scan intake (derived page states, same precedence as ScanBatchPageProgress).
	pc, err := s.store.Q.ScanPageStateCounts(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("scan page state counts: %w", err)
	}
	strandedWs, err := s.strandedScanPageWarnings(ctx, aid)
	if err != nil {
		return nil, err
	}
	out = append(out, strandedWs...)
	if pc.AssignedUnpromoted > 0 {
		out = append(out, WorkflowWarning{Code: "assigned_unpromoted_pages", Severity: "warning", Count: pc.AssignedUnpromoted})
	}
	quarantined, err := s.store.Q.CountOpenQuarantine(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("quarantine count: %w", err)
	}
	if quarantined > 0 {
		out = append(out, WorkflowWarning{Code: "quarantined_uploads", Severity: "warning", Count: quarantined})
	}
	if pc.Processing > 0 {
		out = append(out, WorkflowWarning{Code: "batch_processing", Severity: "info", Count: pc.Processing})
	}
	// Late adds: active students with zero answers rows while the assessment
	// already has some (the query's ≥1-answers guard keeps a brand-new
	// assessment silent). Fix is the materialize action.
	unmaterialized, err := s.store.Q.CountUnmaterializedStudents(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("unmaterialized students count: %w", err)
	}
	if unmaterialized > 0 {
		out = append(out, WorkflowWarning{Code: "unmaterialized_students", Severity: "warning", Count: unmaterialized})
	}

	// Rendering integrity: pages whose PDF text layer contained runs that
	// rasterized as nothing (render.ProbeTextLoss — pdfium silently drops
	// non-embedded CID/CJK glyphs), i.e. content the AI grades without seeing.
	textLoss, err := s.store.Q.CountTextRenderLossPages(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("text render loss count: %w", err)
	}
	if textLoss > 0 {
		out = append(out, WorkflowWarning{Code: "text_render_loss", Severity: "danger", Count: int64(textLoss)})
	}

	// Masking.
	maskErrs, err := s.store.Q.CountMaskErrorPages(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("mask error count: %w", err)
	}
	if maskErrs > 0 {
		out = append(out, WorkflowWarning{Code: "mask_errors", Severity: "danger", Count: maskErrs})
	}
	staleMasks, err := s.staleMaskWarning(ctx, aid)
	if err != nil {
		return nil, err
	}
	out = append(out, staleMasks...)

	// Grading integrity.
	superseded, err := s.store.Q.CountSupersededGradedAnswers(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("superseded answers count: %w", err)
	}
	if superseded > 0 {
		out = append(out, WorkflowWarning{Code: "superseded_answers", Severity: "warning", Count: superseded})
	}
	activeRuns, err := s.store.Q.CountActiveRunsForAssessment(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("active runs count: %w", err)
	}
	if activeRuns > 0 {
		out = append(out, WorkflowWarning{Code: "run_in_progress", Severity: "info", Count: activeRuns})
	}
	noRubric, err := s.store.Q.CountProblemsWithoutRubric(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("no-rubric problems count: %w", err)
	}
	if noRubric > 0 {
		out = append(out, WorkflowWarning{Code: "no_rubric_problems", Severity: "warning", Count: noRubric})
	}

	// Review/officials.
	mixed, err := s.store.Q.MixedMethodVersionStats(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("mixed method version stats: %w", err)
	}
	if mixed.DistinctVersions > 1 {
		out = append(out, WorkflowWarning{Code: "mixed_method_versions", Severity: "warning", Count: mixed.StaleAnswers})
	}
	adjusted, err := s.store.Q.CountAdjustedSpotChecksStillOfficial(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("adjusted spot checks count: %w", err)
	}
	if adjusted > 0 {
		out = append(out, WorkflowWarning{Code: "adjusted_spot_checks", Severity: "warning", Count: adjusted})
	}

	// Publish: the chosen final source is a method that has never produced a
	// model record on this assessment (analysis redesign plan, Task B1) —
	// deriving officials from it yields nothing, so publish would send holes.
	// handleSetFinalSource stays permissive by design; this code is the guard
	// rail instead of a 4xx. Consensus and undecided assessments stay silent.
	fs, err := s.store.Q.FinalSourceModelRecordStats(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("final source record stats: %w", err)
	}
	if fs.FinalIsMethod && fs.ModelRecords == 0 {
		out = append(out, WorkflowWarning{Code: "final_source_no_records", Severity: "danger"})
	}

	// Publish: distinct emails shared by more than one ACTIVE student — grade
	// emails would land in the same mailbox (count of emails, never the values).
	dupEmails, err := s.store.Q.CountDuplicateActiveEmails(ctx)
	if err != nil {
		return nil, fmt.Errorf("duplicate emails count: %w", err)
	}
	if dupEmails > 0 {
		out = append(out, WorkflowWarning{Code: "duplicate_emails", Severity: "danger", Count: dupEmails})
	}

	return out, nil
}

// strandedScanPageWarnings derives the stranded-page hazard codes shared by the
// standing warnings and the launch preview (false-alarm fix 2026-07-11).
// Stranded pages (errored/parked/orphaned) are classified by whether the
// (student, problem) cell they claim is already covered by a live submission:
//
//   - stranded_scan_pages (warning): pages with a known cell that nothing
//     covers — the only bucket allowed to claim answers grade incomplete.
//   - unidentified_scan_pages: pages with no assigned/proposed identity, which
//     can't be checked against coverage. A warning while the assessment still
//     has missing cells (they might be the missing work); info once every cell
//     is covered (nothing incomplete is left for them to be).
//   - dead_scan_pages (info): pages whose cell IS covered — leftovers of a
//     superseded batch, safe to discard, never a grading hazard.
//
// A fully-covered assessment with a dead failed batch therefore produces only
// info notes, never "answers grade incomplete".
func (s *Server) strandedScanPageWarnings(ctx context.Context, aid int64) ([]WorkflowWarning, error) {
	st, err := s.store.Q.StrandedScanPageStats(ctx, aid)
	if err != nil {
		return nil, fmt.Errorf("stranded scan page stats: %w", err)
	}
	var out []WorkflowWarning
	if st.Uncovered > 0 {
		out = append(out, WorkflowWarning{
			Code: "stranded_scan_pages", Severity: "warning", Count: st.Uncovered,
			Detail: fmt.Sprintf("%d orphaned, %d parked, %d failed; answers affected: %d",
				st.UncoveredOrphaned, st.UncoveredParked, st.UncoveredErrored, st.UncoveredCells),
		})
	}
	if st.Unidentified > 0 {
		severity := "info"
		missing, err := s.store.Q.CountMissingCells(ctx, aid)
		if err != nil {
			return nil, fmt.Errorf("missing cells count: %w", err)
		}
		if missing > 0 {
			severity = "warning"
		}
		out = append(out, WorkflowWarning{Code: "unidentified_scan_pages", Severity: severity, Count: st.Unidentified})
	}
	if st.CoveredPages > 0 {
		out = append(out, WorkflowWarning{Code: "dead_scan_pages", Severity: "info", Count: st.CoveredPages})
	}
	return out, nil
}

// staleMaskWarning derives the stale_masks danger code (stale-mask fix
// 2026-07-11): review-ACCEPTED pages whose stored mask fingerprint no longer
// matches the current region set. Such pages pass the "masked + accepted"
// grading gates while sending an OUTDATED (possibly identity-revealing) masked
// image to providers. handlePutMaskRegions reconciles new edits away in the
// same tx that saves them, so this standing code mostly catches pre-fix drift —
// but any hit is a danger. Fix: re-save regions (or re-apply masks) + re-review.
func (s *Server) staleMaskWarning(ctx context.Context, aid int64) ([]WorkflowWarning, error) {
	stale, err := ingest.StaleAcceptedMasks(ctx, s.store.Q, aid)
	if err != nil {
		return nil, fmt.Errorf("stale mask check: %w", err)
	}
	if len(stale) == 0 {
		return nil, nil
	}
	return []WorkflowWarning{{Code: "stale_masks", Severity: "danger", Count: int64(len(stale))}}, nil
}
