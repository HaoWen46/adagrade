// Run history (analysis redesign 2026-07-11, F1 §4): the per-run cost table,
// now behind a collapsed disclosure — supplementary evidence, not headline. The
// runs list arrives already filtered server-side (GET /api/runs?assessment_id=…),
// so the old "most recent 50 system-wide" caveat is gone; only the server's own
// row cap can truncate, and that gets an honest note.

import { useState } from "react";
import type { RunListRow } from "../../lib/types";
import { microsToString, toMicros } from "../../lib/decimal";
import { Card, Spinner, TD, TH, Table } from "../../components/ui";
import { HelpTip } from "../../components/HelpTip";
import { analysisHelp } from "../../lib/helpContent";

/** Keep in sync with AnalysisTab's runs query: the server-side LIMIT it asks for. */
export const RUNS_LIMIT = 200;

export function RunHistory({
  runs,
  isPending,
  errorMessage,
}: {
  runs: RunListRow[] | null;
  isPending: boolean;
  errorMessage: string | null;
}) {
  const [open, setOpen] = useState(false);
  return (
    <section className="space-y-3">
      <div className="flex items-center gap-1.5">
        <button
          type="button"
          onClick={() => setOpen(!open)}
          aria-expanded={open}
          className="flex items-center gap-1.5 text-sm font-semibold text-neutral-900"
        >
          <span className="inline-block w-3 text-neutral-400">{open ? "▾" : "▸"}</span>
          Run history
          {runs !== null && (
            <span className="font-normal text-neutral-400 tabular-nums">
              ({runs.length} run{runs.length === 1 ? "" : "s"})
            </span>
          )}
        </button>
        <HelpTip title="Run history">{analysisHelp.cost}</HelpTip>
      </div>
      {open &&
        (isPending ? (
          <div className="flex justify-center py-6">
            <Spinner className="size-5" />
          </div>
        ) : errorMessage !== null ? (
          <Card>
            <p className="text-sm text-red-600">{errorMessage}</p>
          </Card>
        ) : runs === null || runs.length === 0 ? (
          <Card>
            <p className="text-sm text-neutral-400">No runs for this assessment yet.</p>
          </Card>
        ) : (
          <div className="space-y-2">
            <CostTable runs={runs} />
            {runs.length >= RUNS_LIMIT && (
              <p className="text-xs text-neutral-500">
                Showing the {RUNS_LIMIT} most recent runs — older runs for this assessment may
                not appear.
              </p>
            )}
          </div>
        ))}
    </section>
  );
}

/** Divides a run's total cost by its succeeded-item count for a per-answer average;
 * null when there's nothing to divide by (never a fake $0 — matches D35's null-not-zero
 * convention for unpriced/empty data). */
function costPerAnswer(costUsd: string, succeeded: number): string | null {
  if (succeeded <= 0) return null;
  const micros = toMicros(costUsd);
  if (micros === null) return null;
  return microsToString(micros / BigInt(succeeded));
}

function CostTable({ runs }: { runs: RunListRow[] }) {
  return (
    <Table>
      <thead>
        <tr>
          <TH className="w-20">Run</TH>
          <TH>Method</TH>
          <TH className="w-28">Status</TH>
          <TH className="w-28 text-right">Cost (USD)</TH>
          <TH className="w-28 text-right">Cost / answer</TH>
          <TH className="w-36 text-right">Tokens in / out</TH>
        </tr>
      </thead>
      <tbody>
        {runs.map((r) => {
          const succeeded = r.counts.succeeded ?? 0;
          const perAnswer = costPerAnswer(r.cost_usd, succeeded);
          return (
            <tr key={r.id} className="hover:bg-neutral-50">
              <TD className="tabular-nums">#{r.id}</TD>
              <TD>
                {r.method_name}{" "}
                <span className="text-xs text-neutral-400 tabular-nums">v{r.method_version}</span>
              </TD>
              <TD className="text-xs text-neutral-600">{r.status}</TD>
              <TD className="text-right tabular-nums">${r.cost_usd}</TD>
              <TD className="text-right tabular-nums">{perAnswer !== null ? `$${perAnswer}` : "—"}</TD>
              <TD className="text-right text-xs whitespace-nowrap tabular-nums">
                {r.input_tokens.toLocaleString()} / {r.output_tokens.toLocaleString()}
              </TD>
            </tr>
          );
        })}
      </tbody>
    </Table>
  );
}
