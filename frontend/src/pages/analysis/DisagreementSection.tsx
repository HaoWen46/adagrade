// "Where methods disagree" (analysis redesign 2026-07-11, F1 §2): the per-answer
// spread between method-versions on the SAME work — rendered only when the
// backend found at least two method-versions with comparable records (both
// disagreement arrays arrive empty otherwise). All spreads are decimal strings.

import type { AnalysisDisagreement } from "../../lib/types";
import { TD, TH, Table } from "../../components/ui";
import { HelpTip } from "../../components/HelpTip";
import { GapAnswersTable } from "./shared";

const disagreementHelp = (
  <>
    <p>
      This compares the scores different methods gave the <strong>same</strong> answer (each
      method&apos;s latest score, current rubric only). The <strong>gap</strong> is the highest
      score minus the lowest for that answer.
    </p>
    <p>
      A <strong>big gap</strong> is a gap of at least 1 point — or 10% of the problem&apos;s
      maximum, whichever is larger. Those answers would get meaningfully different grades
      depending on which method you trust, so they are the best place to spot-check: open one,
      look at the pages, and see which method got it right.
    </p>
  </>
);

export function DisagreementSection({ disagreement }: { disagreement: AnalysisDisagreement }) {
  const { problems, top_answers: topAnswers } = disagreement;
  if (problems.length === 0 && topAnswers.length === 0) return null;

  const compared = problems.reduce((n, p) => n + p.answers_compared, 0);
  const bigGaps = problems.reduce((n, p) => n + p.big_gap_count, 0);

  return (
    <section className="space-y-3">
      <h2 className="flex items-center gap-1.5 text-sm font-semibold text-neutral-900">
        Where methods disagree
        <HelpTip title="Where methods disagree">{disagreementHelp}</HelpTip>
      </h2>
      <p className="text-sm text-neutral-600">
        {bigGaps > 0 ? (
          <>
            On <span className="font-semibold tabular-nums">{bigGaps}</span> of{" "}
            <span className="tabular-nums">{compared}</span> answers graded by two or more
            methods, the scores differ by at least a point (or 10% of the problem&apos;s
            maximum) — those students&apos; grades depend on which method you pick.
          </>
        ) : (
          <>
            The methods graded <span className="tabular-nums">{compared}</span> of the same
            answers and no gap reached a point (or 10% of the problem&apos;s maximum,
            whichever is larger) — they largely agree.
          </>
        )}
      </p>

      <Table>
        <thead>
          <tr>
            <TH className="w-24">Problem</TH>
            <TH className="w-20 text-right">Max</TH>
            <TH className="w-36 text-right">Answers compared</TH>
            <TH className="w-32 text-right">Median gap</TH>
            <TH className="w-28 text-right">Big gaps</TH>
          </tr>
        </thead>
        <tbody>
          {problems.map((p) => (
            <tr key={p.problem_id} className="hover:bg-neutral-50">
              <TD className="tabular-nums">{p.problem_number}</TD>
              <TD className="text-right tabular-nums">{p.max_points}</TD>
              <TD className="text-right tabular-nums">{p.answers_compared}</TD>
              <TD className="text-right tabular-nums">{p.median_spread || "—"}</TD>
              <TD
                className={
                  p.big_gap_count > 0
                    ? "text-right font-medium tabular-nums text-amber-700"
                    : "text-right tabular-nums"
                }
              >
                {p.big_gap_count}
              </TD>
            </tr>
          ))}
        </tbody>
      </Table>

      {topAnswers.length > 0 && (
        <div className="space-y-1.5">
          <h3 className="text-xs font-semibold tracking-wide text-neutral-500 uppercase">
            {/* "worth a spot-check" only when a real gap exists — a table of zero
                gaps under that headline would contradict the sentence above. */}
            {bigGaps > 0 ? "Largest gaps — worth a spot-check" : "Largest differences"}
          </h3>
          <GapAnswersTable rows={topAnswers} showProblem />
        </div>
      )}
    </section>
  );
}
