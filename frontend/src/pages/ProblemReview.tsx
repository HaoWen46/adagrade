// Problem review: dense per-student table for one problem (status derived
// server-side, D2); row click drills into the answer view.
//
// Filter + page state lives in the URL (?q= &flagged=1 &page=) so browser-back
// from an answer restores this exact view, and other pages (ReviewTab's flagged
// counts, ConsensusTab's triage link) can deep-link into a pre-filtered list.

import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";
import type {
  AnswerStatus,
  AssessmentDetailResponse,
  ProblemStudentRow,
} from "../lib/types";
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
      </div>

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
    </div>
  );
}
