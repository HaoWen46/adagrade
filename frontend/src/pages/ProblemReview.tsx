// Problem review: dense per-student table for one problem (status derived
// server-side, D2); row click drills into the answer view.
//
// Filter + page state lives in the URL (?q= &flagged=1 &page=) so browser-back
// from an answer restores this exact view, and other pages (ReviewTab's flagged
// counts, ConsensusTab's triage link) can deep-link into a pre-filtered list.

import { useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";
import type {
  AnswerResponse,
  AnswerStatus,
  AssessmentDetailResponse,
  ProblemStudentRow,
} from "../lib/types";
import { SafeImage } from "../components/SafeImage";
import {
  Badge,
  Card,
  Input,
  Pager,
  Spinner,
  TD,
  TH,
  Table,
  buttonClassName,
  type BadgeTone,
} from "../components/ui";
import { HelpTip } from "../components/HelpTip";
import { answerStatusHelp } from "../lib/helpContent";
import { answerStatusLabel, flagLabel } from "../lib/labels";

const PAGE_SIZE = 50;

export const STATUS_TONES: Record<AnswerStatus, BadgeTone> = {
  no_submission: "red",
  ungraded: "neutral",
  graded: "amber",
  official_set: "green",
  published: "indigo",
};

export function ProblemReview() {
  const { aid = "", pid = "" } = useParams();
  const navigate = useNavigate();
  const problemId = Number(pid);
  // URL-backed view state (see header comment). replace:true keeps typing in
  // the search box from flooding the history stack.
  const [searchParams, setSearchParams] = useSearchParams();
  const search = searchParams.get("q") ?? "";
  const flaggedOnly = searchParams.get("flagged") === "1";
  // Same-score side-by-side (guide §6.3): ?view=scores groups the list by
  // official total; ?score= deep-links one expanded bucket.
  const scoresView = searchParams.get("view") === "scores";
  const expandedScore = searchParams.get("score");
  const pageParam = Number(searchParams.get("page"));
  const page = Number.isInteger(pageParam) && pageParam > 0 ? pageParam : 0;
  const setParams = (patch: Record<string, string | null>) => {
    const next = new URLSearchParams(searchParams);
    for (const [k, v] of Object.entries(patch)) {
      if (v === null || v === "") next.delete(k);
      else next.set(k, v);
    }
    setSearchParams(next, { replace: true });
  };

  const detail = useQuery({
    queryKey: ["assessment", aid],
    queryFn: () => api.get<AssessmentDetailResponse>(`/api/assessments/${aid}`),
    enabled: aid !== "",
  });
  const students = useQuery({
    queryKey: ["problem-students", problemId],
    queryFn: () => api.get<{ students: ProblemStudentRow[] }>(`/api/problems/${problemId}/students`),
    enabled: Number.isInteger(problemId),
  });

  const problem = detail.data?.problems.find((p) => p.id === problemId);

  if (students.isPending || detail.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (students.isError || detail.isError) {
    return (
      <div className="mx-auto max-w-5xl">
        <Card title="Problem review">
          <p className="text-sm text-red-600">
            {students.error?.message ?? detail.error?.message}
          </p>
          <Link
            to={`/assessments/${aid}`}
            className="mt-2 inline-block text-sm text-indigo-600 hover:underline"
          >
            Back to assessment
          </Link>
        </Card>
      </div>
    );
  }

  const rows = students.data.students;
  // Progress count stays on the FULL list — search must not change it.
  const officialSet = rows.filter((r) => r.official_total !== undefined).length;

  const q = search.toLowerCase();
  const flaggedCount = rows.filter((r) => r.flags.length > 0).length;
  const filtered = rows.filter(
    (s) =>
      (!flaggedOnly || s.flags.length > 0) &&
      (q === "" || s.student_id.toLowerCase().startsWith(q)),
  );
  const pageCount = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const safePage = Math.min(page, pageCount - 1); // derive-clamp, no setState in render
  const pageRows = filtered.slice(safePage * PAGE_SIZE, (safePage + 1) * PAGE_SIZE);

  return (
    <div className="mx-auto max-w-5xl space-y-4">
      <div>
        <Link
          to={`/assessments/${aid}?tab=review`}
          className="text-xs text-neutral-500 hover:text-neutral-800"
        >
          ← {detail.data.assessment.name}
        </Link>
        <div className="mt-1 flex items-center gap-2.5">
          <h1 className="text-lg font-semibold text-neutral-900">
            Problem {problem?.number ?? pid}
            {problem?.title && ` — ${problem.title}`}
          </h1>
          {problem && <Badge tone="indigo">{problem.max_points} pts</Badge>}
          <span className="text-xs text-neutral-500 tabular-nums">
            {officialSet}/{rows.length} official
          </span>
          <Link
            to={`/runs?launch=1&assessment_id=${aid}&problem_id=${pid}`}
            className={buttonClassName("secondary", "ml-auto px-2.5 py-1 text-xs")}
          >
            Grade with AI…
          </Link>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-4">
        <Input
          className="w-56"
          placeholder="Search by student ID…"
          value={search}
          onChange={(e) => setParams({ q: e.target.value, page: null })}
        />
        <label className="flex items-center gap-1.5 text-sm text-neutral-700">
          <input
            type="checkbox"
            checked={flaggedOnly}
            onChange={(e) => setParams({ flagged: e.target.checked ? "1" : null, page: null })}
            className="size-3.5 accent-indigo-600"
          />
          Flagged only
          <span className="text-xs text-neutral-400 tabular-nums">({flaggedCount})</span>
        </label>
        <div className="ml-auto flex rounded-md border border-neutral-200 text-xs">
          <button
            type="button"
            onClick={() => setParams({ view: null, score: null })}
            className={
              !scoresView ? "rounded-l-md bg-indigo-600 px-2.5 py-1 text-white" : "px-2.5 py-1 text-neutral-600 hover:bg-neutral-50"
            }
          >
            Table
          </button>
          <button
            type="button"
            onClick={() => setParams({ view: "scores", page: null })}
            className={
              scoresView ? "rounded-r-md bg-indigo-600 px-2.5 py-1 text-white" : "px-2.5 py-1 text-neutral-600 hover:bg-neutral-50"
            }
            title="Group answers by official score and compare same-score answers side by side (consistency check)"
          >
            By score
          </button>
        </div>
      </div>

      {scoresView ? (
        <ScoreGroups
          rows={filtered}
          expanded={expandedScore}
          onExpand={(score) => setParams({ score })}
        />
      ) : (
      <Table>
        <thead>
          <tr>
            <TH className="w-32">Student ID</TH>
            <TH>Name</TH>
            <TH className="w-28">
              <span className="inline-flex items-center gap-1.5">
                Status <HelpTip title="Answer status">{answerStatusHelp}</HelpTip>
              </span>
            </TH>
            <TH>Flags</TH>
            <TH className="w-16 text-right">Pages</TH>
            <TH className="w-20 text-right">Records</TH>
            <TH className="w-28 text-right">Official</TH>
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && (
            <tr>
              <TD colSpan={7} className="text-center text-neutral-400">
                No answers for this problem yet — upload submissions first.
              </TD>
            </tr>
          )}
          {rows.length > 0 && filtered.length === 0 && (
            <tr>
              <TD colSpan={7} className="text-center text-neutral-400">
                {flaggedOnly
                  ? search !== ""
                    ? "No flagged answers match this ID."
                    : "No flagged answers on this problem."
                  : "No students match this ID."}
              </TD>
            </tr>
          )}
          {pageRows.map((s) => (
            <tr
              key={s.answer_id}
              onClick={() => void navigate(`/answers/${s.answer_id}`)}
              className="cursor-pointer hover:bg-neutral-50"
            >
              <TD className="font-medium tabular-nums">{s.student_id}</TD>
              <TD>{s.name}</TD>
              <TD>
                <Badge tone={STATUS_TONES[s.status] ?? "neutral"}>
                  {answerStatusLabel(s.status)}
                </Badge>
              </TD>
              <TD>
                {s.flags.length === 0 ? (
                  <span className="text-neutral-300">—</span>
                ) : (
                  <span className="flex flex-wrap gap-1">
                    {s.flags.map((f) => (
                      <Badge key={f} tone="amber" className="text-[10px]">
                        {flagLabel(f)}
                      </Badge>
                    ))}
                  </span>
                )}
              </TD>
              <TD className="text-right tabular-nums">{s.page_count}</TD>
              <TD className="text-right tabular-nums">{s.record_count}</TD>
              <TD className="text-right tabular-nums">
                {s.official_total !== undefined ? (
                  <>
                    <span className="font-medium">{s.official_total}</span>
                    {s.official_source && (
                      <span className="ml-1 text-[11px] text-neutral-400">
                        ({s.official_source})
                      </span>
                    )}
                  </>
                ) : (
                  <span className="text-neutral-300">—</span>
                )}
              </TD>
            </tr>
          ))}
        </tbody>
      </Table>
      )}

      {!scoresView && (
        <div className="flex items-center justify-between">
          <span className="text-xs text-neutral-500 tabular-nums">
            {search !== "" || flaggedOnly
              ? `${filtered.length} of ${rows.length} students`
              : `${rows.length} students`}
          </span>
          <Pager
            page={safePage}
            pageCount={pageCount}
            onPage={(p) => setParams({ page: p === 0 ? null : String(p) })}
          />
        </div>
      )}
    </div>
  );
}

// --- same-score side-by-side (guide §6.3) --------------------------------------------

/** Cards rendered per expanded bucket before "Show all" widens it — keeps the
 * per-answer detail fetches bounded, with the overflow stated explicitly. */
const COMPARE_INITIAL = 12;

/** Groups the (already search/flag-filtered) rows by official total. The
 * consistency-check flow: expand one score, eyeball the masked pages side by
 * side, click through where a deduction looks inconsistent. */
function ScoreGroups({
  rows,
  expanded,
  onExpand,
}: {
  rows: ProblemStudentRow[];
  expanded: string | null;
  onExpand: (score: string | null) => void;
}) {
  const buckets = new Map<string, ProblemStudentRow[]>();
  for (const r of rows) {
    const key = r.official_total ?? "ungraded";
    buckets.set(key, [...(buckets.get(key) ?? []), r]);
  }
  const keys = [...buckets.keys()].sort((a, b) => {
    if (a === "ungraded") return 1;
    if (b === "ungraded") return -1;
    return Number(b) - Number(a); // numeric, descending
  });

  if (rows.length === 0) {
    return (
      <Card title="By score">
        <p className="text-sm text-neutral-400">No answers match the current filters.</p>
      </Card>
    );
  }
  return (
    <div className="space-y-2">
      {keys.map((key) => {
        const group = buckets.get(key) ?? [];
        const open = expanded === key;
        return (
          <div key={key} className="rounded-md border border-neutral-200">
            <button
              type="button"
              onClick={() => onExpand(open ? null : key)}
              className="flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm hover:bg-neutral-50"
            >
              <span className="w-20 font-medium tabular-nums">
                {key === "ungraded" ? "ungraded" : key}
              </span>
              <span className="text-xs text-neutral-500 tabular-nums">
                {group.length} answer{group.length === 1 ? "" : "s"}
              </span>
              <span className="ml-auto text-xs text-neutral-400">{open ? "▾" : "▸"}</span>
            </button>
            {open && <CompareStrip group={group} />}
          </div>
        );
      })}
    </div>
  );
}

function CompareStrip({ group }: { group: ProblemStudentRow[] }) {
  const [showAll, setShowAll] = useState(false);
  const visible = showAll ? group : group.slice(0, COMPARE_INITIAL);
  const hidden = group.length - visible.length;
  return (
    <div className="border-t border-neutral-100 p-3">
      <div className="flex gap-3 overflow-x-auto pb-1.5">
        {visible.map((s) => (
          <CompareCard key={s.answer_id} row={s} />
        ))}
      </div>
      {hidden > 0 && (
        <button
          type="button"
          onClick={() => setShowAll(true)}
          className="mt-1.5 text-xs text-indigo-600 hover:underline"
        >
          Show all {group.length} (+{hidden} more)
        </button>
      )}
    </div>
  );
}

/** One answer in the strip: masked first page + identity-light metadata. Masked
 * variant on purpose — the consistency check doesn't need identity and the
 * strip may be shown on a projector. */
function CompareCard({ row }: { row: ProblemStudentRow }) {
  const answer = useQuery({
    queryKey: ["answer", String(row.answer_id)],
    queryFn: () => api.get<AnswerResponse>(`/api/answers/${row.answer_id}`),
    staleTime: 30_000,
  });
  const firstPage = answer.data?.pages[0];
  return (
    <Link
      to={`/answers/${row.answer_id}`}
      className="w-44 shrink-0 rounded-md border border-neutral-200 p-2 hover:border-indigo-300"
    >
      {firstPage ? (
        <SafeImage
          src={`/api/answer-pages/${firstPage.id}/image?variant=masked`}
          alt={`answer ${row.answer_id} (masked)`}
          className="h-52 w-full rounded-sm border border-neutral-100 object-contain"
        />
      ) : (
        <div className="flex h-52 w-full items-center justify-center rounded-sm border border-neutral-100">
          {answer.isError ? (
            <span className="px-2 text-center text-[11px] text-red-500">{answer.error.message}</span>
          ) : (
            <Spinner className="size-4" />
          )}
        </div>
      )}
      <div className="mt-1.5 flex items-center justify-between text-xs">
        <span className="font-medium tabular-nums">{row.student_id}</span>
        {row.flags.length > 0 && (
          <Badge tone="amber" className="text-[10px]">
            {row.flags.length} flag{row.flags.length === 1 ? "" : "s"}
          </Badge>
        )}
      </div>
    </Link>
  );
}
