// Batch list: one row per scan batch with per-state page counts and a processing
// progress hint. Polls while any batch still has pages in flight; on the
// processing -> settled transition, invalidates scan-pages and scan-matrix so the
// (still-to-come, Task 12) page review UI and matrix pick up the freshly
// identified/promoted pages without a manual refresh.
//
// A batch with errored pages grows a bulk-recovery row (2026-07-11): "Retry
// errored (N)…" — optionally repointing the batch to a different enabled
// provider/model, since the stored provider is what terminal-errored the pages
// in the first place — and "Discard errored (N)" behind a confirm step.

import { Fragment, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../lib/api";
import type { Provider, ScanBatchListRow } from "../../lib/types";
import { Badge, Button, Card, Select, Spinner, Table, TD, TH } from "../ui";

export function BatchListCard({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();
  const wasProcessingRef = useRef(false);

  const list = useQuery({
    queryKey: ["scan-batches", assessmentId],
    queryFn: () =>
      api.get<{ batches: ScanBatchListRow[] }>(`/api/assessments/${assessmentId}/scan-batches`),
    refetchInterval: (query) => {
      const batches = query.state.data?.batches ?? [];
      const stillProcessing = batches.some((b) => b.processing_pages > 0);
      if (wasProcessingRef.current && !stillProcessing) {
        void queryClient.invalidateQueries({ queryKey: ["scan-pages", assessmentId] });
        void queryClient.invalidateQueries({ queryKey: ["scan-matrix", assessmentId] });
      }
      wasProcessingRef.current = stillProcessing;
      return stillProcessing ? 2000 : false;
    },
  });

  const providersQuery = useQuery({
    queryKey: ["providers"],
    queryFn: () => api.get<{ providers: Provider[] }>("/api/providers"),
  });
  const enabledProviders = (providersQuery.data?.providers ?? []).filter((p) => p.enabled);

  const invalidateAll = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["scan-batches", assessmentId] }),
      queryClient.invalidateQueries({ queryKey: ["scan-pages", assessmentId] }),
      queryClient.invalidateQueries({ queryKey: ["scan-matrix", assessmentId] }),
    ]);
  };

  const retryErrored = useMutation({
    mutationFn: ({
      batchId,
      provider,
      model,
    }: {
      batchId: number;
      provider: string;
      model: string;
    }) =>
      api.post<{ retried: number }>(
        `/api/scan-batches/${batchId}/retry-errored`,
        provider !== ""
          ? { ocr_provider: provider, ...(model !== "" ? { ocr_model: model } : {}) }
          : {},
      ),
    onSuccess: invalidateAll,
  });

  const discardErrored = useMutation({
    mutationFn: ({ batchId }: { batchId: number }) =>
      api.post<{ discarded: number }>(`/api/scan-batches/${batchId}/discard-errored`, {}),
    onSuccess: invalidateAll,
  });

  if (list.isPending) {
    return (
      <Card title="Scan batches">
        <div className="flex justify-center py-6">
          <Spinner className="size-5" />
        </div>
      </Card>
    );
  }
  if (list.isError) {
    return (
      <Card title="Scan batches">
        <p className="text-sm text-red-600">{list.error.message}</p>
      </Card>
    );
  }

  const batches = list.data.batches;

  return (
    <Card title="Scan batches">
      {batches.length === 0 ? (
        <p className="text-sm text-neutral-400">No scan batches uploaded yet.</p>
      ) : (
        <Table>
          <thead>
            <tr>
              <TH className="w-16">Batch</TH>
              <TH>OCR</TH>
              <TH className="text-right">Total</TH>
              <TH className="text-right">Processing</TH>
              <TH className="text-right">Orphan</TH>
              <TH className="text-right">Assigned</TH>
              <TH className="text-right">Parked</TH>
              <TH className="text-right">Discarded</TH>
              <TH className="text-right">Errored</TH>
            </tr>
          </thead>
          <tbody>
            {batches.map((b) => (
              <Fragment key={b.id}>
                <tr>
                  <TD className="font-mono text-xs">#{b.id}</TD>
                  <TD>
                    {b.ocr_enabled ? (
                      <Badge tone="indigo">{b.ocr_provider || "cloud"}</Badge>
                    ) : (
                      <Badge tone="neutral">off</Badge>
                    )}
                  </TD>
                  <TD className="text-right tabular-nums">{b.total_pages}</TD>
                  <TD className="text-right tabular-nums">
                    {b.processing_pages > 0 ? (
                      <span className="inline-flex items-center gap-1.5 text-amber-700">
                        <Spinner className="size-3" />
                        {b.processing_pages}
                      </span>
                    ) : (
                      0
                    )}
                  </TD>
                  <TD className="text-right tabular-nums">{b.orphan_pages}</TD>
                  <TD className="text-right tabular-nums">{b.assigned_pages}</TD>
                  <TD className="text-right tabular-nums">{b.parked_pages}</TD>
                  <TD className="text-right tabular-nums">{b.discarded_pages}</TD>
                  <TD className="text-right tabular-nums">
                    {b.errored_pages > 0 ? (
                      <span className="text-red-600">{b.errored_pages}</span>
                    ) : (
                      0
                    )}
                  </TD>
                </tr>
                {b.errored_pages > 0 && (
                  <tr>
                    <TD colSpan={9} className="bg-red-50/50">
                      <ErroredActions
                        batch={b}
                        providers={enabledProviders}
                        onRetry={(provider, model) =>
                          retryErrored.mutate({ batchId: b.id, provider, model })
                        }
                        onDiscard={() => discardErrored.mutate({ batchId: b.id })}
                        retryPending={retryErrored.isPending}
                        discardPending={discardErrored.isPending}
                        error={
                          retryErrored.isError
                            ? retryErrored.error
                            : discardErrored.isError
                              ? discardErrored.error
                              : undefined
                        }
                      />
                    </TD>
                  </tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </Table>
      )}
    </Card>
  );
}

/** Bulk recovery for one batch's errored pages: retry (optionally against a
 * different enabled provider — the batch's stored one is likely the culprit)
 * or discard, the latter behind an explicit confirm click. */
function ErroredActions({
  batch,
  providers,
  onRetry,
  onDiscard,
  retryPending,
  discardPending,
  error,
}: {
  batch: ScanBatchListRow;
  providers: Provider[];
  onRetry: (provider: string, model: string) => void;
  onDiscard: () => void;
  retryPending: boolean;
  discardPending: boolean;
  error?: Error;
}) {
  const [retryOpen, setRetryOpen] = useState(false);
  // "" = keep the batch's current provider/model.
  const [provider, setProvider] = useState("");
  const [model, setModel] = useState("");
  const [confirmDiscard, setConfirmDiscard] = useState(false);

  const providerModels = providers.find((p) => p.name === provider)?.models ?? [];
  const pending = retryPending || discardPending;
  const n = batch.errored_pages;

  return (
    <div className="space-y-2 py-1">
      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="secondary"
          className="px-2.5 py-1 text-xs"
          disabled={pending}
          onClick={() => {
            setRetryOpen((v) => !v);
            setConfirmDiscard(false);
          }}
        >
          Retry errored ({n})…
        </Button>
        {confirmDiscard ? (
          <>
            <span className="text-xs text-red-700">
              Discard {n} errored page{n === 1 ? "" : "s"}?
            </span>
            <Button
              variant="danger"
              className="px-2.5 py-1 text-xs"
              disabled={pending}
              onClick={() => {
                setConfirmDiscard(false);
                onDiscard();
              }}
            >
              {discardPending ? "Discarding…" : "Confirm discard"}
            </Button>
            <Button
              variant="secondary"
              className="px-2.5 py-1 text-xs"
              disabled={pending}
              onClick={() => setConfirmDiscard(false)}
            >
              Cancel
            </Button>
          </>
        ) : (
          <Button
            variant="secondary"
            className="px-2.5 py-1 text-xs"
            disabled={pending}
            onClick={() => {
              setConfirmDiscard(true);
              setRetryOpen(false);
            }}
          >
            Discard errored ({n})
          </Button>
        )}
      </div>

      {retryOpen && (
        <form
          className="flex flex-wrap items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            setRetryOpen(false);
            onRetry(provider, provider !== "" ? model : "");
          }}
        >
          <Select
            aria-label="Retry with provider"
            className="w-44"
            value={provider}
            onChange={(e) => {
              setProvider(e.target.value);
              setModel("");
            }}
          >
            <option value="">
              keep current{batch.ocr_provider ? ` (${batch.ocr_provider})` : ""}
            </option>
            {providers.map((p) => (
              <option key={p.id} value={p.name}>
                {p.name}
              </option>
            ))}
          </Select>
          <Select
            aria-label="Retry with model"
            className="w-44"
            value={model}
            onChange={(e) => setModel(e.target.value)}
            disabled={provider === ""}
          >
            <option value="">(default model)</option>
            {providerModels.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </Select>
          <Button type="submit" className="px-2.5 py-1 text-xs" disabled={pending}>
            {retryPending ? "Retrying…" : "Retry"}
          </Button>
        </form>
      )}

      {error && <p className="text-xs text-red-600">{error.message}</p>}
    </div>
  );
}
