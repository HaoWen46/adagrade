// Consensus tab: per-assessment aggregation policy + re-runnable combine step
// (docs/DECISIONS.md D17). Combines model records that already exist into an
// append-only `aggregate` grading record per answer — zero new AI calls. The
// panel is picked from methods that have records here (Analysis data, deduped).

import { useState, type FormEvent } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import type {
  AggFlag,
  AggregationPolicy,
  AggregationPolicyResponse,
  AggregationReport,
  AnalysisResponse,
  AssessmentDetailResponse,
} from "../lib/types";
import { consensusHelp, consensusIntro, consensusPolicyMixHelp } from "../lib/helpContent";
import { HelpTip } from "../components/HelpTip";
import { PolicyBadge } from "../components/PolicyBadge";
import { Badge, Button, Card, Field, Input, Select, Spinner } from "../components/ui";

const FLAG_TRIGGERS: Array<{ key: AggFlag; label: string }> = [
  { key: "agg_disagreement", label: "models disagree" },
  { key: "agg_missing", label: "too few grades" },
  { key: "agg_low_confidence", label: "models unsure" },
];

/** One checkbox per method version that has records in this assessment. */
interface PanelOption {
  id: number;
  name: string;
  version: number;
  records: number;
  /** Empty for legacy (pre-D25) configs — kept out of the mixed-policy check below. */
  policy: string;
}

export function ConsensusTab({
  assessmentId,
  onGoToReview,
}: {
  assessmentId: string;
  onGoToReview: () => void;
}) {
  const policy = useQuery({
    queryKey: ["aggregation-policy", assessmentId],
    queryFn: () =>
      api.get<AggregationPolicyResponse>(`/api/assessments/${assessmentId}/aggregation`),
  });
  const analysis = useQuery({
    queryKey: ["analysis", assessmentId],
    queryFn: () => api.get<AnalysisResponse>(`/api/assessments/${assessmentId}/analysis`),
  });
  // Final grading source (0027) — explains a "0 officials set" run result.
  // Same cache key as AssessmentDetail, so this is normally a cache hit; the
  // tab renders without waiting for it.
  const detail = useQuery({
    queryKey: ["assessment", assessmentId],
    queryFn: () => api.get<AssessmentDetailResponse>(`/api/assessments/${assessmentId}`),
    enabled: assessmentId !== "",
  });

  if (policy.isPending || analysis.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (policy.isError || analysis.isError) {
    return (
      <Card>
        <p className="text-sm text-red-600">
          {(policy.isError ? policy.error : analysis.error!).message}
        </p>
      </Card>
    );
  }

  // Stats arrive per problem × method — dedupe to one option per method version.
  const options: PanelOption[] = [];
  for (const s of analysis.data.stats) {
    const existing = options.find((o) => o.id === s.method_version_id);
    if (existing) existing.records += s.records;
    else
      options.push({
        id: s.method_version_id,
        name: s.method_name,
        version: s.method_version,
        records: s.records,
        policy: s.policy,
      });
  }

  return (
    <div className="space-y-4">
      <Card>
        <p className="text-sm text-neutral-600">{consensusIntro}</p>
      </Card>

      {options.length === 0 ? (
        <Card>
          <p className="text-sm text-neutral-400">
            No AI grading records yet —{" "}
            <Link
              to={`/runs?launch=1&assessment_id=${assessmentId}`}
              className="font-medium text-indigo-600 hover:underline"
            >
              start a grading run
            </Link>{" "}
            first.
          </p>
        </Card>
      ) : (
        <PolicyForm
          assessmentId={assessmentId}
          initial={policy.data.policy}
          options={options}
        />
      )}

      <RunSection
        assessmentId={assessmentId}
        hasPolicy={policy.data.policy !== null}
        finalSourceKind={detail.data?.assessment.final_source_kind}
        onGoToReview={onGoToReview}
      />
    </div>
  );
}

// --- policy form -------------------------------------------------------------------

function PolicyForm({
  assessmentId,
  initial,
  options,
}: {
  assessmentId: string;
  initial: AggregationPolicy | null;
  options: PanelOption[];
}) {
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<number[]>(initial?.method_version_ids ?? []);
  const [combiner, setCombiner] = useState(initial?.combiner ?? "majority");
  const [fault, setFault] = useState(initial ? String(initial.fault_tolerance) : "0");
  const [triggers, setTriggers] = useState<string[]>(
    initial?.flag_triggers ?? FLAG_TRIGGERS.map((t) => t.key),
  );
  // A saved policy may reference a method version with no records here (yet) —
  // keep it visible so it can be unchecked rather than silently kept in the panel.
  const known = new Set(options.map((o) => o.id));
  const strays = selected.filter((id) => !known.has(id));

  const save = useMutation({
    mutationFn: () =>
      api.put<AggregationPolicyResponse>(`/api/assessments/${assessmentId}/aggregation`, {
        method_version_ids: selected,
        combiner,
        fault_tolerance: parseInt(fault, 10),
        flag_triggers: triggers,
        // set_official is dead since 0027 (officials derive from the final
        // source); sent as false for wire-compat with the unchanged endpoint.
        set_official: false,
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["aggregation-policy", assessmentId] });
    },
  });

  const n = selected.length;
  const maxFault = Math.max(0, Math.floor((n - 1) / 2));
  // Panel + a parseable f is enough to submit; the server enforces 2f < n and
  // its message renders inline below.
  const valid = n > 0 && /^\d+$/.test(fault.trim());

  const togglePanel = (id: number, on: boolean) =>
    setSelected((prev) => (on ? [...prev, id] : prev.filter((x) => x !== id)));
  const toggleTrigger = (key: string, on: boolean) =>
    setTriggers((prev) => (on ? [...prev, key] : prev.filter((t) => t !== key)));

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (valid) save.mutate();
  };

  // Legacy (pre-D25) configs report an empty policy — ignored here since there's
  // nothing to compare them against.
  const selectedPolicies = new Set(
    options
      .filter((o) => selected.includes(o.id) && o.policy !== "")
      .map((o) => o.policy),
  );
  const mixedPolicies = selectedPolicies.size > 1;

  return (
    <Card title="Consensus policy">
      <form className="space-y-4" onSubmit={submit}>
        <div>
          <span className="mb-1 block text-xs font-medium text-neutral-600">
            Panel — methods whose grades get combined
          </span>
          <div className="space-y-1">
            {options.map((o) => (
              <label key={o.id} className="flex items-center gap-1.5 text-sm text-neutral-800">
                <input
                  type="checkbox"
                  checked={selected.includes(o.id)}
                  onChange={(e) => togglePanel(o.id, e.target.checked)}
                  className="size-3.5 accent-indigo-600"
                />
                {o.name}{" "}
                <span className="text-xs text-neutral-400 tabular-nums">v{o.version}</span>
                {o.policy && <PolicyBadge policy={o.policy} />}
                <span className="text-xs text-neutral-500 tabular-nums">
                  — {o.records} records
                </span>
              </label>
            ))}
            {strays.map((id) => (
              <label key={id} className="flex items-center gap-1.5 text-sm text-neutral-500">
                <input
                  type="checkbox"
                  checked
                  onChange={() => togglePanel(id, false)}
                  className="size-3.5 accent-indigo-600"
                />
                method version #{id}
                <span className="text-xs text-neutral-400">— no records here yet</span>
              </label>
            ))}
          </div>
          {mixedPolicies && (
            <p className="mt-2 flex items-start gap-1.5 rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
              <span>
                Panel mixes grading policies ({Array.from(selectedPolicies).join(", ")}).{" "}
                {consensusPolicyMixHelp}
              </span>
            </p>
          )}
        </div>

        <div className="grid max-w-md grid-cols-2 gap-3">
          <Field
            label={
              <>
                Combiner <HelpTip title="Combiner">{consensusHelp.combiner}</HelpTip>
              </>
            }
          >
            <Select
              value={combiner}
              onChange={(e) => setCombiner(e.target.value as AggregationPolicy["combiner"])}
            >
              <option value="majority">majority</option>
              <option value="mean">mean</option>
            </Select>
          </Field>
          <Field
            label={
              <>
                Fault tolerance{" "}
                <HelpTip title="Fault tolerance">{consensusHelp.faultTolerance}</HelpTip>
              </>
            }
          >
            <Input
              inputMode="numeric"
              value={fault}
              onChange={(e) => setFault(e.target.value)}
            />
            <span className="mt-1 block text-xs text-neutral-500 tabular-nums">
              max f for {n} selected = {maxFault}
            </span>
          </Field>
        </div>

        <div>
          <span className="mb-1 block text-xs font-medium text-neutral-600">
            Flag for review when{" "}
            <HelpTip title="Flag triggers">{consensusHelp.flagTriggers}</HelpTip>
          </span>
          <div className="flex flex-wrap gap-x-4 gap-y-1">
            {FLAG_TRIGGERS.map((t) => (
              <label key={t.key} className="flex items-center gap-1.5 text-sm text-neutral-800">
                <input
                  type="checkbox"
                  checked={triggers.includes(t.key)}
                  onChange={(e) => toggleTrigger(t.key, e.target.checked)}
                  className="size-3.5 accent-indigo-600"
                />
                {t.label}
              </label>
            ))}
          </div>
        </div>

        <p className="rounded-md bg-neutral-50 px-3 py-2 text-xs text-neutral-500 ring-1 ring-neutral-200 ring-inset">
          Consensus results become official only when the exam&apos;s <strong>final grading
          source</strong> (Publish tab) is set to consensus — clean answers derive automatically,
          conflicted ones land in the manual-fallback queue.
        </p>

        {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}
        <div className="flex items-center gap-2">
          <Button type="submit" disabled={save.isPending || !valid}>
            {save.isPending ? "Saving…" : "Save policy"}
          </Button>
          {save.isSuccess && !save.isPending && (
            <span className="text-xs text-neutral-500">Saved.</span>
          )}
        </div>
      </form>
    </Card>
  );
}

// --- run + result ------------------------------------------------------------------

function RunSection({
  assessmentId,
  hasPolicy,
  finalSourceKind,
  onGoToReview,
}: {
  assessmentId: string;
  hasPolicy: boolean;
  /** The exam's final grading source (0027); undefined = not chosen yet (or
   * still loading — treated the same, the hint below is advisory). */
  finalSourceKind?: "method" | "consensus";
  onGoToReview: () => void;
}) {
  const queryClient = useQueryClient();

  const run = useMutation({
    mutationFn: () =>
      api.post<AggregationReport>(`/api/assessments/${assessmentId}/aggregate`),
    onSuccess: async () => {
      // New aggregate records, agg_* flags, and possibly official pointers.
      await queryClient.invalidateQueries({ queryKey: ["problem-summaries", assessmentId] });
      await queryClient.invalidateQueries({ queryKey: ["problem-students"] });
      await queryClient.invalidateQueries({ queryKey: ["answer"] });
    },
  });

  return (
    <Card
      title="Run consensus"
      actions={
        <Button
          className="px-2.5 py-1 text-xs"
          disabled={!hasPolicy || run.isPending}
          onClick={() => run.mutate()}
        >
          {run.isPending ? "Running…" : "Run consensus"}
        </Button>
      }
    >
      {!hasPolicy ? (
        <p className="text-sm text-neutral-400">Save a policy above first.</p>
      ) : (
        <p className="text-sm text-neutral-600">
          Applies the saved policy to the grades that exist right now. Safe to re-run any time —
          each run appends fresh consensus records and refreshes the flags.
        </p>
      )}
      {run.isError && <p className="mt-2 text-sm text-red-600">{run.error.message}</p>}
      {run.data && (
        <RunResult
          report={run.data}
          finalSourceKind={finalSourceKind}
          onGoToReview={onGoToReview}
        />
      )}
    </Card>
  );
}

function RunResult({
  report,
  finalSourceKind,
  onGoToReview,
}: {
  report: AggregationReport;
  finalSourceKind?: "method" | "consensus";
  onGoToReview: () => void;
}) {
  const flagged = FLAG_TRIGGERS.map((t) => ({ ...t, count: report.flagged[t.key] ?? 0 })).filter(
    (t) => t.count > 0,
  );
  return (
    <div className="mt-3 space-y-2 border-t border-neutral-200 pt-3">
      <div className="flex flex-wrap gap-x-5 gap-y-1 text-sm text-neutral-800">
        <span>
          <span className="font-semibold tabular-nums">{report.answers_considered}</span> answers
          considered
        </span>
        <span>
          <span className="font-semibold tabular-nums">{report.aggregates_written}</span>{" "}
          aggregates written
        </span>
        <span>
          <span className="font-semibold tabular-nums">{report.officials_set}</span> officials set
          {/* "0 officials" is the headline first-timers misread as failure —
              put the reason next to the number, not in grey small print. */}
          {report.officials_set === 0 && (
            <span className="ml-1.5 text-xs font-normal text-amber-700">
              {finalSourceKind === "consensus"
                ? "— no answers derived cleanly; see the flags below"
                : "— expected: officials only derive once the exam's final grading source is set to consensus (Publish tab)"}
            </span>
          )}
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-1.5">
        {flagged.length === 0 ? (
          <span className="text-xs text-neutral-400">nothing flagged</span>
        ) : (
          flagged.map((t) => (
            <Badge key={t.key} tone="amber">
              {t.label}: {t.count}
            </Badge>
          ))
        )}
      </div>
      <p className="text-xs text-neutral-500">
        <button
          type="button"
          onClick={onGoToReview}
          className="font-medium text-indigo-600 hover:underline"
        >
          Triage flagged answers on the Review tab
        </button>{" "}
        — flagged answers keep waiting for a human decision.
      </p>
    </div>
  );
}
