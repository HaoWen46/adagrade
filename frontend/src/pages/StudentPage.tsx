// Per-student page (spec docs/superpowers/specs/2026-07-28-student-page-design.md):
// read-only, staff-facing answer to "what's the story with this student" — exam
// history, scores, and the context around them (publish/delivery state, provenance,
// regrade threads) without a SQL session.
//
// Two endpoints, two costs. GET /api/students/{sid} is the cheap summary every card
// renders collapsed; GET /api/students/{sid}/assessments/{aid} is fetched only for the
// expanded card and carries everything that is NOT a score. Expansion state lives in
// the URL (?assessment={aid}) so Totals can deep-link a student straight to the exam
// the professor was looking at.
//
// Zero mutations, zero duplication: page images, transcriptions, and every grading
// action stay in AnswerView — a problem row with an answer just links there.
//
// A missing score is null, never 0 (D3): an ungraded problem renders "—" and a student
// with no answer row at all renders "absent". Student name/email and regrade verdicts
// are PII — rendered, never logged (CLAUDE.md).

import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { ApiError, api } from "../lib/api";
import type {
  StudentAssessmentDetailResponse,
  StudentAssessmentProblem,
  StudentAssessmentRow,
  StudentPageResponse,
  StudentProblemProvenance,
  StudentPublishState,
  StudentRegradeRow,
} from "../lib/types";
import { fmtDate } from "../lib/format";
import { flagLabel } from "../lib/labels";
import {
  Badge,
  Card,
  Spinner,
  TD,
  TH,
  Table,
  cx,
  type BadgeTone,
} from "../components/ui";

/** Regrade status vocabulary. The queue's own RegradeStatus values and any condensed
 * forms this endpoint reports both land here; anything unknown falls back to the raw
 * string (lib/labels.ts convention) so a new server enum degrades visibly. */
const REGRADE_STATUS_TONES: Record<string, BadgeTone> = {
  received: "amber",
  under_review: "indigo",
  open: "amber",
  resolved: "green",
  resolved_upheld: "neutral",
  resolved_regraded: "green",
  rejected_bad_token: "red",
  rejected_superseded: "red",
  rejected_sender_mismatch: "red",
};

const REGRADE_STATUS_LABELS: Record<string, string> = {
  received: "received",
  under_review: "under review",
  open: "open",
  resolved: "resolved",
  resolved_upheld: "resolved — upheld",
  resolved_regraded: "resolved — regraded",
  rejected_bad_token: "rejected — bad token",
  rejected_superseded: "rejected — superseded",
  rejected_sender_mismatch: "rejected — sender mismatch",
};

export function StudentPage() {
  const { sid = "" } = useParams();
  // Expansion is URL state (?assessment={aid}) so a Totals link lands pre-expanded and
  // browser-back restores the same card. One card open at a time by construction.
  const [searchParams, setSearchParams] = useSearchParams();
  const param = searchParams.get("assessment");
  const expandedId = param !== null && /^\d+$/.test(param) ? Number(param) : null;

  const toggle = (assessmentId: number) => {
    const next = new URLSearchParams(searchParams);
    if (expandedId === assessmentId) next.delete("assessment");
    else next.set("assessment", String(assessmentId));
    setSearchParams(next, { replace: true, preventScrollReset: true });
  };

  const summary = useQuery({
    queryKey: ["student-page", sid],
    queryFn: () =>
      api.get<StudentPageResponse>(`/api/students/${encodeURIComponent(sid)}`),
    enabled: sid !== "",
    // A 4xx is a settled answer (no such student, or role-gated), not a blip — retrying
    // it only delays the message. Server/network errors still retry.
    retry: (failureCount, error) =>
      !(error instanceof ApiError && error.status >= 400 && error.status < 500) &&
      failureCount < 2,
  });

  if (summary.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }

  if (summary.isError) {
    // A 404 is the ordinary "typed/stale ID" case, not a failure the operator can act
    // on — calm neutral copy rather than a red error line (same treatment as the
    // transcription export card's unavailable state).
    const notFound = summary.error instanceof ApiError && summary.error.status === 404;
    return (
      <div className="mx-auto max-w-4xl">
        <Card title="Student">
          {notFound ? (
            <p className="text-sm text-neutral-600">No student with this ID.</p>
          ) : (
            <p className="text-sm text-red-600">{summary.error.message}</p>
          )}
          <Link to="/students" className="mt-2 inline-block text-sm text-indigo-600 hover:underline">
            Back to students
          </Link>
        </Card>
      </div>
    );
  }

  const { student } = summary.data;
  const assessments = summary.data.assessments ?? [];

  return (
    <div className="mx-auto max-w-4xl space-y-4">
      <div>
        <Link to="/students" className="text-xs text-neutral-500 hover:text-neutral-800">
          ← Students
        </Link>
      </div>

      <Card>
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
          <h1 className="text-lg font-semibold text-neutral-900">{student.name}</h1>
          <span className="text-sm font-medium text-neutral-600 tabular-nums">
            {student.student_id}
          </span>
          <span className="text-sm text-neutral-500">{student.email}</span>
          {student.withdrawn ? (
            <span title="Withdrawn — history kept, excluded from new grading and publishing">
              <Badge tone="amber">withdrawn</Badge>
            </span>
          ) : (
            <Badge tone="green">active</Badge>
          )}
        </div>
      </Card>

      {assessments.length === 0 ? (
        <Card>
          <p className="text-sm text-neutral-400">No assessments for this student yet.</p>
        </Card>
      ) : (
        assessments.map((a) => (
          <AssessmentCard
            key={a.assessment_id}
            sid={sid}
            assessment={a}
            expanded={expandedId === a.assessment_id}
            onToggle={() => toggle(a.assessment_id)}
          />
        ))
      )}
    </div>
  );
}

// --- one assessment ------------------------------------------------------------------

/**
 * Collapsed: name, kind, official total, and the problem rows with their scores — one
 * cheap query's worth. Expanded: the same rows plus provenance, the publish/delivery
 * line, and the regrade threads, from a second query that only runs while open.
 *
 * The container/header classes mirror the Card primitive (which has no clickable-header
 * variant); the same duplication the Identify page's discarded-pages card already makes.
 */
function AssessmentCard({
  sid,
  assessment,
  expanded,
  onToggle,
}: {
  sid: string;
  assessment: StudentAssessmentRow;
  expanded: boolean;
  onToggle: () => void;
}) {
  const detail = useQuery({
    queryKey: ["student-assessment", sid, assessment.assessment_id],
    queryFn: () =>
      api.get<StudentAssessmentDetailResponse>(
        `/api/students/${encodeURIComponent(sid)}/assessments/${assessment.assessment_id}`,
      ),
    enabled: expanded && sid !== "",
    retry: (failureCount, error) =>
      !(error instanceof ApiError && error.status >= 400 && error.status < 500) &&
      failureCount < 2,
  });

  const problems = assessment.problems ?? [];
  // Provenance is keyed back to the summary rows by problem number — the summary drives
  // the table, so a detail row for a problem the summary doesn't list is simply unused.
  const provenance = new Map(
    (detail.data?.problems ?? []).map((p) => [p.number, p] as const),
  );
  const publish = detail.data?.publish ?? null;
  const regrades = detail.data?.regrades ?? [];
  const cols = expanded ? 4 : 3;

  return (
    <section className="rounded-lg border border-neutral-200 bg-white shadow-sm">
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={expanded}
        className="flex w-full items-center justify-between gap-3 border-b border-neutral-200 px-4 py-2.5 text-left hover:bg-neutral-50"
      >
        <span className="flex min-w-0 items-center gap-2">
          <span className="w-3 shrink-0 text-neutral-400">{expanded ? "▾" : "▸"}</span>
          <span className="truncate text-sm font-semibold text-neutral-900">
            {assessment.name}
          </span>
          <Badge tone={assessment.kind === "exam" ? "indigo" : "neutral"}>
            {assessment.kind}
          </Badge>
          {assessment.published && <Badge tone="green">published</Badge>}
        </span>
        <span className="flex shrink-0 items-center gap-3 text-sm tabular-nums">
          <span className="text-xs text-neutral-500">
            graded {assessment.graded}/{assessment.answers}
          </span>
          {/* D3: no official total yet renders "—", never a silent 0. */}
          <span className="font-medium text-neutral-900">
            {assessment.total ?? <span className="text-neutral-400">—</span>}
            <span className="font-normal text-neutral-400"> / {assessment.max}</span>
          </span>
        </span>
      </button>

      <div className="space-y-3 p-4">
        {expanded && (
          <>
            {detail.isPending && (
              <div className="flex justify-center py-2">
                <Spinner className="size-4" />
              </div>
            )}
            {detail.isError && <p className="text-xs text-red-600">{detail.error.message}</p>}
            {/* publish === null means this student was never in a publish batch for this
                assessment — render nothing at all rather than claiming "not published". */}
            {publish && <PublishLine publish={publish} />}
          </>
        )}

        <Table>
          <thead>
            <tr>
              <TH className="w-14">#</TH>
              <TH>Title</TH>
              {expanded && <TH className="w-64">Grading</TH>}
              <TH className="w-32 text-right">Score</TH>
            </tr>
          </thead>
          <tbody>
            {problems.length === 0 && (
              <tr>
                <TD colSpan={cols} className="text-center text-neutral-400">
                  No problems on this assessment.
                </TD>
              </tr>
            )}
            {problems.map((p) => (
              <ProblemRow
                key={p.number}
                problem={p}
                expanded={expanded}
                provenance={provenance.get(p.number)}
              />
            ))}
          </tbody>
        </Table>

        {expanded && detail.isSuccess && <RegradeSection regrades={regrades} />}
      </div>
    </section>
  );
}

// --- problem row ----------------------------------------------------------------------

function ProblemRow({
  problem,
  expanded,
  provenance,
}: {
  problem: StudentAssessmentProblem;
  expanded: boolean;
  /** Undefined while the detail query is idle/in flight, or for a problem the detail
   * response doesn't cover. */
  provenance?: StudentProblemProvenance;
}) {
  const navigate = useNavigate();
  // No answer row = absent: nothing to open in AnswerView, so the row isn't clickable.
  const answerId = problem.answer_id;
  const clickable = answerId !== null;

  return (
    <tr
      onClick={clickable ? () => void navigate(`/answers/${answerId}`) : undefined}
      className={cx(clickable && "cursor-pointer hover:bg-neutral-50")}
    >
      <TD className="font-medium tabular-nums">{problem.number}</TD>
      <TD>{problem.title || <span className="text-neutral-400">untitled</span>}</TD>
      {expanded && (
        <TD>{provenance ? <Provenance p={provenance} /> : <span className="text-neutral-300">—</span>}</TD>
      )}
      <TD className="text-right tabular-nums">
        {/* `== null` deliberately: absent and explicitly-null both mean "no score", and
            neither may ever render as 0 (D3) or as a literal "undefined". */}
        {problem.score == null ? (
          // "absent" (no answer at all) and "—" (has an answer, nothing official yet)
          // are different facts — neither is a 0 (D3).
          <span className="text-neutral-400">{clickable ? "—" : "absent"}</span>
        ) : (
          <>
            <span className="font-medium">{problem.score}</span>
            <span className="text-neutral-400"> / {problem.max}</span>
          </>
        )}
        {expanded && provenance?.changed && (
          <span className="block text-[11px] font-normal text-neutral-400">
            {provenance.published_score != null
              ? `was ${provenance.published_score} at publish`
              : "no score at publish"}
          </span>
        )}
      </TD>
    </tr>
  );
}

/** Who produced the official grade, and what it flagged — the same vocabulary
 * AnswerView's record history uses (model records show the model id itself; the
 * confidence chip is exceptions-only, since "conf high" on every row is noise). */
function Provenance({ p }: { p: StudentProblemProvenance }) {
  const flags = p.flags ?? [];
  return (
    <span className="flex flex-wrap items-center gap-1">
      {p.source === "model" ? (
        <span className="truncate font-mono text-xs text-neutral-800" title="Graded by a model">
          {p.model_id ?? "model"}
        </span>
      ) : p.source ? (
        <Badge
          tone={p.source === "human" ? "indigo" : p.source === "aggregate" ? "green" : "neutral"}
        >
          {p.source}
        </Badge>
      ) : (
        <span className="text-xs text-neutral-400">ungraded</span>
      )}
      {p.confidence && p.confidence !== "high" && (
        <Badge tone={p.confidence === "medium" ? "amber" : "red"}>conf {p.confidence}</Badge>
      )}
      {flags.map((f) => (
        <Badge key={f} tone="amber" className="text-[10px]">
          {flagLabel(f)}
        </Badge>
      ))}
    </span>
  );
}

// --- publish / delivery ----------------------------------------------------------------

/** One line: when the batch went out, whether this student's email actually landed, and
 * where it went — plus the badge for the question this page exists to answer, "does the
 * student's copy still match the truth?". */
function PublishLine({ publish }: { publish: StudentPublishState }) {
  const sent = publish.email_status === "sent";
  const bad = publish.email_status === "failed" || publish.email_status === "uncertain";

  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-neutral-500">
      <span>published {fmtDate(publish.batch_created_at)}</span>
      <span aria-hidden="true">·</span>
      {sent ? (
        <span>
          sent {fmtDate(publish.sent_at ?? undefined)} to{" "}
          <span className="text-neutral-700">{publish.recipient_email}</span>
        </span>
      ) : (
        <span
          className={cx(bad && "text-red-600")}
          title={
            publish.email_status === "uncertain"
              ? "Provider acceptance is unknown — the student may or may not have received it"
              : undefined
          }
        >
          email {publish.email_status} — {publish.recipient_email}
        </span>
      )}
      {publish.changed_since_publish && (
        <>
          <Badge tone="amber">changed since publish</Badge>
          {publish.snapshot_total != null && (
            <span>the student&apos;s email said {publish.snapshot_total}</span>
          )}
        </>
      )}
    </div>
  );
}

// --- regrades ---------------------------------------------------------------------------

function RegradeSection({ regrades }: { regrades: StudentRegradeRow[] }) {
  if (regrades.length === 0) {
    return <p className="text-xs text-neutral-400">No regrade requests for this assessment.</p>;
  }
  return (
    <div className="space-y-1.5">
      <h3 className="text-xs font-semibold tracking-wide text-neutral-500 uppercase">
        Regrade requests
      </h3>
      {regrades.map((r) => (
        <div
          key={r.request_id}
          className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md border border-neutral-200 px-3 py-2 text-xs"
        >
          {/* The queue has no per-request deep link, so this lands on the inbox itself. */}
          <Link to="/regrades" className="font-medium text-indigo-600 hover:underline">
            Request {r.request_id}
          </Link>
          <span className="text-neutral-500">{fmtDate(r.received_at)}</span>
          <Badge tone={REGRADE_STATUS_TONES[r.status] ?? "neutral"}>
            {REGRADE_STATUS_LABELS[r.status] ?? r.status}
          </Badge>
          <span className="ml-auto flex flex-wrap justify-end gap-1">
            {(r.problems ?? []).map((p) => (
              <Badge key={p.number} tone={p.verdict === "regraded" ? "green" : "neutral"}>
                P{p.number} {p.verdict ?? "pending"}
              </Badge>
            ))}
          </span>
        </div>
      ))}
    </div>
  );
}
