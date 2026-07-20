// Shared building blocks for the Analysis tab sections (analysis redesign
// 2026-07-11, F1). Decimal strings render verbatim (D4); aggregation across
// problems uses the bigint-micros helpers — floats appear only as CSS bar
// widths and count-share percentages.

import { Link } from "react-router";
import type {
  DisagreementAnswerRow,
  HumanAgreementRow,
  MethodProblemStat,
} from "../../lib/types";
import { toCents, toMicros } from "../../lib/decimal";
import { PolicyBadge } from "../../components/PolicyBadge";
import { TD, TH, Table, buttonClassName, cx } from "../../components/ui";

// --- exact-decimal display helpers --------------------------------------------------

/** Rounds integer micros (1e6 scale) to a fixed 1- or 2-dp decimal string — exact
 * bigint math, never parseFloat on values that crossed the wire as strings. */
export function microsToFixed(micros: bigint, dp: 1 | 2): string {
  const unit = dp === 1 ? 100_000n : 10_000n;
  const neg = micros < 0n;
  const abs = neg ? -micros : micros;
  const scaled = (abs + unit / 2n) / unit; // rounded, in 10^-dp units
  const base = dp === 1 ? 10n : 100n;
  const whole = scaled / base;
  const frac = (scaled % base).toString().padStart(dp, "0");
  return `${neg ? "-" : ""}${whole}.${frac}`;
}

/** Renders a [0,1] decimal-string rate ("0.5000") as a 1dp percent ("50.0%") using exact
 * integer micros math — never parseFloat on a rate that came across the wire as a string. */
export function rateToPercent(rate: string): string {
  const micros = toMicros(rate);
  if (micros === null) return "—";
  // *1000 then render with a fixed decimal point: micros are already 1e6-scaled, so
  // (micros * 100) / 1e6 = percent, computed as tenths-of-a-percent for 1dp display.
  const tenthsOfPercent = (micros * 1000n) / 1_000_000n;
  const whole = tenthsOfPercent / 10n;
  const frac = tenthsOfPercent % 10n;
  return `${whole}.${frac}%`;
}

/** "n (percent%)" out of `total`; bare count when the denominator is empty. */
export function withShare(n: number, total: number): string {
  if (total <= 0) return String(n);
  return `${n} (${Math.round((n / total) * 100)}%)`;
}

/** Integer count share as a whole percent; "—" when the denominator is empty. */
export function countPercent(n: number, total: number): string {
  if (total <= 0) return "—";
  return `${Math.round((n / total) * 100)}%`;
}

// --- per-method rollup ---------------------------------------------------------------

/** One method-version's assessment-wide rollup: stats summed over problems, agreement
 * combined pairs-weighted. Drives the report cards and the matrix column order. */
export interface MethodAgg {
  methodVersionId: number;
  methodId: number;
  name: string;
  version: number;
  policy: string;
  records: number;
  confHigh: number;
  confMedium: number;
  confLow: number;
  confIllegible: number;
  /** Hand-graded comparison pairs across all problems (0 = no hand grades yet). */
  pairs: number;
  exactMatches: number;
  withinOne: number;
  /** Pairs-weighted mean |Δ| vs hand grades in 1e6-micros; null = no comparable pairs. */
  madMicros: bigint | null;
}

/**
 * Rolls the per-problem stats + agreement rows up to one entry per method-version.
 * Order: agreement (weighted mean |Δ|) ascending when hand grades exist — methods
 * without pairs after, by name — else name, then version.
 */
export function aggregateMethods(
  stats: MethodProblemStat[],
  agreement: HumanAgreementRow[],
): MethodAgg[] {
  type Acc = MethodAgg & { madNum: bigint; madPairs: bigint };
  const byMv = new Map<number, Acc>();
  for (const s of stats) {
    let m = byMv.get(s.method_version_id);
    if (!m) {
      m = {
        methodVersionId: s.method_version_id,
        methodId: s.method_id,
        name: s.method_name,
        version: s.method_version,
        policy: s.policy,
        records: 0,
        confHigh: 0,
        confMedium: 0,
        confLow: 0,
        confIllegible: 0,
        pairs: 0,
        exactMatches: 0,
        withinOne: 0,
        madMicros: null,
        madNum: 0n,
        madPairs: 0n,
      };
      byMv.set(s.method_version_id, m);
    }
    m.records += s.records;
    m.confHigh += s.conf_high;
    m.confMedium += s.conf_medium;
    m.confLow += s.conf_low;
    m.confIllegible += s.conf_illegible;
  }
  for (const a of agreement) {
    const m = byMv.get(a.method_version_id);
    if (!m) continue; // agreement without stats can't happen, but never crash on it
    m.pairs += a.pairs;
    m.exactMatches += a.exact_matches;
    m.withinOne += a.within_one;
    const micros = toMicros(a.mean_abs_diff);
    if (micros !== null && a.pairs > 0) {
      m.madNum += micros * BigInt(a.pairs);
      m.madPairs += BigInt(a.pairs);
    }
  }
  const out = [...byMv.values()];
  for (const m of out) {
    if (m.madPairs > 0n) m.madMicros = m.madNum / m.madPairs;
  }
  const anyHand = out.some((m) => m.madMicros !== null);
  out.sort((a, b) => {
    if (anyHand) {
      if (a.madMicros !== null && b.madMicros !== null && a.madMicros !== b.madMicros) {
        return a.madMicros < b.madMicros ? -1 : 1;
      }
      if ((a.madMicros === null) !== (b.madMicros === null)) {
        return a.madMicros === null ? 1 : -1;
      }
    }
    return a.name.localeCompare(b.name) || a.version - b.version;
  });
  return out;
}

// --- score statistics table (verbatim pre-redesign table, now inside expansions) ------

export function StatsTable({ rows, max }: { rows: MethodProblemStat[]; max: string }) {
  return (
    <Table>
      <thead>
        <tr>
          <TH>Method</TH>
          <TH className="w-20 text-right">Records</TH>
          <TH className="w-28">Mean</TH>
          <TH className="w-20 text-right">Median</TH>
          <TH className="w-20 text-right">Spread</TH>
          <TH className="w-14 text-right">0s</TH>
          <TH className="w-14 text-right">Max</TH>
          <TH className="w-44">Confidence</TH>
          <TH className="w-32 text-right">Tokens in / out</TH>
        </tr>
      </thead>
      <tbody>
        {rows.map((s) => (
          <tr key={s.method_version_id} className="hover:bg-neutral-50">
            <TD>
              {s.method_name}{" "}
              <span className="text-xs text-neutral-400 tabular-nums">v{s.method_version}</span>{" "}
              {s.policy && <PolicyBadge policy={s.policy} />}
            </TD>
            <TD className="text-right tabular-nums">{s.records}</TD>
            <TD>
              <MeanCell mean={s.mean_total} max={max} />
            </TD>
            <TD className="text-right tabular-nums">{s.median_total || "—"}</TD>
            <TD className="text-right tabular-nums">{s.stddev_total || "—"}</TD>
            <TD className="text-right tabular-nums">{s.zeros}</TD>
            <TD className="text-right tabular-nums">{s.maxes}</TD>
            <TD>
              <ConfidenceCell stat={s} />
            </TD>
            <TD className="text-right text-xs whitespace-nowrap tabular-nums">
              {s.input_tokens.toLocaleString()} / {s.output_tokens.toLocaleString()}
            </TD>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}

/** Mean as a decimal string plus a thin mean/max bar (the only place a float is OK). */
export function MeanCell({ mean, max }: { mean: string; max: string }) {
  const meanCents = toCents(mean);
  const maxCents = toCents(max);
  const width =
    meanCents !== null && maxCents !== null && maxCents > 0
      ? Math.min(100, Math.max(0, (meanCents / maxCents) * 100))
      : null;
  return (
    <div>
      <span className="tabular-nums">{mean || "—"}</span>
      {width !== null && (
        <div className="mt-1 h-1 max-w-24 overflow-hidden rounded-full bg-neutral-200">
          <div className="h-full rounded-full bg-indigo-600" style={{ width: `${width}%` }} />
        </div>
      )}
    </div>
  );
}

const CONF_PARTS: Array<{
  key: "conf_high" | "conf_medium" | "conf_low" | "conf_illegible";
  label: string;
  dot: string;
}> = [
  { key: "conf_high", label: "high", dot: "bg-green-500" },
  { key: "conf_medium", label: "medium", dot: "bg-neutral-400" },
  { key: "conf_low", label: "low", dot: "bg-amber-500" },
  { key: "conf_illegible", label: "illegible", dot: "bg-red-500" },
];

function ConfidenceCell({ stat }: { stat: MethodProblemStat }) {
  return (
    <span className="flex items-center gap-2.5">
      {CONF_PARTS.map((p) => (
        <span
          key={p.key}
          title={p.label}
          className="inline-flex items-center gap-1 text-xs tabular-nums"
        >
          <span className={cx("size-1.5 shrink-0 rounded-full", p.dot)} />
          {stat[p.key]}
        </span>
      ))}
    </span>
  );
}

// --- human agreement table (verbatim, now inside matrix expansions) -------------------

export function AgreementTable({ rows }: { rows: HumanAgreementRow[] }) {
  return (
    <Table>
      <thead>
        <tr>
          <TH className="w-24">Problem</TH>
          <TH>Method</TH>
          <TH className="w-28 text-right">Graded pairs</TH>
          <TH className="w-32 text-right">Avg difference</TH>
          <TH className="w-32 text-right">Exact match</TH>
          <TH className="w-32 text-right">Within 1 pt</TH>
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => (
          <tr key={`${r.problem_number}:${r.method_version_id}`} className="hover:bg-neutral-50">
            <TD className="tabular-nums">{r.problem_number}</TD>
            <TD>
              {r.method_name}{" "}
              <span className="text-xs text-neutral-400 tabular-nums">v{r.method_version}</span>
            </TD>
            <TD className="text-right tabular-nums">{r.pairs}</TD>
            <TD className="text-right tabular-nums">{r.mean_abs_diff || "—"}</TD>
            <TD className="text-right tabular-nums">{withShare(r.exact_matches, r.pairs)}</TD>
            <TD className="text-right tabular-nums">{withShare(r.within_one, r.pairs)}</TD>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}

// --- largest-gap answers table (disagreement section + matrix expansions) -------------

export function GapAnswersTable({
  rows,
  showProblem,
}: {
  rows: DisagreementAnswerRow[];
  showProblem: boolean;
}) {
  return (
    <Table>
      <thead>
        <tr>
          <TH className="w-32">Student</TH>
          {showProblem && <TH className="w-24">Problem</TH>}
          <TH>Score by method</TH>
          <TH className="w-20 text-right">Gap</TH>
          <TH className="w-20" />
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => (
          <tr key={r.answer_id} className="hover:bg-neutral-50">
            <TD className="tabular-nums">{r.student_display}</TD>
            {showProblem && <TD className="tabular-nums">{r.problem_number}</TD>}
            <TD className="text-xs">
              {r.scores.map((s, i) => (
                <span key={s.method_version_id} className="whitespace-nowrap">
                  {i > 0 && <span className="text-neutral-300"> · </span>}
                  {s.method_name}{" "}
                  <span className="font-medium text-neutral-900 tabular-nums">{s.total}</span>
                </span>
              ))}
            </TD>
            <TD className="text-right font-medium tabular-nums">{r.spread}</TD>
            <TD className="text-right">
              <Link
                to={`/answers/${r.answer_id}`}
                className={buttonClassName("secondary", "px-2.5 py-1 text-xs")}
              >
                Open
              </Link>
            </TD>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}
