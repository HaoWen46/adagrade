// Score distribution: pure-SVG 10-bucket histogram + mean/σ/%zero/%max summary line
// (trust spec §5, D38). No chart library — plain SVG bars sized from the server's
// exact-decimal stats, never re-derived with float math on the client.
//
// The response's `source` field distinguishes reviewed human ("official") grades from
// a same-shape fallback computed over one run's raw AI records ("ai_fallback") when the
// problem has no official grades yet — that fallback gets an unmissable badge so nobody
// mistakes unreviewed AI output for ground truth.

import type { ScoreDistributionResponse } from "../lib/types";
import { Badge, cx } from "./ui";

const BAR_COUNT = 10;
const CHART_WIDTH = 320;
const CHART_HEIGHT = 96;
const BAR_GAP = 3;

function StatBox({
  label,
  value,
  noTransform,
}: {
  label: string;
  value: string;
  /** B11: `uppercase` turns the std-dev label "σ" into the summation symbol "Σ" — this
   * stat alone renders its label as-typed; every other StatBox stays uppercase. */
  noTransform?: boolean;
}) {
  return (
    <div className="min-w-[4.5rem]">
      <div
        className={cx(
          "text-[10px] font-medium tracking-wide text-neutral-400",
          noTransform ? "normal-case" : "uppercase",
        )}
      >
        {label}
      </div>
      <div className="tabular-nums text-neutral-800">{value}</div>
    </div>
  );
}

export function ScoreDistribution({ data }: { data: ScoreDistributionResponse }) {
  if (data.source === "none" || data.total === null) {
    return (
      <div className="rounded-md border border-neutral-200 bg-neutral-50 px-3 py-2 text-xs text-neutral-400">
        No grades yet for this problem.
      </div>
    );
  }

  const { total, histogram } = data;
  const maxCount = Math.max(1, ...histogram);
  const barWidth = (CHART_WIDTH - BAR_GAP * (BAR_COUNT - 1)) / BAR_COUNT;

  return (
    <div className="space-y-2">
      {data.source === "ai_fallback" && (
        <Badge tone="amber" className="font-semibold">
          AI fallback — no official grades reviewed yet
        </Badge>
      )}
      <svg
        viewBox={`0 0 ${CHART_WIDTH} ${CHART_HEIGHT}`}
        role="img"
        aria-label={`Score histogram: ${total.n} grade${total.n === 1 ? "" : "s"}, mean ${total.mean} of ${data.max_points}`}
        className="w-full max-w-xs"
      >
        {histogram.map((count, i) => {
          const h = (count / maxCount) * (CHART_HEIGHT - 14);
          const x = i * (barWidth + BAR_GAP);
          const y = CHART_HEIGHT - 14 - h;
          return (
            <g key={i}>
              <rect
                x={x}
                y={y}
                width={barWidth}
                height={Math.max(h, count > 0 ? 1 : 0)}
                className={data.source === "ai_fallback" ? "fill-amber-400" : "fill-indigo-500"}
                rx={1.5}
              >
                <title>
                  {count} answer{count === 1 ? "" : "s"} in bucket {i + 1} of {BAR_COUNT}
                </title>
              </rect>
              {count > 0 && (
                <text
                  x={x + barWidth / 2}
                  y={y - 3}
                  textAnchor="middle"
                  className="fill-neutral-500 text-[8px] tabular-nums"
                >
                  {count}
                </text>
              )}
            </g>
          );
        })}
        <line
          x1={0}
          y1={CHART_HEIGHT - 13}
          x2={CHART_WIDTH}
          y2={CHART_HEIGHT - 13}
          className="stroke-neutral-200"
          strokeWidth={1}
        />
        <text x={0} y={CHART_HEIGHT - 2} className="fill-neutral-400 text-[9px]">
          0
        </text>
        <text
          x={CHART_WIDTH}
          y={CHART_HEIGHT - 2}
          textAnchor="end"
          className="fill-neutral-400 text-[9px] tabular-nums"
        >
          {data.max_points}
        </text>
      </svg>
      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
        <StatBox label="n" value={String(total.n)} />
        <StatBox label="mean" value={total.mean} />
        <StatBox label="σ" value={total.stddev} noTransform />
        <StatBox label="% zero" value={`${total.zero_pct}%`} />
        <StatBox label="% max" value={`${total.max_pct}%`} />
      </div>
    </div>
  );
}
