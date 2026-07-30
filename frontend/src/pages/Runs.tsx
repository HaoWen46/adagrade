// AI grading runs: launch (with mask-gate preflight, D10), live progress, and a
// per-leaf item breakdown. Row click expands the run detail; the list polls while
// any run is active, the detail polls while its run is active.
// `/runs?launch=1&assessment_id=X&problem_id=Y` opens the launch dialog pre-filled
// (the "Grade with AI…" shortcut on the problem review page).

import { memo, useEffect, useState, type FormEvent } from "react";
import { Link, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../lib/api";
import type {
  Assessment,
  AssessmentDetailResponse,
  BudgetExceededError,
  Method,
  ModelPricing,
  Provider,
  Run,
  RunCounts,
  RunDetailResponse,
  RunItem,
  RunListRow,
  RunPreview,
  RunStatus,
  ScopeKind,
  SpotCheckResponse,
  SpotCheckSample,
} from "../lib/types";
import { fmtDate } from "../lib/format";
import { runScopeHelp, runStatusHelp, spotCheckHelp } from "../lib/helpContent";
import { estimateRunCostUSD, meetsMinCostCap } from "../lib/decimal";
import { warningView } from "../lib/warnings";
import { roleAtLeast, useMe } from "../lib/auth";
import {
  Badge,
  Button,
  Card,
  Dialog,
  Field,
  Input,
  Select,
  Spinner,
  TD,
  TH,
  Table,
  Textarea,
  buttonClassName,
  type BadgeTone,
} from "../components/ui";
import { HelpTip } from "../components/HelpTip";
import { WorkflowNotice } from "../components/WorkflowNotice";

const COLS = 7;

const STATUS_TONES: Record<RunStatus, BadgeTone> = {
  pending: "neutral",
  running: "indigo",
  paused: "amber",
  cancelled: "amber",
  completed: "green",
  failed: "red",
};

/** All run statuses, in lifecycle order — drives the status filter dropdown. */
const RUN_STATUSES = Object.keys(STATUS_TONES) as RunStatus[];

const ITEM_TONES: Record<string, BadgeTone> = {
  pending: "neutral",
  running: "indigo",
  succeeded: "green",
  failed: "red",
  skipped: "amber",
};

function isActive(status: RunStatus): boolean {
  return status === "pending" || status === "running";
}

interface LaunchPrefill {
  assessmentId: string;
  problemId: string;
  /** "sample" opens the dialog on the calibration-sample scope (Overview's
   * "Start calibration run"); sampleN is the pre-filled sample size. */
  scope?: string;
  sampleN?: string;
}

export function Runs() {
  const [searchParams, setSearchParams] = useSearchParams();
  const [launch, setLaunch] = useState<LaunchPrefill | null>(() =>
    searchParams.get("launch") === "1"
      ? {
          assessmentId: searchParams.get("assessment_id") ?? "",
          problemId: searchParams.get("problem_id") ?? "",
          scope: searchParams.get("scope") ?? undefined,
          sampleN: searchParams.get("n") ?? undefined,
        }
      : null,
  );
  const [expandedId, setExpandedId] = useState<number | null>(null);

  // Consume the ?launch=… params so close/reload doesn't reopen the dialog.
  useEffect(() => {
    if (searchParams.get("launch") === "1") setSearchParams({}, { replace: true });
  }, [searchParams, setSearchParams]);

  // List filters, applied server-side (the list endpoint returns only the most
  // recent 50 rows, so client-side filtering would silently miss older runs).
  const [filterAssessment, setFilterAssessment] = useState("");
  const [filterStatus, setFilterStatus] = useState("");
  const filtering = filterAssessment !== "" || filterStatus !== "";

  const assessments = useQuery({
    queryKey: ["assessments", false],
    queryFn: () => api.get<{ assessments: Assessment[] }>("/api/assessments"),
  });

  const listParams = new URLSearchParams();
  if (filterAssessment !== "") listParams.set("assessment_id", filterAssessment);
  if (filterStatus !== "") listParams.set("status", filterStatus);
  const listQS = listParams.toString();

  const list = useQuery({
    queryKey: ["runs", filterAssessment, filterStatus],
    queryFn: () => api.get<{ runs: RunListRow[] }>(`/api/runs${listQS ? `?${listQS}` : ""}`),
    refetchInterval: (query) =>
      query.state.data?.runs.some((r) => isActive(r.status)) ? 2500 : false,
  });

  return (
    <div className="mx-auto max-w-5xl space-y-4">
      <div className="flex items-center justify-between gap-3">
        <h1 className="text-lg font-semibold text-neutral-900">Runs</h1>
        <Button onClick={() => setLaunch({ assessmentId: "", problemId: "" })}>
          Launch run
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <Select
          aria-label="Filter by assessment"
          value={filterAssessment}
          onChange={(e) => setFilterAssessment(e.target.value)}
        >
          <option value="">All assessments</option>
          {(assessments.data?.assessments ?? []).map((a) => (
            <option key={a.id} value={String(a.id)}>
              {a.name}
            </option>
          ))}
        </Select>
        <Select
          aria-label="Filter by status"
          value={filterStatus}
          onChange={(e) => setFilterStatus(e.target.value)}
        >
          <option value="">All statuses</option>
          {RUN_STATUSES.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </Select>
        <HelpTip title="Run status">{runStatusHelp}</HelpTip>
        {filtering && (
          <Button
            variant="ghost"
            className="px-2 py-1 text-xs"
            onClick={() => {
              setFilterAssessment("");
              setFilterStatus("");
            }}
          >
            Clear filters
          </Button>
        )}
      </div>

      {list.isPending ? (
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      ) : list.isError ? (
        <Card>
          <p className="text-sm text-red-600">{list.error.message}</p>
        </Card>
      ) : (
        <Table>
          <thead>
            <tr>
              <TH className="w-16">ID</TH>
              <TH>Assessment</TH>
              <TH>Method</TH>
              <TH className="w-32">Scope</TH>
              <TH className="w-28">Status</TH>
              <TH className="w-32">Progress</TH>
              <TH className="w-40">Created</TH>
            </tr>
          </thead>
          <tbody>
            {list.data.runs.length === 0 && (
              <tr>
                <TD colSpan={COLS} className="text-center text-neutral-400">
                  {filtering
                    ? "No runs match these filters."
                    : "No runs yet — launch one to grade with AI."}
                </TD>
              </tr>
            )}
            {list.data.runs.map((r) => (
              <RunRow
                key={r.id}
                run={r}
                expanded={expandedId === r.id}
                onToggle={() => setExpandedId(expandedId === r.id ? null : r.id)}
              />
            ))}
          </tbody>
        </Table>
      )}

      {launch && (
        <LaunchRunDialog
          prefill={launch}
          onClose={() => setLaunch(null)}
          onLaunched={(runId) => setExpandedId(runId)}
        />
      )}
    </div>
  );
}

// --- rows ---------------------------------------------------------------------------

function scopeLabel(kind: ScopeKind, scopeID: number): string {
  if (kind === "assessment") return "assessment";
  if (kind === "sample") return `sample of ${scopeID}`;
  return `${kind} id ${scopeID}`;
}

function RunRow({
  run,
  expanded,
  onToggle,
}: {
  run: RunListRow;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <>
      <tr onClick={onToggle} className="cursor-pointer hover:bg-neutral-50">
        <TD className="font-medium tabular-nums">
          <span className="mr-1.5 inline-block w-3 text-neutral-400">{expanded ? "▾" : "▸"}</span>
          {run.id}
        </TD>
        <TD>{run.assessment_name}</TD>
        <TD>
          {run.method_name}{" "}
          <span className="text-xs text-neutral-400 tabular-nums">v{run.method_version}</span>
        </TD>
        <TD className="text-neutral-600">{scopeLabel(run.scope_kind, run.scope_id)}</TD>
        <TD>
          <Badge tone={STATUS_TONES[run.status] ?? "neutral"}>{run.status}</Badge>
        </TD>
        <TD>
          <RunProgress counts={run.counts} />
        </TD>
        <TD className="text-neutral-500">{fmtDate(run.created_at)}</TD>
      </tr>
      {expanded && (
        <tr>
          <TD colSpan={COLS} className="bg-neutral-50/60 px-4 py-3">
            <RunDetail runId={run.id} />
          </TD>
        </tr>
      )}
    </>
  );
}

function RunProgress({ counts }: { counts: RunCounts }) {
  const total =
    (counts.pending ?? 0) +
    (counts.running ?? 0) +
    (counts.succeeded ?? 0) +
    (counts.failed ?? 0) +
    (counts.skipped ?? 0);
  const done = (counts.succeeded ?? 0) + (counts.failed ?? 0);
  return (
    <div>
      <span className="text-xs text-neutral-600 tabular-nums">
        {done} / {total}
      </span>
      <div className="mt-1 h-1 overflow-hidden rounded-full bg-neutral-200">
        <div
          className="h-full rounded-full bg-indigo-600 transition-[width]"
          style={{ width: total > 0 ? `${(done / total) * 100}%` : "0%" }}
        />
      </div>
    </div>
  );
}

// --- expanded detail: items + actions -------------------------------------------------

function RunDetail({ runId }: { runId: number }) {
  const queryClient = useQueryClient();
  const [notice, setNotice] = useState<string | null>(null);
  // F20: default view is server-filtered to failed/running items only (the
  // counts summary below covers the rest); this toggle refetches with ?all=1
  // for the full ~1800-row table when a TA actually wants to see everything.
  const [showAll, setShowAll] = useState(false);
  const me = useMe();
  const isAdmin = roleAtLeast(me.data?.user.role, "admin");

  const detail = useQuery({
    queryKey: ["run", runId, showAll],
    queryFn: () =>
      api.get<RunDetailResponse>(`/api/runs/${runId}${showAll ? "?all=1" : ""}`),
    refetchInterval: (query) => {
      const status = query.state.data?.run.status;
      return status !== undefined && isActive(status) ? 2000 : false;
    },
  });

  // Spot-check gate state (trust spec §4): polled alongside the run while active, and
  // whenever a verdict/waive mutation lands. Since 0027 the gate blocks PUBLISHING
  // with this run's method as the final source (Publish tab) — the sample is still
  // reviewed here, where the run's records live.
  const spotCheck = useQuery({
    queryKey: ["spot-check", runId],
    queryFn: () => api.get<SpotCheckResponse>(`/api/runs/${runId}/spot-check`),
    refetchInterval: (query) => {
      const s = query.state.data?.state;
      if (!s) return 3000; // no data yet — keep polling until the first response lands
      const open = s.waived || (s.total > 0 && s.done === s.total);
      return open ? false : 3000;
    },
  });

  const invalidate = async () => {
    // Partial match (no showAll suffix) invalidates both the filtered and
    // all=1 variants of this run's query.
    await queryClient.invalidateQueries({ queryKey: ["run", runId] });
    await queryClient.invalidateQueries({ queryKey: ["runs"] });
  };
  const invalidateSpotCheck = () => queryClient.invalidateQueries({ queryKey: ["spot-check", runId] });

  const cancel = useMutation({
    mutationFn: () => api.post(`/api/runs/${runId}/cancel`),
    onSuccess: async () => invalidate(),
  });
  const retry = useMutation({
    mutationFn: () => api.post<{ retried: number }>(`/api/runs/${runId}/retry-failed`),
    onSuccess: async (res) => {
      setNotice(`Re-enqueued ${res.retried} failed item${res.retried === 1 ? "" : "s"}.`);
      await invalidate();
    },
  });

  if (detail.isPending) {
    return <Spinner />;
  }
  if (detail.isError) {
    return <p className="text-sm text-red-600">{detail.error.message}</p>;
  }

  const { run, items, truncated } = detail.data;
  const failedItems = run.counts.failed ?? 0;
  const actionError = cancel.error ?? retry.error;

  // Spot-check gate state (0027): surfaced as a note — the gate itself is
  // enforced at publish time when this run's method is the final source.
  const gate = spotCheck.data?.state;
  const gateOpen = gate ? gate.waived || (gate.total > 0 && gate.done === gate.total) : undefined;
  const gateNote =
    gate !== undefined && !gateOpen && run.status === "completed"
      ? gate.total === 0
        ? "Spot-check sample still pending."
        : `Spot-check ${gate.done} of ${gate.total} reviewed — publishing with this method as the final source stays blocked until the sample is done (or an admin waives).`
      : null;

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone={STATUS_TONES[run.status] ?? "neutral"}>{run.status}</Badge>
        <span className="text-xs text-neutral-500">
          started {fmtDate(run.started_at)} · finished {fmtDate(run.finished_at)}
        </span>
        <div className="ml-auto flex items-center gap-2">
          {isActive(run.status) && (
            <Button
              variant="danger"
              className="px-2.5 py-1 text-xs"
              disabled={cancel.isPending}
              onClick={() => cancel.mutate()}
            >
              Cancel run
            </Button>
          )}
          {failedItems > 0 && (
            <Button
              variant="secondary"
              className="px-2.5 py-1 text-xs"
              disabled={retry.isPending}
              onClick={() => retry.mutate()}
            >
              Retry failed ({failedItems})
            </Button>
          )}
        </div>
      </div>

      {run.error && (
        <p className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
          {run.error}
        </p>
      )}
      {notice && (
        <p className="rounded-md bg-green-50 px-3 py-2 text-xs text-green-700 ring-1 ring-green-200 ring-inset">
          {notice}
        </p>
      )}
      {actionError && <p className="text-xs text-red-600">{actionError.message}</p>}
      {gateNote && (
        <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-700 ring-1 ring-amber-200 ring-inset">
          {gateNote}
        </p>
      )}

      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-neutral-600">
        <span>
          Cost <span className="tabular-nums font-medium text-neutral-800">${run.cost_usd}</span>
          {run.cost_cap_usd && (
            <span className="text-neutral-400"> / cap ${run.cost_cap_usd}</span>
          )}
        </span>
        <span className="tabular-nums">
          {run.input_tokens.toLocaleString()} in · {run.output_tokens.toLocaleString()} out tokens
        </span>
      </div>

      <div className="flex flex-wrap items-center justify-between gap-2">
        <RunCountsSummary counts={run.counts} />
        <Button
          variant="secondary"
          className="px-2.5 py-1 text-xs"
          onClick={() => setShowAll((v) => !v)}
        >
          {showAll ? "Show only failed/running items" : "Show all items"}
        </Button>
      </div>

      {truncated && (
        <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-700 ring-1 ring-amber-200 ring-inset">
          Item list truncated at the display cap — not every item is shown below.
        </p>
      )}

      <Table>
        <thead>
          <tr>
            <TH className="w-24">Answer</TH>
            <TH className="w-32">Student ID</TH>
            <TH className="w-24 text-right">Problem</TH>
            <TH>Model</TH>
            <TH className="w-28">State</TH>
            <TH className="w-24 text-right">Attempts</TH>
            <TH>Error</TH>
          </tr>
        </thead>
        <tbody>
          {items.length === 0 && (
            <tr>
              <TD colSpan={7} className="text-center text-neutral-400">
                {showAll ? "No items planned yet." : "No failed or running items."}
              </TD>
            </tr>
          )}
          {items.map((it) => (
            <ItemRow key={it.id} item={it} />
          ))}
        </tbody>
      </Table>

      <SpotCheckStrip
        runId={runId}
        data={spotCheck.data}
        isPending={spotCheck.isPending}
        error={spotCheck.error}
        isAdmin={isAdmin}
        onChanged={invalidateSpotCheck}
      />

    </div>
  );
}

// --- spot-check strip -------------------------------------------------------------

// SpotCheckStrip is the trust-gate review surface (trust spec §4, D37): the sampled
// records for this run, each with an agree/adjust verdict button, the done/total
// progress, and (admin-only) a waive override with an audited reason.
function SpotCheckStrip({
  runId,
  data,
  isPending,
  error,
  isAdmin,
  onChanged,
}: {
  runId: number;
  data: SpotCheckResponse | undefined;
  isPending: boolean;
  error: Error | null;
  isAdmin: boolean;
  onChanged: () => void;
}) {
  const [waiveOpen, setWaiveOpen] = useState(false);
  const [reason, setReason] = useState("");

  const verdict = useMutation({
    mutationFn: ({ recordId, verdict, note }: { recordId: number; verdict: "agree" | "adjusted"; note: string }) =>
      api.post(`/api/runs/${runId}/spot-check/${recordId}`, { verdict, note }),
    onSuccess: onChanged,
  });
  const waive = useMutation({
    mutationFn: () => api.post(`/api/runs/${runId}/spot-check/waive`, { reason }),
    onSuccess: () => {
      setWaiveOpen(false);
      setReason("");
      onChanged();
    },
  });

  if (isPending) {
    return <Spinner className="size-4" />;
  }
  if (error || !data) {
    return <p className="text-xs text-red-600">{error?.message ?? "spot-check load failed"}</p>;
  }

  const { samples, state, agreement } = data;
  if (samples.length === 0 && !state.waived) {
    return null; // nothing sampled yet (run not completed, or zero gradable leaves)
  }

  return (
    <Card
      title={
        <span className="inline-flex items-center gap-1.5">
          Spot-check <HelpTip title="Spot-check">{spotCheckHelp}</HelpTip>
        </span>
      }
      actions={
        <div className="flex items-center gap-2">
          <span className="text-xs tabular-nums text-neutral-500">
            {state.waived ? "waived" : `${state.done} / ${state.total} reviewed`}
            {agreement.total > 0 && ` · ${agreement.agreed}/${agreement.total} agreed`}
          </span>
          {isAdmin && !state.waived && (
            <Button variant="secondary" className="px-2 py-1 text-xs" onClick={() => setWaiveOpen(true)}>
              Waive
            </Button>
          )}
        </div>
      }
    >
      {state.waived && (
        <p className="mb-2 text-xs text-neutral-500">
          This run&apos;s spot-check gate was waived by an admin.
        </p>
      )}
      <ul className="space-y-2">
        {samples.map((s) => (
          <SpotCheckRow
            key={s.id}
            sample={s}
            onAgree={() => verdict.mutate({ recordId: s.id, verdict: "agree", note: "" })}
            onAdjust={(note) => verdict.mutate({ recordId: s.id, verdict: "adjusted", note })}
            saving={verdict.isPending}
          />
        ))}
      </ul>
      {verdict.isError && <p className="mt-2 text-xs text-red-600">{verdict.error.message}</p>}

      {waiveOpen && (
        <Dialog open onClose={() => setWaiveOpen(false)} title="Waive spot-check gate">
          <p className="text-sm text-neutral-600">
            Marks this run&apos;s sample as reviewed without doing it, unblocking publish when
            this method is the final source. The reason is recorded in the audit log.
          </p>
          <Field label="Reason" className="mt-3">
            <Textarea
              required
              rows={3}
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="e.g. reviewed manually offline, low-stakes practice set…"
            />
          </Field>
          {waive.isError && <p className="mt-2 text-xs text-red-600">{waive.error.message}</p>}
          <div className="mt-4 flex justify-end gap-2">
            <Button variant="secondary" onClick={() => setWaiveOpen(false)}>
              Cancel
            </Button>
            <Button
              variant="danger"
              disabled={waive.isPending || reason.trim() === ""}
              onClick={() => waive.mutate()}
            >
              {waive.isPending ? "Waiving…" : "Waive"}
            </Button>
          </div>
        </Dialog>
      )}
    </Card>
  );
}

function SpotCheckRow({
  sample,
  onAgree,
  onAdjust,
  saving,
}: {
  sample: SpotCheckSample;
  onAgree: () => void;
  onAdjust: (note: string) => void;
  saving: boolean;
}) {
  const [noteOpen, setNoteOpen] = useState(false);
  const [note, setNote] = useState(sample.note);
  const decided = sample.verdict !== undefined;
  const answerHref = `/answers/${sample.answer_id}`;

  return (
    <li className="flex flex-wrap items-center gap-2 rounded-md border border-neutral-200 px-3 py-2 text-xs">
      <Link to={answerHref} className="font-medium text-indigo-600 hover:underline">
        Answer #{sample.answer_id}
      </Link>
      <span className="text-neutral-500">problem {sample.problem_number}</span>
      <span className="tabular-nums text-neutral-700">
        {sample.total ?? "—"} pts{sample.confidence ? ` · ${sample.confidence}` : ""}
      </span>
      <div className="ml-auto flex items-center gap-1.5">
        {decided ? (
          <Badge tone={sample.verdict === "agree" ? "green" : "amber"}>{sample.verdict}</Badge>
        ) : noteOpen ? (
          <>
            <Input
              className="w-40 py-1"
              placeholder="note (optional)"
              value={note}
              onChange={(e) => setNote(e.target.value)}
            />
            {/* Adjust records the verdict, then deep-links to AnswerView (the manual-grade
                form) — this endpoint only records the spot-check call, it doesn't edit the
                grade itself (internal/httpapi/runs.go handleSetSpotCheckVerdict). */}
            <Link
              to={answerHref}
              className={buttonClassName("secondary", "px-2 py-1")}
              aria-disabled={saving}
              onClick={(e) => {
                if (saving) {
                  e.preventDefault();
                  return;
                }
                onAdjust(note);
              }}
            >
              Save &amp; open answer
            </Link>
            <Button variant="ghost" className="px-2 py-1" onClick={() => setNoteOpen(false)}>
              Cancel
            </Button>
          </>
        ) : (
          <>
            <Button variant="secondary" className="px-2 py-1" disabled={saving} onClick={onAgree}>
              Agree
            </Button>
            <Button variant="secondary" className="px-2 py-1" onClick={() => setNoteOpen(true)}>
              Adjust…
            </Button>
          </>
        )}
      </div>
    </li>
  );
}

// RunCountsSummary renders the per-state totals from the GROUP-BY counts
// (F20): this is the cheap always-available summary, independent of whether
// the (possibly filtered) item table below is showing everything.
function RunCountsSummary({ counts }: { counts: RunCounts }) {
  const order: Array<keyof RunCounts> = ["pending", "running", "succeeded", "failed", "skipped"];
  const parts = order.filter((k) => (counts[k] ?? 0) > 0);
  if (parts.length === 0) {
    return <span className="text-xs text-neutral-400">No items planned yet.</span>;
  }
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-xs">
      {parts.map((k) => (
        <Badge key={k} tone={ITEM_TONES[k] ?? "neutral"}>
          {k} {counts[k]}
        </Badge>
      ))}
    </div>
  );
}

// ItemRow is memoized (F20): the run-detail poll can carry up to ~1800 rows
// under `?all=1`, so re-rendering every row on every 2 s poll tick is wasted
// work whenever a row's own item object hasn't changed.
const ItemRow = memo(function ItemRow({ item }: { item: RunItem }) {
  return (
    <tr>
      <TD>
        <Link
          to={`/answers/${item.answer_id}`}
          className="text-indigo-600 tabular-nums hover:underline"
        >
          #{item.answer_id}
        </Link>
      </TD>
      <TD className="tabular-nums">{item.student_id}</TD>
      <TD className="text-right tabular-nums">{item.problem_number}</TD>
      <TD className="font-mono text-xs">{item.model}</TD>
      <TD>
        <Badge tone={ITEM_TONES[item.state] ?? "neutral"}>{item.state}</Badge>
      </TD>
      <TD className="text-right tabular-nums">{item.attempts}</TD>
      <TD className="text-xs text-red-600">{item.error ?? ""}</TD>
    </tr>
  );
});

// --- launch dialog --------------------------------------------------------------------

// Fresh-install dead-end guard: with no usable grading method the Method dropdown
// would be a lone "select…" with no way forward. Grading needs a provider first,
// then a method that references it — so surface whichever prerequisite is missing
// with a link to its setup page, inside the dialog.
function MethodSetupPrompt({
  hasProviders,
  providersPending,
}: {
  hasProviders: boolean;
  providersPending: boolean;
}) {
  if (providersPending) {
    return (
      <div className="flex items-center gap-2 py-1 text-xs text-neutral-500">
        <Spinner className="size-4" /> Checking setup…
      </div>
    );
  }
  return (
    <div className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
      {hasProviders ? (
        <>
          <p className="font-medium">No grading method is set up yet.</p>
          <p className="mt-1 text-amber-700">
            A method picks the AI model, prompt, and policy used to grade. Create one, then
            come back to launch a run.
          </p>
          <Link to="/methods" className="mt-1.5 inline-block font-medium underline">
            Set up a method →
          </Link>
        </>
      ) : (
        <>
          <p className="font-medium">No AI provider or grading method is set up yet.</p>
          <p className="mt-1 text-amber-700">
            Grading needs an AI provider first, then a method that uses it:
          </p>
          <p className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1">
            <Link to="/providers" className="font-medium underline">
              1. Add a provider →
            </Link>
            <Link to="/methods" className="font-medium underline">
              2. Create a method →
            </Link>
          </p>
        </>
      )}
    </div>
  );
}

function LaunchRunDialog({
  prefill,
  onClose,
  onLaunched,
}: {
  prefill: LaunchPrefill;
  onClose: () => void;
  onLaunched: (runId: number) => void;
}) {
  const queryClient = useQueryClient();
  const [assessmentId, setAssessmentId] = useState(prefill.assessmentId);
  const [scopeKind, setScopeKind] = useState<ScopeKind>(
    prefill.scope === "sample" ? "sample" : prefill.problemId !== "" ? "problem" : "assessment",
  );
  const [problemId, setProblemId] = useState(prefill.problemId);
  const [answerId, setAnswerId] = useState("");
  // Calibration sample size (guide §3.1 suggests 5–10; default the midpoint).
  const [sampleN, setSampleN] = useState(prefill.sampleN ?? "8");
  const [methodId, setMethodId] = useState("");
  const [costCap, setCostCap] = useState(""); // decimal string, "" = no cap

  const assessments = useQuery({
    queryKey: ["assessments", false],
    queryFn: () => api.get<{ assessments: Assessment[] }>("/api/assessments"),
  });
  const detail = useQuery({
    queryKey: ["assessment", assessmentId],
    queryFn: () => api.get<AssessmentDetailResponse>(`/api/assessments/${assessmentId}`),
    enabled: assessmentId !== "" && scopeKind === "problem",
  });
  const methods = useQuery({
    queryKey: ["methods", false],
    queryFn: () => api.get<{ methods: Method[] }>("/api/methods"),
  });
  const usableMethods = (methods.data?.methods ?? []).filter(
    (m) => !m.archived && m.latest !== undefined,
  );
  const selectedMethod = usableMethods.find((m) => String(m.id) === methodId);

  // Estimate display needs pricing for the selected method's (provider, model): the
  // create-run 409 body only carries an estimate when the launch actually hits the
  // monthly budget (trust spec §3), so a pre-submit estimate is computed here from
  // the same provider-pricing table, client-side, in exact decimal math (never
  // parseFloat — decimal.ts estimateRunCostUSD mirrors store.EstimateCostUSD).
  const providers = useQuery({
    queryKey: ["providers", false],
    queryFn: () => api.get<{ providers: Provider[] }>("/api/providers"),
  });
  const selectedProvider = providers.data?.providers.find(
    (p) => p.name === selectedMethod?.latest?.config.provider,
  );
  const pricing = useQuery({
    queryKey: ["pricing", selectedProvider?.id],
    queryFn: () =>
      api.get<{ pricing: ModelPricing[] }>(`/api/providers/${selectedProvider?.id}/pricing`),
    enabled: selectedProvider !== undefined,
  });
  const modelPricing = pricing.data?.pricing.find(
    (p) => p.model === selectedMethod?.latest?.config.model,
  );

  const scopeId =
    scopeKind === "assessment"
      ? assessmentId
      : scopeKind === "problem"
        ? problemId
        : scopeKind === "sample"
          ? sampleN.trim()
          : answerId.trim();
  const scopeReady =
    assessmentId !== "" &&
    scopeId !== "" &&
    (scopeKind !== "answer" || /^\d+$/.test(scopeId)) &&
    (scopeKind !== "sample" || /^[1-9]\d*$/.test(scopeId));

  // method_id rides along so method-scoped preflight warnings (provider_disabled)
  // can be computed server-side; it's in the queryKey so switching methods refetches.
  const preview = useQuery({
    queryKey: ["run-preview", assessmentId, scopeKind, scopeId, methodId],
    queryFn: () =>
      api.get<RunPreview>(
        `/api/runs/preview?assessment_id=${assessmentId}&scope_kind=${scopeKind}&scope_id=${scopeId}${
          methodId !== "" ? `&method_id=${methodId}` : ""
        }`,
      ),
    enabled: scopeReady,
  });

  const estimate =
    preview.data && modelPricing
      ? estimateRunCostUSD(preview.data.answers, modelPricing.input_usd_per_mtok, modelPricing.output_usd_per_mtok)
      : null;

  const costCapTrimmed = costCap.trim();
  const costCapValid = costCapTrimmed === "" || meetsMinCostCap(costCapTrimmed);

  const launch = useMutation({
    mutationFn: () =>
      api.post<Run>("/api/runs", {
        assessment_id: Number(assessmentId),
        scope_kind: scopeKind,
        scope_id: Number(scopeId),
        method_id: Number(methodId),
        ...(costCapTrimmed !== "" ? { cost_cap_usd: costCapTrimmed } : {}),
      }),
    onSuccess: async (run) => {
      await queryClient.invalidateQueries({ queryKey: ["runs"] });
      onLaunched(run.id);
      onClose();
    },
  });

  // The monthly-budget 409 (trust spec §3, D36) carries {month_to_date, estimate,
  // budget} — the only place that data is ever surfaced (there's no proactive GET for
  // it), so show it inline once a launch attempt actually hits the gate.
  const budgetError =
    launch.error instanceof ApiError && launch.error.status === 409 && launch.error.details
      ? (launch.error.details as BudgetExceededError)
      : null;

  // Mask blockers guarantee the run would fail the mask gate — keep Launch
  // disabled until every in-scope page has an accepted mask.
  const maskOk = !preview.data || preview.data.mask_blockers === 0;
  // provider_disabled is the one preflight warning that blocks (create-run 409s on a
  // missing/disabled provider — guaranteed failure, mirrors the mask gate); every
  // other warning is advisory and renders amber without disabling Launch.
  const previewWarnings = preview.data?.warnings ?? [];
  const providerOk = !previewWarnings.some((w) => w.code === "provider_disabled");
  // B9-UI: GET /api/runs/preview's `blockers` array (task 5) enumerates every
  // per-problem reason this exact launch is guaranteed to fail (missing rubric, missing
  // reference solution) — always an array (never null), so an empty list is "clean".
  const previewBlockers = preview.data?.blockers ?? [];
  // 0 answers in scope is its own guaranteed instant failure (Runner.Plan has nothing to
  // grade) and is NOT expressed in blockers[] — the estimate's own `answers` count is the
  // only signal for it. Permissive (like maskOk/providerOk above) while the estimate is
  // still loading.
  const answersOk = !preview.data || preview.data.answers > 0;
  const valid =
    scopeReady &&
    methodId !== "" &&
    costCapValid &&
    maskOk &&
    providerOk &&
    previewBlockers.length === 0 &&
    answersOk;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (valid) launch.mutate();
  };

  return (
    <Dialog open onClose={onClose} title="Launch run">
      <form className="space-y-3" onSubmit={submit}>
        <Field label="Assessment">
          <Select
            required
            value={assessmentId}
            onChange={(e) => {
              setAssessmentId(e.target.value);
              setProblemId(""); // problems belong to the previous assessment
            }}
            className="w-full"
          >
            <option value="">select…</option>
            {(assessments.data?.assessments ?? []).map((a) => (
              <option key={a.id} value={String(a.id)}>
                {a.name}
              </option>
            ))}
          </Select>
        </Field>
        <div className="grid grid-cols-2 gap-3">
          <Field
            label={
              <>
                Scope <HelpTip title="Run scope">{runScopeHelp}</HelpTip>
              </>
            }
          >
            <Select
              value={scopeKind}
              onChange={(e) => setScopeKind(e.target.value as ScopeKind)}
              className="w-full"
            >
              <option value="assessment">assessment</option>
              <option value="problem">problem</option>
              <option value="answer">answer</option>
              <option value="sample">calibration sample</option>
            </Select>
          </Field>
          {scopeKind === "sample" && (
            <Field label="Sample size">
              <Input
                required
                inputMode="numeric"
                placeholder="5–10"
                value={sampleN}
                onChange={(e) => setSampleN(e.target.value)}
              />
            </Field>
          )}
          {scopeKind === "problem" && (
            <Field label="Problem">
              <Select
                required
                value={problemId}
                onChange={(e) => setProblemId(e.target.value)}
                className="w-full"
              >
                <option value="">select…</option>
                {(detail.data?.problems ?? []).map((p) => (
                  <option key={p.id} value={String(p.id)}>
                    {p.number}
                    {p.title && ` — ${p.title}`}
                  </option>
                ))}
              </Select>
            </Field>
          )}
          {scopeKind === "answer" && (
            <Field label="Answer id">
              <Input
                required
                inputMode="numeric"
                placeholder="e.g. 42"
                value={answerId}
                onChange={(e) => setAnswerId(e.target.value)}
              />
            </Field>
          )}
        </div>
        <Field label="Method">
          {methods.isPending ? (
            <div className="flex items-center gap-2 py-1 text-xs text-neutral-500">
              <Spinner className="size-4" /> Loading methods…
            </div>
          ) : usableMethods.length === 0 ? (
            <MethodSetupPrompt
              hasProviders={(providers.data?.providers.length ?? 0) > 0}
              providersPending={providers.isPending}
            />
          ) : (
            <Select
              required
              value={methodId}
              onChange={(e) => setMethodId(e.target.value)}
              className="w-full"
            >
              <option value="">select…</option>
              {usableMethods.map((m) => (
                <option key={m.id} value={String(m.id)}>
                  {m.name} (v{m.latest?.version} · {m.latest?.config.provider}/
                  {m.latest?.config.model})
                </option>
              ))}
            </Select>
          )}
        </Field>

        <Field
          label={
            <>
              Cost cap (USD, optional){" "}
              <HelpTip title="Cost cap">
                The leaf executor stops enqueuing new grade calls once this run&apos;s
                accumulated spend would exceed the cap. Minimum $0.01 — smaller amounts round
                to $0 in storage and would block the run before it starts.
              </HelpTip>
            </>
          }
        >
          <Input
            inputMode="decimal"
            placeholder="e.g. 5.00 — leave blank for no cap"
            value={costCap}
            onChange={(e) => setCostCap(e.target.value)}
          />
          {!costCapValid && (
            <p className="mt-1 text-xs text-red-600">Cost cap must be at least $0.01.</p>
          )}
        </Field>

        {scopeReady &&
          (preview.isPending ? (
            <Spinner />
          ) : preview.isError ? (
            <p className="text-xs text-red-600">{preview.error.message}</p>
          ) : (
            <div className="space-y-1.5">
              <p className="text-sm text-neutral-600 tabular-nums">
                {preview.data.answers} answer{preview.data.answers === 1 ? "" : "s"} will be
                graded.
              </p>
              {/* B9-UI: 0 answers is its own guaranteed instant failure, not expressed in
                  blockers[] — the estimate's own count is the only signal. */}
              {preview.data.answers === 0 && (
                <p className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
                  No answers are in this scope — the run would fail immediately with
                  nothing to grade.
                </p>
              )}
              {/* The estimate is the launch decision's headline number — small label,
                  prominent figure. Missing pricing renders amber with a fix-it link
                  (the estimate stays unknown until /providers has the model's rates). */}
              <p className="tabular-nums">
                <span className="text-xs text-neutral-500">Estimated cost:</span>{" "}
                {methodId === "" ? (
                  <span className="text-xs text-neutral-500">select a method</span>
                ) : providers.isPending ||
                  (selectedProvider !== undefined && pricing.isFetching && !pricing.data) ? (
                  <span className="text-xs text-neutral-500">…</span>
                ) : estimate !== null ? (
                  <span className="text-base font-semibold text-neutral-900">${estimate}</span>
                ) : (
                  <span className="text-xs text-amber-700">
                    unknown (no pricing entered for this model) —{" "}
                    <Link to="/providers" className="font-medium underline">
                      Add pricing →
                    </Link>
                  </span>
                )}
              </p>
              {preview.data.mask_blockers > 0 && (
                <p className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
                  {preview.data.mask_blockers} page
                  {preview.data.mask_blockers === 1 ? " lacks" : "s lack"} an accepted mask —
                  the run will fail the mask gate.{" "}
                  <Link
                    to={`/assessments/${assessmentId}?tab=masking`}
                    className="font-medium underline"
                  >
                    Review masks →
                  </Link>
                </p>
              )}
              {/* B9-UI: per-problem guaranteed-failure reasons (task 5's blockers[] —
                  missing rubric, missing reference solution). Distinct from the advisory
                  warnings below: any entry here disables Launch outright (see `valid`). */}
              {previewBlockers.length > 0 && (
                <div className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
                  <p className="font-medium">
                    This launch is guaranteed to fail — fix{" "}
                    {previewBlockers.length === 1 ? "this first" : "these first"}:
                  </p>
                  <ul className="mt-1 list-inside list-disc space-y-0.5">
                    {previewBlockers.map((b, i) => (
                      <li key={`${b.code}-${b.problem_id ?? i}`}>{b.message}</li>
                    ))}
                  </ul>
                </div>
              )}
              {/* Launch-scoped hazard warnings (workflow guards): one notice per code.
                  Only provider_disabled blocks (see providerOk above). */}
              {previewWarnings.map((w) => {
                const view = warningView(w, assessmentId);
                return (
                  <WorkflowNotice key={w.code} tone={view.tone} to={view.to}>
                    {view.message}
                  </WorkflowNotice>
                );
              })}
            </div>
          ))}

        {budgetError ? (
          <p className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
            {budgetError.error} — month to date ${budgetError.month_to_date} + estimate $
            {budgetError.estimate} would exceed the ${budgetError.budget} monthly budget.
          </p>
        ) : (
          launch.isError && <p className="text-xs text-red-600">{launch.error.message}</p>
        )}
        <div className="flex justify-end gap-2 pt-1">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={launch.isPending || !valid}>
            {launch.isPending ? "Launching…" : "Launch"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
