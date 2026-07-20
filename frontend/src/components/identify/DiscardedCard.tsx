// Discarded pages (workflow-guards plan 2026-07-10, Task F1): discarded scan
// pages were previously invisible — nothing listed them and nothing offered the
// existing undiscard endpoint — so a mis-click in the duplicate/conflict flow
// silently lost a student's page. This card lists them (collapsed by default,
// discards are usually intentional) with an Undiscard action that returns the
// page to its prior derived state (orphan/assigned/parked).
//
// Data comes from the same ["scan-pages"] query every other Identify card polls
// (the unfiltered list already includes discarded pages), so counts here can
// never disagree with the batch-list counters and undiscarding refreshes every
// card at once.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../lib/api";
import type { ScanPage } from "../../lib/types";
import { Button, Spinner } from "../ui";

export function DiscardedCard({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();

  const pages = useQuery({
    queryKey: ["scan-pages", assessmentId],
    queryFn: () => api.get<{ pages: ScanPage[] }>(`/api/assessments/${assessmentId}/scan-pages`),
    refetchInterval: (q) => {
      const ps = q.state.data?.pages ?? [];
      return ps.some((p) => p.state === "processing" || p.state === "assigned") ? 2000 : false;
    },
  });

  const undiscard = useMutation({
    mutationFn: (pageId: number) => api.post<undefined>(`/api/scan-pages/${pageId}/undiscard`),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["scan-pages", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["scan-matrix", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["scan-batches", assessmentId] }),
      ]);
    },
  });

  const discarded = (pages.data?.pages ?? []).filter((p) => p.state === "discarded");

  return (
    <details className="rounded-lg border border-neutral-200 bg-white shadow-sm">
      <summary className="cursor-pointer px-4 py-2.5 text-sm font-semibold text-neutral-900">
        Discarded pages ({discarded.length})
      </summary>
      <div className="space-y-2 border-t border-neutral-200 p-4">
        {pages.isPending ? (
          <div className="flex justify-center py-4">
            <Spinner className="size-5" />
          </div>
        ) : pages.isError ? (
          <p className="text-sm text-red-600">{pages.error.message}</p>
        ) : discarded.length === 0 ? (
          <p className="text-sm text-neutral-400">No discarded pages.</p>
        ) : (
          <>
            <p className="text-xs text-neutral-500">
              Discarded pages are excluded from identification and grading. Undiscard returns a
              page to where it was (orphan queue, assignment, or parked).
            </p>
            {discarded.map((page) => (
              <div
                key={page.id}
                className="flex items-center justify-between gap-2 rounded-md px-2 py-1.5 text-sm hover:bg-neutral-50"
              >
                <span className="font-mono text-xs text-neutral-600">
                  page #{page.id}
                  {page.discard_reason && (
                    <span className="text-neutral-400"> — {page.discard_reason}</span>
                  )}
                </span>
                <Button
                  variant="secondary"
                  className="px-2.5 py-1 text-xs"
                  disabled={undiscard.isPending}
                  onClick={() => undiscard.mutate(page.id)}
                >
                  Undiscard
                </Button>
              </div>
            ))}
          </>
        )}
        {undiscard.isError && <p className="text-xs text-red-600">{undiscard.error.message}</p>}
      </div>
    </details>
  );
}
