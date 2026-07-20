// Masking tab: draw/drag/resize normalized mask regions on a sample page image,
// save + apply them, then keyboard-review every masked page (a/f, j/k).

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import type {
  AnswerResponse,
  ApplyMasksResult,
  MaskRegion,
  MaskReviewPage,
  ProblemStudentRow,
  ProblemSummary,
} from "../lib/types";
import { maskRegionControlsHelp, maskingHelp } from "../lib/helpContent";
import { useSessionDraft } from "../lib/useSessionDraft";
import { useWorkflowWarnings } from "../lib/warnings";
import { Badge, Button, Card, Dialog, IconButton, Input, Select, Spinner, cx } from "../components/ui";
import { X } from "../components/icons";
import { HelpTip } from "../components/HelpTip";
import { SafeImage } from "../components/SafeImage";
import { RectEditor } from "../components/RectEditor";
import { WorkflowNotice } from "../components/WorkflowNotice";

export function MaskingTab({ assessmentId }: { assessmentId: string }) {
  const regions = useQuery({
    queryKey: ["mask-regions", assessmentId],
    queryFn: () => api.get<{ regions: MaskRegion[] }>(`/api/assessments/${assessmentId}/mask-regions`),
  });
  const sample = useSamplePage(assessmentId);

  // Workflow-guard standing banner (plan 2026-07-10): mask_errors is a danger —
  // a failed re-mask can leave a page carrying its OLD accepted mask, which the
  // per-page badges below don't make obvious as a grading hazard.
  const warnings = useWorkflowWarnings(assessmentId);
  const maskErrors = warnings.data?.warnings.find((w) => w.code === "mask_errors");
  // stale_masks (stale-mask fix 2026-07-11): accepted pages whose masked copy
  // was made under OLD regions — the grading gates now block them, but the
  // per-page badges alone don't say WHY pages fell back to pending.
  const staleMasks = warnings.data?.warnings.find((w) => w.code === "stale_masks");

  return (
    <div className="space-y-4">
      {maskErrors !== undefined && (
        <WorkflowNotice tone="danger">
          {maskErrors.count ?? 0} page{(maskErrors.count ?? 0) === 1 ? "" : "s"} failed masking
          and still carry an OLD accepted mask — the AI would see the unmasked/stale image.
          Re-draw regions or re-apply.
        </WorkflowNotice>
      )}
      {staleMasks !== undefined && (
        <WorkflowNotice tone="danger">
          {staleMasks.count ?? 0} accepted page{(staleMasks.count ?? 0) === 1 ? " was" : "s were"}{" "}
          masked with OLD regions — runs would send outdated, possibly identity-revealing images
          to the AI. Save the regions again (or re-apply masks), then re-review.
        </WorkflowNotice>
      )}
      <Card
        title={
          <span className="inline-flex items-center gap-1.5">
            Mask regions <HelpTip title="Masking">{maskingHelp}</HelpTip>
          </span>
        }
      >
        {regions.isPending || sample.pending ? (
          <div className="flex justify-center py-10">
            <Spinner className="size-6" />
          </div>
        ) : regions.isError ? (
          <p className="text-sm text-red-600">{regions.error.message}</p>
        ) : (
          <RegionEditor
            assessmentId={assessmentId}
            samplePageId={sample.pageId}
            initial={regions.data.regions}
          />
        )}
      </Card>
      <ApplyMasksCard assessmentId={assessmentId} />
      <MaskReviewPanel assessmentId={assessmentId} />
    </div>
  );
}

// --- sample page: first page of the first student that has pages --------------------

function useSamplePage(assessmentId: string): { pageId?: number; pending: boolean } {
  const summary = useQuery({
    queryKey: ["problem-summaries", assessmentId],
    queryFn: () =>
      api.get<{ problems: ProblemSummary[] }>(`/api/assessments/${assessmentId}/problems/summary`),
  });
  const problemId = summary.data?.problems.find((p) => p.with_pages > 0)?.problem_id;

  const students = useQuery({
    queryKey: ["problem-students", problemId],
    queryFn: () => api.get<{ students: ProblemStudentRow[] }>(`/api/problems/${problemId}/students`),
    enabled: problemId !== undefined,
  });
  const answerId = students.data?.students.find((s) => s.page_count > 0)?.answer_id;

  const answer = useQuery({
    queryKey: ["answer", String(answerId)],
    queryFn: () => api.get<AnswerResponse>(`/api/answers/${answerId}`),
    enabled: answerId !== undefined,
  });

  const pending =
    summary.isPending ||
    (problemId !== undefined && students.isPending) ||
    (answerId !== undefined && answer.isPending);
  return { pageId: answer.data?.pages[0]?.id, pending };
}

// --- region editor -------------------------------------------------------------------

const NEW_REGION: Omit<MaskRegion, "x" | "y" | "w" | "h"> = {
  page_scope: "first",
  color: "#4a4a4a",
  padding: 0.01,
};

const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));

/** Region fill: the region's hex color at ~40% alpha (fallback for odd input). */
function fillColor(color: string): string {
  return /^#[0-9a-fA-F]{6}$/.test(color) ? `${color}66` : "rgba(74,74,74,0.4)";
}

function RegionEditor({
  assessmentId,
  samplePageId,
  initial,
}: {
  assessmentId: string;
  samplePageId?: number;
  initial: MaskRegion[];
}) {
  const queryClient = useQueryClient();
  // Local draft, seeded from the server; Save PUTs the whole set back. Backed by
  // sessionStorage (HCI finding C) so a tab switch — which unmounts this editor —
  // doesn't silently destroy an in-progress rectangle: the draft is restored on the
  // next mount and cleared on a successful Save. `draft === null` until the first
  // edit, so we fall back to the freshly-fetched server regions until then.
  const maskDraft = useSessionDraft<MaskRegion[]>(`mask-regions:${assessmentId}`);
  const regions = maskDraft.draft ?? initial;
  const setRegions = maskDraft.setDraft;
  const [selected, setSelected] = useState<number | null>(null);

  const save = useMutation({
    mutationFn: () =>
      api.put<{ saved: number }>(`/api/assessments/${assessmentId}/mask-regions`, { regions }),
    // The PUT also reconciles review acceptances server-side (stale-mask fix
    // 2026-07-11): accepted pages whose fingerprint no longer matches fall back to
    // pending. Cancel any in-flight review-list fetch (its response predates the PUT)
    // and re-sync the review panel + the stale_masks/mask_errors banner alongside the
    // regions themselves, so the panel doesn't keep showing acceptances the server
    // just revoked.
    onMutate: async () => {
      await queryClient.cancelQueries({ queryKey: ["mask-review", assessmentId] });
    },
    onSuccess: async () => {
      // The PUT replaces the whole set. Seed that server truth before clearing the
      // local fallback, then refetch regions plus the reconciled review/warning data.
      queryClient.setQueryData<{ regions: MaskRegion[] }>(["mask-regions", assessmentId], {
        regions,
      });
      maskDraft.clear();
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["mask-regions", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["mask-review", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["workflow-warnings", assessmentId] }),
      ]);
    },
  });

  const sel = selected !== null && selected < regions.length ? selected : null;

  const setRegion = (i: number, patch: Partial<MaskRegion>) =>
    setRegions(regions.map((r, j) => (j === i ? { ...r, ...patch } : r)));

  return (
    <div className="space-y-3">
      {samplePageId === undefined ? (
        <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
          No pages available yet — upload submissions first to preview regions on a sample page.
        </p>
      ) : (
        <>
          <p className="text-xs text-neutral-500">
            Drag on the page to add a region; drag a region to move it, or its corner handle to
            resize. Coordinates are normalized, so regions apply to every student.
          </p>
          <RectEditor
            rects={regions}
            onChange={setRegions}
            imageUrl={`/api/answer-pages/${samplePageId}/image`}
            imageAlt="Sample page"
            selectedIndex={selected}
            onSelect={setSelected}
            newRect={NEW_REGION}
            rectStyle={(r) => ({ backgroundColor: fillColor(r.color) })}
          />
        </>
      )}

      <div className="space-y-1.5">
        {regions.length === 0 ? (
          <p className="text-sm text-neutral-400">No regions defined.</p>
        ) : (
          /* Column labels for the per-region rows below — same grid template. */
          <div className="grid grid-cols-[2.5rem_9rem_7rem_6rem_auto_minmax(0,1fr)] items-center gap-2 px-2 text-xs font-medium text-neutral-500">
            <span />
            <span className="inline-flex items-center gap-1.5">
              Page scope <HelpTip title="Region settings">{maskRegionControlsHelp}</HelpTip>
            </span>
            <span>Color</span>
            <span className="text-right">Padding</span>
          </div>
        )}
        {regions.length > 0 &&
          regions.map((r, i) => (
            <div
              key={i}
              onClick={() => setSelected(i)}
              className={cx(
                "grid grid-cols-[2.5rem_9rem_7rem_6rem_auto_minmax(0,1fr)] items-center gap-2 rounded-md px-2 py-1",
                sel === i ? "bg-indigo-50" : "hover:bg-neutral-50",
              )}
            >
              <span className="text-xs font-medium text-neutral-600">#{i + 1}</span>
              <Select
                value={r.page_scope}
                onChange={(e) =>
                  setRegion(i, { page_scope: e.target.value as MaskRegion["page_scope"] })
                }
                className="w-full"
              >
                <option value="first">first page</option>
                <option value="all">all pages</option>
              </Select>
              <Input
                value={r.color}
                onChange={(e) => setRegion(i, { color: e.target.value })}
                className="py-1 font-mono text-xs"
              />
              <Input
                type="number"
                step="0.005"
                min="0"
                value={r.padding}
                onChange={(e) =>
                  setRegion(i, { padding: e.target.value === "" ? 0 : Number(e.target.value) })
                }
                className="py-1 text-right text-xs tabular-nums"
              />
              <IconButton
                variant="danger"
                label={`Remove region #${i + 1}`}
                onClick={(e) => {
                  e.stopPropagation();
                  setRegions(regions.filter((_, j) => j !== i));
                  setSelected(null);
                }}
              >
                <X />
              </IconButton>
              <span className="truncate text-right text-[11px] text-neutral-400 tabular-nums">
                x {r.x.toFixed(3)} y {r.y.toFixed(3)} w {r.w.toFixed(3)} h {r.h.toFixed(3)}
              </span>
            </div>
          ))}
      </div>

      {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}
      <div className="flex items-center justify-end gap-3">
        {maskDraft.dirty && (
          <span className="mr-auto text-xs text-amber-600">
            Unsaved changes — kept if you switch tabs, cleared when you Save.
          </span>
        )}
        {!maskDraft.dirty && save.isSuccess && (
          <span className="text-xs text-green-700">Saved.</span>
        )}
        <Button onClick={() => save.mutate()} disabled={save.isPending}>
          {save.isPending ? "Saving…" : "Save regions"}
        </Button>
      </div>
    </div>
  );
}

// --- apply --------------------------------------------------------------------------

function ApplyMasksCard({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();
  const apply = useMutation({
    mutationFn: () => api.post<ApplyMasksResult>(`/api/assessments/${assessmentId}/masks/apply`),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["mask-review", assessmentId] });
      // Masked flags on pages change everywhere answers are shown.
      await queryClient.invalidateQueries({ queryKey: ["answer"] });
    },
  });

  return (
    <Card title="Apply masks">
      <div className="flex items-center gap-3">
        <Button onClick={() => apply.mutate()} disabled={apply.isPending}>
          {apply.isPending ? "Queuing…" : "Apply masks"}
        </Button>
        <p className="text-xs text-neutral-500">
          Masking now runs in the background: re-applying skips pages whose regions haven&apos;t
          changed and preserves their review status. Watch the panel below for progress.
        </p>
      </div>
      {apply.isSuccess && (
        <p className="mt-3 text-sm text-green-700">
          Queued {apply.data.enqueued} page{apply.data.enqueued === 1 ? "" : "s"}
          {apply.data.skipped > 0
            ? ` (${apply.data.skipped} already up to date).`
            : "."}
        </p>
      )}
      {apply.isError && <p className="mt-3 text-sm text-red-600">{apply.error.message}</p>}
    </Card>
  );
}

// --- mask review strip -----------------------------------------------------------------

const REVIEW_TONES: Record<string, "neutral" | "green" | "red"> = {
  pending: "neutral",
  accepted: "green",
  flagged: "red",
};

function MaskReviewPanel({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();
  const list = useQuery({
    queryKey: ["mask-review", assessmentId],
    queryFn: () =>
      api.get<{ pages: MaskReviewPage[] }>(`/api/assessments/${assessmentId}/masks/review`),
    // Masking now runs as background mask.page jobs (D27, F2): poll while any page
    // is still unmasked so newly-masked derivatives show up without a manual
    // refresh. A page that carries a mask_error is TERMINAL (D27 review, F1): its
    // job exhausted every attempt on a deterministic failure and will never mask
    // on its own, so it must NOT keep the poll alive — otherwise the panel spins
    // forever with no visible cause. Poll only while some page is both unmasked
    // AND un-errored; a stalled/errored batch stops polling, matching
    // BatchListCard's processing-only gate.
    refetchInterval: (query) => {
      const pages = query.state.data?.pages ?? [];
      return pages.some((p) => !p.masked && !p.mask_error) ? 2000 : false;
    },
  });
  const [pendingOnly, setPendingOnly] = useState(false);
  const [index, setIndex] = useState(0);
  const [confirmAcceptAll, setConfirmAcceptAll] = useState(false);

  // Accept/flag one page. The list above polls every 2s while pages are unmasked, so a
  // plain onSuccess setQueryData patch races the poll: a GET dispatched before the POST
  // but resolving after it clobbers the patch back to "pending" — and when that stale
  // response also stops the polling condition, the wrong status STICKS
  // (refetchOnWindowFocus is globally off, so nothing ever re-syncs). Standard
  // optimistic recipe instead: cancel in-flight list fetches, snapshot, patch the cache
  // in place (keeps the cursor stable), roll back on error, and invalidate on settle so
  // the next confirmed fetch re-syncs the cache with the server.
  const review = useMutation({
    mutationFn: ({ pageId, status }: { pageId: number; status: "accepted" | "flagged" }) =>
      api.post(`/api/answer-pages/${pageId}/mask-review`, { status }),
    onMutate: async ({ pageId, status }) => {
      await queryClient.cancelQueries({ queryKey: ["mask-review", assessmentId] });
      const previous = queryClient.getQueryData<{ pages: MaskReviewPage[] }>([
        "mask-review",
        assessmentId,
      ]);
      queryClient.setQueryData<{ pages: MaskReviewPage[] }>(
        ["mask-review", assessmentId],
        (old) =>
          old && {
            pages: old.pages.map((p) =>
              p.page_id === pageId ? { ...p, review_status: status } : p,
            ),
          },
      );
      return { previous };
    },
    onSuccess: () => {
      if (!pendingOnly) setIndex((i) => i + 1); // clamped below; pending-only advances by removal
    },
    onError: (_err, _vars, context) => {
      if (context?.previous !== undefined) {
        queryClient.setQueryData(["mask-review", assessmentId], context.previous);
      }
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["mask-review", assessmentId] }),
  });

  // Same poll-race hygiene as `review` (no in-place patch here, but a poll response
  // dispatched before the bulk POST must not land after it): cancel in-flight list
  // fetches up front, then re-sync on settle either way.
  const acceptAll = useMutation({
    mutationFn: () =>
      api.post<{ accepted: number }>(`/api/assessments/${assessmentId}/masks/accept-pending`),
    onMutate: async () => {
      await queryClient.cancelQueries({ queryKey: ["mask-review", assessmentId] });
    },
    onSuccess: () => {
      setConfirmAcceptAll(false);
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["mask-review", assessmentId] }),
  });

  if (list.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (list.isError) {
    return (
      <Card title="Mask review">
        <p className="text-sm text-red-600">{list.error.message}</p>
      </Card>
    );
  }

  const pages = list.data.pages;
  const reviewed = pages.filter((p) => p.review_status !== "pending").length;
  const pendingMasked = pages.filter((p) => p.masked && p.review_status === "pending").length;
  const visible = pendingOnly ? pages.filter((p) => p.review_status === "pending") : pages;
  const idx = clamp(index, 0, Math.max(visible.length - 1, 0));
  const current: MaskReviewPage | undefined = visible[idx];

  const go = (delta: number) => setIndex(clamp(idx + delta, 0, Math.max(visible.length - 1, 0)));

  return (
    <Card
      title="Mask review"
      actions={
        <span className="text-xs text-neutral-500 tabular-nums">
          {reviewed}/{pages.length} reviewed
        </span>
      }
    >
      {pages.length === 0 ? (
        <p className="text-sm text-neutral-400">No pages yet — upload submissions first.</p>
      ) : (
        <div
          tabIndex={0}
          onKeyDown={(e) => {
            if (!current) return;
            switch (e.key) {
              case "j":
              case "ArrowDown":
              case "ArrowRight":
                e.preventDefault();
                go(1);
                break;
              case "k":
              case "ArrowUp":
              case "ArrowLeft":
                e.preventDefault();
                go(-1);
                break;
              case "a":
                e.preventDefault();
                review.mutate({ pageId: current.page_id, status: "accepted" });
                break;
              case "f":
                e.preventDefault();
                review.mutate({ pageId: current.page_id, status: "flagged" });
                break;
            }
          }}
          className="space-y-3 rounded-md focus:outline-2 focus:outline-offset-2 focus:outline-indigo-500"
        >
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="flex items-center gap-2">
              <Button
                variant="secondary"
                className="px-2.5 py-1 text-xs"
                title="Previous page (k or ↑)"
                aria-label="Previous page"
                onClick={() => go(-1)}
              >
                ←
              </Button>
              <Button
                variant="secondary"
                className="px-2.5 py-1 text-xs"
                title="Next page (j or ↓)"
                aria-label="Next page"
                onClick={() => go(1)}
              >
                →
              </Button>
              <span className="text-xs text-neutral-500 tabular-nums">
                {visible.length === 0 ? "0 of 0" : `${idx + 1} of ${visible.length}`}
              </span>
            </div>
            <div className="flex items-center gap-3">
              <label className="flex items-center gap-1.5 text-xs text-neutral-600">
                <input
                  type="checkbox"
                  checked={pendingOnly}
                  onChange={(e) => {
                    setPendingOnly(e.target.checked);
                    setIndex(0);
                  }}
                  className="size-3.5 accent-indigo-600"
                />
                pending only
              </label>
              {pendingMasked > 0 && (
                <Button
                  variant="secondary"
                  className="px-2.5 py-1 text-xs"
                  disabled={acceptAll.isPending}
                  onClick={() => setConfirmAcceptAll(true)}
                >
                  Accept all {pendingMasked} pending
                </Button>
              )}
              <Button
                className="px-2.5 py-1 text-xs"
                disabled={!current || review.isPending}
                onClick={() =>
                  current && review.mutate({ pageId: current.page_id, status: "accepted" })
                }
              >
                Accept (a)
              </Button>
              <Button
                variant="danger"
                className="px-2.5 py-1 text-xs"
                disabled={!current || review.isPending}
                onClick={() =>
                  current && review.mutate({ pageId: current.page_id, status: "flagged" })
                }
              >
                Flag (f)
              </Button>
            </div>
          </div>

          {review.isError && <p className="text-xs text-red-600">{review.error.message}</p>}

          {current === undefined ? (
            <p className="py-6 text-center text-sm text-green-700">All pending pages reviewed.</p>
          ) : (
            <>
              <div className="flex items-center gap-2 text-xs text-neutral-600">
                <span className="tabular-nums">student {current.student_id}</span>·
                <span>problem {current.problem_number}</span>·
                <span>page {current.page_index + 1}</span>
                <Badge tone={REVIEW_TONES[current.review_status] ?? "neutral"}>
                  {current.review_status}
                </Badge>
                {current.mask_error ? (
                  <Badge tone="red">masking failed</Badge>
                ) : (
                  !current.masked && <Badge tone="amber">not masked yet</Badge>
                )}
              </div>
              {current.mask_error && (
                <p className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-800 ring-1 ring-red-200 ring-inset">
                  {current.mask_error}. This page will not mask on its own — fix the
                  source image, then click <span className="font-medium">Apply masks</span>{" "}
                  above to retry it.
                </p>
              )}
              <SafeImage
                key={current.page_id}
                src={`/api/answer-pages/${current.page_id}/image${current.masked ? "?variant=masked" : ""}`}
                alt={`Page ${current.page_index + 1}`}
                className="mx-auto h-[70vh] w-full rounded-md border border-neutral-200 object-contain"
              />
              <p className="text-center text-[11px] text-neutral-400">
                Click this panel, then use j/k (or arrows) to navigate · a accept · f flag
              </p>
            </>
          )}
        </div>
      )}
      {confirmAcceptAll && (
        <Dialog open onClose={() => setConfirmAcceptAll(false)} title="Accept all pending masks">
          <div className="space-y-3">
            <p className="text-sm text-neutral-700">
              Accept all {pendingMasked} pending masked page{pendingMasked === 1 ? "" : "s"}?
              Spot-check a few first — accepted pages feed AI grading.
            </p>
            {acceptAll.isError && (
              <p className="text-xs text-red-600">{acceptAll.error.message}</p>
            )}
            <div className="flex justify-end gap-2">
              <Button variant="secondary" onClick={() => setConfirmAcceptAll(false)}>
                Cancel
              </Button>
              <Button onClick={() => acceptAll.mutate()} disabled={acceptAll.isPending}>
                {acceptAll.isPending ? "Accepting…" : `Accept all ${pendingMasked}`}
              </Button>
            </div>
          </div>
        </Dialog>
      )}
    </Card>
  );
}
