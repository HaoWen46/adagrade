// Workflow-guard warnings (plan 2026-07-10): the ONE place backend warning
// codes map to professor-friendly copy + a fix-it link. Every surface (Overview,
// tabs, Launch/Publish dialogs) renders the SAME code the same way by piping a
// WorkflowWarning through warningView into <WorkflowNotice>.
//
// Copy guide: say what's wrong + the consequence + where to fix it. Counts come
// from the warning itself; Detail is a machine-neutral supplement (never student
// names/ids) and is safe to interpolate verbatim.

import { useQuery } from "@tanstack/react-query";
import { api } from "./api";
import type { WorkflowWarning } from "./types";

/** Standing hazard list for an assessment (GET .../workflow-warnings). Shared
 * key so Overview and the tabs read one cache entry. */
export function useWorkflowWarnings(assessmentId: string) {
  return useQuery({
    queryKey: ["workflow-warnings", assessmentId],
    queryFn: () =>
      api.get<{ warnings: WorkflowWarning[] }>(
        `/api/assessments/${assessmentId}/workflow-warnings`,
      ),
  });
}

export interface WarningView {
  message: string;
  /** Route that fixes it; absent when no page fixes it (e.g. server env config). */
  to?: string;
  tone: "info" | "warning" | "danger";
}

/** "3 pages" / "1 page" — copy below leans on counts heavily. */
function n(count: number | undefined, noun: string): string {
  const c = count ?? 0;
  return `${c} ${noun}${c === 1 ? "" : "s"}`;
}

function toTone(severity: string): WarningView["tone"] {
  return severity === "danger" || severity === "warning" || severity === "info"
    ? severity
    : "info";
}

export function warningView(w: WorkflowWarning, assessmentId: string): WarningView {
  const tab = (key: string) => `/assessments/${assessmentId}?tab=${key}`;
  const tone = toTone(w.severity);
  const one = (w.count ?? 0) === 1;

  switch (w.code) {
    // --- scan intake -----------------------------------------------------------------
    // Stranded pages split three ways by cell coverage (false-alarm fix
    // 2026-07-11): only stranded_scan_pages — pages whose (student, problem)
    // cell has NO live submission — may claim answers grade incomplete.
    case "stranded_scan_pages":
      return {
        tone,
        to: tab("identify"),
        message: `${n(w.count, "scanned page")} ${one ? "isn't" : "aren't"} attached to any answer${
          w.detail ? ` (${w.detail})` : ""
        }. Those answers grade incomplete or not at all — resolve them on the Identify tab.`,
      };
    case "unidentified_scan_pages":
      return {
        tone,
        to: tab("identify"),
        message:
          tone === "info"
            ? `${n(w.count, "scanned page")} couldn't be identified, but every answer is already covered by a live submission — likely leftovers from a failed or superseded batch. Discard ${one ? "it" : "them"} on the Identify tab when convenient.`
            : `${n(w.count, "scanned page")} couldn't be identified — if ${one ? "it belongs" : "they belong"} to a student, that work is missing from grading. Resolve or discard ${one ? "it" : "them"} on the Identify tab.`,
      };
    case "dead_scan_pages":
      return {
        tone,
        to: tab("identify"),
        message: `${n(w.count, "scanned page")} from an earlier batch ${one ? "is" : "are"} already covered by newer submissions — nothing grades incomplete. Discard ${one ? "it" : "them"} on the Identify tab to clear this note.`,
      };
    case "assigned_unpromoted_pages":
      return {
        tone,
        to: tab("identify"),
        message: `${n(w.count, "resolved page")} ${one ? "is" : "are"} waiting for Finalize — assignments take effect only after you click Finalize on the Identify tab.`,
      };
    case "quarantined_uploads":
      return {
        tone,
        to: tab("submissions"),
        message: `${n(w.count, "upload")} ${one ? "sits" : "sit"} in quarantine, attached to no student. Their answers grade as missing — assign them on the Submissions tab.`,
      };
    case "batch_processing":
      return {
        tone,
        to: tab("identify"),
        message: `${n(w.count, "scanned page")} ${one ? "is" : "are"} still processing — identification results are incomplete until they finish.`,
      };

    // --- roster lifecycle (plan 2026-07-10) --------------------------------------------
    case "duplicate_student_names":
      return {
        tone,
        to: tab("identify"),
        message: `${n(w.count, "student")} share${one ? "s" : ""} a name with another student — pages from same-named students can't be auto-attributed and always need manual confirmation on the Identify tab.`,
      };
    case "unmaterialized_students":
      return {
        tone,
        to: tab("publish"),
        message: `${n(w.count, "student")} joined after the last upload and ${one ? "has" : "have"} no answer rows — materialize ${one ? "their answers" : "them"} on the Publish tab or upload their work.`,
      };
    case "duplicate_emails":
      return {
        tone,
        to: "/students",
        message: `${n(w.count, "email address")} ${one ? "is" : "are"} shared by more than one active student — their grade emails would go to the same mailbox. Fix the roster on the Students page.`,
      };

    // --- masking ---------------------------------------------------------------------
    case "mask_errors":
      return {
        tone,
        to: tab("masking"),
        message: `${n(w.count, "page")} failed masking — the AI would see an unmasked or stale image. Re-draw regions or re-apply masks on the Masking tab.`,
      };
    case "stale_masks":
      return {
        tone,
        to: tab("masking"),
        message: `${n(w.count, "accepted page")} ${one ? "was" : "were"} masked with OLD regions — runs would send outdated, possibly identity-revealing images to the AI. Re-save regions or re-apply masks, then re-review on the Masking tab.`,
      };
    case "text_render_loss":
      return {
        tone,
        to: tab("submissions"),
        message: `${n(w.count, "page")} ${one ? "has" : "have"} PDF text that did not survive rendering (usually a non-embedded CJK font) — the AI grades ${one ? "an image" : "images"} missing that text. Compare the original PDF with the rendered page and re-export or rescan the file.`,
      };

    // --- grading integrity -----------------------------------------------------------
    case "superseded_answers":
      return {
        tone,
        to: tab("review"),
        message: `${n(w.count, "graded answer")} had pages force-replaced after grading — the recorded grades describe the OLD images. Re-grade or re-check them on the Review tab.`,
      };
    case "run_in_progress":
      return {
        tone,
        to: "/runs",
        message: `${n(w.count, "grading run")} ${one ? "is" : "are"} still pending or running — grades keep changing until ${one ? "it finishes" : "they finish"}. Watch ${one ? "it" : "them"} on the Runs page.`,
      };
    case "no_rubric_problems":
      return {
        tone,
        to: tab("problems"),
        message: `${n(w.count, "problem")} ${one ? "has" : "have"} no rubric — the AI cannot grade ${one ? "it" : "them"}. Add rubrics on the Problems tab.`,
      };

    // --- review / officials ----------------------------------------------------------
    case "mixed_method_versions":
      return {
        tone,
        to: tab("review"),
        message: `Official grades mix more than one version of the final grading method — ${n(
          w.count,
          "answer",
        )} ${one ? "is" : "are"} still on an older version. Re-run the latest version, then review.`,
      };
    case "adjusted_spot_checks":
      return {
        tone,
        to: tab("review"),
        message: `${n(w.count, "spot-check")} ${one ? "was" : "were"} marked "adjusted" but the official grade never changed. Enter the corrected grade on the Review tab.`,
      };

    // --- launch-scoped (run preview) -------------------------------------------------
    case "active_run_overlap":
      return {
        tone,
        to: "/runs",
        message:
          "Another run for this assessment is already pending or running — launching now grades the same answers again in parallel. Check the Runs page first.",
      };
    case "provider_disabled":
      return {
        tone,
        to: "/providers",
        message:
          "The chosen method's AI provider is missing or disabled — every item in this run would fail. Enable the provider first.",
      };
    case "missing_reference_solutions":
      return {
        tone,
        to: tab("problems"),
        message: `The chosen method grades against a reference solution, but not every problem in scope has one — launching would be refused. Add the missing reference solutions on the Problems tab${
          w.detail ? ` (${w.detail})` : ""
        }.`,
      };

    // --- publish-scoped (publish preview) --------------------------------------------
    case "email_file_provider":
      return {
        tone,
        message:
          'Email is using the "file" provider — grade emails are written to files on the server, and students receive nothing. Configure a real email provider before publishing for real.',
      };
    case "email_replyto_dead":
      return {
        tone,
        message:
          "Regrade replies can never be received: the SMTP provider is send-only but a reply-to domain is set. Students who reply to their grade email get silence.",
      };
    case "skipped_students":
      return {
        tone,
        to: tab("identify"),
        message: `${n(w.count, "student")} will receive NO email — every problem of theirs is marked no-submission. If one of them actually submitted, their pages are stranded in intake.`,
      };
    case "final_source_no_records":
      return {
        tone,
        to: tab("publish"),
        message:
          "The chosen final grading source hasn't graded this exam — nothing will become official. Pick a method that has grades on the Publish tab.",
      };
    case "no_regrade_deadline":
      return {
        tone,
        to: tab("regrades"),
        message:
          "No regrade deadline is set — replies will be accepted indefinitely. Set one on the Regrade rounds tab.",
      };

    // Unknown code (newer backend than frontend): still surface it rather than
    // silently dropping a hazard.
    default:
      return {
        tone,
        message: `${w.code.replaceAll("_", " ")}${w.count ? `: ${w.count}` : ""}${
          w.detail ? ` (${w.detail})` : ""
        }.`,
      };
  }
}
