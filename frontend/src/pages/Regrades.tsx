// Regrades v2 queue (spec 2026-07-03-regrade-v2-design.md §5-§8): a kind-filtered queue
// grouped by assessment, and a detail pane whose working surface is the per-problem
// sub-item cards (complaint quote, collapsible AI compare, verdict) directly under a
// compact header; the raw email, published-score snapshot, and SPF/DKIM delivery
// details sit behind collapsed Disclosures (expanded for unparsed/addendum, where the
// raw body is the only content). Plus a TA-clicked gated Send-result action, an
// Unparsed-only Send-reminder action, and per-sub-item / per-assessment AI re-grade
// assist with D27 stop-when-idle polling.
//
// PII (CLAUDE.md): regrade subject/body/from_email/problems[].complaint_text are the
// student's own message — render as plain text only (NEVER dangerouslySetInnerHTML),
// and never console.log them. Points/money render as the decimal strings the API
// sends — no float parsing.

import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api, decodeRegradeSnapshot } from "../lib/api";
import { roleAtLeast, useMe } from "../lib/auth";
import type {
  AIRegradeAllResult,
  AIRegradeEnqueueResult,
  AnswerResponse,
  AssessmentDetailResponse,
  BudgetExceededError,
  GradingRecord,
  PublishPreview,
  PublishSnapshot,
  RegradeAIRecord,
  RegradeAIRecordCriterion,
  RegradeDetail,
  RegradeKind,
  RegradeListResponse,
  RegradeListRow,
  RegradeRemindResponse,
  RegradeResendResultResponse,
  RegradeSendResult409Body,
  RegradeSendResultResponse,
  RegradeStatus,
  RegradeSubItem,
  StudentSubmissionView,
  TAAssignmentRow,
  TAAssignmentsResponse,
  UnverdictedProblem,
} from "../lib/types";
import { fmtDate } from "../lib/format";
import { userLabel } from "../lib/userLabel";
import { HelpTip } from "../components/HelpTip";
import { aiRegradeHelp, regradeKindsHelp, regradeVerdictHelp } from "../lib/helpContent";
import { PolicyBadge } from "../components/PolicyBadge";
import {
  Badge,
  Button,
  Card,
  Dialog,
  Disclosure,
  Field,
  IconButton,
  Input,
  Pager,
  Select,
  Spinner,
  TD,
  TH,
  Table,
  Textarea,
  buttonClassName,
  cx,
  type BadgeTone,
} from "../components/ui";
import { X } from "../components/icons";

const STATUS_TONES: Record<RegradeStatus, BadgeTone> = {
  received: "amber",
  under_review: "indigo",
  resolved_upheld: "neutral",
  resolved_regraded: "green",
  rejected_bad_token: "red",
  rejected_superseded: "red",
  rejected_sender_mismatch: "red",
};

const STATUS_LABELS: Record<RegradeStatus, string> = {
  received: "Received",
  under_review: "Under review",
  resolved_upheld: "Resolved — upheld",
  resolved_regraded: "Resolved — regraded",
  rejected_bad_token: "Rejected — bad token",
  rejected_superseded: "Rejected — superseded",
  rejected_sender_mismatch: "Rejected — sender mismatch",
};

const KIND_LABELS: Record<RegradeKind, string> = {
  filed: "Filed",
  addendum: "Addendum",
  unparsed: "Unparsed",
  handed_off: "Handed off",
};

/** Strip leading/trailing blank lines (and trailing whitespace) from student text so
 * quoted complaints/bodies don't open with double-newline holes. Internal line breaks
 * and indentation are the sender's own formatting — left untouched. */
function trimBlankLines(text: string): string {
  const lines = text.split("\n");
  let start = 0;
  let end = lines.length;
  while (start < end && lines[start].trim() === "") start++;
  while (end > start && lines[end - 1].trim() === "") end--;
  return lines.slice(start, end).join("\n").replace(/[ \t]+$/, "");
}

function isRejected(status: RegradeStatus): boolean {
  return status.startsWith("rejected_");
}

function isOpen(status: RegradeStatus): boolean {
  return status === "received" || status === "under_review";
}

function isResolved(status: RegradeStatus): boolean {
  return status === "resolved_upheld" || status === "resolved_regraded";
}

/** Send-failure recovery (whole-branch review F1): a FILED request that resolved but
 * whose result email never actually reached the student — the provider send failed
 * AFTER the atomic resolve flip, so result_sent_at stayed null. This is the silent
 * dead-end the resend-result affordance exists to fix; see RegradeDetail.result_sent_at. */
function resultSendFailed(d: { kind: RegradeKind; status: RegradeStatus; result_sent_at?: string | null }): boolean {
  return d.kind === "filed" && isResolved(d.status) && !d.result_sent_at;
}

/** A row is dimmed in the queue when it's not a live, adjudicable filed request —
 * addenda and unparsed rows are informational/reminder-only (spec §4/§7); handed_off
 * rows are past adjudication (the assigned TAs took over). */
function isDimmed(row: { kind: RegradeKind; status: RegradeStatus }): boolean {
  if (row.kind === "addendum" || row.kind === "unparsed") return true;
  if (row.kind === "handed_off") return true;
  return isRejected(row.status);
}

/** Queue-table status badge for a row (regrade v2 UI review Finding 5). The backend
 * marks a reminded unparsed row by resolving it resolved_upheld (reminderResolutionNote,
 * internal/httpapi/regrade.go) — there's no separate DB status for "reminded". Rendered
 * verbatim via STATUS_LABELS that reads as "Resolved — upheld", which is misleading for
 * an unparsed row (nothing was adjudicated). The detail pane already disambiguates this
 * with its own "Reminder already sent" badge; this mirrors that at the queue-table level:
 * an open unparsed row is unadjudicated (no verdict applies), and a closed one has had its
 * reminder sent — never "resolved". */
function queueStatusBadge(row: { kind: RegradeKind; status: RegradeStatus }): {
  tone: BadgeTone;
  label: string;
} {
  if (row.kind === "unparsed") {
    return isOpen(row.status)
      ? { tone: "amber", label: "Unparsed" }
      : { tone: "neutral", label: "Reminder sent" };
  }
  return { tone: STATUS_TONES[row.status], label: STATUS_LABELS[row.status] };
}

// --- AI re-grade job polling (spec §8, per-sub-item, D27 stop-when-idle) --------------

/** One AI re-grade job enqueued THIS SESSION that we are watching for completion, keyed
 * by (request id, sub-item id) so two problems on the same request poll independently.
 * `baseline` is the created_at of the ai_record that already existed at enqueue time
 * (re-run case): completion then means a *different* record shows up, not merely "a
 * record exists". Absent for a fresh enqueue. */
interface PendingAIJob {
  requestId: number;
  subItemId: number;
  baseline?: string;
}

function jobKey(requestId: number, subItemId: number): string {
  return `${requestId}:${subItemId}`;
}

/** D27 pending predicate: an enqueued sub-item job is still in flight while the request
 * stays open and the sub-item shows neither a (new) ai_record nor a terminal ai_error. A
 * request that became ineligible (resolved elsewhere) can never receive a result — treat
 * as done so the poll goes idle instead of spinning forever. */
function aiJobStillPending(d: RegradeDetail, subItemId: number, baseline?: string): boolean {
  const sub = d.problems.find((p) => p.id === subItemId);
  if (!sub) return false;
  if (sub.ai_error) return false; // terminal — rendered as the amber "AI unavailable" note
  if (d.kind !== "filed" || !isOpen(d.status)) return false;
  if (!sub.ai_record) return true;
  return baseline !== undefined && (sub.ai_record.created_at ?? "") === baseline;
}

/**
 * Session-scoped poller for enqueued AI re-grade jobs (D27 stop-when-idle idiom): polls
 * only while at least one job enqueued in this session is unresolved, priming the
 * ["regrade", id] detail cache so an open detail pane updates in place, and stops
 * entirely once every watched job has landed (ai_record / ai_error) or its sub-item
 * became ineligible.
 */
function useAIRegradePoller() {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState<PendingAIJob[]>([]);
  // Bumped on every markPending so a re-enqueue that reproduces an earlier watch set
  // gets a FRESH query key — otherwise the old key's cached "all done" result would
  // prune the new job before its first real poll.
  const [generation, setGeneration] = useState(0);

  const markPending = (jobs: PendingAIJob[]) => {
    setPending((prev) => {
      const next = new Map(prev.map((j) => [jobKey(j.requestId, j.subItemId), j] as const));
      for (const j of jobs) next.set(jobKey(j.requestId, j.subItemId), j);
      return [...next.values()];
    });
    setGeneration((g) => g + 1);
  };

  // The key encodes the watched set, so the queryFn closure always matches its key and
  // pruning the set naturally retires the old query instance.
  const requestIds = useMemo(() => [...new Set(pending.map((j) => j.requestId))], [pending]);
  const pollKey = pending.map((j) => `${jobKey(j.requestId, j.subItemId)}:${j.baseline ?? ""}`).join(",");
  const poll = useQuery({
    queryKey: ["regrade-ai-poll", generation, pollKey],
    enabled: pending.length > 0,
    queryFn: async () => {
      const details = await Promise.all(
        requestIds.map((id) => api.get<RegradeDetail>(`/api/regrades/${id}`)),
      );
      const byId = new Map(details.map((d) => [d.id, d] as const));
      details.forEach((d) => queryClient.setQueryData(["regrade", d.id], d));
      const still: PendingAIJob[] = [];
      for (const j of pending) {
        const d = byId.get(j.requestId);
        if (d && aiJobStillPending(d, j.subItemId, j.baseline)) still.push(j);
      }
      return still;
    },
    refetchInterval: (query) => ((query.state.data?.length ?? 0) > 0 ? 3000 : false),
  });

  // Prune landed jobs out of the watched set — the set emptying is what disables the
  // query entirely (stop-when-idle), not a timer.
  const pollData = poll.data;
  useEffect(() => {
    if (pollData === undefined) return;
    const stillKeys = new Set(pollData.map((j) => jobKey(j.requestId, j.subItemId)));
    setPending((prev) => {
      const next = prev.filter((j) => stillKeys.has(jobKey(j.requestId, j.subItemId)));
      return next.length === prev.length ? prev : next;
    });
  }, [pollData]);

  const pendingKeys = useMemo(
    () => new Set(pending.map((j) => jobKey(j.requestId, j.subItemId))),
    [pending],
  );
  return { pendingKeys, markPending };
}

type MarkPending = (jobs: PendingAIJob[]) => void;

/** Queue filter (spec §7): every tab maps onto server-side query params so page slices
 * AND the pager `total` are computed over the same set the tab shows (HCI audit,
 * regrades-list correctness — the old approach fetched "actionable" UNFILTERED and
 * narrowed client-side, so with >1 page a page could render empty while live appeals
 * sat on later pages and the count lied). "actionable" = filed & unresolved
 * (kind=filed&open=1); the two named recovery sets map to undelivered_result=1 and
 * status=rejected_superseded respectively. */
type QueueFilter = "actionable" | "all" | "undelivered" | "superseded" | RegradeKind;

const FILTER_OPTIONS: Array<{ value: QueueFilter; label: string }> = [
  { value: "actionable", label: "Actionable (filed & unresolved)" },
  { value: "all", label: "All" },
  { value: "filed", label: "Filed" },
  { value: "unparsed", label: "Unparsed" },
  { value: "addendum", label: "Addenda" },
  { value: "handed_off", label: "Handed off" },
  { value: "undelivered", label: "Undelivered results" },
  { value: "superseded", label: "Held while unpublished" },
];

/** Server query params for a queue tab. Single source of truth — the list query, the
 * count hints, and the pager all reason over the same server-filtered set. */
function queueServerParams(filter: QueueFilter): Record<string, string> {
  switch (filter) {
    case "actionable":
      return { kind: "filed", open: "1" };
    case "all":
      return {};
    case "undelivered":
      return { undelivered_result: "1" };
    case "superseded":
      return { status: "rejected_superseded" };
    default:
      return { kind: filter };
  }
}

interface AssessmentGroup {
  key: string;
  assessmentId?: number;
  assessmentName: string;
  rows: RegradeListRow[];
}

function groupByAssessment(rows: RegradeListRow[]): AssessmentGroup[] {
  const groups = new Map<string, AssessmentGroup>();
  for (const row of rows) {
    const key = row.assessment_id !== undefined ? String(row.assessment_id) : "unknown";
    let group = groups.get(key);
    if (!group) {
      group = {
        key,
        assessmentId: row.assessment_id,
        assessmentName: row.assessment_name ?? "(assessment unavailable)",
        rows: [],
      };
      groups.set(key, group);
    }
    group.rows.push(row);
  }
  return [...groups.values()].sort((a, b) => a.assessmentName.localeCompare(b.assessmentName));
}

const PAGE_SIZE = 50;

export function Regrades() {
  const me = useMe();
  // TA+ gates verdicts, send-result, remind, and both AI re-grade routes alike
  // (api.go registers all behind requireRole(RoleTA)).
  const canAdjudicate = roleAtLeast(me.data?.user.role, "ta");
  const [filter, setFilter] = useState<QueueFilter>("actionable");
  const [offset, setOffset] = useState(0);
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const { pendingKeys, markPending } = useAIRegradePoller();

  // Reset paging whenever the filter changes so offset never points past a smaller set.
  const setFilterReset = (next: QueueFilter) => {
    setFilter(next);
    setOffset(0);
  };

  // Recovery counts are intentionally global. Following one must clear a narrower
  // student search too, otherwise the advertised rows can still appear missing.
  const showRecoverySet = (next: "undelivered" | "superseded") => {
    setStudentSearch("");
    setDebouncedStudent("");
    setFilterReset(next);
  };

  // Student search: raw input debounced ~300 ms before it reaches the server's
  // case-insensitive external-student-ID prefix filter (`student=` query param), so the
  // queue doesn't refetch on every keystroke. Both state updates land in one batched
  // commit — the offset reset (same convention as setFilterReset) never races the new
  // search value into a stale page.
  const [studentSearch, setStudentSearch] = useState("");
  const [debouncedStudent, setDebouncedStudent] = useState("");
  useEffect(() => {
    const t = setTimeout(() => {
      setDebouncedStudent(studentSearch.trim());
      setOffset(0);
    }, 300);
    return () => clearTimeout(t);
  }, [studentSearch]);

  const list = useQuery({
    // House invalidation convention: "regrades" first so queryKey:["regrades"]
    // invalidation still sweeps every page/filter variant; offset last.
    queryKey: ["regrades", filter, debouncedStudent, offset],
    queryFn: () => {
      const params = new URLSearchParams({
        ...queueServerParams(filter),
        limit: String(PAGE_SIZE),
        offset: String(offset),
      });
      if (debouncedStudent) params.set("student", debouncedStudent);
      return api.get<RegradeListResponse>(`/api/regrades?${params.toString()}`);
    },
  });

  // Every tab is filtered SERVER-SIDE (queueServerParams), so the page rows and the
  // pager `total` below always describe the same set — no client-side narrowing.
  const rows = list.data?.regrades ?? [];
  // Filter-wide row count from the server: drives the numbered pager, counts every row
  // the current tab's filters match, not just this page.
  const total = list.data?.total ?? 0;

  const groups = useMemo(() => groupByAssessment(rows), [rows]);

  // Unparsed-inbox visibility: the default "actionable" tab is kind=filed server-side,
  // so open unparsed rows — replies that need a reminder — are invisible on it. A
  // dedicated count-only fetch (limit=1, we only read `total`; same student search so
  // the hint matches what switching filters would show) surfaces a switch-filter nudge.
  const unparsedCount = useQuery({
    queryKey: ["regrades", "open-unparsed-count", debouncedStudent],
    queryFn: () => {
      const params = new URLSearchParams({ kind: "unparsed", open: "1", limit: "1" });
      if (debouncedStudent) params.set("student", debouncedStudent);
      return api.get<RegradeListResponse>(`/api/regrades?${params.toString()}`);
    },
    enabled: filter === "actionable",
  });
  const hiddenUnparsed = filter === "actionable" ? (unparsedCount.data?.total ?? 0) : 0;

  // Undelivered-result visibility (HCI audit, moderate): a filed request that resolved
  // but whose result email FAILED to send drops out of "Actionable" (it's no longer
  // open), leaving the student silently without their outcome — previously only the
  // detail pane's resultSendFailed banner could reveal it. Count-only fetch of the
  // server's undelivered_result=1 recovery set; a nonzero count renders a queue-level
  // banner linking to the dedicated "Undelivered results" tab.
  const undeliveredCount = useQuery({
    queryKey: ["regrades", "undelivered-count"],
    queryFn: () =>
      api.get<RegradeListResponse>("/api/regrades?undelivered_result=1&limit=1"),
    enabled: filter !== "undelivered",
  });
  const undelivered = filter === "undelivered" ? 0 : (undeliveredCount.data?.total ?? 0);

  // Superseded-rejection count (C3): replies that arrived while an assessment was
  // unpublished are held as rejected_superseded and hidden by the default "actionable"
  // filter. Surface a visible count so they aren't silently lost — one click jumps the
  // filter straight to them. Fetched independently of the current filter view;
  // count-only (limit=1), reading the server's filter-wide `total` so >50 held replies
  // don't under-report.
  const supersededList = useQuery({
    queryKey: ["regrades", "rejected_superseded"],
    queryFn: () =>
      api.get<RegradeListResponse>("/api/regrades?status=rejected_superseded&limit=1"),
  });
  const supersededCount = supersededList.data?.total ?? 0;

  return (
    <div className="mx-auto flex max-w-6xl flex-col gap-4 xl:flex-row">
      <div className="min-w-0 flex-1 space-y-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h1 className="text-lg font-semibold text-neutral-900">Regrades</h1>
          <div className="flex flex-wrap items-end gap-3">
            <Field label="Student">
              <Input
                className="w-44"
                placeholder="Student ID…"
                value={studentSearch}
                onChange={(e) => setStudentSearch(e.target.value)}
              />
            </Field>
            <Field
              label={
                <>
                  Queue <HelpTip title="Regrade queue">{regradeKindsHelp}</HelpTip>
                </>
              }
              className="w-72"
            >
              <Select value={filter} onChange={(e) => setFilterReset(e.target.value as QueueFilter)}>
                {FILTER_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>
                    {opt.label}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
        </div>

        {supersededCount > 0 && filter !== "superseded" && (
          <div className="flex items-center justify-between gap-3 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
            <span>
              <strong>{supersededCount}</strong> repl{supersededCount === 1 ? "y" : "ies"} arrived
              while an assessment was unpublished and {supersededCount === 1 ? "was" : "were"} held
              as <em>rejected — superseded</em>. Re-publishing re-binds new replies automatically;
              review these in case any were missed.
            </span>
            <button
              type="button"
              onClick={() => showRecoverySet("superseded")}
              className="shrink-0 font-medium text-amber-900 underline hover:text-amber-950"
            >
              Show held replies →
            </button>
          </div>
        )}

        {hiddenUnparsed > 0 && (
          <div className="flex items-center justify-between gap-3 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
            <span>
              <strong>{hiddenUnparsed}</strong> unparsed repl
              {hiddenUnparsed === 1 ? "y is" : "ies are"} hidden by this filter — switch to
              Unparsed to triage {hiddenUnparsed === 1 ? "it" : "them"}.
            </span>
            <button
              type="button"
              onClick={() => setFilterReset("unparsed")}
              className="shrink-0 font-medium text-amber-900 underline hover:text-amber-950"
            >
              Show unparsed →
            </button>
          </div>
        )}

        {undelivered > 0 && (
          <div className="flex items-center justify-between gap-3 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800">
            <span>
              <strong>{undelivered}</strong> resolved request{undelivered === 1 ? " has" : "s have"}{" "}
              an undelivered result email — the send failed after resolving, so{" "}
              {undelivered === 1 ? "the student" : "those students"} never received the outcome.
            </span>
            <button
              type="button"
              onClick={() => showRecoverySet("undelivered")}
              className="shrink-0 font-medium text-red-900 underline hover:text-red-950"
            >
              Show undelivered →
            </button>
          </div>
        )}

        {list.isPending ? (
          <div className="flex justify-center py-10">
            <Spinner className="size-6" />
          </div>
        ) : list.isError ? (
          <Card>
            <p className="text-sm text-red-600">{list.error.message}</p>
          </Card>
        ) : groups.length === 0 ? (
          <Card>
            <p className="text-sm text-neutral-400">No regrade requests match this filter.</p>
          </Card>
        ) : (
          <div className="space-y-4">
            {groups.map((group) => (
              <AssessmentGroupCard
                key={group.key}
                group={group}
                selectedId={selectedId}
                onSelect={setSelectedId}
                canAI={canAdjudicate}
                pendingKeys={pendingKeys}
              />
            ))}
          </div>
        )}

        {/* Pagination (I5): the queue is server-paginated; the server's filter-wide
            `total` drives a numbered Pager (0-based, self-hides at one page), so any
            page is directly reachable instead of Older-spam through a capped 50. */}
        {!list.isPending && !list.isError && total > 0 && (
          <div className="flex items-center justify-between gap-3">
            <span className="text-xs text-neutral-500 tabular-nums">
              {total} request{total === 1 ? "" : "s"}
            </span>
            <Pager
              page={offset / PAGE_SIZE}
              pageCount={Math.ceil(total / PAGE_SIZE)}
              onPage={(p) => setOffset(p * PAGE_SIZE)}
            />
          </div>
        )}
      </div>

      {selectedId !== null && (
        // Below xl the panes stack; the detail goes first (right under the page
        // header) so opening a request shows it without hunting, and the ref
        // scrolls it into view when the selection changes.
        <div
          key={selectedId}
          ref={(el) => el?.scrollIntoView({ block: "nearest" })}
          className="order-first w-full shrink-0 xl:order-none xl:w-[26rem]"
        >
          <DetailPane
            id={selectedId}
            // student_withdrawn lives on the LIST row only (the detail payload doesn't
            // carry it) — best-effort join against the currently fetched page; the badge
            // simply doesn't render when the selected row scrolled off this page/filter.
            withdrawn={Boolean(
              (list.data?.regrades ?? []).find((r) => r.id === selectedId)?.student_withdrawn,
            )}
            canAdjudicate={canAdjudicate}
            pendingKeys={pendingKeys}
            markPending={markPending}
            onClose={() => setSelectedId(null)}
          />
        </div>
      )}
    </div>
  );
}

// --- assessment group + queue table ---------------------------------------------------

function AssessmentGroupCard({
  group,
  selectedId,
  onSelect,
  canAI,
  pendingKeys,
}: {
  group: AssessmentGroup;
  selectedId: number | null;
  onSelect: (id: number) => void;
  canAI: boolean;
  pendingKeys: Set<string>;
}) {
  const needsRepublish = useNeedsRepublish(group.assessmentId);
  const [aiAllOpen, setAiAllOpen] = useState(false);
  const taAssignments = useTAAssignments(group.assessmentId);

  // "Pending" mirrors backend eligibility as far as a LIST row can see it: an open filed
  // request. Whether a given sub-item already carries an AI record is detail-only.
  const aiCandidates = group.rows.filter((r) => r.kind === "filed" && isOpen(r.status));
  const runningInGroup = group.rows.filter((r) =>
    [...pendingKeys].some((k) => k.startsWith(`${r.id}:`)),
  ).length;

  return (
    <Card
      title={
        <span className="inline-flex items-center gap-2">
          {group.assessmentName}
          {needsRepublish && (
            <span title="Grades changed since the last publish for this assessment">
              <Badge tone="amber">Needs re-publish</Badge>
            </span>
          )}
        </span>
      }
      actions={
        canAI && group.assessmentId !== undefined && (aiCandidates.length > 0 || runningInGroup > 0) ? (
          <span className="inline-flex items-center gap-3">
            {runningInGroup > 0 && (
              <span className="inline-flex items-center gap-1.5 text-xs text-neutral-500">
                <Spinner className="size-3.5" />
                {runningInGroup} AI re-grade{runningInGroup === 1 ? "" : "s"} running
              </span>
            )}
            {aiCandidates.length > 0 && (
              <Button
                variant="secondary"
                className="px-2.5 py-1 text-xs"
                onClick={() => setAiAllOpen(true)}
              >
                AI re-grade all pending
              </Button>
            )}
          </span>
        ) : undefined
      }
    >
      <Table className="border-0 shadow-none">
        <thead>
          <tr>
            <TH>Student</TH>
            <TH>Kind</TH>
            <TH>Status</TH>
            <TH className="w-40">Handoff</TH>
            <TH>Subject</TH>
            <TH className="w-40">Received</TH>
          </tr>
        </thead>
        <tbody>
          {group.rows.map((row) => {
            const dimmed = isDimmed(row);
            return (
              <tr
                key={row.id}
                onClick={() => onSelect(row.id)}
                className={cx(
                  "cursor-pointer hover:bg-neutral-50",
                  selectedId === row.id && "bg-indigo-50/70",
                  dimmed && "text-neutral-400",
                )}
              >
                <TD className={dimmed ? "text-neutral-400" : undefined}>
                  {row.student_name ?? "—"}
                  {row.student_external_id && (
                    <span className="ml-1 text-xs text-neutral-400">({row.student_external_id})</span>
                  )}
                  {/* Roster-lifecycle (plan 2026-07-10): withdrawn (停修) students keep
                      their regrade channel — flag only, adjudicate as usual. */}
                  {row.student_withdrawn && (
                    <span title="This student is withdrawn — regrade rights are preserved; adjudicate as usual">
                      <Badge tone="amber" className="ml-1.5">
                        withdrawn
                      </Badge>
                    </span>
                  )}
                </TD>
                <TD>
                  <span className="inline-flex flex-wrap items-center gap-1">
                    <Badge tone={KIND_TONES[row.kind]}>{KIND_LABELS[row.kind]}</Badge>
                    {row.turn !== undefined && (
                      <span className="text-xs text-neutral-400 tabular-nums">turn {row.turn}</span>
                    )}
                  </span>
                </TD>
                <TD>
                  {(() => {
                    const { tone, label } = queueStatusBadge(row);
                    return <Badge tone={tone}>{label}</Badge>;
                  })()}
                </TD>
                <TD>
                  {row.kind === "handed_off" ? (
                    <HandoffBadge requestId={row.id} assignmentsByProblemID={taAssignments} />
                  ) : (
                    <span className="text-neutral-300">—</span>
                  )}
                </TD>
                <TD className={cx("max-w-xs truncate", dimmed && "text-neutral-400")}>
                  {row.subject || "(no subject)"}
                </TD>
                <TD className="text-neutral-500">{fmtDate(row.received_at)}</TD>
              </tr>
            );
          })}
        </tbody>
      </Table>

      {aiAllOpen && group.assessmentId !== undefined && (
        <AIRegradeAllDialog assessmentId={group.assessmentId} onClose={() => setAiAllOpen(false)} />
      )}
    </Card>
  );
}

const KIND_TONES: Record<RegradeKind, BadgeTone> = {
  filed: "indigo",
  addendum: "neutral",
  unparsed: "amber",
  handed_off: "green",
};

/** Fetches GET /api/assessments/{id}/ta-assignments (regrade v2 gap 1, TA+) and indexes
 * it by problem_id. Every problem in the assessment is present in the response, with
 * user_id/user_name both null when unassigned — so a present-but-null entry is a real
 * "no TA assigned" answer, distinct from "haven't fetched yet". */
function useTAAssignments(assessmentId: number | undefined): Map<number, TAAssignmentRow> {
  const query = useQuery({
    queryKey: ["ta-assignments", assessmentId],
    queryFn: () =>
      api.get<TAAssignmentsResponse>(`/api/assessments/${assessmentId}/ta-assignments`),
    enabled: assessmentId !== undefined,
    staleTime: 30_000,
  });
  return useMemo(
    () => new Map((query.data?.assignments ?? []).map((a) => [a.problem_id, a] as const)),
    [query.data],
  );
}

/** Spec §6 D60: "handed to ⟨TA⟩" / amber "no TA assigned" per contested problem on a
 * handed-off row. The queue's list rows carry no per-problem info (only kind/status/
 * turn), so this fetches the request's own detail (cached under the same ["regrade",
 * id] key the detail pane uses — opening the row afterwards is instant) to get its
 * `problems[].problem_id`, then resolves each against the assessment-wide
 * ta-assignments map. Multiple contested problems can have different assignees, so this
 * renders one badge per problem rather than collapsing to a single name. */
function HandoffBadge({
  requestId,
  assignmentsByProblemID,
}: {
  requestId: number;
  assignmentsByProblemID: Map<number, TAAssignmentRow>;
}) {
  const detail = useQuery({
    queryKey: ["regrade", requestId],
    queryFn: () => api.get<RegradeDetail>(`/api/regrades/${requestId}`),
    staleTime: 30_000,
  });

  if (detail.isPending) {
    return <Spinner className="size-3.5" />;
  }
  if (detail.isError || !detail.data || detail.data.problems.length === 0) {
    return <span className="text-neutral-300">—</span>;
  }

  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      {detail.data.problems.map((sub) => {
        const assignment = assignmentsByProblemID.get(sub.problem_id);
        const label = `P${sub.problem_number ?? sub.problem_id}`;
        // Key off user_id, not user_name: a TA IS assigned whenever user_id is set, even
        // if the server's name lookup for that user failed (user_name null) — collapsing
        // that into the amber "no TA assigned" case would misreport an assigned problem
        // as unassigned (regrade v2 UI review Finding 3).
        if (assignment?.user_id != null) {
          // TAAssignmentRow carries no email (deliberately minimal, PII-adjacent) — the
          // shared fallback (audit B8) still applies: never show a bare user id.
          const name = userLabel({ display_name: assignment.user_name });
          return (
            <span key={sub.id} title={`${label}: handed to ${name}`}>
              <Badge tone="green">
                {label} → {name}
              </Badge>
            </span>
          );
        }
        return (
          <span key={sub.id} title={`${label}: no TA assigned`}>
            <Badge tone="amber">{label}: no TA assigned</Badge>
          </span>
        );
      })}
    </span>
  );
}

/**
 * Confirm-then-report dialog for POST /api/regrades/ai-regrade-all (spec §8): enumerates
 * eligible SUB-ITEMS (not requests) server-side. On open, fires a {dry_run: true}
 * preview so the confirm step shows the AUTHORITATIVE {enqueued, skipped,
 * estimated_cost} the real call would produce, including a would-be monthly-budget 409,
 * rendered identically to the real one. Confirming fires the real (non-dry) call.
 */
function AIRegradeAllDialog({
  assessmentId,
  onClose,
}: {
  assessmentId: number;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();

  const preview = useQuery({
    queryKey: ["ai-regrade-all-dry-run", assessmentId],
    queryFn: () =>
      api.post<AIRegradeAllResult>("/api/regrades/ai-regrade-all", {
        assessment_id: assessmentId,
        dry_run: true,
      }),
    retry: false,
  });

  const run = useMutation({
    mutationFn: () =>
      api.post<AIRegradeAllResult>("/api/regrades/ai-regrade-all", {
        assessment_id: assessmentId,
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["regrades"] });
    },
  });

  const previewBudgetError =
    preview.error instanceof ApiError && preview.error.status === 409 && preview.error.details
      ? (preview.error.details as BudgetExceededError)
      : null;
  const runBudgetError =
    run.error instanceof ApiError && run.error.status === 409 && run.error.details
      ? (run.error.details as BudgetExceededError)
      : null;

  return (
    <Dialog open onClose={onClose} title="AI re-grade all pending">
      {run.data ? (
        <div className="space-y-3 text-sm">
          <p className="text-neutral-700 tabular-nums">
            <strong>{run.data.enqueued}</strong> problem{run.data.enqueued === 1 ? "" : "s"}{" "}
            enqueued, <strong>{run.data.skipped}</strong> skipped (already had an AI record).
          </p>
          <p className="text-xs text-neutral-500 tabular-nums">
            Estimated cost:{" "}
            {run.data.estimated_cost === ""
              ? "unknown (no pricing entered for the pinned models)"
              : `$${run.data.estimated_cost}`}
          </p>
          {run.data.enqueued > 0 && (
            <p className="text-xs text-neutral-500">
              Results appear on each problem&apos;s sub-item card as jobs finish — the queue polls
              automatically until every job lands.
            </p>
          )}
          <div className="flex justify-end">
            <Button onClick={onClose}>Done</Button>
          </div>
        </div>
      ) : (
        <div className="space-y-3 text-sm">
          {preview.isPending ? (
            <p className="flex items-center gap-2 text-neutral-500">
              <Spinner className="size-3.5" /> Estimating count and cost…
            </p>
          ) : previewBudgetError ? (
            <p className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
              {previewBudgetError.error} — month to date ${previewBudgetError.month_to_date} +
              estimate ${previewBudgetError.estimate} would exceed the $
              {previewBudgetError.budget} monthly budget.
            </p>
          ) : preview.isError ? (
            <p className="text-xs text-red-600">{preview.error.message}</p>
          ) : (
            preview.data && (
              <>
                <p className="text-neutral-700 tabular-nums">
                  Enqueue one stricter AI re-grade for{" "}
                  <strong>{preview.data.enqueued}</strong> eligible problem
                  {preview.data.enqueued === 1 ? "" : "s"} in this assessment (
                  <strong>{preview.data.skipped}</strong> already-graded problem
                  {preview.data.skipped === 1 ? "" : "s"} skipped server-side).
                </p>
                <p className="text-xs text-neutral-500 tabular-nums">
                  Estimated cost:{" "}
                  {preview.data.estimated_cost === ""
                    ? "unknown (no pricing entered for the pinned models)"
                    : `$${preview.data.estimated_cost}`}
                </p>
              </>
            )
          )}
          <p className="text-xs text-neutral-500">
            Cost is estimated from each contested answer&apos;s pinned model pricing — reported
            as “unknown” when no pricing is entered (never a fake $0, D35) — and the monthly
            budget gate (D36) refuses the launch if it would be exceeded, exactly like run
            creation. AI prompts use masked images + redacted request text only, scoped to ONE
            problem's complaint per job.
          </p>
          {runBudgetError ? (
            <p className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
              {runBudgetError.error} — month to date ${runBudgetError.month_to_date} + estimate $
              {runBudgetError.estimate} would exceed the ${runBudgetError.budget} monthly budget.
            </p>
          ) : (
            run.isError && <p className="text-xs text-red-600">{run.error.message}</p>
          )}
          <div className="flex justify-end gap-2">
            <Button variant="secondary" onClick={onClose}>
              Cancel
            </Button>
            <Button
              onClick={() => run.mutate()}
              disabled={run.isPending || preview.isPending || Boolean(previewBudgetError)}
            >
              {run.isPending ? "Enqueuing…" : "Enqueue AI re-grades"}
            </Button>
          </div>
        </div>
      )}
    </Dialog>
  );
}

/**
 * "Needs re-publish" chip: reuses the assessment's publish preview rather than
 * duplicating its changed-vs-last-batch computation — `changed` is only populated when a
 * batch already exists, so an assessment with no publish history never shows the chip.
 * Best-effort: a preview fetch failure just hides the chip rather than surfacing an
 * error in the queue.
 */
function useNeedsRepublish(assessmentId: number | undefined): boolean {
  const preview = useQuery({
    queryKey: ["publish-preview", assessmentId],
    queryFn: () => api.get<PublishPreview>(`/api/assessments/${assessmentId}/publish/preview`),
    enabled: assessmentId !== undefined,
    staleTime: 30_000,
  });
  const pv = preview.data;
  return Boolean(pv?.ever_published && (pv.changed?.length ?? 0) > 0);
}

// --- detail pane ------------------------------------------------------------------

function DetailPane({
  id,
  withdrawn,
  canAdjudicate,
  pendingKeys,
  markPending,
  onClose,
}: {
  id: number;
  withdrawn: boolean;
  canAdjudicate: boolean;
  pendingKeys: Set<string>;
  markPending: MarkPending;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const detail = useQuery({
    queryKey: ["regrade", id],
    queryFn: () => api.get<RegradeDetail>(`/api/regrades/${id}`),
  });

  const assessment = useQuery({
    queryKey: ["assessment", String(detail.data?.assessment_id ?? "")],
    queryFn: () =>
      api.get<AssessmentDetailResponse>(`/api/assessments/${detail.data?.assessment_id}`),
    enabled: detail.data?.assessment_id !== undefined,
    staleTime: 30_000,
  });

  const remind = useMutation({
    mutationFn: () => api.post<RegradeRemindResponse>(`/api/regrades/${id}/remind`),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["regrade", id] }),
        queryClient.invalidateQueries({ queryKey: ["regrades"] }),
      ]);
    },
  });

  // Send-failure recovery (whole-branch review F1): resolved-but-undelivered result
  // email. A 409 here means the request is no longer eligible (already delivered, e.g.
  // by a concurrent resend, or somehow not resolved anymore) — refetch so the failed
  // state clears/updates rather than leaving a stale "failed" banner up.
  const resendResult = useMutation({
    mutationFn: () => api.post<RegradeResendResultResponse>(`/api/regrades/${id}/resend-result`),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["regrade", id] }),
        queryClient.invalidateQueries({ queryKey: ["regrades"] }),
      ]);
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409) {
        void queryClient.invalidateQueries({ queryKey: ["regrade", id] });
      }
    },
  });

  if (detail.isPending) {
    return (
      <Card title="Regrade" actions={<CloseButton onClose={onClose} />}>
        <div className="flex justify-center py-8">
          <Spinner className="size-5" />
        </div>
      </Card>
    );
  }
  if (detail.isError) {
    return (
      <Card title="Regrade" actions={<CloseButton onClose={onClose} />}>
        <p className="text-sm text-red-600">{detail.error.message}</p>
      </Card>
    );
  }

  const d = detail.data;
  const snapshot = decodeRegradeSnapshot<PublishSnapshot>(d.snapshot);
  const rejected = isRejected(d.status);
  const open = d.kind === "filed" && isOpen(d.status);
  const problems = assessment.data?.problems ?? [];
  const problemByNumber = new Map(problems.map((p) => [p.number, p] as const));

  // Collapse defaults (visual-hierarchy redesign): when sub-items were parsed out of the
  // email, the complaints already live on the problem cards — the raw email is reference
  // material and starts collapsed. For unparsed/addendum requests the raw body IS the
  // content, so it starts expanded. Keyed by request id so switching requests re-applies
  // the default instead of inheriting the previous request's toggle state.
  const hasParsedSubItems = d.problems.length > 0;
  const emailBlock = (
    <Disclosure
      key={`email-${d.id}`}
      defaultOpen={!hasParsedSubItems}
      summary={
        <span className="flex min-w-0 items-baseline gap-1.5">
          <span className="shrink-0">Original email</span>
          <span className="truncate font-normal text-neutral-400">
            {d.subject || "(no subject)"}
          </span>
        </span>
      }
    >
      {/* PII: subject/body/from_email are the student's own text — plain text only,
          NEVER dangerouslySetInnerHTML. Whitespace preserved via whitespace-pre-wrap
          since these are plain-text email bodies with the sender's own line breaks
          (only leading/trailing blank lines trimmed). */}
      <div className="space-y-1.5 text-sm">
        <div className="text-xs text-neutral-500">
          From <span className="text-neutral-700">{d.from_email}</span>
        </div>
        <div className="text-xs text-neutral-500">
          Subject <span className="font-medium text-neutral-700">{d.subject || "(no subject)"}</span>
        </div>
        <p className="whitespace-pre-wrap text-neutral-700">{trimBlankLines(d.body)}</p>
      </div>
    </Disclosure>
  );

  return (
    <div className="space-y-3">
      <Card
        title={
          <span className="inline-flex items-center gap-2">
            Regrade #{d.id}
            <Badge tone={KIND_TONES[d.kind]}>{KIND_LABELS[d.kind]}</Badge>
            <Badge tone={STATUS_TONES[d.status]}>{STATUS_LABELS[d.status]}</Badge>
          </span>
        }
        actions={<CloseButton onClose={onClose} />}
      >
        <div className="space-y-2.5 text-sm">
          <div className="space-y-0.5">
            <div className="text-neutral-800">
              {d.student_name || "—"}
              {d.student_external_id && (
                <span className="ml-1 text-xs text-neutral-400">({d.student_external_id})</span>
              )}
              {/* Roster-lifecycle: withdrawn (停修) students keep their regrade channel —
                  flag only, adjudicate as usual. */}
              {withdrawn && (
                <span title="This student is withdrawn — regrade rights are preserved; adjudicate as usual">
                  <Badge tone="amber" className="ml-1.5">
                    withdrawn
                  </Badge>
                </span>
              )}
            </div>
            <div className="text-xs text-neutral-500">
              {d.assessment_name || "—"} · received {fmtDate(d.received_at)}
              {d.turn !== undefined && (
                <span className="tabular-nums"> · attempt {d.turn}</span>
              )}
            </div>
          </div>

          {d.kind === "handed_off" && (
            <div className="rounded-md border border-emerald-200 bg-emerald-50 px-3 py-2 text-xs text-emerald-800">
              <strong>Handed off</strong> — this was the student&apos;s final attempt; each
              contested problem went directly to its assigned TA (unassigned problems are
              flagged). No more automated emails on this thread; further replies are recorded
              as addenda only.
            </div>
          )}

          {d.kind === "addendum" && (
            <div className="rounded-md border border-neutral-200 bg-neutral-50 px-3 py-2 text-xs text-neutral-600">
              A follow-up reply to an already-used reply token — it did not use an attempt and
              nothing was re-processed.
            </div>
          )}

          {rejected && (
            <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800">
              Rejected automatically ({STATUS_LABELS[d.status]}) — no action needed.
            </div>
          )}

          {resultSendFailed(d) && (
            <div className="space-y-2 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800">
              <p>
                <strong>Result email failed to send</strong> — the student did not receive it.
                This request was resolved, but the provider send failed afterward, so the
                result was never actually delivered.
              </p>
              {canAdjudicate && (
                <>
                  <Button
                    variant="danger"
                    className="px-2.5 py-1 text-xs"
                    disabled={resendResult.isPending}
                    onClick={() => resendResult.mutate()}
                  >
                    {resendResult.isPending ? "Resending…" : "Resend result"}
                  </Button>
                  {resendResult.isError && (
                    <p className="text-xs text-red-700">{resendResult.error.message}</p>
                  )}
                </>
              )}
            </div>
          )}

          {(d.spf_verdict || d.dkim_verdict) && (
            // B7: this is the INBOUND reply's SPF/DKIM verdict (was labeled "Delivery
            // details", which read as outbound-email info right next to the
            // resultSendFailed banner above — misleadingly implying it explained that
            // OUTBOUND send failure).
            <Disclosure key={`delivery-${d.id}`} summary="Reply authentication (SPF/DKIM)">
              <div className="flex gap-3 text-xs text-neutral-600">
                {d.spf_verdict && <span>SPF: {d.spf_verdict}</span>}
                {d.dkim_verdict && <span>DKIM: {d.dkim_verdict}</span>}
              </div>
            </Disclosure>
          )}
        </div>
      </Card>

      {/* Unparsed/addendum: the raw email is the only content — it leads, expanded. */}
      {!hasParsedSubItems && emailBlock}

      {d.kind === "unparsed" && canAdjudicate && (
        <Card title="Send reminder">
          <div className="space-y-2 text-sm">
            <p className="text-xs text-neutral-500">
              No <code>&lt;pN&gt;</code> problem block was recognized in this reply, so no
              attempt was used and the student&apos;s reply token is still valid. Reminders are
              never sent automatically — this one references the original email&apos;s subject
              and date, restates the required format and the attempt counter, and carries no
              token itself (replies to the reminder are not processed).
            </p>
            {!isOpen(d.status) ? (
              <Badge tone="neutral">Reminder already sent</Badge>
            ) : (
              <Button
                variant="secondary"
                className="px-2.5 py-1 text-xs"
                disabled={remind.isPending}
                onClick={() => {
                  if (confirm("Send a one-time reminder to this student? This cannot be undone.")) {
                    remind.mutate();
                  }
                }}
              >
                {remind.isPending ? "Sending…" : "Send reminder"}
              </Button>
            )}
            {remind.isError && (
              <p className="text-xs text-red-600">{remind.error.message}</p>
            )}
          </div>
        </Card>
      )}

      {/* WORKING SURFACE: per-problem adjudication cards come first (after the compact
          header); the raw email and score snapshot follow as collapsed reference. */}
      {d.kind === "filed" && (
        <SubItemsSection
          detail={d}
          problemByNumber={problemByNumber}
          snapshot={snapshot}
          canAdjudicate={canAdjudicate}
          pendingKeys={pendingKeys}
          markPending={markPending}
          open={open}
        />
      )}

      {/* Handed-off rows never reached adjudication (the final-turn token consumption
          fires the handoff directly, spec §6) — sub-items carry complaint text only, no
          verdict/AI record. Read-only: the assigned TAs took over person-to-person. */}
      {d.kind === "handed_off" && d.problems.length > 0 && (
        <div className="space-y-3">
          <h2 className="px-1 text-xs font-semibold tracking-wide text-neutral-500 uppercase">
            Contested problems handed off ({d.problems.length})
          </h2>
          {d.problems.map((sub) => (
            <Card key={sub.id} title={`Problem ${sub.problem_number ?? sub.problem_id}`}>
              {/* PII: complaint_text is the student's own message — plain text only. */}
              <blockquote className="border-l-2 border-neutral-300 pl-3 text-sm whitespace-pre-wrap text-neutral-700">
                {trimBlankLines(sub.complaint_text)}
              </blockquote>
            </Card>
          ))}
        </div>
      )}

      {hasParsedSubItems && emailBlock}
      {snapshot && <SnapshotSection key={`snapshot-${d.id}`} snapshot={snapshot} />}
    </div>
  );
}

function CloseButton({ onClose }: { onClose: () => void }) {
  return (
    <IconButton label="Close" onClick={onClose}>
      <X />
    </IconButton>
  );
}

/** "Published scores" reference, collapsed by default behind a one-line total — the
 * per-problem context now lives in each problem card's "currently {score}/{max}". */
function SnapshotSection({ snapshot }: { snapshot: PublishSnapshot }) {
  return (
    <Disclosure
      summary={
        <span className="flex items-baseline justify-between gap-2">
          <span>Published scores</span>
          <span className="font-normal tabular-nums text-neutral-500">
            Total {snapshot.total || "—"} / {snapshot.max}
          </span>
        </span>
      }
    >
      {snapshot.problems.length > 0 ? (
        <Table className="border-0 shadow-none">
          <thead>
            <tr>
              <TH>Problem</TH>
              <TH className="w-24 text-right">Score</TH>
            </tr>
          </thead>
          <tbody>
            {snapshot.problems.map((p) => (
              <tr key={p.number}>
                <TD>
                  {p.number}. {p.title}
                </TD>
                <TD className="text-right tabular-nums">
                  {p.no_submission ? (
                    <span className="text-neutral-400">no submission</span>
                  ) : (
                    `${p.total || "—"} / ${p.max}`
                  )}
                </TD>
              </tr>
            ))}
          </tbody>
        </Table>
      ) : (
        <p className="text-xs text-neutral-500">No per-problem scores in this snapshot.</p>
      )}
    </Disclosure>
  );
}

// --- sub-items (spec §5): one card per contested problem ------------------------------

/** "currently {score}/{max}" for a problem card header, joined from the published
 * snapshot by problem number. Undefined (header clause omitted) when the snapshot is
 * missing, the sub-item has no problem number, or the snapshot has no matching row.
 * Decimal strings rendered verbatim — no float parsing. */
function currentScoreLabel(
  snapshot: PublishSnapshot | null | undefined,
  problemNumber: number | undefined,
): string | undefined {
  if (!snapshot || problemNumber === undefined) return undefined;
  const row = snapshot.problems.find((p) => p.number === problemNumber);
  if (!row) return undefined;
  return row.no_submission ? "no submission" : `${row.total || "—"} / ${row.max}`;
}

function SubItemsSection({
  detail,
  problemByNumber,
  snapshot,
  canAdjudicate,
  pendingKeys,
  markPending,
  open,
}: {
  detail: RegradeDetail;
  problemByNumber: Map<number, { id: number; title: string }>;
  snapshot: PublishSnapshot | null | undefined;
  canAdjudicate: boolean;
  pendingKeys: Set<string>;
  markPending: MarkPending;
  open: boolean;
}) {
  const allVerdicted =
    detail.problems.length > 0 && detail.problems.every((p) => p.verdict !== undefined);

  return (
    <>
      <div className="space-y-3">
        <h2 className="px-1 text-xs font-semibold tracking-wide text-neutral-500 uppercase">
          Contested problems ({detail.problems.length})
        </h2>
        {detail.problems.map((sub) => (
          <SubItemCard
            key={sub.id}
            requestId={detail.id}
            sub={sub}
            problemTitle={
              sub.problem_number !== undefined
                ? problemByNumber.get(sub.problem_number)?.title
                : undefined
            }
            currentScore={currentScoreLabel(snapshot, sub.problem_number)}
            canAdjudicate={canAdjudicate}
            open={open}
            pending={pendingKeys.has(jobKey(detail.id, sub.id))}
            markPending={markPending}
            assessmentId={detail.assessment_id}
            studentExternalId={detail.student_external_id}
          />
        ))}
      </div>

      {canAdjudicate && open && (
        <SendResultCard detail={detail} allVerdicted={allVerdicted} />
      )}
    </>
  );
}

function SubItemCard({
  requestId,
  sub,
  problemTitle,
  currentScore,
  canAdjudicate,
  open,
  pending,
  markPending,
  assessmentId,
  studentExternalId,
}: {
  requestId: number;
  sub: RegradeSubItem;
  problemTitle?: string;
  currentScore?: string;
  canAdjudicate: boolean;
  open: boolean;
  pending: boolean;
  markPending: MarkPending;
  assessmentId?: number;
  studentExternalId?: string;
}) {
  const queryClient = useQueryClient();
  // verdict_note is omitempty server-side (never-verdicted sub-items omit it entirely),
  // so default to "" rather than undefined — otherwise the Textarea below flips from
  // uncontrolled to controlled the moment a verdict lands (regrade v2 UI review Finding 2).
  const [note, setNote] = useState(sub.verdict_note ?? "");
  const [editingNote, setEditingNote] = useState(false);
  const [adoptOpen, setAdoptOpen] = useState(false);

  // "Regraded" needs a record to adopt as this turn's grade. The plain PATCH default
  // adopts the sub-item's AI record — but only if that record exists AND carries a real
  // total. When the round AI re-grade failed (ai_error, no record) or refused as illegible
  // (record present, total null), the default 409/400s into a dead end whose only escape
  // is "Upheld" (FIX 2). In that case, clicking "Regraded" opens an adopt panel that lets
  // the TA adopt an existing grade on the answer (a manual one from Review is the natural
  // pick) instead of firing a doomed request.
  const canDefaultRegrade = Boolean(sub.ai_record && sub.ai_record.total !== null);

  const verdict = useMutation({
    mutationFn: (body: { outcome: "upheld" | "regraded"; note: string }) =>
      api.patch<RegradeSubItem>(`/api/regrades/${requestId}/problems/${sub.id}`, body),
    onSuccess: async () => {
      setEditingNote(false);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["regrade", requestId] }),
        queryClient.invalidateQueries({ queryKey: ["regrades"] }),
      ]);
    },
  });

  // Note-only save for the "Edit note" editor (silent-discard fix): the note state used
  // to be sent ONLY by re-clicking a verdict button — the editor had no save path, so
  // switching rows or closing the pane (keyed by selection) dropped the edit without
  // warning. Saving re-sends the CURRENT verdict with the new note. For a "regraded"
  // verdict the backend re-derives adoption from the sub-item's AI record whenever
  // adopted_record_id is omitted, which would silently swap a manually-adopted grade
  // (or 409 when no adoptable AI record exists) — so resolve the currently-adopted
  // record id from the answer's regrade layers and pass it back explicitly, keeping a
  // note-only save adoption-neutral. Errors (e.g. the 409 adjudicable-state gate when
  // the request resolved under us in another tab) surface under the editor.
  const noteSave = useMutation({
    mutationFn: async (newNote: string) => {
      if (!sub.verdict) throw new Error("No verdict to attach this note to.");
      const body: {
        outcome: "upheld" | "regraded";
        note: string;
        adopted_record_id?: number;
      } = { outcome: sub.verdict, note: newNote };
      if (sub.verdict === "regraded") {
        // Same answer-id resolution chain as AdoptRecordPanel: the AI record carries it
        // directly; without one (manual adoption after an AI failure), look it up from
        // the student's submission by problem number.
        let answerId = sub.ai_record?.answer_id;
        if (answerId === undefined && assessmentId !== undefined && studentExternalId) {
          const sv = await api.get<StudentSubmissionView>(
            `/api/assessments/${assessmentId}/students/${encodeURIComponent(studentExternalId)}/submission`,
          );
          answerId = sv.answers.find((a) => a.problem_number === sub.problem_number)?.answer_id;
        }
        if (answerId === undefined) {
          throw new Error(
            "Couldn't locate this problem's answer to preserve its adopted grade — note not saved.",
          );
        }
        const ans = await api.get<AnswerResponse>(`/api/answers/${answerId}`);
        const adopted = ans.regrade_layers.find(
          (l) => l.sub_item_id === sub.id,
        )?.adopted_record_id;
        if (adopted === undefined) {
          throw new Error(
            "Couldn't determine this verdict's adopted grade — note not saved.",
          );
        }
        body.adopted_record_id = adopted;
      }
      return api.patch<RegradeSubItem>(`/api/regrades/${requestId}/problems/${sub.id}`, body);
    },
    onSuccess: async () => {
      setEditingNote(false);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["regrade", requestId] }),
        queryClient.invalidateQueries({ queryKey: ["regrades"] }),
      ]);
    },
  });

  const aiRegrade = useMutation({
    mutationFn: (rerun: boolean) =>
      api.post<AIRegradeEnqueueResult>(
        `/api/regrades/${requestId}/ai-regrade${rerun ? "?rerun=1" : ""}`,
        { problem_id: sub.problem_id },
      ),
    onSuccess: (_data, rerun) => {
      markPending([
        {
          requestId,
          subItemId: sub.id,
          baseline: rerun ? (sub.ai_record?.created_at ?? "") : undefined,
        },
      ]);
    },
    onError: (err) => {
      // 409 = eligibility changed under us (resolved in another tab, a record landed…).
      // Refetch so the card self-heals to the real state.
      if (err instanceof ApiError && err.status === 409) {
        void queryClient.invalidateQueries({ queryKey: ["regrade", requestId] });
      }
    },
  });

  return (
    <Card
      className="border-neutral-300"
      title={
        <span className="inline-flex min-w-0 flex-wrap items-baseline gap-x-1.5">
          <span>
            Problem {sub.problem_number ?? sub.problem_id}
            {problemTitle ? ` — ${problemTitle}` : ""}
          </span>
          {currentScore !== undefined && (
            <span className="text-xs font-normal text-neutral-500 tabular-nums">
              · currently {currentScore}
            </span>
          )}
        </span>
      }
      actions={
        sub.verdict ? (
          <Badge tone={sub.verdict === "upheld" ? "neutral" : "green"}>
            {sub.verdict === "upheld" ? "Upheld" : "Regraded"}
          </Badge>
        ) : (
          <Badge tone="amber">No verdict</Badge>
        )
      }
    >
      <div className="space-y-2.5 text-sm">
        {/* PII: complaint_text is the student's own message — plain text only, NEVER
            dangerouslySetInnerHTML. Sender's line breaks preserved; only leading/trailing
            blank lines trimmed. */}
        <blockquote className="border-l-2 border-neutral-300 pl-3 whitespace-pre-wrap text-neutral-700">
          {trimBlankLines(sub.complaint_text)}
        </blockquote>

        {sub.ai_error && (
          <div className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
            {sub.ai_error}; resolve manually.
          </div>
        )}

        {/* Evidence being weighed: open by default until a verdict lands, then collapsed
            to its summary line. Keyed so a new record or a verdict flip re-applies the
            default (the user's manual toggle wins otherwise). */}
        {sub.ai_record && (
          <AIComparisonCard
            key={`${sub.id}:${sub.verdict ?? "none"}:${sub.ai_record.created_at ?? ""}`}
            ai={sub.ai_record}
            defaultOpen={!sub.verdict}
            currentScore={currentScore}
          />
        )}

        {canAdjudicate && open && (
          <div className="space-y-2 border-t border-neutral-100 pt-2.5">
            <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
              {!sub.ai_error &&
                (pending ? (
                  <span className="inline-flex items-center gap-2 text-xs text-neutral-500">
                    <Spinner className="size-3.5" />
                    AI re-grade running…
                  </span>
                ) : (
                  <span className="inline-flex items-center gap-1.5">
                    <Button
                      variant={sub.ai_record ? "secondary" : "primary"}
                      className="px-2.5 py-1 text-xs"
                      disabled={aiRegrade.isPending}
                      onClick={() => aiRegrade.mutate(Boolean(sub.ai_record))}
                    >
                      {aiRegrade.isPending
                        ? "Enqueuing…"
                        : sub.ai_record
                          ? "Re-run AI re-grade"
                          : "AI re-grade"}
                    </Button>
                    <HelpTip title="AI re-grade">{aiRegradeHelp}</HelpTip>
                  </span>
                ))}
              <span className="ml-auto inline-flex items-center gap-1.5">
                <span className="text-xs font-medium text-neutral-500">
                  Verdict <HelpTip title="Regrade verdicts">{regradeVerdictHelp}</HelpTip>
                </span>
                <Button
                  variant={sub.verdict === "upheld" ? "primary" : "secondary"}
                  className="px-2.5 py-1 text-xs"
                  disabled={verdict.isPending}
                  onClick={() => verdict.mutate({ outcome: "upheld", note })}
                >
                  Upheld
                </Button>
                <Button
                  variant={sub.verdict === "regraded" ? "primary" : "secondary"}
                  className="px-2.5 py-1 text-xs"
                  disabled={verdict.isPending}
                  onClick={() => {
                    if (canDefaultRegrade) {
                      setAdoptOpen(false);
                      verdict.mutate({ outcome: "regraded", note });
                    } else {
                      // No adoptable AI record — open the adopt panel instead of firing a
                      // PATCH the backend would reject.
                      setAdoptOpen((v) => !v);
                    }
                  }}
                >
                  {canDefaultRegrade ? "Regraded" : "Regraded…"}
                </Button>
                {/* Offered only while the request is still adjudicable — this whole
                    block is gated on `open` (received/under_review), so a resolved
                    request (result email sent) never shows an editor whose PATCH the
                    backend would 409. Entering edit mode re-syncs the draft from the
                    server's note so a stale local draft isn't presented as current. */}
                {sub.verdict && !editingNote && (
                  <Button
                    variant="ghost"
                    className="px-2 py-1 text-xs"
                    onClick={() => {
                      setNote(sub.verdict_note ?? "");
                      noteSave.reset();
                      setEditingNote(true);
                    }}
                  >
                    Edit note
                  </Button>
                )}
              </span>
            </div>
            {aiRegrade.isError && <p className="text-xs text-red-600">{aiRegrade.error.message}</p>}
            {(!sub.verdict || editingNote) && (
              <Textarea
                rows={2}
                value={note}
                onChange={(e) => setNote(e.target.value)}
                placeholder="Note for the student's result email (optional)…"
              />
            )}
            {sub.verdict && editingNote && (
              <div className="flex items-center gap-1.5">
                <Button
                  variant="primary"
                  className="px-2.5 py-1 text-xs"
                  disabled={noteSave.isPending}
                  onClick={() => noteSave.mutate(note)}
                >
                  {noteSave.isPending ? "Saving…" : "Save note"}
                </Button>
                <Button
                  variant="secondary"
                  className="px-2.5 py-1 text-xs"
                  disabled={noteSave.isPending}
                  onClick={() => {
                    // Cancel restores the saved note — the discarded draft is deliberate
                    // here, and re-entering edit mode starts from the server's text.
                    setNote(sub.verdict_note ?? "");
                    setEditingNote(false);
                    noteSave.reset();
                  }}
                >
                  Cancel
                </Button>
              </div>
            )}
            {editingNote && noteSave.isError && (
              <p className="text-xs text-red-600">{noteSave.error.message}</p>
            )}
            {sub.verdict && !editingNote && sub.verdict_note && (
              <p className="whitespace-pre-wrap text-xs text-neutral-600">{sub.verdict_note}</p>
            )}
            {verdict.isError && <p className="text-xs text-red-600">{verdict.error.message}</p>}
            {adoptOpen && !canDefaultRegrade && (
              <AdoptRecordPanel
                requestId={requestId}
                sub={sub}
                note={note}
                assessmentId={assessmentId}
                studentExternalId={studentExternalId}
                onClose={() => setAdoptOpen(false)}
              />
            )}
          </div>
        )}

        {!canAdjudicate && sub.verdict && sub.verdict_note && (
          <div>
            <div className="text-xs font-medium text-neutral-500">Note</div>
            <p className="whitespace-pre-wrap text-xs text-neutral-600">{sub.verdict_note}</p>
          </div>
        )}
      </div>
    </Card>
  );
}

/** Readable label for a grading record's origin in the adopt picker. Never renders
 * student PII — only the record's own provenance and total. */
function recordSourceLabel(rec: GradingRecord): string {
  switch (rec.source) {
    case "human":
      return "Manual grade";
    case "model":
      return rec.method_version !== undefined ? `AI grade (v${rec.method_version})` : "AI grade";
    case "aggregate":
      return "Consensus grade";
    default:
      return rec.source;
  }
}

/**
 * Adopt-a-record escape hatch for the "Regraded" verdict when the sub-item has no
 * adoptable AI record (FIX 2): the round AI re-grade failed (ai_error, no record) or
 * refused as illegible (record present but no total). The PATCH verdict endpoint requires
 * an adopted_record_id in that path, so a plain "Regraded" click would 409/400 into a
 * dead end whose only escape is "Upheld". This resolves the contested answer — via the AI
 * record's answer_id, or a student-submission lookup by problem number when no record
 * exists — lists that answer's grading records that carry a real total (a manual record
 * created in Review is the natural pick), and adopts the chosen one. When none exist yet,
 * it links straight to the answer's review page to create one. No backend change: PATCH
 * already accepts adopted_record_id and the records come from GET /api/answers/{id}.
 *
 * PII: the answer/submission payloads carry the student's name/email — this panel renders
 * only record provenance/total/date, never those fields, and never logs them.
 */
function AdoptRecordPanel({
  requestId,
  sub,
  note,
  assessmentId,
  studentExternalId,
  onClose,
}: {
  requestId: number;
  sub: RegradeSubItem;
  note: string;
  assessmentId?: number;
  studentExternalId?: string;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();

  // answer_id resolution: the AI record carries it directly; without a record, look it up
  // from the student's submission view by problem number.
  const directAnswerId = sub.ai_record?.answer_id;
  const needLookup = directAnswerId === undefined;
  const submission = useQuery({
    queryKey: ["student-submission", String(assessmentId ?? ""), studentExternalId ?? ""],
    queryFn: () =>
      api.get<StudentSubmissionView>(
        `/api/assessments/${assessmentId}/students/${encodeURIComponent(studentExternalId ?? "")}/submission`,
      ),
    enabled: needLookup && assessmentId !== undefined && Boolean(studentExternalId),
    staleTime: 30_000,
  });
  const answerId =
    directAnswerId ??
    submission.data?.answers.find((a) => a.problem_number === sub.problem_number)?.answer_id;

  const answer = useQuery({
    queryKey: ["answer", String(answerId ?? "")],
    queryFn: () => api.get<AnswerResponse>(`/api/answers/${answerId}`),
    enabled: answerId !== undefined,
  });

  // Adoptable = has a real total (the backend rejects a null-total "illegible refusal").
  // Newest first so a freshly-entered manual grade sits at the top.
  const records = (answer.data?.records ?? [])
    .filter((r) => r.total !== null)
    .slice()
    .sort((a, b) => (b.created_at ?? "").localeCompare(a.created_at ?? ""));

  const adopt = useMutation({
    mutationFn: (recordId: number) =>
      api.patch<RegradeSubItem>(`/api/regrades/${requestId}/problems/${sub.id}`, {
        outcome: "regraded",
        note,
        adopted_record_id: recordId,
      }),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["regrade", requestId] }),
        queryClient.invalidateQueries({ queryKey: ["regrades"] }),
      ]);
      onClose();
    },
  });

  const loading =
    submission.isFetching || (answerId !== undefined && (answer.isPending || answer.isFetching));

  return (
    <div className="space-y-2 rounded-md border border-neutral-200 bg-neutral-50/70 px-3 py-2.5">
      <div className="flex items-center justify-between gap-2">
        <p className="text-xs font-medium text-neutral-700">Adopt a grade to record “Regraded”</p>
        <Button variant="ghost" className="px-2 py-0.5 text-xs" onClick={onClose}>
          Cancel
        </Button>
      </div>
      <p className="text-xs text-neutral-500">
        The AI re-grade left no adoptable grade for this answer, so pick the record this turn
        should adopt — usually a grade you enter by hand in Review.
      </p>
      {loading ? (
        <p className="flex items-center gap-2 text-xs text-neutral-500">
          <Spinner className="size-3.5" /> Loading this answer&apos;s grades…
        </p>
      ) : answerId === undefined ? (
        <p className="text-xs text-red-600">
          {submission.isError
            ? submission.error.message
            : "Couldn't locate this student's answer for this problem — open Review to grade it."}
        </p>
      ) : answer.isError ? (
        <p className="text-xs text-red-600">{answer.error.message}</p>
      ) : records.length === 0 ? (
        <div className="space-y-1.5">
          <p className="text-xs text-neutral-600">
            No grade with a score exists on this answer yet — grade it in Review, then come back
            to adopt it.
          </p>
          <a
            href={`/answers/${answerId}`}
            target="_blank"
            rel="noreferrer"
            className={buttonClassName("secondary", "px-2.5 py-1 text-xs")}
          >
            Grade it in Review ↗
          </a>
        </div>
      ) : (
        <>
          <ul className="space-y-1.5">
            {records.map((rec) => (
              <li
                key={rec.id}
                className="flex items-center justify-between gap-2 rounded border border-neutral-200 bg-white px-2.5 py-1.5"
              >
                <span className="min-w-0 text-xs text-neutral-700">
                  <span className="font-medium">{recordSourceLabel(rec)}</span>
                  <span className="tabular-nums"> · {rec.total ?? "—"} pts</span>
                  {rec.created_at && (
                    <span className="text-neutral-400"> · {fmtDate(rec.created_at)}</span>
                  )}
                </span>
                <Button
                  variant="secondary"
                  className="shrink-0 px-2 py-1 text-xs"
                  disabled={adopt.isPending}
                  onClick={() => adopt.mutate(rec.id)}
                >
                  Adopt
                </Button>
              </li>
            ))}
          </ul>
          <a
            href={`/answers/${answerId}`}
            target="_blank"
            rel="noreferrer"
            className="inline-block text-xs text-indigo-600 hover:underline"
          >
            Or grade it fresh in Review ↗
          </a>
        </>
      )}
      {adopt.isError && <p className="text-xs text-red-600">{adopt.error.message}</p>}
    </div>
  );
}

/**
 * Old-vs-new comparison for one sub-item's AI record (spec §8): per-criterion
 * score/comment. No published-snapshot join here (v1 compared against the snapshot's
 * per-problem criteria by name; the v2 per-sub-item record stands alone — the total is
 * shown against the record's own total only) to avoid guessing a snapshot problem match
 * without a submission listing in scope for this file. AI comments are model rationale
 * over student work — render only, never console.log.
 */
function AIComparisonCard({
  ai,
  defaultOpen,
  currentScore,
}: {
  ai: RegradeAIRecord;
  defaultOpen: boolean;
  currentScore?: string;
}) {
  const aiCriteria = ai.criteria ?? [];
  return (
    <Disclosure
      defaultOpen={defaultOpen}
      summary={
        <span className="flex min-w-0 items-center justify-between gap-2">
          <span className="inline-flex items-center gap-2">
            AI re-grade
            {ai.policy && <PolicyBadge policy={ai.policy} />}
          </span>
          {/* Decimal strings shown side-by-side, verbatim (no float parsing) — the TA
              reads the comparison; we don't compute a higher/lower judgement. */}
          <span className="shrink-0 font-normal tabular-nums text-neutral-500">
            AI total {ai.total ?? "—"}
            {currentScore !== undefined ? ` · currently ${currentScore}` : ""}
          </span>
        </span>
      }
    >
      <div className="space-y-2">
        {aiCriteria.length > 0 ? (
          <Table className="border-0 shadow-none">
            <thead>
              <tr>
                <TH>Criterion</TH>
                <TH className="w-24 text-right">AI score</TH>
              </tr>
            </thead>
            <tbody>
              {aiCriteria.map((c: RegradeAIRecordCriterion) => (
                <tr key={c.criterion_id}>
                  <TD>
                    {c.name || `criterion #${c.criterion_id}`}
                    {c.comment && (
                      <p className="mt-0.5 text-xs whitespace-pre-wrap text-neutral-500">{c.comment}</p>
                    )}
                  </TD>
                  <TD className="text-right tabular-nums">
                    {c.score || "—"} / {c.max}
                  </TD>
                </tr>
              ))}
              <tr>
                <TD className="font-medium">Total</TD>
                <TD className="text-right font-medium tabular-nums">{ai.total ?? "—"}</TD>
              </tr>
            </tbody>
          </Table>
        ) : (
          <p className="text-xs text-neutral-500">
            The AI record carries no per-criterion scores{ai.total !== null ? ` (total ${ai.total})` : ""}.
          </p>
        )}
        <div className="flex items-center justify-between gap-2">
          <p className="text-xs text-neutral-500">
            Advisory only — never changes the official grade.
            {ai.created_at && (
              <span className="text-neutral-400"> Generated {fmtDate(ai.created_at)}.</span>
            )}
          </p>
          <a
            href={`/answers/${ai.answer_id}`}
            target="_blank"
            rel="noreferrer"
            className={buttonClassName("ghost", "shrink-0 px-2 py-0.5 text-xs")}
          >
            Open answer ↗
          </a>
        </div>
      </div>
    </Disclosure>
  );
}

// --- send-result (spec §5): gated, per-problem checklist -------------------------------

function SendResultCard({
  detail,
  allVerdicted,
}: {
  detail: RegradeDetail;
  allVerdicted: boolean;
}) {
  const queryClient = useQueryClient();

  const send = useMutation({
    mutationFn: () => api.post<RegradeSendResultResponse>(`/api/regrades/${detail.id}/send-result`),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["regrade", detail.id] }),
        queryClient.invalidateQueries({ queryKey: ["regrades"] }),
      ]);
    },
    onError: (err) => {
      // 409: either the gate re-fired (raced verdict elsewhere) or the request was
      // already resolved by a concurrent send. Refetch so the checklist below reflects
      // the current live state instead of the stale client-side computation.
      if (err instanceof ApiError && err.status === 409) {
        void queryClient.invalidateQueries({ queryKey: ["regrade", detail.id] });
      }
    },
  });

  // The 409 body from POST /api/regrades/{id}/send-result is now the structured
  // {error, unverdicted: [{problem_id, problem_number}]} payload (regrade v2 gap 3,
  // internal/httpapi/regrade.go apiError409Unverdicted) — computed server-side straight
  // from the sub-items lacking a verdict at the moment of the gate check, so it's
  // authoritative for a race (a verdict landing elsewhere between this pane's last fetch
  // and the send attempt). Before any send attempt (or once a send succeeds) there's no
  // 409 to read, so the checklist falls back to detail.problems, which the pane already
  // keeps fresh via invalidation.
  const raced = send.error instanceof ApiError && send.error.status === 409;
  const authoritativeUnverdicted: UnverdictedProblem[] | null =
    raced && send.error instanceof ApiError && send.error.details
      ? ((send.error.details as RegradeSendResult409Body).unverdicted ?? null)
      : null;
  const unverdictedIDs = authoritativeUnverdicted
    ? new Set(authoritativeUnverdicted.map((p) => p.problem_id))
    : null;
  const isUnverdicted = (p: RegradeSubItem) =>
    unverdictedIDs ? unverdictedIDs.has(p.problem_id) : p.verdict === undefined;
  const unverdicted = detail.problems.filter(isUnverdicted);

  return (
    <Card title="Send result">
      <div className="space-y-2.5 text-sm">
        {/* Compact per-problem verdict chips (one line, wraps) instead of a checklist
            paragraph per problem. */}
        <div className="flex flex-wrap items-center gap-1.5">
          {detail.problems.map((p) => {
            const stillUnverdicted = isUnverdicted(p);
            const label = `P${p.problem_number ?? p.problem_id}`;
            return stillUnverdicted ? (
              <Badge key={p.id} tone="amber">{label} · no verdict</Badge>
            ) : (
              <Badge key={p.id} tone={p.verdict === "regraded" ? "green" : "neutral"}>
                {label} · {p.verdict ?? "verdicted"}
              </Badge>
            );
          })}
        </div>

        {detail.turn !== undefined && (
          <p className="text-xs text-neutral-500 tabular-nums">
            Attempt {detail.turn} of {detail.regrade_max}. The result email carries the
            next-attempt reply token — on the final attempt it instead directs further replies
            to each problem&apos;s assigned TA.
          </p>
        )}

        {raced ? (
          <p className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
            {send.error instanceof Error ? send.error.message : "Send failed"} — refreshed;
            re-check the verdicts above before retrying.
          </p>
        ) : (
          send.isError && <p className="text-xs text-red-600">{send.error.message}</p>
        )}

        {send.isSuccess && (
          <p className="rounded-md bg-emerald-50 px-3 py-2 text-xs text-emerald-700 ring-1 ring-emerald-200 ring-inset">
            Result #{send.data.turn} sent{send.data.final ? " — final attempt." : "."}
          </p>
        )}

        <Button
          className="w-full"
          disabled={!allVerdicted || unverdicted.length > 0 || send.isPending || send.isSuccess}
          onClick={() => send.mutate()}
        >
          {send.isPending
            ? "Sending…"
            : unverdicted.length > 0
              ? `Verdict ${unverdicted.length} problem${unverdicted.length === 1 ? "" : "s"} first`
              : "Send result"}
        </Button>
      </div>
    </Card>
  );
}
