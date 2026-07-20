// ID-region card: three fixed-kind rects (student_id, name, problem_id) drawn over a
// template page, one PUT replaces the whole set. Finalize seeds masks from the
// student_id/name kinds now (D66) — this card no longer offers a "copy to mask
// regions" button.
//
// Template image source: prefer a LOCAL-ONLY file the user picks from their own
// device (never uploaded anywhere — an object: URL stays entirely in the browser),
// falling back to any existing answer page (useSamplePage) once one exists. The
// local picker exists because batch upload requires all three regions to already be
// drawn, so the old "sample scan page" bootstrap (which came from a scan batch)
// can't be the only source — a template picked before any batch exists breaks that
// circular dependency. Because the picked file may show a real, filled-in student
// script, we must never fetch/upload it anywhere; only URL.createObjectURL it.

import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../lib/api";
import type { IDRegion, IDRegionKind } from "../../lib/types";
import { useSessionDraft } from "../../lib/useSessionDraft";
import { Button, Card, Spinner, cx } from "../ui";
import { RectEditor } from "../RectEditor";
import { useSamplePage } from "./useSamplePage";

const KIND_COLORS: Record<IDRegionKind, string> = {
  student_id: "#2563eb",
  name: "#16a34a",
  problem_id: "#ea580c",
};

const KIND_LABELS: Record<IDRegionKind, string> = {
  student_id: "Student ID",
  name: "Name",
  problem_id: "Problem",
};

const KIND_ORDER: IDRegionKind[] = ["student_id", "name", "problem_id"];

/** Region fill: the region's hex color at ~40% alpha (fallback for odd input). */
function fillColor(color: string): string {
  return /^#[0-9a-fA-F]{6}$/.test(color) ? `${color}66` : "rgba(74,74,74,0.4)";
}

export function IDRegionCard({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();
  const regionsQuery = useQuery({
    queryKey: ["id-regions", assessmentId],
    queryFn: () => api.get<{ regions: IDRegion[] }>(`/api/assessments/${assessmentId}/id-regions`),
  });
  const sample = useSamplePage(assessmentId);

  // LOCAL-ONLY template image: never uploaded or fetched anywhere. The object URL
  // lives only in this tab and is revoked on replacement/unmount so the browser
  // doesn't keep a PII-bearing blob alive longer than necessary.
  const [localImage, setLocalImage] = useState<{ file: File; url: string } | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // Single revocation mechanism: this cleanup runs both when localImage changes
  // (React tears down the previous effect instance before the next one, revoking the
  // outgoing URL on replacement) and when the component unmounts (revoking whatever
  // URL is current). No other code path revokes — the file itself never leaves the
  // browser, only its object URL's lifetime is being managed here.
  useEffect(() => {
    return () => {
      if (localImage) URL.revokeObjectURL(localImage.url);
    };
  }, [localImage]);

  const onPickLocalImage = (f: File | null) => {
    setLocalImage(f ? { file: f, url: URL.createObjectURL(f) } : null);
  };

  const templateUrl = localImage?.url ?? sample.pageUrl;

  // Local draft, backed by sessionStorage (HCI finding C): the Identify tab is
  // rendered conditionally by AssessmentDetail, so switching tabs unmounts this card
  // and would silently destroy the three in-progress ID rectangles. Persisting the
  // draft restores it on the next mount and clears it on a successful Save. `draft`
  // stays null until the first edit, so we fall back to server regions — which also
  // matters because this card mounts BEFORE its query resolves.
  const idDraft = useSessionDraft<IDRegion[]>(`id-regions:${assessmentId}`);
  const [activeKind, setActiveKind] = useState<IDRegionKind>("student_id");
  const [selected, setSelected] = useState<number | null>(null);

  const draft = idDraft.draft ?? regionsQuery.data?.regions ?? [];
  const setDraft = (next: IDRegion[]) => {
    // RectEditor's drag state is index-based: a create-drag captures `rects.length`
    // (the pre-filter draft length) as its target index and keeps mutating that index
    // for the rest of the drag. If we ever filter-then-append here, the freshly
    // created rect lands at a different index than the drag is tracking, so every
    // subsequent onMove is silently dropped and a phantom 0×0 rect of that kind is
    // left behind. To keep indices stable for the whole drag, a kind can only be
    // (re)drawn while it has no rect in the draft yet — see the per-row "Redraw"
    // button below, which removes the existing rect *before* drawing starts. If a
    // create-drag still arrives for a kind that already exists (e.g. a background
    // click before hitting Redraw), reject it outright: return the draft unchanged so
    // the orphaned drag is inert (its index matches nothing) and no phantom rect is
    // created.
    const created = next[next.length - 1];
    if (next.length > draft.length && created) {
      if (draft.some((r) => r.kind === created.kind)) return;
    }
    idDraft.setDraft(next);
  };

  const redrawKind = (kind: IDRegionKind) => {
    // Remove-then-draw: clears any existing rect of this kind so the next
    // create-drag's index lines up with the (shorter) draft for its whole lifetime.
    idDraft.setDraft(draft.filter((r) => r.kind !== kind));
    setActiveKind(kind);
    setSelected(null);
  };

  const drawnKinds = new Set(draft.map((r) => r.kind));
  const allDrawn = draft.length === 3;
  const allValid = allDrawn && draft.every((r) => r.w >= 0.005 && r.h >= 0.005);

  const save = useMutation({
    mutationFn: () =>
      api.put<{ saved: number }>(`/api/assessments/${assessmentId}/id-regions`, {
        regions: draft,
      }),
    onSuccess: () => {
      // The PUT replaces the whole set, so publish that truth to the cache before
      // dropping the local fallback. A background refetch still confirms it.
      queryClient.setQueryData<{ regions: IDRegion[] }>(["id-regions", assessmentId], {
        regions: draft,
      });
      idDraft.clear();
      return queryClient.invalidateQueries({ queryKey: ["id-regions", assessmentId] });
    },
  });

  const pending = regionsQuery.isPending || (localImage === null && sample.pending);

  return (
    <Card title="ID regions">
      {pending ? (
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      ) : regionsQuery.isError ? (
        <p className="text-sm text-red-600">{regionsQuery.error.message}</p>
      ) : (
        <div className="space-y-3">
          <p className="text-xs text-neutral-500">
            Student ID and name boxes are auto-masked before AI grading.
          </p>

          <div className="flex items-center gap-2">
            <input
              ref={fileInputRef}
              type="file"
              accept="image/*"
              className="hidden"
              aria-label="Choose a local template image"
              onChange={(e) => onPickLocalImage(e.target.files?.[0] ?? null)}
            />
            <Button
              variant="secondary"
              className="px-2.5 py-1 text-xs"
              onClick={() => fileInputRef.current?.click()}
            >
              {localImage ? "Replace template image" : "Pick template image"}
            </Button>
            {localImage && (
              <>
                <span className="truncate text-xs text-neutral-500">{localImage.file.name}</span>
                <Button
                  variant="ghost"
                  className="px-2 py-1 text-xs"
                  onClick={() => {
                    onPickLocalImage(null);
                    if (fileInputRef.current) fileInputRef.current.value = "";
                  }}
                >
                  Clear
                </Button>
              </>
            )}
          </div>
          <p className="text-[11px] text-neutral-400">
            This image stays on your device — it is only shown here to help you draw the boxes
            and is never uploaded or sent anywhere.
          </p>

          {templateUrl === undefined ? (
            <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
              Pick any scanned page as a template — it stays on your device.
            </p>
          ) : (
            <>
              <div className="flex flex-wrap items-center gap-1.5">
                {KIND_ORDER.map((kind) => {
                  const drawn = drawnKinds.has(kind);
                  return (
                    <button
                      key={kind}
                      type="button"
                      onClick={() => (drawn ? redrawKind(kind) : setActiveKind(kind))}
                      className={cx(
                        "inline-flex items-center gap-1.5 rounded-md border px-2.5 py-1 text-xs font-medium transition-colors",
                        activeKind === kind
                          ? "border-neutral-900 bg-neutral-900 text-white"
                          : "border-neutral-300 bg-white text-neutral-700 hover:bg-neutral-50",
                      )}
                    >
                      <span
                        className="size-2 rounded-full"
                        style={{ backgroundColor: KIND_COLORS[kind] }}
                      />
                      {KIND_LABELS[kind]}
                      <span className={drawn ? "text-green-600" : "text-neutral-400"}>
                        {drawn ? "✓" : "—"}
                      </span>
                    </button>
                  );
                })}
              </div>
              <p className="text-xs text-neutral-500">
                Select a not-yet-drawn kind above, then drag on the page to draw that box. To
                redraw a box, click its &quot;Redraw&quot; button first.
              </p>
              <RectEditor
                rects={draft}
                onChange={setDraft}
                imageUrl={templateUrl}
                imageAlt="Template scan page"
                selectedIndex={selected}
                onSelect={setSelected}
                newRect={{ kind: activeKind, color: KIND_COLORS[activeKind], padding: 0.01 }}
                rectStyle={(r) => ({ backgroundColor: fillColor(r.color) })}
              />
              <div className="space-y-1.5">
                {KIND_ORDER.map((kind) => {
                  const idx = draft.findIndex((r) => r.kind === kind);
                  const r = idx >= 0 ? draft[idx] : undefined;
                  return (
                    <div
                      key={kind}
                      onClick={() => idx >= 0 && setSelected(idx)}
                      className={cx(
                        "flex items-center gap-2 rounded-md px-2 py-1",
                        idx >= 0 && selected === idx ? "bg-indigo-50" : "hover:bg-neutral-50",
                      )}
                    >
                      <span
                        className="size-2 rounded-full"
                        style={{ backgroundColor: KIND_COLORS[kind] }}
                      />
                      <span className="w-20 text-xs font-medium text-neutral-600">
                        {KIND_LABELS[kind]}
                      </span>
                      <span className="flex-1 truncate text-[11px] text-neutral-400 tabular-nums">
                        {r
                          ? `x ${r.x.toFixed(3)} y ${r.y.toFixed(3)} w ${r.w.toFixed(3)} h ${r.h.toFixed(3)}`
                          : "not drawn yet"}
                      </span>
                      {r && (
                        <>
                          <Button
                            variant="ghost"
                            className="px-2 py-1 text-xs"
                            onClick={(e) => {
                              e.stopPropagation();
                              redrawKind(kind);
                            }}
                          >
                            Redraw
                          </Button>
                          <Button
                            variant="ghost"
                            className="px-2 py-1 text-xs"
                            onClick={(e) => {
                              e.stopPropagation();
                              idDraft.setDraft(draft.filter((_, j) => j !== idx));
                              setSelected(null);
                            }}
                          >
                            Remove
                          </Button>
                        </>
                      )}
                    </div>
                  );
                })}
              </div>
              {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}
              <div className="flex items-center justify-end gap-3">
                {idDraft.dirty && (
                  <span className="mr-auto text-xs text-amber-600">
                    Unsaved changes — kept if you switch tabs, cleared when you Save.
                  </span>
                )}
                {!idDraft.dirty && save.isSuccess && (
                  <span className="text-xs text-green-700">Saved.</span>
                )}
                <Button onClick={() => save.mutate()} disabled={!allValid || save.isPending}>
                  {save.isPending ? "Saving…" : "Save ID regions"}
                </Button>
              </div>
            </>
          )}
        </div>
      )}
    </Card>
  );
}
