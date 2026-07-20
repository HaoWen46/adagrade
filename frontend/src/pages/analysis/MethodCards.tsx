// Method report cards (analysis redesign 2026-07-11, F1 §1): one card per
// method-version with records, answering "which method should I trust?" at a
// glance — coverage, hand-grade agreement, override rate, confidence, cost.
// All aggregation is exact bigint-micros math; decimal strings render verbatim.

import type { OverrideRateRow, RunListRow } from "../../lib/types";
import { microsToString, toMicros } from "../../lib/decimal";
import { Badge, Card, cx } from "../../components/ui";
import { HelpTip } from "../../components/HelpTip";
import { countPercent, microsToFixed, rateToPercent, type MethodAgg } from "./shared";
import { PolicyBadge } from "../../components/PolicyBadge";

/** Badge gate: a "closest to hand grades" call needs at least this many hand-graded
 * pairs per method, on at least two methods, to be a fair comparison. */
const BADGE_MIN_PAIRS = 10;

const closestHelp = (
  <>
    <p>
      Among the methods with at least {BADGE_MIN_PAIRS} hand-graded answers each, this method
      has the lowest average difference from the hand grades.
    </p>
    <p>
      The badge only appears when at least two methods each have {BADGE_MIN_PAIRS}+ hand-graded
      answers — with less data than that, calling a winner would be noise, not signal.
    </p>
  </>
);

export function MethodCards({
  methods,
  gradable,
  overrides,
  runs,
  onGoToReview,
}: {
  methods: MethodAgg[];
  /** Assessment-wide answers-with-pages denominator (from problems/summary);
   * null while that query hasn't resolved. */
  gradable: number | null;
  overrides: OverrideRateRow[];
  /** This assessment's runs; null while loading/errored (cost rows show "—"). */
  runs: RunListRow[] | null;
  onGoToReview: () => void;
}) {
  const overrideByMv = new Map(overrides.map((r) => [r.method_version_id, r]));

  // "Closest to hand grades" only when ≥2 methods each have ≥10 pairs (fair fight).
  const eligible = methods.filter((m) => m.pairs >= BADGE_MIN_PAIRS && m.madMicros !== null);
  let closestId: number | null = null;
  if (eligible.length >= 2) {
    let best = eligible[0];
    for (const m of eligible) {
      if (m.madMicros !== null && best.madMicros !== null && m.madMicros < best.madMicros) best = m;
    }
    closestId = best.methodVersionId;
  }

  return (
    <div
      className={cx(
        "grid gap-3",
        methods.length > 1 && "md:grid-cols-2 xl:grid-cols-4",
      )}
    >
      {methods.map((m) => (
        <MethodCard
          key={m.methodVersionId}
          m={m}
          gradable={gradable}
          closest={m.methodVersionId === closestId}
          override={overrideByMv.get(m.methodVersionId)}
          runs={runs}
          onGoToReview={onGoToReview}
        />
      ))}
    </div>
  );
}

function MethodCard({
  m,
  gradable,
  closest,
  override,
  runs,
  onGoToReview,
}: {
  m: MethodAgg;
  gradable: number | null;
  closest: boolean;
  override: OverrideRateRow | undefined;
  runs: RunListRow[] | null;
  onGoToReview: () => void;
}) {
  return (
    <Card>
      <div className="flex flex-wrap items-center gap-1.5">
        <h3 className="text-sm font-semibold text-neutral-900">
          {m.name}{" "}
          <span className="font-normal text-neutral-400 tabular-nums">v{m.version}</span>
        </h3>
        {m.policy && <PolicyBadge policy={m.policy} />}
        {closest && (
          <span className="inline-flex items-center gap-1">
            <Badge tone="green">Closest to hand grades</Badge>
            <HelpTip title="Closest to hand grades">{closestHelp}</HelpTip>
          </span>
        )}
      </div>
      <dl className="mt-3 space-y-3">
        <CardRow label="Graded">
          <span className="tabular-nums">
            {m.records}
            {gradable !== null ? ` of ${gradable}` : ""} answers
          </span>
        </CardRow>
        <CardRow label="vs. hand grades">
          <AgreementRow m={m} onGoToReview={onGoToReview} />
        </CardRow>
        <CardRow label="Overridden">
          <OverrideRow override={override} />
        </CardRow>
        <CardRow label="Confidence">
          <ConfidenceBar m={m} />
        </CardRow>
        <CardRow label="Cost">
          <CostRow methodVersionId={m.methodVersionId} runs={runs} />
        </CardRow>
      </dl>
    </Card>
  );
}

function CardRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-[10px] font-medium tracking-wide text-neutral-400 uppercase">{label}</dt>
      <dd className="mt-0.5 text-sm text-neutral-800">{children}</dd>
    </div>
  );
}

/** Pairs-weighted mean |Δ| vs hand grades + exact/within-1 shares; a calibration CTA
 * when no hand grades exist yet for this method. */
function AgreementRow({ m, onGoToReview }: { m: MethodAgg; onGoToReview: () => void }) {
  if (m.madMicros === null || m.pairs === 0) {
    return (
      <button
        type="button"
        onClick={onGoToReview}
        className="text-left text-xs font-medium text-indigo-600 hover:underline"
      >
        No hand grades yet — grade a few for calibration →
      </button>
    );
  }
  return (
    <div>
      <span className="tabular-nums">off by {microsToFixed(m.madMicros, 2)} pts on average</span>
      <div className="mt-0.5 text-xs text-neutral-500 tabular-nums">
        exact {countPercent(m.exactMatches, m.pairs)} · within 1 pt{" "}
        {countPercent(m.withinOne, m.pairs)} · {m.pairs} hand-graded answer
        {m.pairs === 1 ? "" : "s"}
      </div>
    </div>
  );
}

/** Override rate over answers with officials; absence explicitly reads "no official
 * grades yet" — never a fake 0% (OverrideRateRow doc). */
function OverrideRow({ override }: { override: OverrideRateRow | undefined }) {
  if (!override) {
    return <span className="text-xs text-neutral-400">no official grades yet</span>;
  }
  return (
    <div>
      <span className="tabular-nums">{rateToPercent(override.override_rate)}</span>
      <div className="mt-0.5 text-xs text-neutral-500 tabular-nums">
        {override.human_overrides} of {override.answers} answer
        {override.answers === 1 ? "" : "s"} with officials
      </div>
      {override.human_overrides > 0 && (
        <div className="mt-0.5 text-xs text-neutral-500 tabular-nums">
          {override.scored_disagreements > 0 && (
            <span>
              {override.scored_disagreements} replaced a scored AI grade
              {override.mean_abs_diff ? ` (off by ${override.mean_abs_diff} pts)` : ""}
            </span>
          )}
          {override.scored_disagreements > 0 && override.filled_blanks > 0 ? " · " : ""}
          {override.filled_blanks > 0 && (
            <span>{override.filled_blanks} filled an AI abstention</span>
          )}
        </div>
      )}
    </div>
  );
}

// One stacked 4-segment bar: how sure the AI itself was, in plain words.
const CONF_SEGMENTS: Array<{
  key: "confHigh" | "confMedium" | "confLow" | "confIllegible";
  short: string;
  title: string;
  cls: string;
}> = [
  {
    key: "confHigh",
    short: "high",
    title: "high — the AI was sure it read and graded the answer correctly",
    cls: "bg-green-500",
  },
  {
    key: "confMedium",
    short: "medium",
    title: "medium — the AI was mostly sure",
    cls: "bg-neutral-400",
  },
  {
    key: "confLow",
    short: "low",
    title: "low — the AI was unsure; worth a human look",
    cls: "bg-amber-500",
  },
  {
    key: "confIllegible",
    short: "unreadable",
    title: "unreadable — the AI could not read the handwriting",
    cls: "bg-red-500",
  },
];

function ConfidenceBar({ m }: { m: MethodAgg }) {
  const total = m.confHigh + m.confMedium + m.confLow + m.confIllegible;
  if (total === 0) {
    return <span className="text-xs text-neutral-400">—</span>;
  }
  return (
    <div>
      <div className="flex h-2 w-full overflow-hidden rounded-full bg-neutral-200">
        {CONF_SEGMENTS.map((seg) =>
          m[seg.key] > 0 ? (
            <div
              key={seg.key}
              title={`${m[seg.key]} of ${total} — ${seg.title}`}
              className={seg.cls}
              style={{ width: `${(m[seg.key] / total) * 100}%` }}
            />
          ) : null,
        )}
      </div>
      <div className="mt-1 text-xs text-neutral-500 tabular-nums">
        {CONF_SEGMENTS.filter((seg) => m[seg.key] > 0)
          .map((seg) => `${m[seg.key]} ${seg.short}`)
          .join(" · ")}
      </div>
    </div>
  );
}

/** Actual spend across this method-version's runs on this assessment: total + per
 * answer (total ÷ succeeded items). "—" while runs are loading or none are listed. */
function CostRow({
  methodVersionId,
  runs,
}: {
  methodVersionId: number;
  runs: RunListRow[] | null;
}) {
  const mine = (runs ?? []).filter((r) => r.method_version_id === methodVersionId);
  if (mine.length === 0) {
    return <span className="text-xs text-neutral-400">—</span>;
  }
  let total = 0n;
  let succeeded = 0;
  for (const r of mine) {
    total += toMicros(r.cost_usd) ?? 0n;
    succeeded += r.counts.succeeded ?? 0;
  }
  return (
    <span className="tabular-nums">
      ${microsToString(total)} total
      {succeeded > 0 && (
        <span className="text-xs text-neutral-500">
          {" "}
          · ${microsToString(total / BigInt(succeeded))} / answer
        </span>
      )}
    </span>
  );
}
