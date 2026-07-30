// LaTeX transcription export — the always-present ladder card on the Overview tab
// (spec docs/superpowers/specs/2026-07-25-latex-transcription-export-design.md §6.1).
//
// ZIPs of student answers rendered as LaTeX source plus the masked page images, for a
// professor who wants a text rendering of the cohort. Deliberately outside the grading
// workflow: nothing here writes a grade.
//
// Placement (spec §6.1): this card sits directly under the grading-workflow card and is
// rendered from the moment an assessment exists. It never appears, never disappears —
// a control that materializes halfway through an exam's life cannot be planned around,
// and one that is always disabled is dead chrome. What changes is its CONTENT: a ladder
// that names the single thing currently standing between the professor and the artifact
// (problems → student work → mask review), and becomes the downloads table once none do.
//
// Two facts from the spec drive the download rows themselves:
//
//  - The FIRST download of a problem transcribes its uncached answers and takes
//    20–60s. A button that looks idle for 40 seconds is the worst outcome here, so the
//    row's button carries a spinner and a "Preparing…" label, states the wait in
//    words, and refuses re-clicks while in flight.
//  - Transcriptions are cached content-addressed, so a re-download is byte-identical
//    and genuinely free. pending === 0 therefore renders "free" rather than
//    "$0.0000" — the zero is the point, and a fake-looking $0 hides it.
//
// Student answer content flows through the ZIP, never through this component — no
// transcription text is rendered or logged here (CLAUDE.md).

import type { ReactNode } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api, apiDownload } from "../lib/api";
import type { TranscriptionStatusResponse, TranscriptionStatusRow } from "../lib/types";
import { Badge, Button, Card, Spinner, TD, TH, Table, cx, type BadgeTone } from "../components/ui";

/** Cost cell copy. Two non-obvious cases, both deliberate:
 *  - nothing pending → "free" (the ZIP is rebuilt from cache; a "$0.0000" reads like
 *    a rounding artefact rather than the guarantee it actually is)
 *  - no pricing resolved → "unknown", never a fake $0 (D35). */
function costLabel(row: TranscriptionStatusRow): string {
  if (row.pending === 0) return "free";
  return row.est_cost_usd ? `$${row.est_cost_usd}` : "unknown";
}

/** Same two rules as costLabel, but the exam-level row also states how many answers the
 * price covers — it is the one click that can spend real money on a whole cohort. */
function examCostLabel(pending: number, est: string): string {
  if (pending === 0) return "free";
  return `${pending} pending · ${est ? `~$${est}` : "unknown"}`;
}

function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
}

export function TranscriptionExportCard({ assessmentId }: { assessmentId: string }) {
  const status = useQuery({
    queryKey: ["transcription-status", assessmentId],
    queryFn: () =>
      api.get<TranscriptionStatusResponse>(
        `/api/assessments/${assessmentId}/transcription-status`,
      ),
    // A 4xx here is a settled answer (export unavailable, or role-gated), not a blip —
    // retrying it three times only delays the message. Server/network errors still retry.
    retry: (failureCount, error) =>
      !(error instanceof ApiError && error.status >= 400 && error.status < 500) &&
      failureCount < 2,
  });

  return (
    <Card title="LaTeX transcriptions">
      {status.isPending && (
        <div className="flex justify-center py-6">
          <Spinner className="size-5" />
        </div>
      )}

      {/* The export is optional and its endpoints may simply not be enabled on this
          server — a 404 is a "not available here", not a failure the operator can act
          on, so it gets the calm neutral treatment rather than a red error line. */}
      {status.isError &&
        (status.error instanceof ApiError && status.error.status === 404 ? (
          <div className="rounded-md border border-neutral-200 bg-neutral-50 px-3 py-3 text-sm text-neutral-600">
            Transcription export is not available on this server.
          </div>
        ) : (
          <p className="text-sm text-red-600">{status.error.message}</p>
        ))}

      {status.isSuccess && <Ladder assessmentId={assessmentId} data={status.data} />}
    </Card>
  );
}

// --- the ladder ----------------------------------------------------------------------

/** One rung: the single unmet precondition, with the count that measures it and the one
 * link that clears it. `blocked === false` means the downloads table is what renders. */
interface Rung {
  badge: string;
  tone: BadgeTone;
  message: string;
  action: { href: string; label: string };
}

/**
 * Ladder derivation (spec §6.1), evaluated in order — the FIRST unmet gate is the whole
 * content of the card. Reporting three simultaneous blockers on a brand-new assessment
 * would be noise: only the next actionable one is ever shown.
 *
 * `ready` (the exam-level gate) is the server's own verdict, not something re-derived
 * here from the gate counts — the ZIP endpoints refuse with 409 on exactly that flag, so
 * deriving a second opinion client-side could offer a button the server would reject.
 * The three explicit gate checks run first only because `!ready` on its own cannot say
 * WHICH precondition is missing.
 */
function rungFor(
  data: TranscriptionStatusResponse,
  tabHref: (key: string) => string,
): Rung | null {
  const g = data.gates;
  if (g.problems === 0) {
    return {
      badge: "0 problems",
      tone: "neutral",
      message: "Waiting on problems — define the exam's problems first.",
      action: { href: tabHref("problems"), label: "Set up problems →" },
    };
  }
  if (g.students_with_work === 0) {
    return {
      badge: `0/${g.students_total} have work`,
      tone: "neutral",
      message: "Waiting on scans — no student work yet.",
      action: { href: tabHref("submissions"), label: "Upload student work →" },
    };
  }
  if (!data.ready) {
    return {
      badge: "masks incomplete",
      tone: "amber",
      message: `Mask review ${g.pages_mask_accepted}/${g.pages_total} pages accepted — transcription can start when every page's mask is accepted.`,
      action: { href: tabHref("masking"), label: "Review masks →" },
    };
  }
  return null;
}

function Ladder({
  assessmentId,
  data,
}: {
  assessmentId: string;
  data: TranscriptionStatusResponse;
}) {
  const tabHref = (key: string) => `/assessments/${assessmentId}?tab=${key}`;
  const rung = rungFor(data, tabHref);

  if (rung !== null) {
    // Blocked: exactly one line. No dead table, no disabled buttons — the card states
    // where the work actually is and links straight to it.
    return (
      <div className="space-y-1.5">
        <div className="flex flex-wrap items-center gap-2">
          <Badge tone={rung.tone}>{rung.badge}</Badge>
          <p className="text-sm text-neutral-600">{rung.message}</p>
        </div>
        <TabLink href={rung.action.href}>{rung.action.label}</TabLink>
      </div>
    );
  }

  // `?? []` guards Go's encoding/json marshalling a nil slice as `null` (same idiom as
  // the rest of the app); `ready` implies problems exist, so this is belt-and-braces.
  const rows = data.problems ?? [];
  const answers = rows.reduce((n, r) => n + r.answers, 0);

  return (
    <div className="space-y-3">
      <p className="text-xs text-neutral-500">
        Downloads the whole exam, or one problem at a time: each student&apos;s answer as
        LaTeX source plus the masked page images. The first download transcribes whatever
        is still pending and can take up to a minute; after that the transcriptions are
        cached, so re-downloading is free and produces the same bytes.
      </p>
      {/* `configured` is not a rung: when no model is configured the server refuses the
          ZIP itself, and an empty `model` is what that looks like here. */}
      {data.model !== "" && (
        <p className="text-xs text-neutral-500">
          Model {data.model} ·{" "}
          {data.verified
            ? "each .tex is compile-checked before it ships"
            : "no compile gate on this server — the manifest marks the .tex unverified"}
          .
        </p>
      )}
      <Table>
        <thead>
          <tr>
            <TH className="w-16">#</TH>
            <TH>Title</TH>
            <TH className="w-24 text-right">Answers</TH>
            <TH className="w-24 text-right">Cached</TH>
            <TH className="w-24 text-right">Pending</TH>
            <TH className="w-28 text-right">Est. cost</TH>
            <TH className="w-40" />
          </tr>
        </thead>
        <tbody>
          {/* The exam-level bundle leads: "the midterm's LaTeX" is the object the
              professor came for — the per-problem rows are the refinement, not the
              headline (spec §6.1). */}
          <tr className="bg-neutral-50/60">
            <TD colSpan={4} className="font-medium text-neutral-900">
              Entire exam · {plural(rows.length, "problem")}
            </TD>
            <TD colSpan={2} className="text-right tabular-nums text-neutral-600">
              {examCostLabel(data.total_pending, data.total_est_cost_usd)}
            </TD>
            <TD className="text-right">
              <ZipDownload
                assessmentId={assessmentId}
                path={`/api/assessments/${assessmentId}/transcription.zip`}
                filename="transcription.zip"
                pending={data.total_pending}
                disabled={answers === 0}
                title={
                  answers === 0
                    ? "No answers to export yet"
                    : data.total_pending === 0
                      ? "Already transcribed — this download is free"
                      : `Transcribes ${plural(data.total_pending, "answer")} first, which can take up to a minute`
                }
              />
            </TD>
          </tr>
          {rows.length === 0 && (
            <tr>
              <TD colSpan={7} className="text-center text-neutral-400">
                No problems yet.
              </TD>
            </tr>
          )}
          {rows.map((row) => (
            <ExportRow key={row.number} assessmentId={assessmentId} row={row} />
          ))}
        </tbody>
      </Table>
    </div>
  );
}

function ExportRow({
  assessmentId,
  row,
}: {
  assessmentId: string;
  row: TranscriptionStatusRow;
}) {
  const nothingToExport = row.answers === 0;

  return (
    <tr>
      <TD className="font-medium tabular-nums">{row.number}</TD>
      <TD>{row.title || <span className="text-neutral-400">untitled</span>}</TD>
      <TD className="text-right tabular-nums">{row.answers}</TD>
      <TD className="text-right tabular-nums">{row.cached}</TD>
      <TD className={cx("text-right tabular-nums", row.pending > 0 && "text-amber-700")}>
        {row.pending}
      </TD>
      <TD className="text-right tabular-nums">{costLabel(row)}</TD>
      <TD className="text-right">
        {/* A single problem's masks can regress (or lag the others) after this view
            loaded — the exam-level ladder only reaches this table when everything is
            ready, but a refetch can land mid-session. The ZIP endpoint answers 409 in
            that state, so the row says why instead of offering a button that fails. */}
        {row.ready ? (
          <ZipDownload
            assessmentId={assessmentId}
            path={`/api/assessments/${assessmentId}/problems/${row.number}/transcription.zip`}
            filename={`transcription-p${row.number}.zip`}
            pending={row.pending}
            disabled={nothingToExport}
            title={
              nothingToExport
                ? "No answers to export for this problem"
                : row.pending === 0
                  ? "Already transcribed — this download is free"
                  : `Transcribes ${plural(row.pending, "answer")} first, which can take up to a minute`
            }
          />
        ) : (
          <span className="text-xs text-amber-700">
            awaiting masks ({plural(row.pages_pending_mask, "page")})
          </span>
        )}
      </TD>
    </tr>
  );
}

/**
 * One ZIP button plus its own in-flight and error state. The mutation lives per instance
 * on purpose: the exam bundle and two problem rows can be downloading at once without
 * either disabling the others, and a failure lands beside the button that caused it.
 */
function ZipDownload({
  assessmentId,
  path,
  filename,
  pending,
  disabled,
  title,
}: {
  assessmentId: string;
  path: string;
  /** Fallback only — the server's Content-Disposition wins when it sends one. */
  filename: string;
  /** Answers this download would have to transcribe; drives the "how long" copy. */
  pending: number;
  disabled: boolean;
  title: string;
}) {
  const queryClient = useQueryClient();
  const download = useMutation({
    mutationFn: () => apiDownload(path, filename),
    onSuccess: async () => {
      // The pending answers just became cached — refresh so the counts and the "free"
      // cost labels reflect that a re-download now costs nothing.
      await queryClient.invalidateQueries({
        queryKey: ["transcription-status", assessmentId],
      });
    },
  });

  return (
    <>
      <Button
        variant="secondary"
        className="px-2 py-1 text-xs"
        disabled={download.isPending || disabled}
        onClick={() => download.mutate()}
        title={title}
      >
        {download.isPending ? (
          <>
            <Spinner className="size-3.5" /> Preparing…
          </>
        ) : (
          "Download ZIP"
        )}
      </Button>
      {/* The wait is stated in words, not just implied by a spinner: an uncached
          problem can sit here for the better part of a minute. */}
      {download.isPending && (
        <p className="mt-1 text-xs text-neutral-500">
          {pending > 0
            ? `Transcribing ${plural(pending, "answer")} — this can take up to a minute.`
            : "Building the ZIP…"}
        </p>
      )}
      {download.isError && (
        <p className="mt-1 text-xs text-red-600">{download.error.message}</p>
      )}
    </>
  );
}

/** Mirrors OverviewTab's TabLink (not imported: OverviewTab renders this card, and a
 * back-import would close an import cycle). */
function TabLink({ href, children }: { href: string; children: ReactNode }) {
  return (
    <Link to={href} className="text-sm font-medium text-indigo-600 hover:underline">
      {children}
    </Link>
  );
}
