// Answer view: page images (left) + grading panel and record history (right).
// Keyboard: n/p (or arrows) move between students in the problem, u jumps to the
// next ungraded answer, f to the next flagged one. All point math goes through
// decimal.ts (D4 — no floats).

import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { api } from "../lib/api";
import type {
  AnswerPage,
  AnswerResponse,
  AssessmentDetailResponse,
  GradingRecord,
  ProblemStudentRow,
  RegradeLayer,
  RubricCriterion,
  RubricResponse,
  RubricVersion,
  ScoreAdjustment,
} from "../lib/types";
import { sumDecimalStrings, toCents } from "../lib/decimal";
import { fmtDate } from "../lib/format";
import {
  Badge,
  Button,
  Card,
  Dialog,
  Field,
  Input,
  Spinner,
  Textarea,
  cx,
} from "../components/ui";
import { HelpTip } from "../components/HelpTip";
import { flagChipsHelp, recordHistoryHelp } from "../lib/helpContent";
import { answerStatusLabel, flagLabel } from "../lib/labels";
import { PolicyBadge } from "../components/PolicyBadge";
import { SafeImage } from "../components/SafeImage";
import { STATUS_TONES } from "./ProblemReview";

const MANUAL_FLAGS = ["blank", "needs_review", "image_superseded"] as const;

export function AnswerView() {
  const { id = "" } = useParams();
  const navigate = useNavigate();

  const answerQuery = useQuery({
    queryKey: ["answer", id],
    queryFn: () => api.get<AnswerResponse>(`/api/answers/${id}`),
    enabled: id !== "",
  });

  const problemId = answerQuery.data?.problem.id;
  const studentsQuery = useQuery({
    queryKey: ["problem-students", problemId],
    queryFn: () => api.get<{ students: ProblemStudentRow[] }>(`/api/problems/${problemId}/students`),
    enabled: problemId !== undefined,
  });
  // Whether the exam's final grading source is chosen (0027) decides what a
  // fallback save actually does — the ManualGradeCard copy must not promise
  // "becomes official" when nothing can be official yet. Same cache key as
  // AssessmentDetail/ProblemReview, so this is usually a cache hit.
  const assessmentId = answerQuery.data?.answer.assessment_id;
  const detailQuery = useQuery({
    queryKey: ["assessment", String(assessmentId)],
    queryFn: () => api.get<AssessmentDetailResponse>(`/api/assessments/${assessmentId}`),
    enabled: assessmentId !== undefined,
  });
  const rubricQuery = useQuery({
    queryKey: ["rubric", problemId],
    queryFn: () => api.get<RubricResponse>(`/api/problems/${problemId}/rubric`),
    enabled: problemId !== undefined,
  });

  const rows = studentsQuery.data?.students ?? [];
  const position = rows.findIndex((r) => r.answer_id === answerQuery.data?.answer.id);
  const [jump, setJump] = useState("");

  const goTo = (row?: ProblemStudentRow) => {
    if (row) void navigate(`/answers/${row.answer_id}`);
  };
  const next = () => goTo(position >= 0 ? rows[position + 1] : undefined);
  const prev = () => goTo(position > 0 ? rows[position - 1] : undefined);
  const nextUngraded = () => {
    for (let step = 1; step <= rows.length; step++) {
      const row = rows[(Math.max(position, 0) + step) % rows.length];
      if (row.status === "ungraded") return goTo(row);
    }
  };
  // Consensus triage loop: same wrap-around walk as nextUngraded, but over
  // flagged answers (a.flags covers agg_* and manual flags alike).
  const nextFlagged = () => {
    for (let step = 1; step <= rows.length; step++) {
      const row = rows[(Math.max(position, 0) + step) % rows.length];
      if (row.flags.length > 0) return goTo(row);
    }
  };

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      if (
        t &&
        (t.tagName === "INPUT" ||
          t.tagName === "TEXTAREA" ||
          t.tagName === "SELECT" ||
          t.isContentEditable)
      ) {
        return;
      }
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === "n" || e.key === "ArrowRight") {
        e.preventDefault();
        next();
      } else if (e.key === "p" || e.key === "ArrowLeft") {
        e.preventDefault();
        prev();
      } else if (e.key === "u") {
        e.preventDefault();
        nextUngraded();
      } else if (e.key === "f") {
        e.preventDefault();
        nextFlagged();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  if (answerQuery.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (answerQuery.isError) {
    return (
      <div className="mx-auto max-w-5xl">
        <Card title="Answer">
          <p className="text-sm text-red-600">{answerQuery.error.message}</p>
          <Link to="/assessments" className="mt-2 inline-block text-sm text-indigo-600 hover:underline">
            Back to assessments
          </Link>
        </Card>
      </div>
    );
  }

  const data = answerQuery.data;
  const status = position >= 0 ? rows[position].status : undefined;
  const officialTotal =
    data.records.find((r) => r.id === data.answer.official_record_id)?.total ?? undefined;
  // Rounds design: the effective grade is the topmost adopted overlay, else round 0.
  const layers = data.regrade_layers ?? [];
  const topAdopted = [...layers].reverse().find((l) => l.adopted_total !== undefined);
  const effectiveTotal = topAdopted?.adopted_total ?? officialTotal;

  return (
    <div className="mx-auto max-w-7xl space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <Link
          to={`/assessments/${data.answer.assessment_id}/problems/${data.answer.problem_id}/review`}
          className="text-xs text-neutral-500 hover:text-neutral-800"
        >
          ← Problem {data.problem.number} review
        </Link>
        <div className="flex items-center gap-2">
          {position >= 0 && (
            <span className="text-xs whitespace-nowrap text-neutral-500 tabular-nums">
              {position + 1} of {rows.length}
            </span>
          )}
          <Button
            variant="secondary"
            className="px-2.5 py-1 text-xs"
            onClick={prev}
            disabled={position <= 0}
            title="Previous student (p or ←)"
            aria-label="Previous student"
          >
            ←
          </Button>
          <Button
            variant="secondary"
            className="px-2.5 py-1 text-xs"
            onClick={next}
            disabled={position < 0 || position >= rows.length - 1}
            title="Next student (n or →)"
            aria-label="Next student"
          >
            →
          </Button>
          <Button
            variant="secondary"
            className="px-2.5 py-1 text-xs whitespace-nowrap"
            onClick={nextUngraded}
            title="Next ungraded (u)"
          >
            Ungraded →
          </Button>
          <Button
            variant="secondary"
            className="px-2.5 py-1 text-xs whitespace-nowrap"
            onClick={nextFlagged}
            title="Next flagged (f)"
          >
            Flagged →
          </Button>
          <Input
            list="answer-view-student-ids"
            className="w-36 py-1 text-xs"
            placeholder="Go to student ID…"
            aria-label="Go to student"
            value={jump}
            onChange={(e) => {
              const v = e.target.value;
              setJump(v);
              // Datalist selections fire change with the full ID — jump immediately.
              const hit = rows.find((r) => r.student_id.toLowerCase() === v.toLowerCase());
              if (hit) {
                setJump("");
                goTo(hit);
              }
            }}
            onKeyDown={(e) => {
              if (e.key !== "Enter") return;
              const v = e.currentTarget.value.toLowerCase();
              if (v === "") return;
              const hit = rows.find((r) => r.student_id.toLowerCase().startsWith(v));
              if (hit) {
                setJump("");
                goTo(hit);
              }
            }}
          />
          <datalist id="answer-view-student-ids">
            {rows.map((r) => (
              <option key={r.answer_id} value={r.student_id} />
            ))}
          </datalist>
        </div>
      </div>

      <div className="grid items-start gap-4 lg:grid-cols-2">
        <PagesColumn pages={data.pages} />

        <div className="space-y-4">
          <Card>
            <div className="flex items-start justify-between gap-4">
              <div className="min-w-0">
                <div className="flex flex-wrap items-center gap-2.5">
                  <h1 className="text-lg font-semibold text-neutral-900">{data.student.name}</h1>
                  <span className="text-sm text-neutral-500 tabular-nums">
                    {data.student.student_id}
                  </span>
                  {status && (
                    <Badge tone={STATUS_TONES[status]}>{answerStatusLabel(status)}</Badge>
                  )}
                </div>
                {/* The "what am I looking at" line — assessment + problem carry the
                    identification weight, so they read at body size; pts/email stay dim. */}
                <p className="mt-1 text-sm">
                  <span className="font-medium text-neutral-700">{data.assessment_name}</span>
                  <span className="text-neutral-300"> · </span>
                  <span className="font-medium text-neutral-700">
                    Problem {data.problem.number}
                    {data.problem.title && ` — ${data.problem.title}`}
                  </span>
                  <span className="text-xs text-neutral-400">
                    {" "}
                    · {data.problem.max_points} pts · {data.student.email}
                  </span>
                </p>
              </div>
              {/* The one number this page exists to produce, made glanceable. */}
              <div className="shrink-0 text-right">
                <p
                  className={cx(
                    "text-2xl leading-none font-semibold tabular-nums",
                    effectiveTotal !== undefined ? "text-neutral-900" : "text-neutral-300",
                  )}
                >
                  {effectiveTotal ?? "—"}
                  <span className="text-sm font-normal text-neutral-400">
                    {" "}
                    / {data.problem.max_points}
                  </span>
                </p>
                <p className="mt-1 text-[10px] font-medium tracking-wide text-neutral-400 uppercase">
                  {topAdopted
                    ? `regraded · turn ${topAdopted.turn}`
                    : effectiveTotal !== undefined
                      ? "official grade"
                      : "no official yet"}
                </p>
              </div>
            </div>
            <div className="mt-2.5">
              <FlagChips answerKey={id} problemId={data.problem.id} flags={data.answer.flags} />
            </div>
            {data.answer.published_at && (
              <div className="mt-2.5 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
                <strong>Published.</strong> This answer's official grade is locked. To change it,
                unpublish the assessment first (Publish tab, admin) — attempts to set an official
                grade here will be rejected until then.
              </div>
            )}
          </Card>

          {layers.length > 0 && <RegradeLayersCard layers={layers} officialTotal={officialTotal} maxPoints={data.problem.max_points} />}

          <ManualGradeCard
            answerKey={id}
            data={data}
            rubricQuery={rubricQuery}
            unresolved={data.answer.official_record_id == null}
            noFinalSource={
              detailQuery.isSuccess &&
              detailQuery.data.assessment.final_source_kind === undefined
            }
          />

          <Card
            title={
              <span className="inline-flex items-center gap-1.5">
                History <HelpTip title="Grading history">{recordHistoryHelp}</HelpTip>
              </span>
            }
          >
            <RecordHistory
              answerKey={id}
              data={data}
              currentRubric={rubricQuery.data?.current ?? null}
            />
          </Card>
        </div>
      </div>
    </div>
  );
}

// --- pages --------------------------------------------------------------------------

function PagesColumn({ pages }: { pages: AnswerPage[] }) {
  const [zoom, setZoom] = useState<string | null>(null);
  const [maskedShown, setMaskedShown] = useState<Record<number, boolean>>({});
  const submissionId = pages[0]?.submission_id;

  return (
    <div className="space-y-3">
      {submissionId !== undefined && (
        <a
          href={`/api/submissions/${submissionId}/pdf`}
          target="_blank"
          rel="noreferrer"
          className="inline-block text-sm text-indigo-600 hover:underline"
        >
          Source PDF ↗
        </a>
      )}
      {pages.length === 0 && (
        <Card>
          <p className="text-sm text-neutral-400">No pages mapped to this answer.</p>
        </Card>
      )}
      {pages.map((p) => {
        const showMasked = p.masked && (maskedShown[p.id] ?? false);
        const src = `/api/answer-pages/${p.id}/image${showMasked ? "?variant=masked" : ""}`;
        return (
          <div
            key={p.id}
            className="overflow-hidden rounded-lg border border-neutral-200 bg-white shadow-sm"
          >
            <div className="flex items-center justify-between border-b border-neutral-200 px-3 py-1.5">
              <span className="text-xs font-medium text-neutral-600">Page {p.page_index + 1}</span>
              <div className="flex items-center gap-2">
                {p.masked && p.mask_review !== "pending" && (
                  <Badge tone={p.mask_review === "accepted" ? "green" : "red"}>
                    mask {p.mask_review}
                  </Badge>
                )}
                {p.masked && (
                  <label className="flex items-center gap-1 text-xs text-neutral-600">
                    <input
                      type="checkbox"
                      checked={showMasked}
                      onChange={(e) =>
                        setMaskedShown({ ...maskedShown, [p.id]: e.target.checked })
                      }
                      className="size-3.5 accent-indigo-600"
                    />
                    show masked
                  </label>
                )}
              </div>
            </div>
            <SafeImage
              src={src}
              alt={`Page ${p.page_index + 1}`}
              loading="lazy"
              className="w-full cursor-zoom-in"
              onClick={() => setZoom(src)}
            />
          </div>
        );
      })}
      <Dialog open={zoom !== null} onClose={() => setZoom(null)} title="Page" className="max-w-5xl">
        {zoom && <SafeImage src={zoom} alt="Zoomed page" className="w-full" />}
      </Dialog>
    </div>
  );
}

// --- flags --------------------------------------------------------------------------

function FlagChips({
  answerKey,
  problemId,
  flags,
}: {
  answerKey: string;
  problemId: number;
  flags: string[];
}) {
  const queryClient = useQueryClient();
  const toggle = useMutation({
    mutationFn: ({ flag, add }: { flag: string; add: boolean }) =>
      api.post(`/api/answers/${answerKey}/flags`, { flag, add }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["answer", answerKey] });
      await queryClient.invalidateQueries({ queryKey: ["problem-students", problemId] });
      await queryClient.invalidateQueries({ queryKey: ["problem-summaries"] });
    },
  });

  const other = flags.filter((f) => !(MANUAL_FLAGS as readonly string[]).includes(f));

  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {MANUAL_FLAGS.map((flag) => {
        const active = flags.includes(flag);
        return (
          <button
            key={flag}
            type="button"
            disabled={toggle.isPending}
            onClick={() => toggle.mutate({ flag, add: !active })}
            className={cx(
              "rounded-full px-2 py-0.5 text-xs font-medium ring-1 transition-colors ring-inset disabled:opacity-50",
              active
                ? "bg-amber-50 text-amber-800 ring-amber-200"
                : "bg-white text-neutral-400 ring-neutral-200 hover:text-neutral-700",
            )}
          >
            {flagLabel(flag)}
          </button>
        );
      })}
      {other.map((f) => (
        <Badge key={f} tone="red">
          {flagLabel(f)}
        </Badge>
      ))}
      <HelpTip title="Flags">{flagChipsHelp}</HelpTip>
      {toggle.isError && <span className="text-xs text-red-600">{toggle.error.message}</span>}
    </div>
  );
}

// --- regrade layers ---------------------------------------------------------------------

// RegradeLayersCard is the layer pager (rounds design): one segment per grading
// layer — the original grading plus every adjudicated regrade turn that touched
// this problem — labeled in words, defaulting to the latest. Regrades never
// rewrite round 0; they stack on it, and the effective grade is the top layer.
function RegradeLayersCard({
  layers,
  officialTotal,
  maxPoints,
}: {
  layers: RegradeLayer[];
  officialTotal?: string | null;
  maxPoints: string;
}) {
  // Index 0 = original grading; 1..n = the adjudicated turns, oldest first.
  const [idx, setIdx] = useState(layers.length);
  const safe = Math.min(idx, layers.length);
  const layer = safe === 0 ? null : layers[safe - 1];
  const label = (i: number) => (i === 0 ? "Original grading" : `Regrade · turn ${layers[i - 1].turn}`);

  // The score each layer carries: an adopted total for regraded turns; upheld
  // turns carry the layer below (walk down until something decided).
  const carried = (i: number): string | undefined => {
    for (let j = i; j >= 1; j--) {
      const t = layers[j - 1].adopted_total;
      if (t !== undefined) return t;
    }
    return officialTotal ?? undefined;
  };

  return (
    <Card
      title={
        <span className="inline-flex items-center gap-2">
          <Button
            variant="secondary"
            className="px-2 py-0.5 text-xs"
            aria-label="Previous layer"
            title="Previous layer"
            disabled={safe === 0}
            onClick={() => setIdx(safe - 1)}
          >
            ←
          </Button>
          <span className="text-sm font-semibold whitespace-nowrap text-neutral-900">
            {label(safe)}
          </span>
          <Button
            variant="secondary"
            className="px-2 py-0.5 text-xs"
            aria-label="Next layer"
            title="Next layer"
            disabled={safe >= layers.length}
            onClick={() => setIdx(safe + 1)}
          >
            →
          </Button>
          <span className="text-xs whitespace-nowrap text-neutral-400 tabular-nums">
            {safe + 1} of {layers.length + 1}
          </span>
        </span>
      }
      actions={
        <span className="text-sm font-semibold text-neutral-900 tabular-nums">
          {carried(safe) ?? "—"}
          <span className="font-normal text-neutral-400"> / {maxPoints}</span>
        </span>
      }
    >
      {layer === null ? (
        <p className="text-xs text-neutral-500">
          Round 0 — the exam&apos;s grading source (plus manual fallbacks) below. Regrade turns
          stack on top of it; page → to see them.
        </p>
      ) : (
        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone={layer.verdict === "regraded" ? "amber" : "green"}>{layer.verdict}</Badge>
            {layer.verdict === "regraded" ? (
              <span className="text-xs text-neutral-600 tabular-nums">
                new score {layer.adopted_total ?? "—"} / {maxPoints}
              </span>
            ) : (
              <span className="text-xs text-neutral-500">
                unchanged — carries {carried(safe - 1) ?? "—"} / {maxPoints} from the layer below
              </span>
            )}
            <span className="ml-auto text-[11px] text-neutral-400">{fmtDate(layer.verdict_at)}</span>
          </div>
          {layer.note && (
            <blockquote className="rounded-r-md border-l-[3px] border-indigo-200 bg-neutral-50 px-3 py-2 text-sm leading-relaxed whitespace-pre-wrap text-neutral-700">
              {layer.note}
            </blockquote>
          )}
          <p className="text-[11px] text-neutral-400">
            Adjudicated on the{" "}
            <Link to="/regrades" className="text-indigo-600 hover:underline">
              Regrades page
            </Link>{" "}
            (request #{layer.request_id}). This layer never rewrites the original grading below.
          </p>
        </div>
      )}
    </Card>
  );
}

// --- manual (fallback) grading ---------------------------------------------------------

// ManualGradeCard is the human FALLBACK (round-based grading, 0027): it only
// takes effect where the exam's chosen source left this answer undecided —
// otherwise a saved grade is recorded but ignored. Collapsed by default; opens
// itself exactly when this answer is unresolved, because that's when a human
// is actually needed.
//
// The copy is state-aware (HCI audit): before a final grading source is chosen,
// a save changes nothing visible — officials only derive once the source is
// picked on Publish — so promising "becomes its official grade" here read as a
// failed save. Track the recorded human grade and say what actually happened.
function ManualGradeCard({
  answerKey,
  data,
  rubricQuery,
  unresolved,
  noFinalSource,
}: {
  answerKey: string;
  data: AnswerResponse;
  rubricQuery: UseQueryResult<RubricResponse>;
  unresolved: boolean;
  /** True once the assessment detail confirms no final grading source is chosen
   * yet — saves are recorded now, official only after the Publish-tab choice. */
  noFinalSource: boolean;
}) {
  const [open, setOpen] = useState(unresolved);
  const humanRecorded = data.records.some((r) => r.source === "human");
  const publishTab = (
    <Link
      to={`/assessments/${data.answer.assessment_id}?tab=publish`}
      className="text-indigo-600 hover:underline"
    >
      Publish tab
    </Link>
  );

  return (
    <Card
      title={
        <span className="inline-flex items-center gap-2">
          <button
            type="button"
            onClick={() => setOpen((v) => !v)}
            aria-expanded={open}
            className="inline-flex items-center gap-1.5 text-left"
          >
            <span className="w-3 text-xs text-neutral-400">{open ? "▾" : "▸"}</span>
            Manual grade (fallback)
          </button>
          {!unresolved ? (
            <Badge tone="green">official</Badge>
          ) : humanRecorded && noFinalSource ? (
            // A grade IS saved — "unresolved — needs you" here would read as a
            // failed save. Nothing more is needed from the grader on this answer.
            <Badge tone="amber">manual grade recorded — awaiting final source</Badge>
          ) : (
            <Badge tone="red">unresolved — needs you</Badge>
          )}
        </span>
      }
    >
      {open ? (
        <>
          <p className="mb-3 text-xs text-neutral-500">
            {!unresolved ? (
              "This answer is decided by the exam's grading source. A grade saved here is recorded but ignored unless the source later becomes undecided (e.g. the answer gets flagged)."
            ) : noFinalSource ? (
              humanRecorded ? (
                <>
                  Your manual grade is recorded. It becomes this answer&apos;s official grade
                  once a final grading source is chosen on the {publishTab}.
                </>
              ) : (
                <>
                  No final grading source is chosen yet — the grade you save here is recorded
                  now and becomes official once a source is chosen on the {publishTab}.
                </>
              )
            ) : (
              "The chosen grading source left this answer undecided — the grade you save here becomes its official grade."
            )}
          </p>
          {rubricQuery.isPending ? (
            <Spinner />
          ) : rubricQuery.isError ? (
            <p className="text-sm text-red-600">{rubricQuery.error.message}</p>
          ) : rubricQuery.data.current?.criteria?.length ? (
            <GradeForm
              key={`${rubricQuery.data.current.id}:${data.answer.id}`}
              answerKey={answerKey}
              data={data}
              rubric={rubricQuery.data.current}
              noFinalSource={noFinalSource}
            />
          ) : (
            <p className="text-sm text-neutral-400">
              No rubric yet — define one on the assessment page before grading.
            </p>
          )}
        </>
      ) : (
        <p className="text-xs text-neutral-400">
          {!unresolved
            ? "Nothing needed here — this answer is resolved (by the source, or by a saved fallback)."
            : humanRecorded && noFinalSource
              ? "Manual grade recorded — it becomes official once a final source is chosen on the Publish tab."
              : "Expand to grade this answer manually."}
        </p>
      )}
    </Card>
  );
}

interface ScoreDraft {
  score: string;
  rationale: string;
}

type ScoreCheck = "empty" | "bad" | "offstep" | "ok";

function GradeForm({
  answerKey,
  data,
  rubric,
  noFinalSource,
}: {
  answerKey: string;
  data: AnswerResponse;
  rubric: RubricVersion;
  /** No final grading source chosen yet — the post-save confirmation must say
   * "recorded, official later", not imply the grade took effect. */
  noFinalSource: boolean;
}) {
  const queryClient = useQueryClient();
  const criteria = rubric.criteria ?? [];
  const [drafts, setDrafts] = useState<Record<number, ScoreDraft>>(() =>
    Object.fromEntries(criteria.map((c) => [c.id, { score: "", rationale: "" }])),
  );
  const [comment, setComment] = useState("");
  const [adjustments, setAdjustments] = useState<ScoreAdjustment[] | null>(null);

  const invalidate = async () => {
    await queryClient.invalidateQueries({ queryKey: ["answer", answerKey] });
    await queryClient.invalidateQueries({ queryKey: ["problem-students", data.problem.id] });
    await queryClient.invalidateQueries({ queryKey: ["problem-summaries"] });
    await queryClient.invalidateQueries({ queryKey: ["publish-preview"] });
  };

  // 0027: the record is a fallback — the server derives whether it applies.
  const save = useMutation({
    mutationFn: () =>
      api.post<GradingRecord>(`/api/answers/${answerKey}/records`, {
        rubric_version_id: rubric.id,
        comment,
        scores: criteria.map((c) => ({
          criterion_id: c.id,
          score: drafts[c.id].score.trim(),
          rationale: drafts[c.id].rationale,
        })),
      }),
    onSuccess: async (rec) => {
      setAdjustments(rec.adjustments?.length ? rec.adjustments : null);
      await invalidate();
    },
  });

  const incrementCents = toCents(rubric.score_increment);
  const check = (v: string): ScoreCheck => {
    const t = v.trim();
    if (t === "") return "empty";
    const cents = toCents(t);
    if (cents === null || cents < 0) return "bad";
    if (incrementCents !== null && incrementCents > 0 && cents % incrementCents !== 0) {
      return "offstep";
    }
    return "ok";
  };

  const checks = criteria.map((c) => check(drafts[c.id].score));
  const submittable = checks.every((s) => s === "ok" || s === "offstep");
  const total = submittable
    ? sumDecimalStrings(criteria.map((c) => drafts[c.id].score.trim()))
    : null;

  const setDraft = (id: number, patch: Partial<ScoreDraft>) =>
    setDrafts({ ...drafts, [id]: { ...drafts[id], ...patch } });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (submittable) save.mutate();
  };

  return (
    <form className="space-y-3" onSubmit={submit}>
      {/* One block per criterion: full-width criterion text with the score input
          beside it, rationale on its own line below. The old three-column row
          split the width between two text fields, so criterion descriptions —
          the thing being judged against — always truncated. */}
      <div className="space-y-3">
        {criteria.map((c, i) => (
          <div key={c.id} className="border-b border-neutral-100 pb-3 last:border-0 last:pb-0">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0 pt-1">
                <p className="text-sm leading-snug text-neutral-800">{c.description}</p>
                {c.partial_credit_notes && (
                  <p className="mt-0.5 text-[11px] leading-relaxed text-neutral-400">
                    {c.partial_credit_notes}
                  </p>
                )}
              </div>
              <div className="flex shrink-0 items-center gap-1">
                <Input
                  placeholder="0"
                  value={drafts[c.id].score}
                  onChange={(e) => setDraft(c.id, { score: e.target.value })}
                  title={
                    checks[i] === "offstep"
                      ? `Not a multiple of ${rubric.score_increment} — the server will snap it`
                      : undefined
                  }
                  className={cx(
                    "w-14 py-1 text-right tabular-nums",
                    checks[i] === "bad" && "border-red-400 focus:border-red-500 focus:ring-red-500/20",
                    checks[i] === "offstep" &&
                      "border-amber-400 focus:border-amber-500 focus:ring-amber-500/20",
                  )}
                />
                <span className="text-xs whitespace-nowrap text-neutral-400 tabular-nums">
                  / {c.points}
                </span>
              </div>
            </div>
            <Input
              placeholder="rationale (optional)"
              value={drafts[c.id].rationale}
              onChange={(e) => setDraft(c.id, { rationale: e.target.value })}
              className="mt-1.5 py-1 text-xs"
            />
          </div>
        ))}
        <div className="flex justify-end">
          <Badge tone={total === null ? "neutral" : "indigo"}>
            total {total ?? "—"} / {data.problem.max_points}
          </Badge>
        </div>
      </div>

      <Field label="Comment">
        <Textarea rows={2} value={comment} onChange={(e) => setComment(e.target.value)} />
      </Field>

      {adjustments && (
        <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
          Scores were snapped to the grading scale:{" "}
          {adjustments.map((a) => `${a.from} → ${a.to}`).join(", ")}.
        </p>
      )}
      {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}

      <div className="flex flex-wrap items-center justify-end gap-3">
        {/* Explicit success confirmation (same pattern as ConsensusTab's policy
            "Saved.") — without a final source the status badge alone can't
            prove the save landed, so say so here, truthfully. */}
        {save.isSuccess && !save.isPending && (
          <span className="text-xs text-green-700">
            {noFinalSource
              ? "Saved — recorded now, official once a final source is chosen (Publish tab)."
              : "Saved."}
          </span>
        )}
        <span className="text-xs text-neutral-400">rubric v{rubric.version}</span>
        <Button type="submit" disabled={!submittable || save.isPending}>
          {save.isPending ? "Saving…" : "Save fallback grade"}
        </Button>
      </div>
    </form>
  );
}

// --- record history --------------------------------------------------------------------

// The history must stay scannable when a consensus panel leaves 6+ model records
// on one answer: records render as one-line rows (official pinned first and
// tinted; details on expand), and a per-criterion comparison table answers the
// question a vertical pile can't — WHERE do the models disagree?

/** Identity cluster for one record: source badge for human/aggregate/etc.,
    the model name itself for model records (the "model" badge was pure noise
    repeated once per panel member). */
function RecordIdentity({ rec }: { rec: GradingRecord }) {
  if (rec.source === "model") {
    return (
      <span className="truncate font-mono text-xs text-neutral-800" title={rec.provider}>
        {rec.model_id}
      </span>
    );
  }
  return (
    <Badge tone={rec.source === "human" ? "indigo" : rec.source === "aggregate" ? "green" : "neutral"}>
      {rec.source}
    </Badge>
  );
}

/** Confidence chip, exceptions only: "conf high" on every row of a healthy
    panel is noise — surface medium as a nudge and low/illegible as a warning. */
function ConfidenceChip({ confidence }: { confidence?: string }) {
  if (!confidence || confidence === "high") return null;
  return (
    <Badge tone={confidence === "medium" ? "amber" : "red"}>conf {confidence}</Badge>
  );
}

/** Source accent dot: a quiet color key repeated on every row so the eye can
    separate human/consensus/model records without reading a badge each time. */
const SOURCE_DOT: Record<string, string> = {
  human: "bg-indigo-500",
  aggregate: "bg-green-500",
  regrade_ai: "bg-violet-400",
};

/** ScoreCell: the earned score plus a small fill bar — full credit lands green,
    partial indigo, below half amber, so a criterion breakdown reads at a glance
    instead of as a column of near-identical numbers. */
function ScoreCell({ score, points }: { score: string; points?: string }) {
  const s = toCents(score);
  const m = points !== undefined ? toCents(points) : null;
  const frac = s !== null && m !== null && m > 0 ? Math.max(0, Math.min(1, s / m)) : null;
  return (
    <span className="flex w-full flex-col items-end gap-1">
      <span className="text-xs font-medium text-neutral-800 tabular-nums">
        {score}
        {points !== undefined && <span className="font-normal text-neutral-400"> / {points}</span>}
      </span>
      {frac !== null && (
        <span className="h-1 w-full overflow-hidden rounded-full bg-neutral-200/80">
          <span
            className={cx(
              "block h-full rounded-full",
              frac >= 1 ? "bg-green-500" : frac >= 0.5 ? "bg-indigo-500" : "bg-amber-500",
            )}
            style={{ width: `${frac * 100}%` }}
          />
        </span>
      )}
    </span>
  );
}

function RecordHistory({
  answerKey,
  data,
  currentRubric,
}: {
  answerKey: string;
  data: AnswerResponse;
  currentRubric: RubricVersion | null;
}) {
  const [compare, setCompare] = useState(false);
  const criteriaById = new Map<number, RubricCriterion>(
    (currentRubric?.criteria ?? []).map((c) => [c.id, c]),
  );

  if (data.records.length === 0) {
    return <p className="text-sm text-neutral-400">No grading records yet.</p>;
  }

  // Official first, everything else in server order (newest first) — with
  // re-runs the record that counts must never be buried mid-list.
  const official = data.records.find((r) => r.id === data.answer.official_record_id);
  const ordered = official
    ? [official, ...data.records.filter((r) => r.id !== official.id)]
    : data.records;

  const modelCount = data.records.filter((r) => r.source === "model").length;
  const comparable = currentRubric
    ? ordered.filter((r) => r.rubric_version_id === currentRubric.id)
    : [];

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-xs text-neutral-500 tabular-nums">
          {data.records.length} record{data.records.length === 1 ? "" : "s"}
          {modelCount > 0 && ` · ${modelCount} model grade${modelCount === 1 ? "" : "s"}`}
        </span>
        {comparable.length >= 2 && (
          <Button
            variant="secondary"
            className="px-2 py-1 text-xs"
            onClick={() => setCompare((v) => !v)}
          >
            {compare ? "Hide comparison" : "Compare scores"}
          </Button>
        )}
      </div>

      {compare && currentRubric && (
        <CompareTable
          records={comparable}
          criteria={currentRubric.criteria ?? []}
          officialId={data.answer.official_record_id}
          maxPoints={data.problem.max_points}
          hiddenCount={ordered.length - comparable.length}
        />
      )}

      <div className="divide-y divide-neutral-100 rounded-md border border-neutral-200">
        {ordered.map((rec) => (
          <RecordRow
            key={rec.id}
            rec={rec}
            answerKey={answerKey}
            data={data}
            criteriaById={criteriaById}
            isCurrentRubric={rec.rubric_version_id === currentRubric?.id}
            official={rec.id === data.answer.official_record_id}
          />
        ))}
      </div>
    </div>
  );
}

/** CompareTable: records as rows, criteria as columns — per-criterion scores
    side by side, with cells that deviate from the official grade tinted amber. */
function CompareTable({
  records,
  criteria,
  officialId,
  maxPoints,
  hiddenCount,
}: {
  records: GradingRecord[];
  criteria: RubricCriterion[];
  officialId?: number | null;
  maxPoints: string;
  hiddenCount: number;
}) {
  const official = records.find((r) => r.id === officialId);
  const officialScore = (criterionId: number) =>
    official?.criterion_scores.find((cs) => cs.criterion_id === criterionId)?.score;

  return (
    <div className="space-y-1.5">
      <div className="overflow-x-auto rounded-md border border-neutral-200">
        <table className="w-full border-collapse text-left text-xs">
          <thead>
            <tr>
              <th className="border-b border-neutral-200 bg-neutral-50 px-2.5 py-1.5 font-semibold text-neutral-500">
                Record
              </th>
              {criteria.map((c, i) => (
                <th
                  key={c.id}
                  title={`${c.description} — ${c.points} pts`}
                  className="w-14 border-b border-neutral-200 bg-neutral-50 px-2.5 py-1.5 text-right font-semibold text-neutral-500"
                >
                  C{i + 1}
                  <span className="font-normal text-neutral-400"> /{c.points}</span>
                </th>
              ))}
              <th className="w-20 border-b border-neutral-200 bg-neutral-50 px-2.5 py-1.5 text-right font-semibold text-neutral-500">
                Total /{maxPoints}
              </th>
            </tr>
          </thead>
          <tbody>
            {records.map((rec) => {
              const isOfficial = rec.id === officialId;
              return (
                <tr key={rec.id} className={cx(isOfficial && "bg-green-50/40")}>
                  <td className="border-b border-neutral-100 px-2.5 py-1.5">
                    <span className="inline-flex items-center gap-1.5">
                      <RecordIdentity rec={rec} />
                      {isOfficial && <Badge tone="green">official</Badge>}
                      <ConfidenceChip confidence={rec.confidence} />
                    </span>
                  </td>
                  {criteria.map((c) => {
                    const score = rec.criterion_scores.find(
                      (cs) => cs.criterion_id === c.id,
                    )?.score;
                    const base = officialScore(c.id);
                    const differs =
                      official !== undefined && !isOfficial && score !== undefined && score !== base;
                    return (
                      <td
                        key={c.id}
                        className={cx(
                          "border-b border-neutral-100 px-2.5 py-1.5 text-right tabular-nums",
                          differs ? "font-medium text-amber-700" : "text-neutral-700",
                        )}
                      >
                        {score ?? "—"}
                      </td>
                    );
                  })}
                  <td className="border-b border-neutral-100 px-2.5 py-1.5 text-right font-semibold text-neutral-900 tabular-nums">
                    {rec.total ?? "—"}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <p className="text-[11px] leading-relaxed text-neutral-400">
        {criteria.map((c, i) => `C${i + 1} — ${c.description} (${c.points} pts)`).join(" · ")}
        {official && " · amber = differs from the official score"}
        {hiddenCount > 0 &&
          ` · ${hiddenCount} older-rubric record${hiddenCount === 1 ? "" : "s"} not compared`}
      </p>
    </div>
  );
}

/** RecordRow: one-line collapsed summary (identity · chips · total · date),
    click to expand the full detail. The official row is tinted and pinned by
    RecordHistory; it starts expanded since its rationale is the one that counts. */
function RecordRow({
  rec,
  data,
  criteriaById,
  isCurrentRubric,
  official,
}: {
  rec: GradingRecord;
  answerKey: string;
  data: AnswerResponse;
  criteriaById: Map<number, RubricCriterion>;
  isCurrentRubric: boolean;
  official: boolean;
}) {
  const [expanded, setExpanded] = useState(official);

  return (
    <div className={cx(official && "bg-green-50/40")}>
      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        aria-expanded={expanded}
        className="flex w-full flex-wrap items-center gap-2 px-3 py-2 text-left hover:bg-neutral-50"
      >
        <span className="w-3 shrink-0 text-xs text-neutral-400">{expanded ? "▾" : "▸"}</span>
        <span
          className={cx("size-1.5 shrink-0 rounded-full", SOURCE_DOT[rec.source] ?? "bg-neutral-300")}
        />
        <RecordIdentity rec={rec} />
        {official && <Badge tone="green">official</Badge>}
        {rec.policy && rec.policy !== "standard" && <PolicyBadge policy={rec.policy} />}
        <ConfidenceChip confidence={rec.confidence} />
        <span className="ml-auto flex items-center gap-3">
          <span className="text-sm font-semibold text-neutral-900 tabular-nums">
            {rec.total ?? "—"}
            <span className="font-normal text-neutral-400"> / {data.problem.max_points}</span>
          </span>
          <span className="hidden text-[11px] whitespace-nowrap text-neutral-400 sm:inline">
            {fmtDate(rec.created_at)}
          </span>
        </span>
      </button>
      {expanded && (
        <RecordDetail rec={rec} criteriaById={criteriaById} isCurrentRubric={isCurrentRubric} />
      )}
    </div>
  );
}

// RecordDetail is read-only since 0027: records are evidence, not choices —
// the "Set official" per-record action is gone (officials derive from the
// exam's final source; humans grade fallbacks in the card above).
function RecordDetail({
  rec,
  criteriaById,
  isCurrentRubric,
}: {
  rec: GradingRecord;
  criteriaById: Map<number, RubricCriterion>;
  isCurrentRubric: boolean;
}) {
  // Human version integers from the backend ("method v2 · prompt v3"), never
  // the raw method_version_id / prompt_template_version_id DB ids.
  const versionLine = [
    rec.method_version !== undefined ? `method v${rec.method_version}` : null,
    rec.prompt_version !== undefined ? `prompt v${rec.prompt_version}` : null,
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <div className="px-3 pb-3 pl-8">
      {/* Fixed-fraction grid, not a per-record <table>: auto table layout sized
          every record's columns differently, so nothing lined up across records. */}
      <div>
        {rec.criterion_scores.map((cs) => {
          const criterion = criteriaById.get(cs.criterion_id);
          const known = isCurrentRubric && criterion !== undefined;
          return (
            <div
              key={cs.criterion_id}
              className="grid grid-cols-[minmax(0,4fr)_4.5rem_minmax(0,5fr)] items-start gap-x-3 border-b border-neutral-100 py-1.5 last:border-0"
            >
              <span className="text-xs leading-relaxed text-neutral-700">
                {known ? criterion.description : `criterion #${cs.criterion_id}`}
              </span>
              <ScoreCell score={cs.score} points={known ? criterion.points : undefined} />
              <span className="text-xs leading-relaxed text-neutral-500">{cs.rationale}</span>
            </div>
          );
        })}
      </div>

      {!isCurrentRubric && (
        <p className="mt-1 text-[11px] text-neutral-400">
          Graded with an older rubric (version id {rec.rubric_version_id}).
        </p>
      )}
      {/* Full provenance — the collapsed row only surfaces exceptions (non-standard
          policy, non-high confidence), so the detail spells everything out. */}
      {(rec.policy || rec.confidence || versionLine) && (
        <p className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-[11px] text-neutral-400">
          {rec.policy && <PolicyBadge policy={rec.policy} />}
          {rec.confidence && <span>confidence {rec.confidence}</span>}
          {versionLine && <span>{versionLine}</span>}
        </p>
      )}
      {rec.adjustments && rec.adjustments.length > 0 && (
        <p className="mt-1 text-[11px] text-amber-700">
          adjusted: {rec.adjustments.map((a) => `${a.from} → ${a.to}`).join(", ")}
        </p>
      )}
      {rec.comment && (
        <blockquote className="mt-2.5 rounded-r-md border-l-[3px] border-indigo-200 bg-neutral-50 px-3 py-2 text-sm leading-relaxed whitespace-pre-wrap text-neutral-700">
          {rec.comment}
        </blockquote>
      )}
      {rec.transcription && (
        <details className="mt-2">
          <summary className="cursor-pointer text-xs text-neutral-500 hover:text-neutral-800">
            Transcription
          </summary>
          <pre className="mt-1 rounded-md border border-neutral-200 bg-neutral-50 p-2 font-mono text-xs whitespace-pre-wrap text-neutral-700">
            {rec.transcription}
          </pre>
        </details>
      )}
      <p className="mt-2 text-[11px] text-neutral-400 sm:hidden">{fmtDate(rec.created_at)}</p>
    </div>
  );
}
