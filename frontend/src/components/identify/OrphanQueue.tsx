// Orphan queue: the keyboard-driven successor to the deleted ReviewStrip
// (git show 54e036d:frontend/src/components/identify/ReviewStrip.tsx), scoped to
// pages the identify pass couldn't place (orphan) or that errored (error).
// Mechanics adapted verbatim from ReviewStrip: isFormTarget guard, id-anchored
// cursor with lastIdxRef (a poll can reorder/drop the visible list under the
// cursor), the focusable keyboard wrapper div, and the client-side roster
// search card.
//
// OCR text (ocr_student_id/ocr_name/ocr_problem) and proposed/assigned names
// are PII — render only, NEVER console.log.

import { useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../../lib/api";
import type { AssessmentDetailResponse, ScanPage, Student } from "../../lib/types";
import { Badge, Button, Card, Dialog, Field, Input, Select, Spinner, cx } from "../ui";
import { SafeImage } from "../SafeImage";

/** Keyboard shortcuts only fire when focus isn't inside a form control. */
function isFormTarget(target: EventTarget | null): boolean {
  const t = target as HTMLElement | null;
  return (
    t?.tagName === "INPUT" ||
    t?.tagName === "TEXTAREA" ||
    t?.tagName === "SELECT" ||
    !!t?.isContentEditable
  );
}

const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));

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

interface ConflictInfo {
  incumbentPageId?: number;
  incumbentSubmissionId?: number;
  duplicate?: boolean;
}

export function OrphanQueue({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();

  const pages = useQuery({
    queryKey: ["scan-pages", assessmentId],
    queryFn: () => api.get<{ pages: ScanPage[] }>(`/api/assessments/${assessmentId}/scan-pages`),
    refetchInterval: (q) => {
      const ps = q.state.data?.pages ?? [];
      return ps.some((p) => p.state === "processing" || p.state === "assigned") ? 2000 : false;
    },
  });

  const assessment = useQuery({
    queryKey: ["assessment", assessmentId],
    queryFn: () => api.get<AssessmentDetailResponse>(`/api/assessments/${assessmentId}`),
  });

  const [currentPageId, setCurrentPageId] = useState<number | null>(null);
  // Last resolved index — used only to pick the nearest neighbor when the
  // anchored page drops out of `visible` (poll reordered/settled it).
  const lastIdxRef = useRef(0);
  const [showFullPage, setShowFullPage] = useState(false);
  const [studentId, setStudentId] = useState("");
  const [rosterQuery, setRosterQuery] = useState("");
  const [problemId, setProblemId] = useState<number | null>(null);
  const [discardTarget, setDiscardTarget] = useState<ScanPage | null>(null);
  const [conflict, setConflict] = useState<ConflictInfo | null>(null);
  // Tracks what the pickers were last seeded from: page id AND the proposed
  // fields, not just the id. During the post-assign refetch window the anchor
  // advances while the list snapshot is transient; an id-only key seeds once
  // against that snapshot and never re-runs when the real proposal lands,
  // leaving the pickers empty (Enter then no-ops).
  const [seededFor, setSeededFor] = useState<string | undefined>(undefined);
  // Page id whose pickers the user manually edited — a proposal landing for
  // the SAME page must not clobber in-progress edits (a page change re-seeds
  // unconditionally).
  const [touchedPageId, setTouchedPageId] = useState<number | null>(null);
  // Enter/Assign with nothing selected must never be silent: highlights the
  // missing picker(s) and shows an inline hint until they're filled.
  const [missingHint, setMissingHint] = useState(false);
  // Shortcuts only fire while the queue wrapper (or a child) has focus —
  // surface that instead of failing silently.
  const [queueFocused, setQueueFocused] = useState(false);

  const roster = useQuery({
    queryKey: ["students"],
    queryFn: () => api.get<{ students: Student[] }>("/api/students"),
  });

  const assign = useMutation({
    mutationFn: ({ pageId, sid, pid }: { pageId: number; sid: string; pid: number }) =>
      api.post<{ assigned: true }>(`/api/scan-pages/${pageId}/assign`, {
        student_id: sid,
        problem_id: pid,
      }),
    onSuccess: async () => {
      setConflict(null);
      await invalidateAll(queryClient, assessmentId);
      // Picker reset is the seed block's job: clearing here raced the refetch
      // (the anchor had already advanced and seeded the next page's proposal
      // by the time this ran) and wiped the freshly seeded pickers.
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409) {
        const details = err.details as
          | {
              incumbent_page_id?: number;
              incumbent_submission_id?: number;
              duplicate?: boolean;
            }
          | undefined;
        setConflict({
          incumbentPageId: details?.incumbent_page_id,
          incumbentSubmissionId: details?.incumbent_submission_id,
          duplicate: details?.duplicate,
        });
      }
    },
  });

  const discard = useMutation({
    mutationFn: ({ pageId, reason }: { pageId: number; reason: string }) =>
      api.post<{ discarded: true }>(`/api/scan-pages/${pageId}/discard`, { reason }),
    onSuccess: async () => {
      await invalidateAll(queryClient, assessmentId);
      setDiscardTarget(null);
    },
  });

  // Errored pages (render/OCR failure) get their next stage re-enqueued:
  // identify when crops already exist, else a render chunk for just this
  // page (RetryPage picks whichever applies). §7.1 "re-identify unresolved
  // pages after region edits" is a separate, deferred capability (design
  // spec addendum) -- this is only the per-page error-recovery retry.
  const retry = useMutation({
    mutationFn: (pageId: number) => api.post<void>(`/api/scan-pages/${pageId}/retry`),
    onSuccess: async () => invalidateAll(queryClient, assessmentId),
  });

  if (pages.isPending || assessment.isPending) {
    return (
      <Card title="Orphan queue">
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      </Card>
    );
  }
  if (pages.isError) {
    return (
      <Card title="Orphan queue">
        <p className="text-sm text-red-600">{pages.error.message}</p>
      </Card>
    );
  }
  if (assessment.isError) {
    return (
      <Card title="Orphan queue">
        <p className="text-sm text-red-600">{assessment.error.message}</p>
      </Card>
    );
  }

  const problems = assessment.data.problems;
  const visible = pages.data.pages.filter((p) => p.state === "orphan" || p.state === "error");

  const rawIdx = currentPageId === null ? -1 : visible.findIndex((p) => p.id === currentPageId);
  const idx = rawIdx === -1 ? clamp(lastIdxRef.current, 0, Math.max(visible.length - 1, 0)) : rawIdx;
  const current: ScanPage | undefined = visible[idx];
  lastIdxRef.current = idx;

  // Re-anchor during render if the cursor drifted (React's documented pattern
  // for adjusting state from derived data) — bails out once ids match.
  const nextAnchor = current ? current.id : null;
  if (nextAnchor !== currentPageId) {
    setCurrentPageId(nextAnchor);
  }

  // Seed the picker fields from the current page's proposal whenever the
  // anchored page OR its proposed fields change underneath the pickers (the
  // latter covers proposal data landing after a transient refetch snapshot).
  const seedKey = current
    ? `${current.id}|${current.proposed_student_id ?? ""}|${current.proposed_problem_id ?? 0}`
    : undefined;
  if (seedKey !== seededFor) {
    // Same page + user already editing → keep their in-progress edits.
    if (!(current && touchedPageId === current.id)) {
      setStudentId(current?.proposed_student_id ?? "");
      setProblemId(current?.proposed_problem_id ?? null);
      setRosterQuery("");
      setConflict(null);
      setMissingHint(false);
      setTouchedPageId(null);
    }
    setSeededFor(seedKey);
  }

  const go = (delta: number) => {
    const next = visible[clamp(idx + delta, 0, Math.max(visible.length - 1, 0))];
    setCurrentPageId(next ? next.id : null);
  };

  const markTouched = () => {
    if (current) setTouchedPageId(current.id);
  };

  const doAssign = () => {
    // While a mutation is in flight the Assign button is disabled and reads
    // "Assigning…" — Enter is ignored but the pending state stays visible.
    if (!current || assign.isPending) return;
    if (!studentId || !problemId) {
      setMissingHint(true); // never a silent no-op
      return;
    }
    setMissingHint(false);
    assign.mutate({ pageId: current.id, sid: studentId, pid: problemId });
  };

  const rosterResults = (roster.data?.students ?? []).filter((s) => {
    if (s.withdrawn) return false;
    if (rosterQuery.trim() === "") return true;
    const q = rosterQuery.trim().toLowerCase();
    return s.student_id.toLowerCase().includes(q) || s.name.toLowerCase().includes(q);
  });

  // Resolved identity for the lead block. The name is ALWAYS roster-derived:
  // via the roster query for a user selection, or via proposed_name (resolved
  // server-side from the roster) for a seeded proposal — never OCR of the
  // name box.
  const selectedName =
    (roster.data?.students ?? []).find((s) => s.student_id === studentId)?.name ??
    (studentId && studentId === current?.proposed_student_id ? current?.proposed_name : undefined);

  const identityNote = (): string => {
    if (!current || studentId !== current.proposed_student_id) return "selected from roster";
    switch (current.proposal_source) {
      case "ocr_id":
        return "name from roster — ID matched, name box unreadable";
      case "ocr_id_near":
        // Covers every fuzzy ID rung (embedded verbatim read, one character
        // off, two characters off with a uniqueness gap) — the copy must not
        // overpromise a specific error shape.
        return "closest roster match — the OCR'd ID doesn't match any roster ID exactly; compare the ID digits on the page before assigning";
      case "ocr_name":
        return "ID from roster — name matched, ID box unreadable";
      case "ocr_agree":
        return "ID and name from roster — both boxes matched";
      default:
        return "proposed from OCR — confirm before assigning";
    }
  };

  // Near-miss proposals put the raw OCR'd ID right in the lead block so the
  // TA can compare it against the proposed roster ID at a glance (the raw
  // reads section repeats it further down, but the discrepancy is the whole
  // point of this provenance — it must be visible where the decision is made).
  const nearMissRawId =
    current?.proposal_source === "ocr_id_near" &&
    studentId === current.proposed_student_id &&
    current.ocr_student_id
      ? current.ocr_student_id
      : undefined;

  const imgSrc = (kind: "student_id" | "name" | "problem_id") =>
    current ? `/api/scan-pages/${current.id}/crop?kind=${kind}` : undefined;
  const fullPageSrc = current && current.has_image ? `/api/scan-pages/${current.id}/image` : undefined;

  return (
    <Card
      title="Orphan queue"
      actions={
        <span className="text-xs text-neutral-500 tabular-nums">
          {visible.length === 0 ? "0 of 0" : `${idx + 1} of ${visible.length}`}
        </span>
      }
    >
      {visible.length === 0 ? (
        <p className="py-6 text-center text-sm text-green-700">No orphan or errored pages.</p>
      ) : (
        <div
          tabIndex={0}
          onFocus={() => setQueueFocused(true)}
          onBlur={(e) => {
            // focus-within: only report "unfocused" when focus left the queue
            // entirely, not when it moved between children.
            if (!e.currentTarget.contains(e.relatedTarget as Node | null)) {
              setQueueFocused(false);
            }
          }}
          onKeyDown={(e) => {
            if (isFormTarget(e.target)) return;
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
              case "Enter":
                e.preventDefault();
                doAssign();
                break;
              case "d":
                e.preventDefault();
                setDiscardTarget(current);
                break;
              case "v":
                e.preventDefault();
                setShowFullPage((v) => !v);
                break;
            }
          }}
          className="space-y-3 rounded-md focus:outline-2 focus:outline-offset-2 focus:outline-indigo-500"
        >
          {current === undefined ? (
            <p className="py-6 text-center text-sm text-green-700">No page selected.</p>
          ) : (
            <>
              <div className="flex flex-wrap items-center gap-1.5 text-xs">
                <span className="font-mono text-neutral-600">page #{current.id}</span>
                <Badge tone={current.state === "error" ? "red" : "amber"}>{current.state}</Badge>
                {current.error && <Badge tone="red">{current.error}</Badge>}
                {current.ocr_engine && <Badge tone="neutral">{current.ocr_engine}</Badge>}
              </div>

              {/* Identity lead: resolved ID + roster name when a proposal (or a
                  roster selection) exists; unknown otherwise, deferring to the
                  roster search. Never auto-assigned — Enter/Assign confirms. */}
              {studentId ? (
                <div className="rounded-md bg-indigo-50 px-3 py-2 ring-1 ring-indigo-200 ring-inset">
                  <p className="text-base font-semibold text-indigo-900">
                    <span className="font-mono">{studentId}</span>
                    {selectedName && <span> — {selectedName}</span>}
                  </p>
                  <p className="mt-0.5 text-[11px] text-indigo-700">{identityNote()}</p>
                  {nearMissRawId && (
                    <p className="mt-0.5 text-[11px] text-indigo-900">
                      page reads <span className="font-mono font-semibold">{nearMissRawId}</span>
                      {" · "}proposed <span className="font-mono font-semibold">{studentId}</span>
                    </p>
                  )}
                </div>
              ) : (
                <div className="rounded-md bg-neutral-50 px-3 py-2 ring-1 ring-neutral-200 ring-inset">
                  <p className="text-sm font-medium text-neutral-500">Student unknown</p>
                  <p className="mt-0.5 text-[11px] text-neutral-500">
                    ID unreadable or not on roster — search the roster to select the student.
                  </p>
                </div>
              )}

              {current.proposal_source === "ocr_disagree" && (
                <div className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
                  ID and name disagree — check both boxes.
                </div>
              )}

              <div className="grid gap-3 sm:grid-cols-2">
                <div className="space-y-2">
                  {showFullPage ? (
                    fullPageSrc ? (
                      <SafeImage
                        key={fullPageSrc}
                        src={fullPageSrc}
                        alt="Full page"
                        className="mx-auto h-[50vh] w-full rounded-md border border-neutral-200 object-contain"
                      />
                    ) : (
                      <p className="rounded-md bg-neutral-50 px-3 py-6 text-center text-sm text-neutral-400">
                        No page image yet.
                      </p>
                    )
                  ) : (
                    <div className="flex flex-wrap gap-3">
                      {(["student_id", "name", "problem_id"] as const).map((kind) => (
                        <div key={kind} className="space-y-1">
                          <p className="text-[11px] font-medium text-neutral-500">
                            {kind === "student_id"
                              ? "Student ID"
                              : kind === "name"
                                ? "Name"
                                : "Problem"}
                          </p>
                          <SafeImage
                            src={imgSrc(kind) as string}
                            alt={kind}
                            className="h-16 object-contain"
                          />
                        </div>
                      ))}
                    </div>
                  )}
                  <div className="space-y-0.5 text-xs text-neutral-500">
                    <p className="text-[11px] font-medium text-neutral-400">Raw OCR reads</p>
                    <p>
                      OCR student id: <span className="font-mono">{current.ocr_student_id || "—"}</span>
                    </p>
                    <p>
                      OCR name: <span>{current.ocr_name || "—"}</span>
                    </p>
                    <p>
                      OCR problem: <span>{current.ocr_problem || "—"}</span>
                    </p>
                  </div>
                </div>

                <div className="space-y-3">
                  {conflict && (
                    <div className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
                      {conflict.duplicate ? (
                        <p>This looks like a duplicate of an already-assigned page. Discard this page.</p>
                      ) : conflict.incumbentPageId ? (
                        <>
                          <p>Cell already has page #{conflict.incumbentPageId}.</p>
                          <p className="mt-1">
                            Unassign it from the matrix first, or discard this page.
                          </p>
                        </>
                      ) : conflict.incumbentSubmissionId ? (
                        <>
                          <p>Cell is covered by a direct submission.</p>
                          <p className="mt-1">
                            On the{" "}
                            <a href="?tab=submissions" className="font-medium underline">
                              Submissions tab
                            </a>
                            , retract that student&apos;s submission (its reconciliation row →
                            Retract) to free the cell, then re-run — or discard this page.
                          </p>
                        </>
                      ) : (
                        <p>Cell is already occupied.</p>
                      )}
                    </div>
                  )}
                  {assign.isError && !conflict && (
                    <p className="text-xs text-red-600">{assign.error.message}</p>
                  )}

                  {/* Identity is selection-only: typing filters the roster, the
                      click selects. No free-text identity entry. */}
                  <div className="space-y-1.5">
                    <p className="text-xs font-medium text-neutral-600">
                      Student — search the roster, click to select
                    </p>
                    <Input
                      placeholder="search by id or name…"
                      value={rosterQuery}
                      onChange={(e) => {
                        setRosterQuery(e.target.value);
                        markTouched();
                      }}
                      className={missingHint && !studentId ? "ring-2 ring-amber-400" : undefined}
                    />
                    {roster.isPending ? (
                      <Spinner className="size-4" />
                    ) : (
                      <div className="max-h-32 space-y-0.5 overflow-y-auto">
                        {rosterResults.slice(0, 20).map((s) => (
                          <button
                            key={s.id}
                            type="button"
                            onClick={() => {
                              setStudentId(s.student_id);
                              markTouched();
                            }}
                            className={cx(
                              "flex w-full items-center justify-between gap-2 rounded px-2 py-1 text-left text-sm",
                              s.student_id === studentId
                                ? "bg-indigo-50 text-indigo-900"
                                : "hover:bg-neutral-50",
                            )}
                          >
                            <span className="font-mono tabular-nums">{s.student_id}</span>
                            <span
                              className={
                                s.student_id === studentId ? "text-indigo-700" : "text-neutral-500"
                              }
                            >
                              {s.name}
                            </span>
                          </button>
                        ))}
                        {rosterResults.length === 0 && (
                          <p className="px-2 py-1 text-sm text-neutral-400">No matches.</p>
                        )}
                      </div>
                    )}
                  </div>

                  <Field label="Problem">
                    <Select
                      value={problemId ?? ""}
                      onChange={(e) => {
                        setProblemId(e.target.value === "" ? null : Number(e.target.value));
                        markTouched();
                      }}
                      className={
                        missingHint && !problemId ? "rounded-md ring-2 ring-amber-400" : undefined
                      }
                    >
                      <option value="">Select a problem…</option>
                      {problems.map((p) => (
                        <option key={p.id} value={p.id}>
                          Problem {p.number}
                        </option>
                      ))}
                    </Select>
                  </Field>

                  {missingHint && (!studentId || !problemId) && (
                    <p className="text-xs font-medium text-amber-700">
                      {!studentId && !problemId
                        ? "Select a student and a problem before assigning."
                        : !studentId
                          ? "Select a student from the roster before assigning."
                          : "Select a problem before assigning."}
                    </p>
                  )}

                  {retry.isError && (
                    <p className="text-xs text-red-600">{retry.error.message}</p>
                  )}

                  <div className="flex justify-between gap-2">
                    <span className="inline-flex gap-2">
                      <Button
                        variant="danger"
                        className="px-2.5 py-1 text-xs"
                        onClick={() => setDiscardTarget(current)}
                      >
                        Discard (d)
                      </Button>
                      {current.state === "error" && (
                        <Button
                          variant="secondary"
                          className="px-2.5 py-1 text-xs"
                          disabled={retry.isPending}
                          onClick={() => retry.mutate(current.id)}
                        >
                          {retry.isPending ? "Retrying…" : "Retry"}
                        </Button>
                      )}
                    </span>
                    <Button
                      className="px-2.5 py-1 text-xs"
                      disabled={!studentId || !problemId || assign.isPending}
                      onClick={doAssign}
                    >
                      {assign.isPending ? "Assigning…" : "Assign (Enter)"}
                    </Button>
                  </div>
                </div>
              </div>
            </>
          )}
          {queueFocused ? (
            <p className="text-center text-[11px] text-neutral-400">
              j/k navigate · Enter assign · d discard · v toggle page
            </p>
          ) : (
            <p className="text-center text-[11px] font-medium text-amber-700">
              Click the queue to enable keyboard shortcuts — j/k · Enter · d · v
            </p>
          )}
        </div>
      )}

      {discardTarget && (
        <DiscardDialog
          page={discardTarget}
          onClose={() => setDiscardTarget(null)}
          onConfirm={(reason) => discard.mutate({ pageId: discardTarget.id, reason })}
          pending={discard.isPending}
        />
      )}
    </Card>
  );
}

function DiscardDialog({
  page,
  onClose,
  onConfirm,
  pending,
}: {
  page: ScanPage;
  onClose: () => void;
  onConfirm: (reason: string) => void;
  pending: boolean;
}) {
  const [reason, setReason] = useState("");
  return (
    <Dialog open onClose={onClose} title={`Discard page #${page.id}`}>
      <form
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          onConfirm(reason.trim());
        }}
      >
        <Field label="Reason">
          <Input autoFocus value={reason} onChange={(e) => setReason(e.target.value)} />
        </Field>
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" disabled={pending}>
            {pending ? "Discarding…" : "Discard"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
