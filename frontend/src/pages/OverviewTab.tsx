// Overview tab (the assessment's default): Gradescope-style workflow dashboard.
// Renders the grading pipeline as a numbered checklist whose live statuses come from
// the same endpoints (and react-query keys) the individual tabs already use, so the
// caches are shared, plus one clear next-action link per step.

import type { ReactNode } from "react";
import { Link } from "react-router";
import { useQueries, useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";
import type {
  Assessment,
  IngestReport,
  MaskReviewPage,
  Method,
  ProblemSummary,
  Provider,
  RubricResponse,
  Run,
  RunListRow,
  WorkflowWarning,
} from "../lib/types";
import { warningView } from "../lib/warnings";
import { Badge, Card, Spinner, buttonClassName, type BadgeTone } from "../components/ui";
import { WorkflowNotice } from "../components/WorkflowNotice";
import { TranscriptionExportCard } from "./TranscriptionExportCard";

export function OverviewTab({
  assessmentId,
  assessment,
}: {
  assessmentId: string;
  assessment: Assessment;
}) {
  const summary = useQuery({
    queryKey: ["problem-summaries", assessmentId],
    queryFn: () =>
      api.get<{ problems: ProblemSummary[] }>(
        `/api/assessments/${assessmentId}/problems/summary`,
      ),
  });
  const problems = summary.data?.problems ?? [];

  // Per-problem rubric fetches share ProblemPanel's ["rubric", id] key.
  const rubrics = useQueries({
    queries: problems.map((p) => ({
      queryKey: ["rubric", p.problem_id],
      queryFn: () => api.get<RubricResponse>(`/api/problems/${p.problem_id}/rubric`),
      staleTime: 30_000,
    })),
  });

  const report = useQuery({
    queryKey: ["ingest-report", assessmentId],
    queryFn: () => api.get<IngestReport>(`/api/assessments/${assessmentId}/ingest/report`),
  });

  const masks = useQuery({
    queryKey: ["mask-review", assessmentId],
    queryFn: () =>
      api.get<{ pages: MaskReviewPage[] }>(`/api/assessments/${assessmentId}/masks/review`),
  });

  const methods = useQuery({
    queryKey: ["methods", false],
    queryFn: () => api.get<{ methods: Method[] }>("/api/methods"),
  });

  const providers = useQuery({
    queryKey: ["providers"],
    queryFn: () => api.get<{ providers: Provider[] }>("/api/providers"),
  });

  // Key mirrors Runs.tsx's ["runs", filterAssessment, filterStatus] so hopping
  // between here and a filtered Runs page reuses the cache. Overview is the
  // recommended home base, so it must stay live while a run is grading: poll every
  // 4s WHILE any run is still pending/running (mirrors Runs.tsx's isActive gate) so
  // Step 4's "last run: running 12/340" advances, and stop once none are active so a
  // quiet Overview doesn't poll forever (refetchOnWindowFocus is globally off).
  const runs = useQuery({
    queryKey: ["runs", assessmentId, ""],
    queryFn: () =>
      api.get<{ runs: RunListRow[] }>(`/api/runs?assessment_id=${assessmentId}`),
    refetchInterval: (query) =>
      (query.state.data?.runs ?? []).some(isActiveRun) ? 4000 : false,
  });
  const runsActive = (runs.data?.runs ?? []).some(isActiveRun);

  const tabHref = (key: string) => `/assessments/${assessmentId}?tab=${key}`;

  // Workflow-guard warnings (plan 2026-07-10): each standing hazard renders as a
  // compact notice line under the step it belongs to. email_*/publish-scoped codes
  // are the Publish tab's job, so they have no step here and simply don't render.
  //
  // Inlined here (not the shared useWorkflowWarnings hook) so this home-base view can
  // POLL the live hazard notices: the run_in_progress / batch_processing notices
  // clear only when the server recomputes them, so without polling they freeze
  // forever alongside Step 4. Refetch every 4s while a run is active (from the runs
  // query above) or a transient hazard is present, and stop once the standing list is
  // quiet. Same cache key as the shared hook, so the other tabs still read one entry.
  const warnings = useQuery({
    queryKey: ["workflow-warnings", assessmentId],
    queryFn: () =>
      api.get<{ warnings: WorkflowWarning[] }>(
        `/api/assessments/${assessmentId}/workflow-warnings`,
      ),
    refetchInterval: (query) => {
      const live = (query.state.data?.warnings ?? []).some((w) =>
        LIVE_WARNING_CODES.has(w.code),
      );
      return live || runsActive ? 4000 : false;
    },
  });
  const noticesFor = (...codes: string[]): ReactNode => {
    const hits = (warnings.data?.warnings ?? []).filter((w) => codes.includes(w.code));
    if (hits.length === 0) return null;
    return hits.map((w) => {
      const v = warningView(w, assessmentId);
      return (
        <WorkflowNotice key={w.code} tone={v.tone} to={v.to}>
          {v.message}
        </WorkflowNotice>
      );
    });
  };

  // --- step 1: problems & rubrics ---------------------------------------------------
  const problemCount = problems.length;
  const rubricCount = rubrics.filter((r) => r.data?.current != null).length;
  const problemsStep: StepState = summary.isPending
    ? loading()
    : summary.isError
      ? failed(summary.error.message)
      : problemCount === 0
        ? {
            badge: "no problems yet",
            tone: "amber",
            detail: "Add every problem (number, max points) and a rubric for each — all later steps depend on them.",
          }
        : rubrics.some((r) => r.isPending)
          ? loading()
          : {
              badge: `rubrics ${rubricCount}/${problemCount}`,
              tone: rubricCount === problemCount ? "green" : "amber",
              detail: `${problemCount} problem${problemCount === 1 ? "" : "s"} · ${rubricCount} with a rubric. The AI grades against the rubric, so finish these first.`,
            };

  // --- step 2: collect student work -------------------------------------------------
  // A student "has work" via either intake path: a direct PDF submission (Submissions
  // tab) OR at least one scan page mapped into their answers (Identify tab
  // assign+Finalize). mapped_pages already counts answer_pages regardless of how they
  // got there (ingestion.sql IngestReportRows), so scan-only students are visible here
  // without a second query — same report the reconciliation table below reads (audit
  // B2: this used to count submission_id only, so a scan-path assessment showed
  // "0/14 have work" beside "40/40 pages accepted").
  const students = report.data?.students ?? [];
  const submitted = students.filter((s) => s.submission_id !== undefined).length;
  const viaScansOnly = students.filter(
    (s) => s.submission_id === undefined && s.mapped_pages > 0,
  ).length;
  const haveWork = submitted + viaScansOnly;
  const collectStep: StepState = report.isPending
    ? loading()
    : report.isError
      ? failed(report.error.message)
      : students.length === 0
        ? {
            badge: "no students yet",
            tone: "amber",
            detail: (
              <>
                Add the roster on the{" "}
                <Link to="/students" className="font-medium text-indigo-600 hover:underline">
                  Students page
                </Link>{" "}
                first, then upload their work.
              </>
            ),
          }
        : {
            badge: `${haveWork}/${students.length} have work`,
            tone: haveWork === students.length ? "green" : haveWork > 0 ? "indigo" : "neutral",
            detail: `${haveWork} of ${students.length} students have work${
              viaScansOnly > 0 ? ` (${viaScansOnly} via scanned pages, no PDF submission)` : ""
            }. Pick the intake that matches what you have:`,
          };

  // --- step 3: mask identities -------------------------------------------------------
  const pages = masks.data?.pages ?? [];
  const accepted = pages.filter((p) => p.review_status === "accepted").length;
  const maskStep: StepState = masks.isPending
    ? loading()
    : masks.isError
      ? failed(masks.error.message)
      : pages.length === 0
        ? {
            badge: "no pages yet",
            tone: "neutral",
            detail: "Once student work is in, masks hide names and IDs before pages go to the AI.",
          }
        : {
            badge: `${accepted}/${pages.length} pages accepted`,
            tone: accepted === pages.length ? "green" : "amber",
            detail:
              "Masks hide student names and IDs before pages go to the AI. Accept each page's mask (unaccepted pages block grading runs).",
          };

  // --- step 4: AI grading --------------------------------------------------------------
  const usableMethods = (methods.data?.methods ?? []).filter(
    (m) => !m.archived && m.latest !== undefined,
  );
  const noProviders = providers.isSuccess && providers.data.providers.length === 0;
  const noMethods = methods.isSuccess && usableMethods.length === 0;
  const latestRun = runs.data?.runs[0];
  const gradingStep: StepState =
    methods.isPending || providers.isPending || runs.isPending
      ? loading()
      : methods.isError
        ? failed(methods.error.message)
        : noProviders
          ? {
              badge: "no provider yet",
              tone: "amber",
              detail: (
                <>
                  No AI provider is configured — add one on the{" "}
                  <Link to="/providers" className="font-medium text-indigo-600 hover:underline">
                    Providers page
                  </Link>{" "}
                  first (a default grading method is seeded from it).
                </>
              ),
            }
          : noMethods
            ? {
                badge: "no method yet",
                tone: "amber",
                detail: (
                  <>
                    No grading method yet — create one on the{" "}
                    <Link to="/methods" className="font-medium text-indigo-600 hover:underline">
                      Methods page
                    </Link>
                    .
                  </>
                ),
              }
            : latestRun === undefined
              ? {
                  badge: "no runs yet",
                  tone: "neutral",
                  detail: "Launch a run to have the AI grade every answer in this assessment.",
                }
              : {
                  badge: `last run: ${runProgress(latestRun)}`,
                  tone: runTone(latestRun),
                  detail: (
                    <>
                      Latest run {runProgress(latestRun)}. Full history on the{" "}
                      <Link
                        to="/runs"
                        className="font-medium text-indigo-600 hover:underline"
                      >
                        Runs page
                      </Link>
                      .
                    </>
                  ),
                };
  const canLaunch = !noProviders && !noMethods && methods.isSuccess;

  // --- steps 5-7: review, final source, publish (all from problems/summary) ----------
  const answerCount = problems.reduce((n, p) => n + p.answers, 0);
  const officialCount = problems.reduce((n, p) => n + p.official_set, 0);
  const publishedCount = problems.reduce((n, p) => n + p.published, 0);

  const reviewStep: StepState = summary.isPending
    ? loading()
    : summary.isError
      ? failed(summary.error.message)
      : answerCount === 0
        ? {
            badge: "no answers yet",
            tone: "neutral",
            detail: "Answers appear here once student work is collected.",
          }
        : {
            badge: `${officialCount}/${answerCount} answers official`,
            tone:
              officialCount === answerCount ? "green" : officialCount > 0 ? "indigo" : "neutral",
            detail:
              "Spot-check grades per problem and override any the AI got wrong. Grades only become official after step 6.",
          };

  const finalStep: StepState =
    assessment.final_source_kind === undefined
      ? {
          badge: "not chosen",
          tone: "red",
          detail:
            "Not chosen — no grade is official yet. Pick which source (an AI run or consensus) becomes each student's final grade.",
        }
      : {
          badge: `source: ${assessment.final_source_kind}`,
          tone: "green",
          detail:
            assessment.final_source_kind === "method"
              ? "Final grades come from one pinned grading run (manual grades fill gaps)."
              : "Final grades come from consensus across methods (manual grades fill gaps).",
        };

  const publishStep: StepState = summary.isPending
    ? loading()
    : summary.isError
      ? failed(summary.error.message)
      : publishedCount === 0
        ? {
            badge: "not published",
            tone: "neutral",
            detail: "Publishing emails each student their grade and opens the regrade window.",
          }
        : {
            badge: `${publishedCount}/${answerCount} published`,
            tone: publishedCount === answerCount ? "green" : "indigo",
            detail: "Publishing emails each student their grade and opens the regrade window.",
          };

  // --- step 4: calibration sample (guide §3.1) ------------------------------------
  const sampleRuns = (runs.data?.runs ?? []).filter((r) => r.scope_kind === "sample");
  const latestSample = sampleRuns[0];
  const calibrateStep: StepState = runs.isPending
    ? loading()
    : sampleRuns.length === 0
      ? {
          badge: "no calibration run",
          tone: "neutral",
          detail:
            "Grade a small stratified sample first (the guide suggests 5–10 answers), hand-grade a few of the same answers, and compare on Analysis before grading the whole class.",
        }
      : {
          badge: `${sampleRuns.length} calibration run${sampleRuns.length === 1 ? "" : "s"} — latest ${latestSample.status}`,
          tone: latestSample.status === "completed" ? "green" : "indigo",
          detail:
            "Compare AI vs hand grades on Analysis; after a rubric or prompt change, re-run the sample before the class-wide run.",
        };

  // --- step 9: regrades (guide 發布後) ----------------------------------------------
  const regradeStep: StepState = {
    badge: assessment.regrade_deadline ? "deadline set" : "no deadline",
    tone: assessment.regrade_deadline ? "green" : publishedCount > 0 ? "amber" : "neutral",
    detail:
      "Students reply to their result email with <p1>…</p1> blocks; adjudicate per problem in the Regrade inbox. Deadline and per-round methods live on Regrade rounds.",
  };

  return (
    <div className="space-y-4">
      <Card title="Grading workflow">
        <ol className="divide-y divide-neutral-100">
          <StepRow
            n={1}
            title="Problems & rubrics"
            state={problemsStep}
            action={<TabLink href={tabHref("problems")}>Set up problems →</TabLink>}
          />
          <StepRow
            n={2}
            title="Collect student work"
            state={collectStep}
            notices={noticesFor(
              "stranded_scan_pages",
              "unidentified_scan_pages",
              "dead_scan_pages",
              "text_render_loss",
              "assigned_unpromoted_pages",
              "quarantined_uploads",
              "batch_processing",
              "unmaterialized_students",
            )}
            action={
              <>
                <TabLink href={tabHref("submissions")}>One PDF per student → Submissions</TabLink>
                <TabLink href={tabHref("identify")}>Scanner pile (big PDF/zip) → Identify</TabLink>
              </>
            }
          />
          <StepRow
            n={3}
            title="Mask identities"
            state={maskStep}
            notices={noticesFor("mask_errors", "stale_masks")}
            action={<TabLink href={tabHref("masking")}>Review masks →</TabLink>}
          />
          <StepRow
            n={4}
            title="Calibrate on a sample"
            state={calibrateStep}
            action={
              <>
                {canLaunch && (
                  <Link
                    to={`/runs?launch=1&assessment_id=${assessmentId}&scope=sample&n=8`}
                    className={buttonClassName("secondary", "px-2.5 py-1 text-xs")}
                  >
                    Start calibration run
                  </Link>
                )}
                <TabLink href={tabHref("analysis")}>Compare in Analysis →</TabLink>
              </>
            }
          />
          <StepRow
            n={5}
            title="AI grading"
            state={gradingStep}
            notices={noticesFor("run_in_progress", "no_rubric_problems")}
            action={
              gradingStep.loading ? null : canLaunch ? (
                <Link
                  to={`/runs?launch=1&assessment_id=${assessmentId}`}
                  className={buttonClassName("primary", "px-2.5 py-1 text-xs")}
                >
                  Start AI grading
                </Link>
              ) : noProviders ? (
                <TabLink href="/providers">Add a provider →</TabLink>
              ) : (
                <TabLink href="/methods">Create a method →</TabLink>
              )
            }
          />
          <StepRow
            n={6}
            title="Review grades"
            state={reviewStep}
            notices={noticesFor(
              "superseded_answers",
              "mixed_method_versions",
              "adjusted_spot_checks",
            )}
            action={<TabLink href={tabHref("review")}>Open review →</TabLink>}
          />
          <StepRow
            n={7}
            title="Choose the final grading source"
            state={finalStep}
            action={<TabLink href={tabHref("publish")}>Choose on Publish →</TabLink>}
          />
          <StepRow
            n={8}
            title="Publish grades"
            state={publishStep}
            action={<TabLink href={tabHref("publish")}>Open publish →</TabLink>}
          />
          <StepRow
            n={9}
            title="Handle regrades"
            state={regradeStep}
            action={
              <>
                <TabLink href={tabHref("regrades")}>Regrade rounds →</TabLink>
                <TabLink href="/regrades">Open regrade inbox →</TabLink>
              </>
            }
          />
        </ol>
      </Card>
      {/* The LaTeX export lives here, right under the workflow, and is rendered for
          every assessment from creation onward (spec §6.1): it is object-scoped ("the
          midterm's LaTeX"), not stage-scoped, so it belongs beside the assessment's
          home view rather than behind the fifth tab. Its own content is the ladder that
          says which workflow step still stands between here and the ZIP. */}
      <TranscriptionExportCard assessmentId={assessmentId} />
    </div>
  );
}

// --- step plumbing -------------------------------------------------------------------

interface StepState {
  badge: string;
  tone: BadgeTone;
  detail: ReactNode;
  loading?: boolean;
}

function loading(): StepState {
  return { badge: "", tone: "neutral", detail: null, loading: true };
}

function failed(message: string): StepState {
  return { badge: "unavailable", tone: "red", detail: message };
}

// Workflow-warning codes that describe a transient, self-clearing state — while one
// of these is present the Overview keeps polling workflow-warnings so the notice
// disappears on its own once the server recomputes.
const LIVE_WARNING_CODES = new Set(["run_in_progress", "batch_processing"]);

/** A run still doing work — mirrors Runs.tsx's isActive; drives the Overview poll gate. */
function isActiveRun(run: Run): boolean {
  return run.status === "pending" || run.status === "running";
}

function runProgress(run: Run): string {
  const total = Object.values(run.counts).reduce((a, b) => a + (b ?? 0), 0);
  const succeeded = run.counts.succeeded ?? 0;
  return `${run.status} ${succeeded}/${total}`;
}

function runTone(run: Run): BadgeTone {
  switch (run.status) {
    case "completed":
      return "green";
    case "pending":
    case "running":
      return "indigo";
    case "failed":
      return "red";
    default: // paused | cancelled
      return "amber";
  }
}

function StepRow({
  n,
  title,
  state,
  notices,
  action,
}: {
  n: number;
  title: string;
  state: StepState;
  /** Workflow-guard notice lines for hazards belonging to this step. */
  notices?: ReactNode;
  action: ReactNode;
}) {
  return (
    <li className="flex gap-3 py-3 first:pt-0 last:pb-0">
      <span className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full bg-neutral-100 text-xs font-semibold text-neutral-600">
        {n}
      </span>
      <div className="min-w-0 flex-1 space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-medium text-neutral-900">{title}</span>
          {state.loading ? <Spinner className="size-3.5" /> : <Badge tone={state.tone}>{state.badge}</Badge>}
        </div>
        {state.detail !== null && <p className="text-sm text-neutral-600">{state.detail}</p>}
        {notices != null && <div className="space-y-1.5 pt-0.5">{notices}</div>}
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 pt-0.5">{action}</div>
      </div>
    </li>
  );
}

function TabLink({ href, children }: { href: string; children: ReactNode }) {
  return (
    <Link to={href} className="text-sm font-medium text-indigo-600 hover:underline">
      {children}
    </Link>
  );
}
