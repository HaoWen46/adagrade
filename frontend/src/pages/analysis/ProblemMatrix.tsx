// Problem matrix (analysis redesign 2026-07-11, F1 §3): ONE table — rows are
// problems, columns are method-versions plus a Flags column. Each row expands
// (collapsed by default) into the full pre-redesign detail: the verbatim
// per-problem stats table, that problem's hand-grade agreement, the lazily
// fetched score distribution WITH its per-criterion breakdown, and the
// problem's largest between-method gaps.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../../lib/api";
import type {
  AnalysisDisagreement,
  DisagreementAnswerRow,
  DisagreementProblemRow,
  DistributionCriterion,
  HumanAgreementRow,
  MethodProblemStat,
  ScoreDistributionResponse,
} from "../../lib/types";
import { toMicros } from "../../lib/decimal";
import { Badge, Spinner, TD, TH, Table, type BadgeTone } from "../../components/ui";
import { HelpTip } from "../../components/HelpTip";
import { ScoreDistribution } from "../../components/ScoreDistribution";
import {
  AgreementTable,
  GapAnswersTable,
  MeanCell,
  StatsTable,
  microsToFixed,
  type MethodAgg,
} from "./shared";

/** One problem's consecutive stats rows (rows arrive ordered by problem, then method). */
export interface ProblemGroup {
  problemId: number;
  number: number;
  max: string;
  rows: MethodProblemStat[];
}

const matrixHelp = (
  <>
    <p>
      Each cell is one method&apos;s <strong>average score</strong> on one problem; the small
      line under it is the average difference from hand grades on that problem (and how many
      hand-graded answers that is based on — &ldquo;—&rdquo; means none yet). An amber dot
      means the AI reported low confidence or unreadable handwriting on more than 15% of that
      cell&apos;s answers.
    </p>
    <p>
      <strong>Flags</strong> call out patterns worth a look — lots of zeros, everyone at
      maximum, an unsure AI, methods giving the same students very different scores, mixed
      grading policies, or incomplete grading. Hover a flag for what triggered it.
    </p>
    <p>
      Click a row to expand the full statistics, hand-grade agreement, score distribution
      (with the per-criterion breakdown), and that problem&apos;s largest between-method gaps.
    </p>
  </>
);

export function ProblemMatrix({
  groups,
  methods,
  agreement,
  disagreement,
  policyMixIds,
  withPagesByProblem,
}: {
  groups: ProblemGroup[];
  methods: MethodAgg[];
  agreement: HumanAgreementRow[];
  disagreement: AnalysisDisagreement;
  policyMixIds: Set<number>;
  /** problem_id → answers-with-pages (from problems/summary); null until loaded. */
  withPagesByProblem: Map<number, number> | null;
}) {
  const [expanded, setExpanded] = useState<number | null>(null);

  const agreeByKey = new Map<string, HumanAgreementRow>();
  for (const a of agreement) {
    agreeByKey.set(`${a.problem_number}:${a.method_version_id}`, a);
  }
  const disByProblem = new Map<number, DisagreementProblemRow>();
  for (const p of disagreement.problems) disByProblem.set(p.problem_id, p);
  const gapsByProblemNumber = new Map<number, DisagreementAnswerRow[]>();
  for (const t of disagreement.top_answers) {
    const list = gapsByProblemNumber.get(t.problem_number) ?? [];
    list.push(t);
    gapsByProblemNumber.set(t.problem_number, list);
  }

  const cols = methods.length + 2;

  return (
    <section className="space-y-3">
      <h2 className="flex items-center gap-1.5 text-sm font-semibold text-neutral-900">
        Problem breakdown
        <HelpTip title="Problem breakdown">{matrixHelp}</HelpTip>
      </h2>
      <Table>
        <thead>
          <tr>
            <TH className="w-28">Problem</TH>
            {methods.map((m) => (
              <TH key={m.methodVersionId}>
                {m.name}{" "}
                <span className="font-normal text-neutral-400 normal-case tabular-nums">
                  v{m.version}
                </span>
              </TH>
            ))}
            <TH className="w-56">Flags</TH>
          </tr>
        </thead>
        <tbody>
          {groups.map((g) => (
            <MatrixRow
              key={g.problemId}
              group={g}
              methods={methods}
              cols={cols}
              agreeByKey={agreeByKey}
              dis={disByProblem.get(g.problemId)}
              gaps={gapsByProblemNumber.get(g.number) ?? []}
              mixedPolicies={policyMixIds.has(g.problemId)}
              withPages={withPagesByProblem?.get(g.problemId)}
              agreement={agreement}
              expanded={expanded === g.problemId}
              onToggle={() => setExpanded(expanded === g.problemId ? null : g.problemId)}
            />
          ))}
        </tbody>
      </Table>
    </section>
  );
}

// --- one problem row + its expansion ---------------------------------------------------

function MatrixRow({
  group: g,
  methods,
  cols,
  agreeByKey,
  dis,
  gaps,
  mixedPolicies,
  withPages,
  agreement,
  expanded,
  onToggle,
}: {
  group: ProblemGroup;
  methods: MethodAgg[];
  cols: number;
  agreeByKey: Map<string, HumanAgreementRow>;
  dis: DisagreementProblemRow | undefined;
  gaps: DisagreementAnswerRow[];
  mixedPolicies: boolean;
  withPages: number | undefined;
  agreement: HumanAgreementRow[];
  expanded: boolean;
  onToggle: () => void;
}) {
  const statByMv = new Map(g.rows.map((s) => [s.method_version_id, s]));
  const flags = problemFlags(g, dis, mixedPolicies, withPages);

  return (
    <>
      <tr onClick={onToggle} className="cursor-pointer hover:bg-neutral-50">
        <TD className="font-medium tabular-nums">
          <span className="mr-1.5 inline-block w-3 text-neutral-400">{expanded ? "▾" : "▸"}</span>
          {g.number}
          <span className="ml-1.5 text-xs font-normal text-neutral-400">max {g.max}</span>
        </TD>
        {methods.map((m) => (
          <MatrixCell
            key={m.methodVersionId}
            stat={statByMv.get(m.methodVersionId)}
            agree={agreeByKey.get(`${g.number}:${m.methodVersionId}`)}
            max={g.max}
          />
        ))}
        <TD>
          {flags.length === 0 ? (
            <span className="text-xs text-neutral-300">—</span>
          ) : (
            <span className="flex flex-wrap gap-1">
              {flags.map((f) => (
                <span key={f.label} title={f.title}>
                  <Badge tone={f.tone}>{f.label}</Badge>
                </span>
              ))}
            </span>
          )}
        </TD>
      </tr>
      {expanded && (
        <tr>
          <TD colSpan={cols} className="bg-neutral-50/60 px-4 py-3">
            <ProblemDetail group={g} agreement={agreement} gaps={gaps} />
          </TD>
        </tr>
      )}
    </>
  );
}

/** Mean + bar, an "AI unsure" amber dot past 15% low/unreadable, and the ±Δ-vs-hand
 * second line ("—" when this problem × method has no hand-graded pairs yet). */
function MatrixCell({
  stat,
  agree,
  max,
}: {
  stat: MethodProblemStat | undefined;
  agree: HumanAgreementRow | undefined;
  max: string;
}) {
  if (!stat) {
    return <TD className="text-xs text-neutral-300">no records</TD>;
  }
  const lowIll = stat.conf_low + stat.conf_illegible;
  const unsure = stat.records > 0 && lowIll / stat.records > 0.15;
  const madMicros = agree && agree.pairs > 0 ? toMicros(agree.mean_abs_diff) : null;
  return (
    <TD>
      <div className="flex items-start gap-1.5">
        <MeanCell mean={stat.mean_total} max={max} />
        {unsure && (
          <span
            title={`AI unsure on ${lowIll} of ${stat.records} answers (low confidence or unreadable)`}
            className="mt-1.5 size-1.5 shrink-0 rounded-full bg-amber-500"
          />
        )}
      </div>
      <div className="mt-0.5 text-xs text-neutral-500 tabular-nums">
        {agree && madMicros !== null
          ? `±${microsToFixed(madMicros, 1)} vs hand (${agree.pairs})`
          : "—"}
      </div>
    </TD>
  );
}

// --- flags (client-computed, deliberately conservative thresholds) ---------------------

interface ProblemFlag {
  label: string;
  tone: BadgeTone;
  title: string;
}

function problemFlags(
  g: ProblemGroup,
  dis: DisagreementProblemRow | undefined,
  mixedPolicies: boolean,
  withPages: number | undefined,
): ProblemFlag[] {
  const flags: ProblemFlag[] = [];
  let records = 0;
  let zeros = 0;
  let maxes = 0;
  let lowIll = 0;
  let minRecords = Number.POSITIVE_INFINITY;
  for (const s of g.rows) {
    records += s.records;
    zeros += s.zeros;
    maxes += s.maxes;
    lowIll += s.conf_low + s.conf_illegible;
    minRecords = Math.min(minRecords, s.records);
  }

  if (records > 0 && zeros / records >= 0.3) {
    flags.push({
      label: "many zeros",
      tone: "amber",
      title: `${zeros} of ${records} AI scores on this problem are 0 — check the rubric, the scans, or whether pages were mapped to the right problem.`,
    });
  }
  if (records > 0 && maxes / records >= 0.9) {
    flags.push({
      label: "everyone aced it",
      tone: "green",
      title: `${maxes} of ${records} AI scores are the maximum — the problem (or its rubric) may not separate students.`,
    });
  }
  if (records > 0 && lowIll / records >= 0.2) {
    flags.push({
      label: "AI unsure",
      tone: "amber",
      title: `The AI reported low confidence or unreadable handwriting on ${lowIll} of ${records} grades — check the scans by hand.`,
    });
  }
  if (dis && dis.answers_compared > 0 && dis.big_gap_count / dis.answers_compared > 0.2) {
    flags.push({
      label: "methods split",
      tone: "amber",
      title: `The methods gave meaningfully different scores (at least 1 point, or 10% of the maximum) on ${dis.big_gap_count} of the ${dis.answers_compared} answers they all graded.`,
    });
  }
  if (mixedPolicies) {
    flags.push({
      label: "mixed policies",
      tone: "amber",
      title:
        "Official grades for this problem were produced under different grading policies — see the warning at the top of this tab.",
    });
  }
  if (withPages !== undefined && Number.isFinite(minRecords) && minRecords < withPages) {
    flags.push({
      label: `graded ${minRecords} of ${withPages}`,
      tone: "neutral",
      title: `Not every method has graded every answer with pages — the least-complete method covers ${minRecords} of ${withPages}.`,
    });
  }
  return flags;
}

// --- expansion body ---------------------------------------------------------------------

function ProblemDetail({
  group: g,
  agreement,
  gaps,
}: {
  group: ProblemGroup;
  agreement: HumanAgreementRow[];
  gaps: DisagreementAnswerRow[];
}) {
  const agreeRows = agreement.filter((a) => a.problem_number === g.number);
  return (
    <div className="space-y-4">
      <div className="space-y-1.5">
        <h4 className="text-xs font-semibold tracking-wide text-neutral-500 uppercase">
          Score statistics
        </h4>
        <StatsTable rows={g.rows} max={g.max} />
      </div>
      <div className="space-y-1.5">
        <h4 className="text-xs font-semibold tracking-wide text-neutral-500 uppercase">
          Agreement with hand grades
        </h4>
        {agreeRows.length === 0 ? (
          <p className="text-xs text-neutral-400">
            No hand grades for this problem yet (same rubric version).
          </p>
        ) : (
          <AgreementTable rows={agreeRows} />
        )}
      </div>
      <DistributionDetail problemId={g.problemId} />
      {gaps.length > 0 && (
        <div className="space-y-1.5">
          <h4 className="text-xs font-semibold tracking-wide text-neutral-500 uppercase">
            Largest gaps between methods
          </h4>
          <GapAnswersTable rows={gaps} showProblem={false} />
        </div>
      )}
    </div>
  );
}

/** Lazily fetched score distribution (only while the row is expanded) — the shared
 * ["score-distribution", id] cache key means no refetch when ReviewTab already loaded
 * it. Unlike ReviewTab, the response's per-criterion stats are ALSO rendered here. */
function DistributionDetail({ problemId }: { problemId: number }) {
  const dist = useQuery({
    queryKey: ["score-distribution", problemId],
    queryFn: () =>
      api.get<ScoreDistributionResponse>(`/api/problems/${problemId}/score-distribution`),
  });
  return (
    <div className="space-y-1.5">
      <h4 className="text-xs font-semibold tracking-wide text-neutral-500 uppercase">
        Score distribution
      </h4>
      {dist.isPending ? (
        <Spinner className="size-4" />
      ) : dist.isError ? (
        <p className="text-xs text-red-600">{dist.error.message}</p>
      ) : (
        <>
          <ScoreDistribution data={dist.data} />
          <CriteriaTable criteria={dist.data.criteria} />
        </>
      )}
    </div>
  );
}

/** The distribution's per-criterion stats — data the pre-redesign tab dropped. */
function CriteriaTable({ criteria }: { criteria: DistributionCriterion[] }) {
  if (criteria.length === 0) return null;
  return (
    <Table>
      <thead>
        <tr>
          <TH>Criterion</TH>
          <TH className="w-20 text-right">Points</TH>
          <TH className="w-14 text-right">n</TH>
          <TH className="w-20 text-right">Mean</TH>
          <TH className="w-20 text-right">σ</TH>
          <TH className="w-20 text-right">% zero</TH>
          <TH className="w-20 text-right">% max</TH>
        </tr>
      </thead>
      <tbody>
        {criteria.map((c) => (
          <tr key={c.criterion_id} className="hover:bg-neutral-50">
            <TD className="text-xs">{c.description}</TD>
            <TD className="text-right tabular-nums">{c.points}</TD>
            <TD className="text-right tabular-nums">{c.n}</TD>
            <TD className="text-right tabular-nums">{c.mean || "—"}</TD>
            <TD className="text-right tabular-nums">{c.stddev || "—"}</TD>
            <TD className="text-right tabular-nums">{c.zero_pct ? `${c.zero_pct}%` : "—"}</TD>
            <TD className="text-right tabular-nums">{c.max_pct ? `${c.max_pct}%` : "—"}</TD>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}
