// Analysis tab (redesigned 2026-07-11): the assessment as a dataset for
// comparing grading methods. Top to bottom — the mixed-policy warning, one
// report card per method-version, where the methods disagree on the same
// answers, a problems × methods matrix (rows expand into the full per-problem
// detail), and the run-cost history behind a disclosure. Decimal strings render
// verbatim (D4); aggregation uses bigint-micros; floats appear only as CSS bar
// widths and count percentages.

import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";
import type {
  AnalysisResponse,
  PolicyMixRow,
  ProblemSummary,
  RunListRow,
} from "../lib/types";
import { policyMixHelp } from "../lib/helpContent";
import { HelpTip } from "../components/HelpTip";
import { Card, Spinner } from "../components/ui";
import { aggregateMethods } from "./analysis/shared";
import { MethodCards } from "./analysis/MethodCards";
import { DisagreementSection } from "./analysis/DisagreementSection";
import { ProblemMatrix, type ProblemGroup } from "./analysis/ProblemMatrix";
import { RUNS_LIMIT, RunHistory } from "./analysis/RunHistory";

const cardsHelp = (
  <>
    <p>One card per grading method (and version) that has graded this assessment.</p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>Graded</strong> — how many of the assessment&apos;s answers with pages this
        method has scored.
      </li>
      <li>
        <strong>vs. hand grades</strong> — the average difference from the most recent human
        grade on the same answer (same rubric version), weighted by how many hand-graded
        answers each problem has. Lower is better; a handful of hand grades is enough to start
        calibrating.
      </li>
      <li>
        <strong>Overridden</strong> — of this method&apos;s answers that already have an
        official grade, the share where the official is a human record, split into the two cases
        it conflates: answers where a human <em>replaced a grade the AI actually scored</em> (real
        disagreement, with the mean point gap), and answers the AI <em>abstained</em> on
        (illegible / no score) that a human simply <em>filled in</em> — which is not the AI being
        wrong. &ldquo;No official grades yet&rdquo; means no data — not a 0% override rate.
      </li>
      <li>
        <strong>Confidence</strong> — the AI&apos;s own certainty it could read and grade each
        answer. A large low/unreadable share means check the scans yourself.
      </li>
      <li>
        <strong>Cost</strong> — actual spend across this method&apos;s runs on this assessment
        (never an estimate), total and per graded answer.
      </li>
    </ul>
  </>
);

export function AnalysisTab({
  assessmentId,
  onGoToReview,
}: {
  assessmentId: string;
  onGoToReview: () => void;
}) {
  const analysis = useQuery({
    queryKey: ["analysis", assessmentId],
    queryFn: () => api.get<AnalysisResponse>(`/api/assessments/${assessmentId}/analysis`),
  });

  // Per-run costs feed the report cards and the Run history disclosure. The server
  // filters by assessment (GET /api/runs?assessment_id=…), so this is no longer the
  // old "recent 50 system-wide, filter client-side" workaround. Kept a separate query
  // (rather than failing the whole tab) since cost is supplementary, not core analysis.
  const runs = useQuery({
    queryKey: ["runs", "assessment", assessmentId],
    queryFn: () =>
      api.get<{ runs: RunListRow[] }>(
        `/api/runs?assessment_id=${assessmentId}&limit=${RUNS_LIMIT}`,
      ),
  });

  // "Graded 40 of 40" denominators — answers with pages, per problem and overall.
  // Same cache key as ReviewTab/AssessmentDetail so no extra request when warm.
  const summary = useQuery({
    queryKey: ["problem-summaries", assessmentId],
    queryFn: () =>
      api.get<{ problems: ProblemSummary[] }>(`/api/assessments/${assessmentId}/problems/summary`),
  });

  if (analysis.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (analysis.isError) {
    return (
      <Card>
        <p className="text-sm text-red-600">{analysis.error.message}</p>
      </Card>
    );
  }

  const {
    stats,
    agreement,
    policy_mix: policyMix,
    override_rate: overrideRate,
    disagreement,
  } = analysis.data;

  if (stats.length === 0) {
    return (
      <Card>
        <p className="text-sm text-neutral-400">No AI grading records yet — launch a run first.</p>
      </Card>
    );
  }

  const methods = aggregateMethods(stats, agreement);

  // Rows arrive ordered by problem number, then method — group consecutively.
  const groups: ProblemGroup[] = [];
  for (const s of stats) {
    const last = groups[groups.length - 1];
    if (last && last.problemId === s.problem_id) {
      last.rows.push(s);
    } else {
      groups.push({ problemId: s.problem_id, number: s.problem_number, max: s.max_points, rows: [s] });
    }
  }

  const withPagesByProblem = summary.data
    ? new Map(summary.data.problems.map((p) => [p.problem_id, p.with_pages]))
    : null;
  const gradable = summary.data
    ? summary.data.problems.reduce((n, p) => n + p.with_pages, 0)
    : null;

  return (
    <div className="space-y-6">
      {policyMix.length > 0 && <PolicyMixWarning rows={policyMix} />}

      <section className="space-y-3">
        <h2 className="flex items-center gap-1.5 text-sm font-semibold text-neutral-900">
          Method report card{methods.length === 1 ? "" : "s"}
          <HelpTip title="Method report cards">{cardsHelp}</HelpTip>
        </h2>
        <MethodCards
          methods={methods}
          gradable={gradable}
          overrides={overrideRate}
          runs={runs.data?.runs ?? null}
          onGoToReview={onGoToReview}
        />
      </section>

      <DisagreementSection disagreement={disagreement} />

      <ProblemMatrix
        groups={groups}
        methods={methods}
        agreement={agreement}
        disagreement={disagreement}
        policyMixIds={new Set(policyMix.map((r) => r.problem_id))}
        withPagesByProblem={withPagesByProblem}
      />

      <RunHistory
        runs={runs.data?.runs ?? null}
        isPending={runs.isPending}
        errorMessage={runs.isError ? runs.error.message : null}
      />
    </div>
  );
}

// --- mixed-policy warning ---------------------------------------------------------------

/** Amber warning when official grades for one problem came from different policies —
 * ambiguity would then be resolved differently for different students (policyMixHelp). */
function PolicyMixWarning({ rows }: { rows: PolicyMixRow[] }) {
  return (
    <Card className="border-amber-200 bg-amber-50/60">
      <div className="flex items-start gap-2">
        <h3 className="flex items-center gap-1.5 text-sm font-semibold text-amber-900">
          Mixed grading policies among official grades
          <HelpTip title="Mixed grading policies">{policyMixHelp}</HelpTip>
        </h3>
      </div>
      <ul className="mt-1.5 space-y-0.5 text-xs text-amber-800">
        {rows.map((r) => (
          <li key={r.problem_id}>
            Problem {r.problem_number}: {r.policies.join(", ")}
          </li>
        ))}
      </ul>
      <p className="mt-1.5 text-xs text-amber-700">
        The same standard should apply to every student within a problem.
      </p>
    </Card>
  );
}
