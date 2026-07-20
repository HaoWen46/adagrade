// Publish tab: coverage preview, publish (with a typed-name confirm since this sends
// real student email), batch history with per-item statuses + resend-failed, and
// admin-only unpublish (also typed-name confirm — it reopens grading).
//
// Student names/emails render here (preview skip/changed lists, batch item rows) —
// never console.log them (CLAUDE.md). Points/money are display-only decimal strings.

import { useRef, useState, type FormEvent } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../lib/api";
import { roleAtLeast, useMe } from "../lib/auth";
import type {
  AggregationPolicyResponse,
  MaterializeAnswersResult,
  PublishAlreadyPublishedError,
  PublishAttachment,
  PublishBatch,
  PublishBatchesResponse,
  PublishBlockerRow,
  PublishCoverageGateError,
  PublishEmailStatus,
  PublishNothingToPublishError,
  PublishPreview,
  PublishResult,
  PublishStudentRef,
  ResendFailedResult,
  ResendItemResult,
  RunListRow,
  UnassignedTAProblem,
  UnpublishResult,
} from "../lib/types";
import { HelpTip } from "../components/HelpTip";
import { WorkflowNotice } from "../components/WorkflowNotice";
import { warningView } from "../lib/warnings";
import { publishCoverageHelp } from "../lib/helpContent";
import { answerStatusLabel } from "../lib/labels";
import { fmtDate } from "../lib/format";
import { ScoreDistribution } from "../components/ScoreDistribution";
import type { ProblemSummary, ScoreDistributionResponse } from "../lib/types";
import {
  Badge,
  Button,
  Card,
  Dialog,
  Field,
  IconButton,
  Input,
  Select,
  Spinner,
  TD,
  TH,
  Table,
  Textarea,
  cx,
} from "../components/ui";
import { Send } from "../components/icons";

const emailStatusTone: Record<PublishEmailStatus, "neutral" | "green" | "red" | "amber"> = {
  pending: "neutral",
  claimed: "amber",
  sending: "amber",
  sent: "green",
  failed: "red",
  uncertain: "red",
  skipped: "amber",
};

const emailStatusLabel: Record<PublishEmailStatus, string> = {
  pending: "pending",
  claimed: "claimed",
  sending: "sending",
  sent: "sent",
  failed: "failed",
  uncertain: "uncertain — duplicate risk",
  skipped: "skipped",
};

function emailDeliveryInFlight(status: PublishEmailStatus): boolean {
  return status === "pending" || status === "claimed" || status === "sending";
}

export function PublishTab({
  assessmentId,
  assessmentName,
  onGoToReview,
}: {
  assessmentId: string;
  assessmentName: string;
  onGoToReview: () => void;
}) {
  const me = useMe();
  const canPublish = roleAtLeast(me.data?.user.role, "lecturer");
  const canUnpublish = roleAtLeast(me.data?.user.role, "admin");
  const [publishOpen, setPublishOpen] = useState(false);
  const [unpublishOpen, setUnpublishOpen] = useState(false);
  const [lastWarning, setLastWarning] = useState<string | null>(null);

  const preview = useQuery({
    queryKey: ["publish-preview", assessmentId],
    queryFn: () => api.get<PublishPreview>(`/api/assessments/${assessmentId}/publish/preview`),
  });

  if (preview.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (preview.isError) {
    // B1: a TA hitting the lecturer/admin-gated preview endpoint used to see a bare
    // "403 Forbidden" line indistinguishable from a real failure — call out the role
    // gate explicitly and point them at what they CAN do (review grades) instead of
    // leaving a blank-feeling tab.
    if (preview.error instanceof ApiError && preview.error.status === 403) {
      return (
        <Card title="Publish">
          <div className="rounded-md border border-neutral-200 bg-neutral-50 px-3 py-3 text-sm text-neutral-600">
            Publishing is lecturer/admin-only — ask them to publish; you can still review grades on
            the{" "}
            <button
              type="button"
              onClick={onGoToReview}
              className="text-indigo-600 hover:underline"
            >
              Review tab
            </button>
            .
          </div>
        </Card>
      );
    }
    return (
      <Card>
        <p className="text-sm text-red-600">{preview.error.message}</p>
      </Card>
    );
  }

  const pv = preview.data;

  return (
    <div className="space-y-4">
      {lastWarning && (
        <div className="rounded-md border border-red-300 bg-red-50 px-3 py-2 text-sm font-semibold text-red-800">
          {lastWarning}
        </div>
      )}
      <FinalSourceCard assessmentId={assessmentId} pv={pv} canChoose={canPublish} />
      <PreviewCard
        assessmentId={assessmentId}
        pv={pv}
        canPublish={canPublish}
        canUnpublish={canUnpublish}
        onPublish={() => setPublishOpen(true)}
        onUnpublish={() => setUnpublishOpen(true)}
        onGoToReview={onGoToReview}
      />
      <BatchHistory assessmentId={assessmentId} emailDisabled={pv.email_disabled} />

      {publishOpen && (
        <PublishDialog
          assessmentId={assessmentId}
          assessmentName={assessmentName}
          pv={pv}
          onClose={() => setPublishOpen(false)}
          onPublished={(warning) => setLastWarning(warning ?? null)}
        />
      )}
      {unpublishOpen && (
        <UnpublishDialog
          assessmentId={assessmentId}
          assessmentName={assessmentName}
          onClose={() => setUnpublishOpen(false)}
        />
      )}
    </div>
  );
}

// --- final grading source (0027) -------------------------------------------------------

// FinalSourceCard is where the exam commits to its ONE grading source (a single
// immutable completed run or the consensus). Officials derive from this choice; the red
// unresolved list below reflects whatever the source leaves undecided.
//
// Picker honesty (analysis-redesign plan 2026-07-11, F2): options are annotated from
// the analysis stats rollup — a method with model records shows how many answers it
// graded, a record-less method is disabled with the reason (choosing it would make
// nothing official), and consensus is disabled until the Consensus tab panel exists.
// Annotations degrade to plain labels while those queries load (or fail) so the
// picker itself never blocks on them.
function FinalSourceCard({
  assessmentId,
  pv,
  canChoose,
}: {
  assessmentId: string;
  pv: PublishPreview;
  canChoose: boolean;
}) {
  const queryClient = useQueryClient();
  const current = pv.final_source;
  // Select encoding: "" (unset) | "consensus" | "run:<id>".
  const currentValue = !current
    ? ""
    : current.kind === "consensus"
      ? "consensus"
      : `run:${current.run_id}`;
  const [value, setValue] = useState(currentValue);

  const runs = useQuery({
    queryKey: ["runs", "final-source", assessmentId],
    queryFn: () =>
      api.get<{ runs: RunListRow[] }>(
        `/api/runs?assessment_id=${assessmentId}&status=completed&limit=200`,
      ),
  });
  const completedRuns = runs.data?.runs ?? [];
  const currentRunMissing =
    current?.kind === "method" &&
    current.run_id !== undefined &&
    !completedRuns.some((run) => run.id === current.run_id);

  // Consensus needs a configured panel before it can decide anything. A non-null
  // policy is enough to enable it (D17: an n=1 panel is legal) — whether the combine
  // step has RUN yet isn't cheaply inferable here, so no "not run yet" annotation.
  const aggregation = useQuery({
    queryKey: ["aggregation-policy", assessmentId],
    queryFn: () =>
      api.get<AggregationPolicyResponse>(`/api/assessments/${assessmentId}/aggregation`),
  });
  const panelMissing = aggregation.isSuccess && aggregation.data.policy === null;

  const save = useMutation({
    mutationFn: () => {
      const body =
        value === ""
          ? { kind: null }
          : value === "consensus"
            ? { kind: "consensus" }
            : { kind: "method", run_id: Number(value.slice("run:".length)) };
      return api.put<{ officials_moved: number }>(
        `/api/assessments/${assessmentId}/final-source`,
        body,
      );
    },
    onSuccess: async () => {
      // The derivation just rewrote officials — refresh everything that shows them.
      await queryClient.invalidateQueries({ queryKey: ["publish-preview", assessmentId] });
      await queryClient.invalidateQueries({ queryKey: ["problem-summaries"] });
      await queryClient.invalidateQueries({ queryKey: ["problem-students"] });
      await queryClient.invalidateQueries({ queryKey: ["assessment", assessmentId] });
      await queryClient.invalidateQueries({ queryKey: ["assessment-totals"] });
      // Re-pointing the source can fire/clear the final_source_no_records standing
      // warning (analysis-redesign plan, B1) — refresh it everywhere it renders.
      await queryClient.invalidateQueries({ queryKey: ["workflow-warnings", assessmentId] });
    },
  });

  const dirty = value !== currentValue;
  const gate = current?.kind === "method" && !current.spot_check_open;

  return (
    <Card
      title="Final grading source"
      actions={
        canChoose ? (
          <Button
            className="px-2.5 py-1 text-xs"
            disabled={!dirty || save.isPending}
            onClick={() => save.mutate()}
          >
            {save.isPending ? "Applying…" : "Apply"}
          </Button>
        ) : undefined
      }
    >
      <div className="space-y-2.5">
        <p className="text-xs text-neutral-500">
          The selected source grades the whole exam: either one immutable completed run or the
          configured consensus. Manual grades only ever fill what that source leaves undecided —
          later runs cannot silently replace a pinned run.
        </p>
        <Select
          value={value}
          disabled={!canChoose}
          onChange={(e) => setValue(e.target.value)}
          className="w-full max-w-md"
        >
          <option value="">— not chosen (nothing is official) —</option>
          {currentRunMissing && current?.kind === "method" && current.run_id !== undefined && (
            <option value={`run:${current.run_id}`}>
              {`run #${current.run_id} — ${current.method_name ?? "selected method"} v${current.method_version ?? "?"} (selected)`}
            </option>
          )}
          {completedRuns.map((run) => {
            // A3/A4 (task-5-report.md): the server now 422s pinning either shape as the
            // final source — disable the option here with the same reason rather than
            // let the operator pick it and hit a 422 on Apply. Scope is checked first: a
            // problem-/answer-scoped run is unpinnable regardless of how many items it
            // succeeded on, so that reason wins when both apply.
            const wrongScope = run.scope_kind !== "assessment";
            const noSucceeded = (run.counts.succeeded ?? 0) === 0;
            const disabledReason = wrongScope
              ? " — problem-scoped; only assessment-wide runs can be pinned"
              : noSucceeded
                ? " — graded nothing"
                : "";
            return (
              <option key={run.id} value={`run:${run.id}`} disabled={wrongScope || noSucceeded}>
                {`run #${run.id} — ${run.method_name} v${run.method_version} — ${run.scope_kind} — ${run.counts.succeeded ?? 0} succeeded${disabledReason}`}
              </option>
            );
          })}
          {runs.isSuccess && completedRuns.length === 0 && !currentRunMissing && (
            <option disabled>complete an AI grading run first</option>
          )}
          {panelMissing ? (
            <option value="consensus" disabled>
              consensus — set up the panel first
            </option>
          ) : (
            <option value="consensus">consensus — combine methods per the Consensus tab</option>
          )}
        </Select>
        {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}
        {!current && (
          <p className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700 ring-1 ring-red-200 ring-inset">
            No source chosen — no grade is official and publishing is blocked.
          </p>
        )}
        {gate && current && (
          <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
            Spot-check pending for selected run #{current.run_id} ({current.method_name ?? "method"}):{" "}
            <span className="tabular-nums">
              {current.spot_check_done ?? 0} of {current.spot_check_total ?? 0}
            </span>{" "}
            reviewed. Finish the sample on the{" "}
            <Link
              to={`/runs?assessment_id=${assessmentId}`}
              className="text-indigo-700 underline"
            >
              Runs page
            </Link>{" "}
            (or an admin can waive it) before publishing.
          </p>
        )}
      </div>
    </Card>
  );
}

// --- preview card --------------------------------------------------------------------

function PreviewCard({
  assessmentId,
  pv,
  canPublish,
  canUnpublish,
  onPublish,
  onUnpublish,
  onGoToReview,
}: {
  assessmentId: string;
  pv: PublishPreview;
  canPublish: boolean;
  canUnpublish: boolean;
  onPublish: () => void;
  onUnpublish: () => void;
  onGoToReview: () => void;
}) {
  const coveredPct =
    pv.total_answers > 0 ? Math.round(((pv.graded + pv.no_submission) / pv.total_answers) * 100) : 0;
  const changed = pv.changed ?? [];
  const skipped = pv.skipped ?? [];
  // Publish-scoped hazard warnings (workflow guards): rendered up top so they sit
  // right under the header's Publish button; warning-or-worse also forces an
  // acknowledgment checkbox in the publish dialog.
  const warnings = pv.warnings ?? [];
  const skippedWarning = warnings.some((w) => w.code === "skipped_students");

  return (
    <Card
      title="Publish preview"
      actions={
        <div className="flex gap-2">
          {canUnpublish && pv.has_live_batch && (
            <Button variant="danger" className="px-2.5 py-1.5 text-xs" onClick={onUnpublish}>
              Unpublish
            </Button>
          )}
          {canPublish && !pv.has_live_batch && (
            <Button
              className="px-2.5 py-1.5 text-xs"
              disabled={!pv.publishable}
              onClick={onPublish}
              title={
                pv.publishable
                  ? undefined
                  : !pv.final_source
                    ? "Choose the final grading source first"
                    : "Resolve the unresolved list (and spot-check, for method sources) first"
              }
            >
              Publish
            </Button>
          )}
        </div>
      }
    >
      <div className="space-y-4">
        {warnings.length > 0 && (
          <div className="space-y-1.5">
            {warnings.map((w) => {
              const view = warningView(w, assessmentId);
              return (
                <WorkflowNotice key={w.code} tone={view.tone} to={view.to}>
                  {view.message}
                </WorkflowNotice>
              );
            })}
          </div>
        )}

        <div className="flex flex-wrap items-center gap-2">
          <Badge tone={pv.publishable ? "green" : !pv.final_source ? "red" : "amber"}>
            {pv.publishable
              ? "Ready to publish"
              : !pv.final_source
                ? "No grading source chosen"
                : pv.blocked === 0 &&
                    pv.not_ingested === 0 &&
                    pv.final_source.kind === "method" &&
                    !pv.final_source.spot_check_open
                  ? "Spot-check pending"
                  : "Unresolved answers remain"}
          </Badge>
          <HelpTip title="Coverage gate">{publishCoverageHelp}</HelpTip>
          {pv.has_live_batch && <Badge tone="indigo">Already published — live batch exists</Badge>}
          {pv.email_disabled && (
            <Badge tone="red" className="font-semibold">
              Email provider is "none" — no student email will send
            </Badge>
          )}
        </div>

        {pv.email_disabled && (
          <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800">
            <strong>Warning:</strong> the configured email provider is <code>none</code>. Publishing
            will record every item as <em>skipped</em> and send nothing. Configure a real provider
            before publishing if students should receive results by email.
          </div>
        )}

        {/* Before a final source is chosen, EVERY answer is "unresolved" (nothing is
            official yet) — the coverage grid and the unresolved list would then scream
            BLOCKED-for-everyone and push TAs toward hand-grading the whole exam. Until a
            source is picked, show one calm callout instead of those scary zeros; the
            grid/blockers return the moment a source is set and the numbers mean something. */}
        {!pv.final_source ? (
          <div className="rounded-md border border-neutral-200 bg-neutral-50 px-3 py-3 text-sm text-neutral-600">
            No source chosen yet — pick one above. Manual grading is only for what the source
            leaves undecided, so there is nothing to resolve here until a source is set.
          </div>
        ) : (
          <>
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
              <Stat label="Coverage" value={`${coveredPct}%`} />
              <Stat label="Graded" value={String(pv.graded)} />
              <Stat label="No submission" value={String(pv.no_submission)} />
              <Stat label="Blocked" value={String(pv.blocked)} tone={pv.blocked > 0 ? "red" : undefined} />
              <Stat
                label="Not ingested"
                value={String(pv.not_ingested)}
                tone={pv.not_ingested > 0 ? "red" : undefined}
              />
            </div>

            {/* B3-copy: these two messages used to always render together, contradicting
                each other (re-publish guidance for a state where publish is actually
                blocked). A live batch means publish is refused outright, so only the
                unpublish-first guidance applies; the changed-students sentence belongs to
                the OTHER state — no live batch, but a previous publish exists — where a
                re-publish is actually possible. */}
            {pv.has_live_batch ? (
              <div className="space-y-1.5 rounded-md border border-indigo-200 bg-indigo-50 px-3 py-2 text-xs text-indigo-900">
                <p>
                  A live batch already exists — to re-publish corrected results,{" "}
                  <strong>{canUnpublish ? "Unpublish first" : "ask an admin to unpublish first"}</strong>,
                  fix the grades, then publish again. Publishing directly while a batch is live is
                  refused.
                </p>
                {pv.not_ingested > 0 && (
                  <p>
                    The live batch does not cover the {pv.not_ingested} unresolved student
                    {pv.not_ingested === 1 ? "" : "s"} listed below.
                  </p>
                )}
              </div>
            ) : (
              pv.ever_published && (
                <div className="rounded-md border border-indigo-200 bg-indigo-50 px-3 py-2 text-xs text-indigo-900">
                  <p>
                    {changed.length} student{changed.length === 1 ? "" : "s"} changed since the last
                    publish. A re-publish sends only to changed students unless you choose "resend to
                    everyone".
                  </p>
                </div>
              )
            )}

            {pv.blockers.length > 0 && (
              <BlockersList
                assessmentId={assessmentId}
                blockers={pv.blockers}
                canMaterialize={canPublish}
                onGoToReview={onGoToReview}
              />
            )}

            {pv.unassigned_ta_problems.length > 0 && (
              <UnassignedTAWarning problems={pv.unassigned_ta_problems} />
            )}

            {skipped.length > 0 && (
              <StudentRefList
                title="Skipped (no submission — no email)"
                refs={skipped}
                highlight={skippedWarning}
                explanation={
                  skippedWarning
                    ? "These students get NO email — if one of them actually submitted, their pages are stranded in intake."
                    : undefined
                }
              />
            )}
            {pv.ever_published && changed.length > 0 && (
              <StudentRefList title="Changed since last publish" refs={changed} />
            )}
          </>
        )}

        <ProblemDistributions assessmentId={assessmentId} />
      </div>
    </Card>
  );
}

function Stat({ label, value, tone }: { label: string; value: string; tone?: "red" }) {
  return (
    <div>
      <div className="text-[10px] font-medium tracking-wide text-neutral-400 uppercase">{label}</div>
      <div className={cx("text-lg font-semibold tabular-nums", tone === "red" ? "text-red-600" : "text-neutral-900")}>
        {value}
      </div>
    </div>
  );
}

function BlockersList({
  assessmentId,
  blockers,
  canMaterialize,
  onGoToReview,
}: {
  assessmentId: string;
  blockers: PublishBlockerRow[];
  /** lecturer+ — POST .../materialize-answers is lecturer-gated server-side. */
  canMaterialize: boolean;
  onGoToReview: () => void;
}) {
  const queryClient = useQueryClient();
  // Materialize action (roster-lifecycle plan 2026-07-10): the not_ingested dead end —
  // a student added to the roster AFTER the upload has zero answer rows, so no page can
  // ever be attached to them. This creates the missing (empty) answer rows so their
  // work can be uploaded/assigned; it never touches existing rows (idempotent).
  const notIngestedRows = Array.from(
    new Map(
      blockers
        .filter((b) => b.kind === "not_ingested")
        .map((b) => [b.student_external_id, { external_id: b.student_external_id, name: b.student_name }]),
    ).values(),
  );
  const notIngestedStudents = notIngestedRows.length;
  const [confirmOpen, setConfirmOpen] = useState(false);
  const materialize = useMutation({
    mutationFn: () =>
      api.post<MaterializeAnswersResult>(`/api/assessments/${assessmentId}/materialize-answers`),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["publish-preview", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["workflow-warnings", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["problem-summaries", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["ingest-report", assessmentId] }),
      ]);
      setConfirmOpen(false);
    },
  });

  return (
    <div>
      <div className="mb-1.5 flex items-center justify-between gap-3">
        <h3 className="text-xs font-semibold text-red-700">
          Unresolved — no grade under the chosen source ({blockers.length})
        </h3>
        <div className="flex items-center gap-3">
          {canMaterialize && notIngestedStudents > 0 && (
            <Button
              variant="secondary"
              className="px-2 py-1 text-xs"
              disabled={materialize.isPending}
              title="Creates the missing answer rows for roster students who have none — then upload or assign their pages as usual"
              onClick={() => setConfirmOpen(true)}
            >
              {materialize.isPending
                ? "Creating…"
                : `Create answer rows for ${notIngestedStudents} student${notIngestedStudents === 1 ? "" : "s"}`}
            </Button>
          )}
          {blockers.some((b) => b.kind !== "not_ingested") && (
            <button
              type="button"
              onClick={onGoToReview}
              className="text-xs text-indigo-600 hover:underline"
            >
              Go to Review →
            </button>
          )}
        </div>
      </div>
      {materialize.isError && (
        <p className="mb-1.5 text-xs text-red-600">{materialize.error.message}</p>
      )}
      {/* B13: this guidance used to repeat in every not-ingested row's PROBLEM cell — one
          line above the table instead; the KIND column already carries the per-row label. */}
      {notIngestedStudents > 0 && (
        // Plain anchors: tabs are URL-driven (?tab=), so a same-page href works.
        <p className="mb-1.5 text-xs text-neutral-500">
          Not in any upload — add pages via{" "}
          <a href="?tab=submissions" className="text-indigo-600 hover:underline">
            Submissions
          </a>{" "}
          or{" "}
          <a href="?tab=identify" className="text-indigo-600 hover:underline">
            Identify
          </a>
          .
        </p>
      )}
      <Table>
        <thead>
          <tr>
            <TH>Student</TH>
            <TH>Problem</TH>
            <TH>Kind</TH>
          </tr>
        </thead>
        <tbody>
          {blockers.map((b) => (
            <tr key={`${b.student_external_id}-${b.kind}-${b.problem_number}`}>
              <TD>
                {b.student_name}{" "}
                <span className="text-neutral-400">({b.student_external_id})</span>
              </TD>
              <TD>
                {b.kind === "not_ingested" ? (
                  <span className="text-neutral-400">—</span>
                ) : (
                  <Link
                    to={`/answers/${b.answer_id}`}
                    className="text-indigo-600 hover:underline"
                  >
                    #{b.problem_number} {b.problem_title} — grade manually →
                  </Link>
                )}
              </TD>
              <TD>
                <Badge tone="red">
                  {b.kind === "not_ingested" ? answerStatusLabel(b.kind) : "unresolved"}
                </Badge>
              </TD>
            </tr>
          ))}
        </tbody>
      </Table>

      {/* B14: materialize is a CROSS JOIN across every not-ingested student — one click
          used to create pageless answer rows for all of them at once with no preview.
          Naming names + the downstream consequence before the irreversible-feeling click
          gives the operator a last chance to notice a student who shouldn't be here. */}
      {confirmOpen && (
        <Dialog
          open
          onClose={() => setConfirmOpen(false)}
          title={`Create answer rows for ${notIngestedStudents} student${notIngestedStudents === 1 ? "" : "s"}?`}
        >
          <div className="space-y-3">
            <p className="text-sm text-neutral-600">
              This creates the missing answer rows for the students below so their pages can be
              uploaded or assigned. Students with no pages will publish as no-submission zeros and
              receive no email.
            </p>
            <div className="max-h-40 overflow-y-auto rounded-md border border-neutral-200">
              <ul className="divide-y divide-neutral-100 text-xs">
                {notIngestedRows.map((s) => (
                  <li key={s.external_id} className="flex justify-between px-2.5 py-1.5">
                    <span>{s.name}</span>
                    <span className="text-neutral-400">{s.external_id}</span>
                  </li>
                ))}
              </ul>
            </div>
            {materialize.isError && (
              <p className="text-xs text-red-600">{materialize.error.message}</p>
            )}
            <div className="flex justify-end gap-2 pt-1">
              <Button
                variant="secondary"
                onClick={() => setConfirmOpen(false)}
                disabled={materialize.isPending}
              >
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={materialize.isPending}
                onClick={() => materialize.mutate()}
              >
                {materialize.isPending
                  ? "Creating…"
                  : `Create answer rows for ${notIngestedStudents} student${notIngestedStudents === 1 ? "" : "s"}`}
              </Button>
            </div>
          </div>
        </Dialog>
      )}
    </div>
  );
}

/** Warn-only publish-preview banner (spec §6 D60): problems with no assigned TA. If a
 * student escalates one of these to the final regrade turn, the handoff email has no
 * recipient — this is the operator's chance to catch that before it happens. Never
 * blocks publish (no gating logic here, unlike BlockersList). */
function UnassignedTAWarning({ problems }: { problems: UnassignedTAProblem[] }) {
  const nums = problems
    .map((p) => p.problem_number)
    .sort((a, b) => a - b)
    .join(", ");
  return (
    <div className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
      Problem{problems.length === 1 ? "" : "s"} {nums} {problems.length === 1 ? "has" : "have"} no
      assigned TA — if a student escalates one of these to the final regrade turn, the handoff
      email has no recipient. Assign TAs on the problems editor.
    </div>
  );
}

function StudentRefList({
  title,
  refs,
  highlight,
  explanation,
}: {
  title: string;
  refs: PublishStudentRef[];
  /** Amber-elevate the list (workflow guards: skipped_students warning present). */
  highlight?: boolean;
  explanation?: string;
}) {
  return (
    <div>
      <h3 className={cx("mb-1.5 text-xs font-semibold", highlight ? "text-amber-800" : "text-neutral-700")}>
        {title} ({refs.length})
      </h3>
      {explanation && <p className="mb-1.5 text-xs text-amber-800">{explanation}</p>}
      <div
        className={cx(
          "max-h-40 overflow-y-auto rounded-md border",
          highlight ? "border-amber-300 ring-1 ring-amber-300 ring-inset" : "border-neutral-200",
        )}
      >
        <ul className="divide-y divide-neutral-100 text-xs">
          {refs.map((r) => (
            <li key={r.student_id} className="flex justify-between px-2.5 py-1.5">
              <span>{r.name}</span>
              <span className="text-neutral-400">{r.external_id}</span>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}

// --- per-problem score distributions --------------------------------------------------

function ProblemDistributions({ assessmentId }: { assessmentId: string }) {
  const summary = useQuery({
    queryKey: ["problem-summaries", assessmentId],
    queryFn: () =>
      api.get<{ problems: ProblemSummary[] }>(`/api/assessments/${assessmentId}/problems/summary`),
  });

  const problems = summary.data?.problems ?? [];
  if (problems.length === 0) return null;

  return (
    <div>
      <h3 className="mb-1.5 text-xs font-semibold text-neutral-700">Score distributions</h3>
      <div className="grid gap-3 sm:grid-cols-2">
        {problems.map((p) => (
          <div key={p.problem_id} className="rounded-md border border-neutral-200 p-2.5">
            <div className="mb-1.5 text-xs font-medium text-neutral-600">
              #{p.number} {p.title || <span className="text-neutral-400">untitled</span>}
            </div>
            <ProblemDistributionEmbed problemId={p.problem_id} />
          </div>
        ))}
      </div>
    </div>
  );
}

function ProblemDistributionEmbed({ problemId }: { problemId: number }) {
  const dist = useQuery({
    queryKey: ["score-distribution", problemId],
    queryFn: () => api.get<ScoreDistributionResponse>(`/api/problems/${problemId}/score-distribution`),
  });
  if (dist.isPending) {
    return (
      <div className="flex justify-center py-3">
        <Spinner className="size-4" />
      </div>
    );
  }
  if (dist.isError) {
    return <p className="text-xs text-red-600">{dist.error.message}</p>;
  }
  return <ScoreDistribution data={dist.data} />;
}

// --- publish dialog: note + resend-all + typed-name confirm -------------------------

function PublishDialog({
  assessmentId,
  assessmentName,
  pv,
  onClose,
  onPublished,
}: {
  assessmentId: string;
  assessmentName: string;
  pv: PublishPreview;
  onClose: () => void;
  onPublished: (warning?: string) => void;
}) {
  const queryClient = useQueryClient();
  const [note, setNote] = useState("");
  const [resendAll, setResendAll] = useState(false);
  const [confirmText, setConfirmText] = useState("");
  // Default to the "recommended" compressed report when attachments are available
  // (report font configured) — a lecturer publishing expects students to get their
  // annotated reports, and defaulting to "none" beside a "recommended" option sent
  // bare-email results that only an admin unpublish could recover (HCI audit).
  const [attachment, setAttachment] = useState<PublishAttachment>(
    pv.report_attachments_available ? "compressed" : "none",
  );
  const [zip, setZip] = useState(false);
  const [acked, setAcked] = useState(false);

  // Outstanding workflow warnings (severity ≥ warning) require an explicit
  // acknowledgment on top of the typed-name confirm — warn, don't block: the
  // backend gate is untouched, this is purely a "did you see this?" speed bump.
  const outstanding = (pv.warnings ?? []).filter((w) => w.severity !== "info");
  const needsAck = outstanding.length > 0;

  const skippedIds = new Set((pv.skipped ?? []).map((s) => s.external_id));
  const recipientCount =
    resendAll || !pv.ever_published
      ? pv.student_count - (pv.skipped?.length ?? 0)
      : (pv.changed ?? []).filter((c) => !skippedIds.has(c.external_id)).length;

  const publish = useMutation({
    mutationFn: () =>
      api.post<PublishResult>(`/api/assessments/${assessmentId}/publish`, {
        note,
        resend_all: resendAll,
        attachment,
        zip: attachment === "none" ? false : zip,
      }),
    onSuccess: async (res) => {
      await queryClient.invalidateQueries({ queryKey: ["publish-preview", assessmentId] });
      await queryClient.invalidateQueries({ queryKey: ["publish-batches", assessmentId] });
      await queryClient.invalidateQueries({ queryKey: ["assessment-totals", assessmentId] });
      onPublished(res.warning);
      onClose();
    },
  });

  const nameMatches = confirmText.trim() === assessmentName;
  const confirmed = nameMatches && (!needsAck || acked);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (confirmed) publish.mutate();
  };

  const errorInfo = publishErrorInfo(publish.error);

  return (
    <Dialog open onClose={onClose} title={pv.ever_published ? "Re-publish results" : "Publish results"}>
      <form className="space-y-3" onSubmit={submit}>
        <div className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          This will email <strong>{recipientCount}</strong> student{recipientCount === 1 ? "" : "s"}
          {pv.ever_published ? " a corrected-results email" : " their results"}.
          {pv.email_disabled && " (Email provider is \"none\" — nothing will actually send.)"}
        </div>

        {pv.ever_published && (
          <label className="flex items-center gap-2 text-sm text-neutral-700">
            <input
              type="checkbox"
              checked={resendAll}
              onChange={(e) => setResendAll(e.target.checked)}
              className="size-3.5 rounded border-neutral-300"
            />
            Resend to everyone (not just changed students)
          </label>
        )}

        {pv.from && (
          <p className="text-xs text-neutral-500">
            Students will receive mail from <strong>{pv.from}</strong>.
          </p>
        )}

        <AttachmentField
          attachment={attachment}
          onChange={(value) => {
            setAttachment(value);
            if (value === "none") setZip(false);
          }}
          zip={zip}
          onZipChange={setZip}
          available={pv.report_attachments_available}
        />

        <Field label="Note (optional, included in the batch record)">
          <Textarea rows={3} value={note} onChange={(e) => setNote(e.target.value)} />
        </Field>

        {needsAck && (
          <label className="flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
            <input
              type="checkbox"
              checked={acked}
              onChange={(e) => setAcked(e.target.checked)}
              className="mt-0.5 size-3.5 rounded border-neutral-300"
            />
            <span>
              I understand {outstanding.length} issue{outstanding.length === 1 ? " is" : "s are"}{" "}
              outstanding (see the warnings on the publish preview) and want to publish anyway.
            </span>
          </label>
        )}

        <Field label={<>Type the assessment name (<strong>{assessmentName}</strong>) to confirm</>}>
          <Input
            autoFocus
            value={confirmText}
            onChange={(e) => setConfirmText(e.target.value)}
            placeholder={assessmentName}
          />
        </Field>

        {errorInfo && (
          <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800">
            {errorInfo}
          </div>
        )}

        <div className="flex justify-end gap-2 pt-1">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" disabled={!confirmed || publish.isPending}>
            {publish.isPending ? "Publishing…" : "Publish"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

const ATTACHMENT_OPTIONS: { value: PublishAttachment; label: string; hint: string }[] = [
  { value: "none", label: "No attachment", hint: "email body only, no report file" },
  {
    value: "compressed",
    label: "Compressed report",
    hint: "recommended — smaller mailbox load",
  },
  { value: "original", label: "Original report", hint: "full resolution" },
];

/** Attachment radio (3 options) + ZIP fallback checkbox for the publish dialog (spec §3
 * D44/D45). Disabled with a hint when ADAMARKER_REPORT_FONT is unconfigured — publishing
 * a non-"none" attachment in that state 400s server-side, so the UI refuses it earlier. */
function AttachmentField({
  attachment,
  onChange,
  zip,
  onZipChange,
  available,
}: {
  attachment: PublishAttachment;
  onChange: (value: PublishAttachment) => void;
  zip: boolean;
  onZipChange: (value: boolean) => void;
  available: boolean;
}) {
  return (
    <div>
      <div className="mb-1.5 text-xs font-medium text-neutral-700">Report attachment</div>
      <div className="space-y-1.5">
        {ATTACHMENT_OPTIONS.map((opt) => {
          const disabled = opt.value !== "none" && !available;
          return (
            <label
              key={opt.value}
              className={cx(
                "flex items-start gap-2 text-sm",
                disabled ? "text-neutral-400" : "text-neutral-700",
              )}
            >
              <input
                type="radio"
                name="publish-attachment"
                value={opt.value}
                checked={attachment === opt.value}
                disabled={disabled}
                onChange={() => onChange(opt.value)}
                className="mt-0.5 size-3.5 border-neutral-300"
              />
              <span>
                <span className="font-medium">{opt.label}</span>
                {" — "}
                <span className={disabled ? "text-neutral-400" : "text-neutral-500"}>{opt.hint}</span>
              </span>
            </label>
          );
        })}
      </div>
      {!available && (
        <p className="mt-1 text-xs text-amber-700">
          Set ADAMARKER_REPORT_FONT + make report-fonts to enable report attachments.
        </p>
      )}
      <label
        className={cx(
          "mt-2 flex items-center gap-2 text-sm",
          attachment === "none" ? "text-neutral-400" : "text-neutral-700",
        )}
      >
        <input
          type="checkbox"
          checked={zip}
          disabled={attachment === "none"}
          onChange={(e) => onZipChange(e.target.checked)}
          className="size-3.5 rounded border-neutral-300"
        />
        Send as ZIP of images instead of PDF — last resort for mail-size limits
      </label>
    </div>
  );
}

/** Renders the three distinct publish 409 shapes (already-published, coverage gate,
 * nothing-to-publish) plus generic errors — each surfaced with its own message so the
 * two 409 paths never look identical to the user. */
function publishErrorInfo(error: Error | null): string | null {
  if (!error) return null;
  if (error instanceof ApiError && error.status === 409 && error.details) {
    const details = error.details as
      | PublishAlreadyPublishedError
      | PublishCoverageGateError
      | PublishNothingToPublishError;
    if ("blockers" in details) {
      return `Coverage gate not satisfied: ${details.blockers.length} blocker(s) remain. Close this dialog and resolve them first.`;
    }
    return details.error;
  }
  return error.message;
}

// --- unpublish dialog: admin-only, typed-name confirm --------------------------------

function UnpublishDialog({
  assessmentId,
  assessmentName,
  onClose,
}: {
  assessmentId: string;
  assessmentName: string;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const [confirmText, setConfirmText] = useState("");

  const unpublish = useMutation({
    mutationFn: () => api.post<UnpublishResult>(`/api/assessments/${assessmentId}/unpublish`),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["publish-preview", assessmentId] });
      await queryClient.invalidateQueries({ queryKey: ["publish-batches", assessmentId] });
      onClose();
    },
  });

  const nameMatches = confirmText.trim() === assessmentName;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (nameMatches) unpublish.mutate();
  };

  return (
    <Dialog open onClose={onClose} title="Unpublish results">
      <form className="space-y-3" onSubmit={submit}>
        <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800">
          This supersedes the live publish batch and <strong>reopens grading</strong> for this
          assessment. It does not un-send email already delivered to students. To correct results,
          unpublish, fix grades, then publish again.
        </div>
        <div className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          Outstanding regrade reply-links stop working while this assessment is unpublished. A
          student who replies in the gap is held as <em>rejected — superseded</em> in the Regrades
          queue; re-publishing re-binds those replies to the new results automatically, but resolve
          them from the queue if they were missed.
        </div>

        <Field label={<>Type the assessment name (<strong>{assessmentName}</strong>) to confirm</>}>
          <Input
            autoFocus
            value={confirmText}
            onChange={(e) => setConfirmText(e.target.value)}
            placeholder={assessmentName}
          />
        </Field>

        {unpublish.isError && (
          <p className="text-xs text-red-600">{unpublish.error.message}</p>
        )}

        <div className="flex justify-end gap-2 pt-1">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" disabled={!nameMatches || unpublish.isPending}>
            {unpublish.isPending ? "Unpublishing…" : "Unpublish"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

// --- batch history ---------------------------------------------------------------------

/**
 * Polls GET .../publish/batches every 2s only while any item is still in the active
 * delivery lifecycle (pending/claimed/sending) — same stop-when-idle idiom as SubmissionsTab's
 * useDirectUploads (D27): polling forever after every send resolves was a past
 * Critical finding, so this must go idle once nothing is in flight.
 */
function usePublishBatches(assessmentId: string) {
  const wasInFlightRef = useRef(false);
  const queryClient = useQueryClient();
  return useQuery({
    queryKey: ["publish-batches", assessmentId],
    queryFn: () =>
      api.get<PublishBatchesResponse>(`/api/assessments/${assessmentId}/publish/batches`),
    refetchInterval: (query) => {
      const batches = query.state.data?.batches ?? [];
      const stillInFlight = batches.some((b) =>
        b.items.some((it) => emailDeliveryInFlight(it.email_status)),
      );
      if (wasInFlightRef.current && !stillInFlight) {
        void queryClient.invalidateQueries({ queryKey: ["publish-preview", assessmentId] });
      }
      wasInFlightRef.current = stillInFlight;
      return stillInFlight ? 2000 : false;
    },
  });
}

function BatchHistory({ assessmentId, emailDisabled }: { assessmentId: string; emailDisabled: boolean }) {
  const batches = usePublishBatches(assessmentId);
  const [expanded, setExpanded] = useState<number | null>(null);

  if (batches.isPending) {
    return (
      <Card title="Publish history">
        <div className="flex justify-center py-6">
          <Spinner className="size-5" />
        </div>
      </Card>
    );
  }
  if (batches.isError) {
    return (
      <Card title="Publish history">
        <p className="text-sm text-red-600">{batches.error.message}</p>
      </Card>
    );
  }

  const rows = batches.data.batches;

  return (
    <Card title="Publish history">
      <Table>
        <thead>
          <tr>
            <TH className="w-16">Batch</TH>
            <TH>Created</TH>
            <TH>Note</TH>
            <TH className="w-24 text-right">Items</TH>
            <TH className="w-28 text-right">Sent</TH>
            <TH className="w-28 text-right">Failed</TH>
            <TH className="w-28 text-right">Uncertain</TH>
            <TH className="w-28 text-right">Skipped</TH>
            <TH className="w-24" />
            <TH className="w-16" />
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && (
            <tr>
              <TD colSpan={10} className="text-center text-neutral-400">
                No publish batches yet.
              </TD>
            </tr>
          )}
          {rows.map((b) => (
            <BatchRows
              key={b.id}
              assessmentId={assessmentId}
              emailDisabled={emailDisabled}
              batch={b}
              expanded={expanded === b.id}
              onToggle={() => setExpanded(expanded === b.id ? null : b.id)}
            />
          ))}
        </tbody>
      </Table>
    </Card>
  );
}

function BatchRows({
  assessmentId,
  emailDisabled,
  batch,
  expanded,
  onToggle,
}: {
  assessmentId: string;
  emailDisabled: boolean;
  batch: PublishBatch;
  expanded: boolean;
  onToggle: () => void;
}) {
  const queryClient = useQueryClient();
  // B5: read the per-status breakdown straight from the backend counts rather than
  // filtering `items` — Skipped (no-submission/email-disabled items that were never
  // mailed) used to have no summary-row column at all, so 4 skipped items looked like 4
  // lost emails until the row was expanded.
  const { items_count: items, sent_count: sent, failed_count: failed, uncertain_count: uncertain, skipped_count: skipped } = batch;

  const resend = useMutation({
    mutationFn: () => api.post<ResendFailedResult>(`/api/publish/batches/${batch.id}/resend-failed`),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["publish-batches", assessmentId] });
    },
  });

  return (
    <>
      <tr onClick={onToggle} className="cursor-pointer hover:bg-neutral-50">
        <TD className="font-medium tabular-nums">
          <span className="mr-1.5 inline-block w-3 text-neutral-400">{expanded ? "▾" : "▸"}</span>
          {batch.id}
        </TD>
        <TD className="text-xs text-neutral-500">{fmtDate(batch.created_at)}</TD>
        <TD className="max-w-xs truncate text-xs text-neutral-600">
          {batch.note || <span className="text-neutral-400">—</span>}
        </TD>
        <TD className="text-right tabular-nums">{items}</TD>
        <TD className="text-right tabular-nums">{sent}</TD>
        <TD className="text-right tabular-nums">{failed}</TD>
        <TD
          className={cx(
            "text-right tabular-nums",
            uncertain > 0 && "font-semibold text-red-700",
          )}
          title={uncertain > 0 ? "Provider acceptance is unknown; resending may duplicate email" : undefined}
        >
          {uncertain}
        </TD>
        <TD className="text-right tabular-nums">{skipped}</TD>
        <TD>
          {batch.superseded ? (
            <Badge tone="neutral">superseded</Badge>
          ) : (
            <Badge tone="green">live</Badge>
          )}
        </TD>
        <TD className="text-right">
          {failed > 0 && (
            <Button
              variant="ghost"
              className="px-2 py-1 text-xs"
              onClick={(e) => {
                e.stopPropagation();
                resend.mutate();
              }}
              disabled={resend.isPending || emailDisabled}
              title={emailDisabled ? "Configure a real email provider before resending" : undefined}
            >
              {resend.isPending ? "Resending…" : "Resend failed"}
            </Button>
          )}
        </TD>
      </tr>
      {expanded && (
        <tr>
          <TD colSpan={10} className="bg-neutral-50/60 px-4 py-3">
            {resend.isError && (
              <p className="mb-2 text-xs text-red-600">{resend.error.message}</p>
            )}
            {uncertain > 0 && (
              <div className="mb-3 rounded-md border border-red-300 bg-red-50 px-3 py-2 text-xs text-red-800">
                <strong>Duplicate risk:</strong> {uncertain} delivery outcome
                {uncertain === 1 ? " is" : "s are"} uncertain. The provider may already have
                accepted the email. Review each item and explicitly acknowledge that risk before
                resending.
              </div>
            )}
            {/* B6: this used to repeat once per row (14 identical notices on a 14-row
                superseded batch) alongside an enabled-LOOKING Resend button on every row,
                including never-sent skipped items. One notice for the whole batch, and no
                per-row Resend affordance at all once superseded — the row-level rendering
                below omits the button entirely rather than disabling it per row. */}
            {batch.superseded && (
              <div className="mb-3 rounded-md border border-neutral-200 bg-neutral-50 px-3 py-2 text-xs text-neutral-600">
                Superseded — a batch that was unpublished can&apos;t be individually resent;
                re-publish instead.
              </div>
            )}
            <Table>
              <thead>
                <tr>
                  <TH>Student</TH>
                  <TH>Recipient</TH>
                  <TH className="w-24">Status</TH>
                  <TH>Error</TH>
                  <TH className="w-20" />
                </tr>
              </thead>
              <tbody>
                {batch.items.map((it) => (
                  <tr key={it.id}>
                    <TD>{it.student_name}</TD>
                    <TD className="text-neutral-500">{it.recipient_email}</TD>
                    <TD>
                      <Badge tone={emailStatusTone[it.email_status]}>
                        {emailStatusLabel[it.email_status]}
                      </Badge>
                    </TD>
                    <TD className="text-xs">
                      {it.error && (
                        <span className={it.warning ? "text-amber-700" : "text-red-600"}>
                          {it.warning && <Badge tone="amber" className="mr-1">warning</Badge>}
                          {stripWarningPrefix(it.error, it.warning)}
                        </span>
                      )}
                      {it.email_status === "uncertain" && !it.error && (
                        <span className="font-medium text-red-700">
                          Provider acceptance is unknown; a resend may deliver a duplicate.
                        </span>
                      )}
                    </TD>
                    <TD className="text-right">
                      {/* Live batch, skipped item: nothing was ever sent, so there is
                          nothing to "resend" — omit the button rather than let it 409. */}
                      {!batch.superseded && it.email_status !== "skipped" && (
                        <ResendItemButton
                          assessmentId={assessmentId}
                          item={it}
                          emailDisabled={emailDisabled}
                        />
                      )}
                    </TD>
                  </tr>
                ))}
              </tbody>
            </Table>
          </TD>
        </tr>
      )}
    </>
  );
}

/** Strips the publish.WarningPrefix ("warning: ") that non-terminal 15MB-guard item
 * warnings carry (spec §3) so the UI shows the underlying message, not the raw prefix
 * the server uses to distinguish warnings from real failures. */
function stripWarningPrefix(error: string, isWarning: boolean): string {
  const prefix = "warning: ";
  return isWarning && error.startsWith(prefix) ? error.slice(prefix.length) : error;
}

/**
 * Per-item resend (spec §4 D46): re-enqueues one terminal publish_item delivery
 * ("student says they never got it"), reusing the parent batch's attachment/zip
 * settings. Active deliveries cannot be replaced. An uncertain provider outcome
 * needs a second, explicit duplicate-risk acknowledgment before the server will arm
 * a new generation. Batch-history polling covers pending/claimed/sending.
 *
 * B6: the caller (BatchRows) never mounts this component for a superseded batch
 * (the individual-resend action is live-batch-only, I1 — the server 409s it) or for a
 * `skipped` item in a live batch (nothing was ever sent, so there is nothing to
 * resend) — this component no longer needs to special-case either state itself.
 */
function ResendItemButton({
  assessmentId,
  item,
  emailDisabled,
}: {
  assessmentId: string;
  item: PublishBatch["items"][number];
  emailDisabled: boolean;
}) {
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const [duplicateRiskAcked, setDuplicateRiskAcked] = useState(false);
  const isInFlight = emailDeliveryInFlight(item.email_status);
  const isUncertain = item.email_status === "uncertain";

  const closeConfirm = () => {
    setConfirming(false);
    setDuplicateRiskAcked(false);
  };

  const resend = useMutation({
    mutationFn: () =>
      api.post<ResendItemResult>(
        `/api/publish/items/${item.id}/resend`,
        isUncertain ? { acknowledge_duplicate_risk: duplicateRiskAcked } : undefined,
      ),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["publish-batches", assessmentId] });
      closeConfirm();
    },
  });

  if (emailDisabled) {
    return (
      <div className="text-right" onClick={(e) => e.stopPropagation()}>
        <IconButton
          disabled
          label={`Resend to ${item.student_name} — configure a real email provider first`}
        >
          <Send />
        </IconButton>
        <p className="mt-1 text-xs text-neutral-500">
          Email delivery is disabled — configure a real provider before resending.
        </p>
      </div>
    );
  }

  if (isInFlight && !confirming) {
    return (
      <div className="text-right" onClick={(e) => e.stopPropagation()}>
        <IconButton
          disabled
          label={`Resend to ${item.student_name} — delivery is ${item.email_status}; wait for it to finish`}
        >
          <Send />
        </IconButton>
        <p className="mt-1 text-xs text-neutral-500">
          Delivery {item.email_status} — resend is disabled while this attempt is active.
        </p>
      </div>
    );
  }

  if (confirming) {
    const canConfirm = !isInFlight && (!isUncertain || duplicateRiskAcked);

    return (
      <Dialog
        open
        onClose={closeConfirm}
        title={isUncertain ? "Resend to this student — duplicate risk" : "Resend to this student"}
      >
        <div className="space-y-3">
          <p className="text-sm text-neutral-700">
            Resend the published results email to <strong>1 student</strong> ({item.student_name}
            ) from its current <strong>{emailStatusLabel[item.email_status]}</strong> state?
          </p>

          {isInFlight && (
            <div className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
              This delivery is already {item.email_status}. Wait for the active attempt to finish
              before deciding whether another email is needed.
            </div>
          )}

          {isUncertain && (
            <label className="flex items-start gap-2 rounded-md border border-red-300 bg-red-50 px-3 py-2 text-xs font-medium text-red-800">
              <input
                type="checkbox"
                checked={duplicateRiskAcked}
                onChange={(e) => setDuplicateRiskAcked(e.target.checked)}
                className="mt-0.5 size-3.5 rounded border-red-400"
              />
              <span>
                I understand the provider may already have accepted this email and that resending
                can deliver a duplicate to the student.
              </span>
            </label>
          )}

          {resend.isError && <p className="text-xs text-red-600">{resend.error.message}</p>}
          <div className="flex justify-end gap-2 pt-1">
            <Button variant="secondary" onClick={closeConfirm} disabled={resend.isPending}>
              Cancel
            </Button>
            <Button
              variant="danger"
              onClick={() => resend.mutate()}
              disabled={!canConfirm || resend.isPending}
            >
              {resend.isPending ? "Sending…" : "Resend"}
            </Button>
          </div>
        </div>
      </Dialog>
    );
  }

  return (
    <div className="text-right" onClick={(e) => e.stopPropagation()}>
      <IconButton
        variant={isUncertain ? "danger" : "default"}
        onClick={() => {
          setDuplicateRiskAcked(false);
          setConfirming(true);
        }}
        label={
          isUncertain
            ? `Review resend risk for ${item.student_name}`
            : `Resend to ${item.student_name}`
        }
      >
        <Send />
      </IconButton>
      {resend.isError && <p className="mt-1 text-xs text-red-600">{resend.error.message}</p>}
    </div>
  );
}
