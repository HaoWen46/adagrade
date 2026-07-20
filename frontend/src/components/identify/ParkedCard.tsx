// Parked-page resolution: two sections sourced from the same ["scan-pages"]
// query, split by parked_reason.
//
// Conflicts (parked_reason === "conflict"): parked page vs. incumbent side by
// side when parked_against is set (the "against a live submission" case has no
// incumbent page image to show). "Keep incumbent" discards the parked page;
// "Replace" retracts/unassigns the incumbent and assigns the parked page. The
// backend's graded guard on replace maps to a 400 ApiError with a fixed
// "requires force" message — only that specific failure reveals the force
// checkbox; any other failure (e.g. a published-answer 400) shows the plain
// error text with the checkbox hidden, since force cannot override published.
//
// Duplicates (parked_reason === "duplicate"): collapsed by default, one row per
// page with a Discard action, plus a sequential "Discard all duplicates" loop.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../../lib/api";
import type { ScanPage } from "../../lib/types";
import { Button, Card, Spinner } from "../ui";
import { SafeImage } from "../SafeImage";
import { HelpTip } from "../HelpTip";
import { scanParkedHelp } from "../../lib/helpContent";

async function invalidateAll(
  queryClient: ReturnType<typeof useQueryClient>,
  assessmentId: string,
) {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: ["scan-pages", assessmentId] }),
    queryClient.invalidateQueries({ queryKey: ["scan-matrix", assessmentId] }),
    queryClient.invalidateQueries({ queryKey: ["scan-batches", assessmentId] }),
  ]);
}

export function ParkedCard({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();

  const pages = useQuery({
    queryKey: ["scan-pages", assessmentId],
    queryFn: () => api.get<{ pages: ScanPage[] }>(`/api/assessments/${assessmentId}/scan-pages`),
    refetchInterval: (q) => {
      const ps = q.state.data?.pages ?? [];
      return ps.some((p) => p.state === "processing" || p.state === "assigned") ? 2000 : false;
    },
  });

  const resolve = useMutation({
    mutationFn: ({
      pageId,
      action,
      force,
    }: {
      pageId: number;
      action: "keep" | "replace";
      force: boolean;
    }) =>
      api.post<{ resolved: true }>(`/api/scan-pages/${pageId}/resolve-conflict`, {
        action,
        force,
      }),
    onSuccess: async () => invalidateAll(queryClient, assessmentId),
  });

  const discard = useMutation({
    mutationFn: ({ pageId, reason }: { pageId: number; reason: string }) =>
      api.post<{ discarded: true }>(`/api/scan-pages/${pageId}/discard`, { reason }),
    onSuccess: async () => invalidateAll(queryClient, assessmentId),
  });

  if (pages.isPending) {
    return (
      <Card title="Parked pages">
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      </Card>
    );
  }
  if (pages.isError) {
    return (
      <Card title="Parked pages">
        <p className="text-sm text-red-600">{pages.error.message}</p>
      </Card>
    );
  }

  const parked = pages.data.pages.filter((p) => p.state === "parked");
  const conflicts = parked.filter((p) => p.parked_reason === "conflict");
  const duplicates = parked.filter((p) => p.parked_reason === "duplicate");

  return (
    <Card
      title={
        <span className="inline-flex items-center gap-1.5">
          Parked pages
          <HelpTip title="Parked pages">{scanParkedHelp}</HelpTip>
        </span>
      }
      actions={
        <span className="text-xs text-neutral-500 tabular-nums">
          {conflicts.length} conflicts · {duplicates.length} duplicates
        </span>
      }
    >
      <div className="space-y-4">
        {/* Honesty line (workflow-guards plan 2026-07-10): resolving a conflict is
            destructive for the loser — say so before the buttons, not after. */}
        <p className="text-xs text-neutral-500">
          Keeping one page discards the other — the system stores one page per student per
          problem.
        </p>
        <div className="space-y-3">
          <h3 className="text-xs font-semibold tracking-wide text-neutral-500 uppercase">
            Conflicts
          </h3>
          {conflicts.length === 0 ? (
            <p className="text-sm text-neutral-400">No conflicts.</p>
          ) : (
            conflicts.map((page) => (
              <ConflictRow
                key={page.id}
                page={page}
                onKeep={() => resolve.mutate({ pageId: page.id, action: "keep", force: false })}
                onReplace={(force) => resolve.mutate({ pageId: page.id, action: "replace", force })}
                // Gate pending/error on THIS row's page id: one shared mutation
                // drives every row, so without the guard a click on one conflict
                // made every row read "Working…" and surfaced its error (and force
                // checkbox) under all of them (HCI audit).
                pending={resolve.isPending && resolve.variables?.pageId === page.id}
                error={
                  resolve.isError && resolve.variables?.pageId === page.id
                    ? resolve.error
                    : undefined
                }
              />
            ))
          )}
        </div>

        <details className="rounded-md border border-neutral-200">
          <summary className="cursor-pointer px-3 py-2 text-xs font-semibold tracking-wide text-neutral-500 uppercase">
            Duplicates ({duplicates.length})
          </summary>
          <div className="space-y-2 border-t border-neutral-200 p-3">
            {duplicates.length === 0 ? (
              <p className="text-sm text-neutral-400">No duplicates.</p>
            ) : (
              <>
                <div className="flex justify-end">
                  <DiscardAllButton
                    pages={duplicates}
                    onDiscardOne={(pageId) =>
                      discard.mutateAsync({ pageId, reason: "duplicate" })
                    }
                  />
                </div>
                {duplicates.map((page) => (
                  <div
                    key={page.id}
                    className="flex items-center justify-between gap-2 rounded-md px-2 py-1.5 text-sm hover:bg-neutral-50"
                  >
                    <span className="font-mono text-xs text-neutral-600">
                      page #{page.id}
                      {page.parked_against && ` — dup of #${page.parked_against}`}
                    </span>
                    <Button
                      variant="danger"
                      className="px-2.5 py-1 text-xs"
                      disabled={discard.isPending}
                      onClick={() => discard.mutate({ pageId: page.id, reason: "duplicate" })}
                    >
                      Discard
                    </Button>
                  </div>
                ))}
              </>
            )}
            {discard.isError && <p className="text-xs text-red-600">{discard.error.message}</p>}
          </div>
        </details>
      </div>
    </Card>
  );
}

/** Only the backend's fixed-vocabulary graded-guard 400 reveals the force
 * checkbox — any other failure (e.g. a published-answer block) shows its
 * message with the checkbox hidden, since force cannot override it. */
function needsForce(error: Error): boolean {
  return error instanceof ApiError && error.status === 400 && /requires force/i.test(error.message);
}

function ConflictRow({
  page,
  onKeep,
  onReplace,
  pending,
  error,
}: {
  page: ScanPage;
  onKeep: () => void;
  onReplace: (force: boolean) => void;
  pending: boolean;
  error?: Error;
}) {
  const [force, setForce] = useState(false);
  const [attempted, setAttempted] = useState(false);

  return (
    <div className="space-y-2 rounded-md border border-neutral-200 p-3">
      <div className="flex flex-wrap items-center gap-1.5 text-xs text-neutral-500">
        <span className="font-mono">page #{page.id}</span>
        {page.parked_against && <span>vs. incumbent #{page.parked_against}</span>}
        {!page.parked_against && page.proposed_student_id && (
          <span>
            vs. submission for {page.proposed_student_id}
            {page.proposed_problem_id && ` (problem ${page.proposed_problem_id})`}
          </span>
        )}
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        <div className="space-y-1">
          <p className="text-[11px] font-medium text-neutral-500">Parked page</p>
          <SafeImage
            src={`/api/scan-pages/${page.id}/image`}
            alt="Parked page"
            className="h-40 w-full rounded-md border border-neutral-200 object-contain"
          />
        </div>
        {page.parked_against ? (
          <div className="space-y-1">
            <p className="text-[11px] font-medium text-neutral-500">Incumbent page</p>
            <SafeImage
              src={`/api/scan-pages/${page.parked_against}/image`}
              alt="Incumbent page"
              className="h-40 w-full rounded-md border border-neutral-200 object-contain"
            />
          </div>
        ) : (
          <div className="flex h-40 items-center justify-center rounded-md bg-neutral-50 text-xs text-neutral-400">
            Covered by a direct submission — no page image to compare.
          </div>
        )}
      </div>

      {attempted && error && (
        <div className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
          <p>{error.message}</p>
          {needsForce(error) && (
            <label className="mt-1.5 flex items-center gap-1.5">
              <input
                type="checkbox"
                checked={force}
                onChange={(e) => setForce(e.target.checked)}
                className="size-3.5 accent-indigo-600"
              />
              Force (overrides the graded-answer guard)
            </label>
          )}
        </div>
      )}

      {/* No parked_against means the incumbent is a live direct submission,
          not a scan page — Replace has nothing to unassign/retract and the
          backend now rejects it with a 400 (D65). Point at the actual
          remedy: the Retract control on the Submissions tab (HCI audit). */}
      {!page.parked_against && (
        <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
          This cell is covered by a direct submission. On the{" "}
          <a href="?tab=submissions" className="font-medium underline">
            Submissions tab
          </a>
          , find{" "}
          {page.proposed_student_id ? (
            <>
              <span className="font-mono">{page.proposed_student_id}</span>&apos;s
            </>
          ) : (
            "that student's"
          )}{" "}
          reconciliation row and click <strong>Retract</strong> to free the cell, then re-run
          identification or assign manually.
        </p>
      )}

      <div className="flex justify-end gap-2">
        <Button
          variant="secondary"
          className="px-2.5 py-1 text-xs"
          disabled={pending}
          // Set attempted here too: the error block renders on `attempted && error`,
          // so without this a failed Keep (e.g. a colleague resolved the same
          // conflict first → 400 "page is not parked") showed nothing (HCI audit).
          onClick={() => {
            setAttempted(true);
            onKeep();
          }}
        >
          {pending ? "Working…" : "Keep incumbent"}
        </Button>
        {page.parked_against && (
          <Button
            variant="danger"
            className="px-2.5 py-1 text-xs"
            disabled={pending}
            onClick={() => {
              setAttempted(true);
              onReplace(force);
            }}
          >
            {pending ? "Working…" : "Replace"}
          </Button>
        )}
      </div>
    </div>
  );
}

function DiscardAllButton({
  pages,
  onDiscardOne,
}: {
  pages: ScanPage[];
  onDiscardOne: (pageId: number) => Promise<unknown>;
}) {
  const [running, setRunning] = useState(false);
  const [progress, setProgress] = useState<{
    done: number;
    total: number;
    succeeded: number;
    firstError?: string;
  } | null>(null);

  const run = async () => {
    setRunning(true);
    setProgress(null);
    let succeeded = 0;
    let firstError: string | undefined;
    try {
      // Sequential loop (not Promise.all): each discard invalidates/refetches
      // shared queries, and running them one at a time avoids racing writes to
      // the same page list. Continue on error so one bad page doesn't stall
      // the rest of the batch — failures are tallied and the first message is
      // surfaced once the loop finishes.
      for (let i = 0; i < pages.length; i++) {
        try {
          await onDiscardOne(pages[i].id);
          succeeded++;
        } catch (err) {
          if (firstError === undefined) {
            firstError = err instanceof Error ? err.message : String(err);
          }
        }
        setProgress({ done: i + 1, total: pages.length, succeeded, firstError });
      }
    } finally {
      setRunning(false);
    }
  };

  return (
    <div className="flex flex-col items-end gap-1">
      <Button variant="danger" className="px-2.5 py-1 text-xs" disabled={running} onClick={run}>
        {running ? "Discarding…" : "Discard all duplicates"}
      </Button>
      {progress && (
        <p className="text-xs text-neutral-500">
          Discarded {progress.succeeded} of {progress.total}
          {progress.firstError && (
            <span className="text-red-600"> — {progress.firstError}</span>
          )}
        </p>
      )}
    </div>
  );
}
