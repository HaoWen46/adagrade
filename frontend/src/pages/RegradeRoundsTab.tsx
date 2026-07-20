// Regrade rounds tab (rounds design): the exam-scoped cockpit for regrade
// turns — the reply deadline, one method per round (usually a single strict
// model; a consensus here would manufacture new conflicts), per-round work
// counts, and the manual batch-grade button. Requests trickle in by email
// asynchronously, so this is a standing dashboard of three buckets that fill
// over time; the global Regrades page stays the per-request adjudication inbox.

import { useEffect, useState } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import { roleAtLeast, useMe } from "../lib/auth";
import type { Method, RegradeRound, RegradeRoundsResponse } from "../lib/types";
import { fmtDate } from "../lib/format";
import { Badge, Button, Card, Input, Select, Spinner, TD, TH, Table } from "../components/ui";

// In-flight grading poll cadence + backstop window (FIX 1). Vision records land
// off-request MINUTES after enqueue, so the real completion signal is the round's
// pending count actually dropping (handled below) — the indicator stays up until then.
// The backstop is a generous 10 min so it only ever fires when something is genuinely
// wrong; when it does, we don't silently drop the indicator (that reads as failure and
// re-enables the button mid-flight, inviting a double-spend) — we switch to an honest
// "still processing — refresh to check" state that keeps the double-click guard.
const INFLIGHT_POLL_MS = 4000;
const INFLIGHT_BACKSTOP_MS = 600_000;

export function RegradeRoundsTab({ assessmentId }: { assessmentId: string }) {
  const me = useMe();
  const canConfigure = roleAtLeast(me.data?.user.role, "lecturer");

  const rounds = useQuery({
    queryKey: ["regrade-rounds", assessmentId],
    queryFn: () => api.get<RegradeRoundsResponse>(`/api/assessments/${assessmentId}/regrade-rounds`),
  });
  const methods = useQuery({
    queryKey: ["methods", false],
    queryFn: () => api.get<{ methods: Method[] }>("/api/methods"),
  });

  if (rounds.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (rounds.isError) {
    return (
      <Card>
        <p className="text-sm text-red-600">{rounds.error.message}</p>
      </Card>
    );
  }

  const data = rounds.data;
  const usable = (methods.data?.methods ?? []).filter((m) => !m.archived && m.latest !== undefined);

  return (
    <div className="space-y-4">
      <DeadlineCard assessmentId={assessmentId} data={data} canConfigure={canConfigure} />

      <Card
        title="Turns"
        actions={
          <Link to="/regrades" className="text-xs text-indigo-600 hover:underline">
            Open the request inbox →
          </Link>
        }
      >
        <p className="mb-3 text-xs text-neutral-500">
          Each email turn gets its own method (usually one strict model) to grade that
          turn&apos;s contested problems, then you adjudicate each request in the inbox.
          Requests arrive asynchronously — grade a turn whenever its pending bucket has work.
          Withdrawn (停修) students keep their regrade channel: their requests stay in these
          buckets and carry an amber <em>withdrawn</em> badge in the inbox.
        </p>
        <Table>
          <thead>
            <tr>
              <TH className="w-20">Turn</TH>
              <TH>Method</TH>
              <TH className="w-24 text-right">Pending</TH>
              <TH className="w-32 text-right">Awaiting verdict</TH>
              <TH className="w-28 text-right">Adjudicated</TH>
              <TH className="w-44" />
            </tr>
          </thead>
          <tbody>
            {data.rounds.map((round) => (
              <RoundRow
                key={round.turn}
                assessmentId={assessmentId}
                round={round}
                methods={usable}
                canConfigure={canConfigure}
              />
            ))}
          </tbody>
        </Table>
      </Card>
    </div>
  );
}

// --- deadline ---------------------------------------------------------------------------

function DeadlineCard({
  assessmentId,
  data,
  canConfigure,
}: {
  assessmentId: string;
  data: RegradeRoundsResponse;
  canConfigure: boolean;
}) {
  const queryClient = useQueryClient();
  // datetime-local wants "YYYY-MM-DDTHH:mm" in local time.
  const toLocal = (iso?: string) => {
    if (!iso) return "";
    const d = new Date(iso);
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
  };
  const [value, setValue] = useState(toLocal(data.regrade_deadline));

  const save = useMutation({
    mutationFn: (deadline: string | null) =>
      api.put(`/api/assessments/${assessmentId}/regrade-deadline`, {
        deadline: deadline === null ? null : new Date(deadline).toISOString(),
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["regrade-rounds", assessmentId] });
      await queryClient.invalidateQueries({ queryKey: ["assessment", assessmentId] });
    },
  });

  return (
    <Card
      title="Regrade deadline"
      actions={
        data.regrade_deadline ? (
          data.deadline_passed ? (
            <Badge tone="red">closed — replies are rejected</Badge>
          ) : (
            <Badge tone="green">open until {fmtDate(data.regrade_deadline)}</Badge>
          )
        ) : (
          <Badge tone="amber">no deadline — replies accepted indefinitely</Badge>
        )
      }
    >
      <div className="flex flex-wrap items-center gap-2">
        <Input
          type="datetime-local"
          className="w-60"
          value={value}
          disabled={!canConfigure}
          onChange={(e) => setValue(e.target.value)}
        />
        {canConfigure && (
          <>
            <Button
              className="px-2.5 py-1 text-xs"
              disabled={value === "" || save.isPending}
              onClick={() => save.mutate(value)}
            >
              Set deadline
            </Button>
            {data.regrade_deadline && (
              <Button
                variant="secondary"
                className="px-2.5 py-1 text-xs"
                disabled={save.isPending}
                onClick={() => {
                  setValue("");
                  save.mutate(null);
                }}
              >
                Clear
              </Button>
            )}
          </>
        )}
      </div>
      <p className="mt-2 text-xs text-neutral-500">
        Replies received after the deadline are recorded in the inbox as rejected — no turn is
        burned, nothing files. Different exams close on different dates.
      </p>
      {save.isError && <p className="mt-2 text-xs text-red-600">{save.error.message}</p>}
    </Card>
  );
}

// --- one round row ------------------------------------------------------------------------

function RoundRow({
  assessmentId,
  round,
  methods,
  canConfigure,
}: {
  assessmentId: string;
  round: RegradeRound;
  methods: Method[];
  canConfigure: boolean;
}) {
  const queryClient = useQueryClient();
  const [estimate, setEstimate] = useState<string | null>(null);
  // In-flight grading state (FIX 1): the batch enqueue returns immediately, but the AI
  // records land minutes later off-request. Until then round.pending stays put, so the
  // plain refetch re-shows the same red "Pending N" and the same enabled button — and TAs
  // re-click and double-spend. After a confirmed enqueue we hold this marker, swap the
  // button for a "grading in progress" indicator, and poll the round until pending drops
  // (records landed) or the backstop window elapses. `pendingAt` is the count captured at
  // enqueue time; `count` is how many are being worked on (the enqueued count, or the
  // pending count when the backend deduped the enqueue to 0 — idempotency must still not
  // look like the click was ignored). `stalled` flips at the backstop: we keep the marker
  // (and thus the double-click guard) but stop auto-polling and ask for a manual refresh.
  const [inFlight, setInFlight] = useState<{
    count: number;
    pendingAt: number;
    startedAt: number;
    stalled: boolean;
  } | null>(null);

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["regrade-rounds", assessmentId] });

  // Poll while in flight; at the backstop, flip to `stalled` (an honest "still
  // processing" state) instead of dropping the marker — that keeps the button guarded
  // and stops hammering the server, leaving the user a manual refresh.
  useEffect(() => {
    if (!inFlight || inFlight.stalled) return;
    const timer = setInterval(() => {
      if (Date.now() - inFlight.startedAt > INFLIGHT_BACKSTOP_MS) {
        setInFlight((f) => (f ? { ...f, stalled: true } : f));
        return;
      }
      void queryClient.invalidateQueries({ queryKey: ["regrade-rounds", assessmentId] });
    }, INFLIGHT_POLL_MS);
    return () => clearInterval(timer);
  }, [inFlight, queryClient, assessmentId]);

  // Records landed: pending actually dropped below the enqueue-time count — clear the
  // marker so the row returns to its normal (now-smaller) state.
  useEffect(() => {
    if (inFlight && round.pending < inFlight.pendingAt) setInFlight(null);
  }, [round.pending, inFlight]);

  const setMethod = useMutation({
    mutationFn: (methodID: number) =>
      api.put(`/api/assessments/${assessmentId}/regrade-rounds/${round.turn}`, { method_id: methodID }),
    onSuccess: invalidate,
  });

  // Two-step batch: a dry run first surfaces the cost estimate, then confirm.
  const grade = useMutation({
    mutationFn: (dryRun: boolean) =>
      api.post<{ enqueued: number; estimated_cost: string }>(
        `/api/assessments/${assessmentId}/regrade-rounds/${round.turn}/grade`,
        { dry_run: dryRun },
      ),
    onSuccess: async (res, dryRun) => {
      if (dryRun) {
        setEstimate(res.estimated_cost || "unknown");
      } else {
        setEstimate(null);
        setInFlight({
          count: res.enqueued > 0 ? res.enqueued : round.pending,
          pendingAt: round.pending,
          startedAt: Date.now(),
          stalled: false,
        });
        await invalidate();
      }
    },
  });

  return (
    <tr>
      <TD className="font-medium tabular-nums">turn {round.turn}</TD>
      <TD>
        <div className="flex items-center gap-2">
          <Select
            value={round.method_id !== undefined ? String(round.method_id) : ""}
            disabled={!canConfigure || round.locked || setMethod.isPending}
            onChange={(e) => setMethod.mutate(Number(e.target.value))}
            className="w-64"
          >
            <option value="" disabled>
              choose a method…
            </option>
            {methods.map((m) => (
              <option key={m.id} value={String(m.id)}>
                {m.name} ({m.latest?.config.provider}/{m.latest?.config.model})
              </option>
            ))}
          </Select>
          {round.locked && (
            <Badge tone="neutral" className="whitespace-nowrap">
              frozen — turn has grades
            </Badge>
          )}
        </div>
        {setMethod.isError && <p className="mt-1 text-xs text-red-600">{setMethod.error.message}</p>}
        {grade.isError && <p className="mt-1 text-xs text-red-600">{grade.error.message}</p>}
      </TD>
      <TD className={`text-right tabular-nums ${round.pending > 0 ? "font-semibold text-red-600" : ""}`}>
        {round.pending}
      </TD>
      <TD className={`text-right tabular-nums ${round.graded > 0 ? "font-semibold text-amber-600" : ""}`}>
        {round.graded}
      </TD>
      <TD className="text-right tabular-nums">{round.adjudicated}</TD>
      <TD>
        {inFlight ? (
          inFlight.stalled ? (
            // Backstop hit (10 min) without records landing: don't disappear (that
            // reads as failure) and don't re-enable the button (double-spend risk) —
            // stay honest and offer a manual re-check. If pending has since dropped,
            // the effect below clears the marker and the row returns to normal.
            <span className="flex items-center gap-1.5 whitespace-nowrap text-xs text-amber-600">
              still processing —
              <Button
                variant="ghost"
                className="px-1.5 py-0.5 text-xs"
                onClick={() => void invalidate()}
              >
                refresh to check
              </Button>
            </span>
          ) : (
            <span className="flex items-center gap-1.5 whitespace-nowrap text-xs text-neutral-500">
              <Spinner className="size-3.5" />
              {inFlight.count} grading in progress…
            </span>
          )
        ) : estimate === null ? (
          <Button
            variant="secondary"
            className="px-2.5 py-1 text-xs whitespace-nowrap"
            disabled={round.pending === 0 || round.method_id === undefined || grade.isPending}
            title={
              round.method_id === undefined
                ? "Choose this turn's method first"
                : round.pending === 0
                  ? "Nothing pending in this turn"
                  : undefined
            }
            onClick={() => grade.mutate(true)}
          >
            Grade pending ({round.pending})
          </Button>
        ) : (
          <span className="flex items-center gap-1.5 whitespace-nowrap">
            <span className="text-xs text-neutral-600 tabular-nums">
              ~{estimate === "unknown" ? "unknown cost" : `$${estimate}`}
            </span>
            <Button
              className="px-2 py-1 text-xs"
              disabled={grade.isPending}
              onClick={() => grade.mutate(false)}
            >
              Confirm
            </Button>
            <Button variant="ghost" className="px-2 py-1 text-xs" onClick={() => setEstimate(null)}>
              Cancel
            </Button>
          </span>
        )}
      </TD>
    </tr>
  );
}
