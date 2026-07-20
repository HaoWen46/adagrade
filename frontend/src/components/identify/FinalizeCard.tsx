// Assessment-wide finalize: promotes every assigned page to a submission,
// incrementally and repeatably (already-promoted pages are a no-op on
// re-finalize). Missing roster×problem cells (from the matrix's empty cells)
// block finalize until acknowledged — mirrors the old per-batch
// ReconciliationCard pattern (git show 54e036d:frontend/src/components/
// identify/ReconciliationCard.tsx) but scoped to the whole assessment instead
// of one batch, and sourced from scan-missing instead of a batch reconciliation
// payload.
//
// Promotion progress is NOT read from the POST response — it's the same
// ["scan-pages"] poll every other Task 11/12 card shares (state === "assigned"
// keeps it polling at 2s until pages flip to "promoted").

import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../../lib/api";
import type { FinalizeReport, MissingCell, ScanPage } from "../../lib/types";
import { Button, Card, Dialog, Spinner } from "../ui";
import { HelpTip } from "../HelpTip";
import { WorkflowNotice } from "../WorkflowNotice";
import { scanFinalizeHelp } from "../../lib/helpContent";

function finalizeErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 409) {
      const details = err.details as Record<string, unknown> | undefined;
      if (typeof details?.missing_cells === "number") {
        return `${details.missing_cells} cell(s) have no page yet — acknowledge to finalize anyway.`;
      }
    }
    return err.message;
  }
  return err instanceof Error ? err.message : "Finalize failed.";
}

/** Groups MissingCell rows by student for display: "B11902001 · 王小明 — Q2, Q3". */
function groupMissingByStudent(
  missing: MissingCell[],
): { student_id: string; name: string; problems: number[] }[] {
  const byStudent = new Map<string, { student_id: string; name: string; problems: number[] }>();
  for (const m of missing) {
    const entry = byStudent.get(m.student_id);
    if (entry) {
      entry.problems.push(m.problem_number);
    } else {
      byStudent.set(m.student_id, { student_id: m.student_id, name: m.name, problems: [m.problem_number] });
    }
  }
  return Array.from(byStudent.values());
}

export function FinalizeCard({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [acked, setAcked] = useState(false);

  // Shared with MatrixCard/OrphanQueue/ParkedCard's polling predicate: "assigned"
  // pages are mid-promotion, so keep polling until they settle to "promoted".
  const pages = useQuery({
    queryKey: ["scan-pages", assessmentId],
    queryFn: () => api.get<{ pages: ScanPage[] }>(`/api/assessments/${assessmentId}/scan-pages`),
    refetchInterval: (q) => {
      const ps = q.state.data?.pages ?? [];
      return ps.some((p) => p.state === "processing" || p.state === "assigned") ? 2000 : false;
    },
  });

  // Only fetched while the ack dialog is open — missing cells aren't part of
  // the pages payload, and there's no reason to poll them the rest of the time.
  const missing = useQuery({
    queryKey: ["scan-missing", assessmentId],
    queryFn: () => api.get<{ missing: MissingCell[] }>(`/api/assessments/${assessmentId}/scan-missing`),
    enabled: confirmOpen,
  });

  const finalize = useMutation({
    mutationFn: (ack_missing: boolean) =>
      api.post<FinalizeReport>(`/api/assessments/${assessmentId}/scan-finalize`, { ack_missing }),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["scan-pages", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["scan-matrix", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["scan-batches", assessmentId] }),
      ]);
      setConfirmOpen(false);
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409) {
        const details = err.details as Record<string, unknown> | undefined;
        if (typeof details?.missing_cells === "number" && details.missing_cells > 0) {
          setAcked(false);
          setConfirmOpen(true);
          void queryClient.invalidateQueries({ queryKey: ["scan-missing", assessmentId] });
        }
      }
    },
  });

  // Hooks must all run before ANY early return (React #310: the pending/error
  // returns below would otherwise skip the effect on first render, then the
  // resolved render adds a hook and unmounts the whole tree). Counts derive
  // null-safely so this block is order-independent of the query state.
  const allPages = pages.data?.pages ?? [];
  const assignedCount = allPages.filter((p) => p.state === "assigned").length;
  const promotedCount = allPages.filter((p) => p.state === "promoted").length;
  // Derived from the mutation ALONE — promotedCount must NOT participate.
  // promotedCount is server-persisted and stays > 0 forever after round 1's
  // first promotion, so `assignedCount > 0 && promotedCount > 0` would
  // deadlock round 2: as soon as any later page gets (re-)assigned,
  // assignedCount > 0 again and the button locks on "Promoting…"
  // permanently, even though no finalize call is in flight. Gating on
  // finalize.isSuccess instead means this only fires for the run that just
  // happened; finalize is idempotent, so a stale-enabled button after a page
  // reload (which resets isSuccess to false) is harmless — worst case the
  // user re-clicks Finalize and it's a no-op for already-promoted pages.
  const promoting = finalize.isSuccess && assignedCount > 0;

  // Round complete: clear isSuccess so newly assigned pages re-enable Finalize
  // instead of re-triggering the promoting lock (mutation state has no other
  // natural reset point in a long-lived tab).
  useEffect(() => {
    if (finalize.isSuccess && assignedCount === 0) {
      finalize.reset();
    }
  }, [finalize.isSuccess, assignedCount]);

  if (pages.isPending) {
    return (
      <Card title="Finalize">
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      </Card>
    );
  }
  if (pages.isError) {
    return (
      <Card title="Finalize">
        <p className="text-sm text-red-600">{pages.error.message}</p>
      </Card>
    );
  }

  const groupedMissing = groupMissingByStudent(missing.data?.missing ?? []);
  const missingCellCount = missing.data?.missing.length ?? 0;

  return (
    <Card
      title="Finalize"
      actions={
        <span className="inline-flex items-center gap-1.5">
          <Button
            onClick={() => finalize.mutate(false)}
            disabled={finalize.isPending || promoting}
          >
            {finalize.isPending ? "Finalizing…" : promoting ? "Promoting…" : "Finalize"}
          </Button>
          <HelpTip title="Finalize">{scanFinalizeHelp}</HelpTip>
        </span>
      }
    >
      <p className="text-sm text-neutral-600">
        Promotes every assigned page to a submission answer. Finalize is assessment-wide and
        incremental — safe to re-run any time as more pages get identified; already-promoted
        pages are left alone.
      </p>

      <div className="mt-3 flex flex-wrap gap-4 text-xs text-neutral-500 tabular-nums">
        <span>{assignedCount} ready to promote</span>
        <span>{promotedCount} promoted</span>
      </div>

      {/* Workflow-guard banner (plan 2026-07-10, assigned_unpromoted_pages): derived
          from the card's own polled page counts — the same state the standing
          warnings endpoint counts — so it can never disagree with the numbers
          above. Hidden while a just-clicked Finalize is promoting them. */}
      {assignedCount > 0 && !promoting && !finalize.isPending && (
        <div className="mt-3">
          <WorkflowNotice tone="warning">
            Assignments take effect only after Finalize — {assignedCount} resolved page
            {assignedCount === 1 ? " is" : "s are"} waiting.
          </WorkflowNotice>
        </div>
      )}

      {finalize.isError && !confirmOpen && (
        <p className="mt-3 text-sm text-red-600">{finalizeErrorMessage(finalize.error)}</p>
      )}
      {finalize.isSuccess && !promoting && (
        <p className="mt-3 text-sm text-green-700">
          Enqueued {finalize.data.enqueued} page promotion{finalize.data.enqueued === 1 ? "" : "s"}
          {" · "}
          {finalize.data.already_promoted} already promoted.
        </p>
      )}
      {promoting && (
        <p className="mt-3 flex items-center gap-2 text-sm text-neutral-600">
          <Spinner className="size-4" />
          Promoting pages to submissions — {promotedCount}/{promotedCount + assignedCount} done.
        </p>
      )}

      <Dialog open={confirmOpen} onClose={() => setConfirmOpen(false)} title="Finalize with missing cells">
        <div className="space-y-3">
          <p className="text-sm text-neutral-600">
            Some student × problem cells have no page yet. Finalizing now will proceed without
            them — you can finalize again later once more pages are identified.
          </p>
          {missing.isPending ? (
            <div className="flex justify-center py-4">
              <Spinner className="size-5" />
            </div>
          ) : missing.isError ? (
            <p className="text-sm text-red-600">{missing.error.message}</p>
          ) : (
            <ul className="max-h-48 space-y-0.5 overflow-y-auto text-sm">
              {groupedMissing.map((g) => (
                <li key={g.student_id}>
                  <span className="font-mono text-xs tabular-nums">{g.student_id}</span>{" "}
                  <span className="text-neutral-600">{g.name}</span>
                  {" — "}
                  <span className="text-neutral-500">
                    {g.problems.map((n) => `Q${n}`).join(", ")}
                  </span>
                </li>
              ))}
            </ul>
          )}
          <label className="flex items-start gap-2 text-sm text-neutral-700">
            <input
              type="checkbox"
              checked={acked}
              onChange={(e) => setAcked(e.target.checked)}
              className="mt-0.5 size-3.5 accent-indigo-600"
            />
            Acknowledge {missingCellCount > 0 ? missingCellCount : ""} missing cell
            {missingCellCount === 1 ? "" : "s"} and finalize anyway.
          </label>
          <div className="flex justify-end gap-2">
            <Button variant="secondary" onClick={() => setConfirmOpen(false)}>
              Cancel
            </Button>
            <Button disabled={!acked || finalize.isPending} onClick={() => finalize.mutate(true)}>
              {finalize.isPending ? "Finalizing…" : "Finalize"}
            </Button>
          </div>
        </div>
      </Dialog>
    </Card>
  );
}
