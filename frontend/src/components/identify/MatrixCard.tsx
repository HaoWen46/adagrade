// Assignment matrix: one row per roster student, one column per problem number.
// No virtualization (SubmissionsTab's ~250-row table is the precedent for this
// scale). Cell state -> badge mapping mirrors MatrixCellState 1:1. Clicking a
// cell backed by a page opens a Dialog with the full page image and an
// Unassign action.
//
// "only conflicts" filter: parked pages are NEVER assigned (parked_reason set
// means the page sits OUTSIDE the matrix), so filtering on
// `assigned_student_id === row.student_id` for parked pages is always false —
// this must instead locate the INCUMBENT cell the parked page contests:
//   (a) parked_against set -> the incumbent is another scan page; collect
//       incumbent page ids into a Set, then flag any row with a cell whose
//       page_id is in that Set.
//   (b) parked_against absent -> the conflict is against a live SUBMISSION,
//       not a page; the parked page's own proposed_student_id/proposed_problem_id
//       name the contested cell directly.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../lib/api";
import type { MatrixCellState, ScanMatrix, ScanPage } from "../../lib/types";
import { Badge, Button, Card, Dialog, Spinner, Table, TD, TH, type BadgeTone } from "../ui";
import { SafeImage } from "../SafeImage";
import { HelpTip } from "../HelpTip";
import { scanPageLifecycleHelp } from "../../lib/helpContent";

const CELL_LABEL: Record<MatrixCellState, string> = {
  empty: "–",
  auto: "A",
  manual: "M",
  promoted: "✓",
  submitted: "S",
};

const CELL_TONE: Record<MatrixCellState, BadgeTone> = {
  empty: "red",
  auto: "indigo",
  manual: "indigo",
  promoted: "green",
  submitted: "neutral",
};

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

export function MatrixCard({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();
  const [onlyMissing, setOnlyMissing] = useState(false);
  const [onlyConflicts, setOnlyConflicts] = useState(false);
  const [dialogPageId, setDialogPageId] = useState<number | null>(null);

  const matrix = useQuery({
    queryKey: ["scan-matrix", assessmentId],
    queryFn: () => api.get<ScanMatrix>(`/api/assessments/${assessmentId}/scan-matrix`),
  });

  // Shared with OrphanQueue/ParkedCard's polling predicate (scan-pages query):
  // "assigned" keeps polling because a promote job may still be in flight.
  const pages = useQuery({
    queryKey: ["scan-pages", assessmentId],
    queryFn: () => api.get<{ pages: ScanPage[] }>(`/api/assessments/${assessmentId}/scan-pages`),
    refetchInterval: (q) => {
      const ps = q.state.data?.pages ?? [];
      return ps.some((p) => p.state === "processing" || p.state === "assigned") ? 2000 : false;
    },
  });

  const unassign = useMutation({
    mutationFn: (pageId: number) => api.post<void>(`/api/scan-pages/${pageId}/unassign`),
    onSuccess: async () => {
      await invalidateAll(queryClient, assessmentId);
      setDialogPageId(null);
    },
  });

  if (matrix.isPending || pages.isPending) {
    return (
      <Card title="Assignment matrix">
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      </Card>
    );
  }
  if (matrix.isError) {
    return (
      <Card title="Assignment matrix">
        <p className="text-sm text-red-600">{matrix.error.message}</p>
      </Card>
    );
  }
  if (pages.isError) {
    return (
      <Card title="Assignment matrix">
        <p className="text-sm text-red-600">{pages.error.message}</p>
      </Card>
    );
  }

  const { problems, rows } = matrix.data;
  const allPages = pages.data.pages;

  // (a) incumbent page ids contested by a parked page with parked_against set.
  const incumbentPageIds = new Set(
    allPages
      .filter((p) => p.state === "parked" && p.parked_reason === "conflict" && p.parked_against)
      .map((p) => p.parked_against as number),
  );
  // (b) parked-against-a-submission conflicts, keyed by (student_id, problem_id).
  const submissionConflictKeys = new Set(
    allPages
      .filter(
        (p) =>
          p.state === "parked" &&
          p.parked_reason === "conflict" &&
          !p.parked_against &&
          p.proposed_student_id &&
          p.proposed_problem_id,
      )
      .map((p) => `${p.proposed_student_id}:${p.proposed_problem_id}`),
  );

  const rowHasConflict = (row: ScanMatrix["rows"][number]) =>
    row.cells.some(
      (c) =>
        (c.page_id !== undefined && incumbentPageIds.has(c.page_id)) ||
        submissionConflictKeys.has(`${row.student_id}:${c.problem_id}`),
    );

  const filteredRows = rows.filter((row) => {
    if (onlyMissing && !row.cells.some((c) => c.state === "empty")) return false;
    if (onlyConflicts && !rowHasConflict(row)) return false;
    return true;
  });

  const missingCount = rows.reduce(
    (sum, row) => sum + row.cells.filter((c) => c.state === "empty").length,
    0,
  );

  const dialogPage = dialogPageId ? allPages.find((p) => p.id === dialogPageId) : undefined;

  return (
    <Card
      title={
        <span className="inline-flex items-center gap-1.5">
          Assignment matrix
          <HelpTip title="Page lifecycle">{scanPageLifecycleHelp}</HelpTip>
        </span>
      }
      actions={
        <span className="text-xs text-neutral-500 tabular-nums">
          {rows.length} students · {missingCount} cells missing
        </span>
      }
    >
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <label className="flex items-center gap-1.5 text-xs text-neutral-600">
          <input
            type="checkbox"
            checked={onlyMissing}
            onChange={(e) => setOnlyMissing(e.target.checked)}
            className="size-3.5 accent-indigo-600"
          />
          only missing
        </label>
        <label className="flex items-center gap-1.5 text-xs text-neutral-600">
          <input
            type="checkbox"
            checked={onlyConflicts}
            onChange={(e) => setOnlyConflicts(e.target.checked)}
            className="size-3.5 accent-indigo-600"
          />
          only conflicts
        </label>
      </div>

      {rows.length === 0 ? (
        <p className="text-sm text-neutral-400">No roster students yet.</p>
      ) : filteredRows.length === 0 ? (
        <p className="text-sm text-neutral-400">No rows match the current filters.</p>
      ) : (
        <Table>
          <thead>
            <tr>
              <TH>Student</TH>
              {problems.map((p) => (
                <TH key={p.id} className="text-center">
                  Q{p.number}
                </TH>
              ))}
            </tr>
          </thead>
          <tbody>
            {filteredRows.map((row) => (
              <tr key={row.student_id}>
                <TD>
                  <span className="font-mono text-xs tabular-nums">{row.student_id}</span>
                  <span className="ml-2 text-neutral-500">{row.name}</span>
                </TD>
                {row.cells.map((cell) => (
                  <TD key={cell.problem_id} className="text-center">
                    {cell.page_id !== undefined ? (
                      <button
                        type="button"
                        onClick={() => setDialogPageId(cell.page_id as number)}
                        className="inline-flex"
                        title={`Open page #${cell.page_id}`}
                      >
                        <Badge tone={CELL_TONE[cell.state]}>{CELL_LABEL[cell.state]}</Badge>
                      </button>
                    ) : (
                      <Badge tone={CELL_TONE[cell.state]}>{CELL_LABEL[cell.state]}</Badge>
                    )}
                  </TD>
                ))}
              </tr>
            ))}
          </tbody>
        </Table>
      )}

      <Dialog
        open={dialogPageId !== null}
        onClose={() => setDialogPageId(null)}
        title={dialogPageId ? `Page #${dialogPageId}` : ""}
      >
        {dialogPageId !== null && (
          <div className="space-y-3">
            <SafeImage
              src={`/api/scan-pages/${dialogPageId}/image`}
              alt="Scan page"
              className="mx-auto max-h-[60vh] w-full rounded-md border border-neutral-200 object-contain"
            />
            {/* A promoted page is finalized into a real submission, so Unassign can't
                touch it. That submission is exactly what the Retract control on the
                Submissions tab removes — explain the block and name the remedy rather
                than leaving a permanently-disabled button (HCI audit). */}
            {dialogPage?.state === "promoted" && (
              <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
                This page is finalized into{" "}
                {dialogPage.assigned_student_id ? (
                  <>
                    <span className="font-mono">{dialogPage.assigned_student_id}</span>&apos;s
                  </>
                ) : (
                  "a"
                )}{" "}
                submission, so it can no longer be unassigned here. To put it on a different
                student, go to the{" "}
                <a href="?tab=submissions" className="font-medium underline">
                  Submissions tab
                </a>
                , find that student&apos;s reconciliation row, and click <strong>Retract</strong> —
                that frees the cell so you can re-identify or re-upload the page.
              </p>
            )}
            {unassign.isError && <p className="text-xs text-red-600">{unassign.error.message}</p>}
            <div className="flex justify-end gap-2">
              <Button variant="secondary" onClick={() => setDialogPageId(null)}>
                Close
              </Button>
              {dialogPage?.state !== "promoted" && (
                <Button
                  variant="danger"
                  disabled={unassign.isPending}
                  onClick={() => unassign.mutate(dialogPageId)}
                >
                  {unassign.isPending ? "Unassigning…" : "Unassign"}
                </Button>
              )}
            </div>
          </div>
        )}
      </Dialog>
    </Card>
  );
}
