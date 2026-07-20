// Human-readable labels for raw API enum strings (shared seam — pages must use
// these helpers instead of rolling competing maps). Unknown values fall back to
// the raw string so new server enums degrade visibly, never to a blank.

const ANSWER_STATUS_LABELS: Record<string, string> = {
  official_set: "official",
  graded: "graded (not official)",
  ungraded: "ungraded",
  no_submission: "no submission",
  not_ingested: "not in any upload",
};

export function answerStatusLabel(s: string): string {
  return ANSWER_STATUS_LABELS[s] ?? s;
}

const FLAG_LABELS: Record<string, string> = {
  blank: "blank page",
  needs_review: "needs review",
  image_superseded: "image replaced",
};

export function flagLabel(s: string): string {
  return FLAG_LABELS[s] ?? s;
}

const QUARANTINE_REASON_LABELS: Record<string, string> = {
  unknown_student: "filename couldn't be uniquely matched to an active roster student",
  invalid_pdf: "not a readable PDF",
  invalid_image: "not a readable image",
  duplicate_in_batch: "duplicate file in this upload",
};

export function quarantineReasonLabel(s: string): string {
  return QUARANTINE_REASON_LABELS[s] ?? s;
}

/** DELETE /api/students/{id} 409 `blocking` kinds (B15) — the raw strings the server
 * names (submissions, answers, scan_pages, publish_items, regrade_requests, quarantine). */
const STUDENT_BLOCKING_KIND_LABELS: Record<string, string> = {
  submissions: "submitted work",
  answers: "graded answers",
  scan_pages: "scanned pages",
  publish_items: "published grade records",
  regrade_requests: "regrade requests (open or resolved)",
  quarantine: "resolved quarantine uploads",
};

export function studentBlockingKindLabel(s: string): string {
  return STUDENT_BLOCKING_KIND_LABELS[s] ?? s;
}
