// Review tab: per-problem grading rollups (D2 count summaries) linking into the
// per-problem review page. Each row expands to its score distribution (S3, trust
// spec §5) — the bare counts alone don't show whether scores cluster or spread.

import { useState } from "react";
import { Link } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";
import type { AssessmentDetailResponse, ProblemSummary, ScoreDistributionResponse } from "../lib/types";
import { Card, Spinner, TD, TH, Table, buttonClassName } from "../components/ui";
import { HelpTip } from "../components/HelpTip";
import { reviewCountsHelp } from "../lib/helpContent";
import { ScoreDistribution } from "../components/ScoreDistribution";

const COLS = 11;

export function ReviewTab({ assessmentId }: { assessmentId: string }) {
  const [expanded, setExpanded] = useState<number | null>(null);
  const summary = useQuery({
    queryKey: ["problem-summaries", assessmentId],
    queryFn: () =>
      api.get<{ problems: ProblemSummary[] }>(`/api/assessments/${assessmentId}/problems/summary`),
  });
  // Same key as AssessmentDetail's fetch so the cache is shared — no extra
  // request when this tab renders inside an already-loaded assessment page.
  const detail = useQuery({
    queryKey: ["assessment", assessmentId],
    queryFn: () => api.get<AssessmentDetailResponse>(`/api/assessments/${assessmentId}`),
    enabled: assessmentId !== "",
  });

  if (summary.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (summary.isError) {
    return (
      <Card>
        <p className="text-sm text-red-600">{summary.error.message}</p>
      </Card>
    );
  }

  const problems = summary.data.problems;
  const graded = problems.reduce((n, p) => n + p.ai_graded + p.human_graded, 0);
  const official = problems.reduce((n, p) => n + p.official_set, 0);
  const noFinalSource =
    graded > 0 && official === 0 && detail.data !== undefined && !detail.data.assessment.final_source_kind;

  return (
    <>
      {noFinalSource && (
        <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
          {graded} {graded === 1 ? "grade exists" : "grades exist"} but no final grading source is chosen, so
          nothing is official yet. Choose one on the{" "}
          <Link
            to={`/assessments/${assessmentId}?tab=publish`}
            className="font-medium text-amber-900 underline"
          >
            Publish tab
          </Link>
          .
        </p>
      )}
      <Table>
        <thead>
          <tr>
            <TH className="w-16">#</TH>
            <TH>Title</TH>
            <TH className="w-20 text-right">Max</TH>
            <TH className="w-20 text-right">
              <span className="inline-flex items-center gap-1.5">
                Answers <HelpTip title="Review counts">{reviewCountsHelp}</HelpTip>
              </span>
            </TH>
            <TH className="w-24 text-right">With pages</TH>
            <TH className="w-20 text-right">Official</TH>
            <TH className="w-16 text-right">AI</TH>
            <TH className="w-20 text-right">Human</TH>
            <TH className="w-20 text-right">Flagged</TH>
            <TH className="w-24 text-right">Published</TH>
            <TH className="w-20" />
          </tr>
        </thead>
        <tbody>
          {problems.length === 0 && (
            <tr>
              <TD colSpan={COLS} className="text-center text-neutral-400">
                No problems yet.
              </TD>
            </tr>
          )}
          {problems.map((p) => (
            <ProblemRow
              key={p.problem_id}
              assessmentId={assessmentId}
              problem={p}
              expanded={expanded === p.problem_id}
              onToggle={() => setExpanded(expanded === p.problem_id ? null : p.problem_id)}
            />
          ))}
        </tbody>
      </Table>
    </>
  );
}

function ProblemRow({
  assessmentId,
  problem: p,
  expanded,
  onToggle,
}: {
  assessmentId: string;
  problem: ProblemSummary;
  expanded: boolean;
  onToggle: () => void;
}) {
  const dist = useQuery({
    queryKey: ["score-distribution", p.problem_id],
    queryFn: () => api.get<ScoreDistributionResponse>(`/api/problems/${p.problem_id}/score-distribution`),
    enabled: expanded,
  });

  return (
    <>
      <tr onClick={onToggle} className="cursor-pointer hover:bg-neutral-50">
        <TD className="font-medium tabular-nums">
          <span className="mr-1.5 inline-block w-3 text-neutral-400">{expanded ? "▾" : "▸"}</span>
          {p.number}
        </TD>
        <TD>{p.title || <span className="text-neutral-400">untitled</span>}</TD>
        <TD className="text-right tabular-nums">{p.max_points}</TD>
        <TD className="text-right tabular-nums">{p.answers}</TD>
        <TD className="text-right tabular-nums">{p.with_pages}</TD>
        <TD className="text-right tabular-nums">
          {p.official_set}/{p.answers}
        </TD>
        <TD className="text-right tabular-nums">{p.ai_graded}</TD>
        <TD className="text-right tabular-nums">{p.human_graded}</TD>
        <TD className="text-right tabular-nums">
          {/* Flagged counts are the triage entry point: deep-link into the
              problem review with the flagged-only filter pre-applied. */}
          {p.flagged > 0 ? (
            <Link
              to={`/assessments/${assessmentId}/problems/${p.problem_id}/review?flagged=1`}
              onClick={(e) => e.stopPropagation()}
              title="Review flagged answers"
              className="font-medium text-amber-700 underline decoration-amber-300 hover:decoration-amber-700"
            >
              {p.flagged}
            </Link>
          ) : (
            p.flagged
          )}
        </TD>
        <TD className="text-right tabular-nums">{p.published}</TD>
        <TD className="text-right">
          <Link
            to={`/assessments/${assessmentId}/problems/${p.problem_id}/review`}
            onClick={(e) => e.stopPropagation()}
            className={buttonClassName("secondary", "px-2.5 py-1 text-xs")}
          >
            Review
          </Link>
        </TD>
      </tr>
      {expanded && (
        <tr>
          <TD colSpan={COLS} className="bg-neutral-50/60 px-4 py-3">
            {dist.isPending ? (
              <Spinner className="size-4" />
            ) : dist.isError ? (
              <p className="text-xs text-red-600">{dist.error.message}</p>
            ) : (
              <ScoreDistribution data={dist.data} />
            )}
          </TD>
        </tr>
      )}
    </>
  );
}
