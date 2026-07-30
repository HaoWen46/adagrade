// Assessment detail: header (rename/kind/archived) + two-level stage nav
// (docs/superpowers/specs/2026-07-10-assessment-stage-nav-design.md) — five
// workflow stages (Overview · Problems · Student work · Grading · Results)
// with the individual views as pills inside the active stage.
//
// Tab state lives in the URL (?tab=key) so every view is linkable from
// anywhere; the active stage is derived from the tab key. Other pages navigate
// with plain links to /assessments/{id}?tab=<key>.

import { useMemo, useState, type FormEvent } from "react";
import { Link, useParams, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../lib/api";
import { roleAtLeast, useMe } from "../lib/auth";
import { toCents } from "../lib/decimal";
import { userLabel } from "../lib/userLabel";
import type {
  AssessmentDetailResponse,
  AssessmentTotalRow,
  AssessmentTotalsResponse,
  GradersResponse,
  Problem,
  ProblemSummary,
  ProblemTAResponse,
  PublishPreview,
  TAAssignmentRow,
  TAAssignmentsResponse,
} from "../lib/types";
import {
  Badge,
  Button,
  Card,
  Dialog,
  Field,
  IconButton,
  Input,
  Select,
  Spinner,
  TD,
  TH,
  Table,
  Textarea,
  buttonClassName,
  cx,
} from "../components/ui";
import { Pencil } from "../components/icons";
import { HelpTip } from "../components/HelpTip";
import { regradeTAHelp, totalsHelp } from "../lib/helpContent";
import { OverviewTab } from "./OverviewTab";
import { ProblemPanel } from "./ProblemPanel";
import { SubmissionsTab } from "./SubmissionsTab";
import { IdentifyTab } from "./IdentifyTab";
import { MaskingTab } from "./MaskingTab";
import { ReviewTab } from "./ReviewTab";
import { ConsensusTab } from "./ConsensusTab";
import { AnalysisTab } from "./AnalysisTab";
import { PublishTab } from "./PublishTab";
import { RegradeRoundsTab } from "./RegradeRoundsTab";

type TabKey =
  | "overview"
  | "problems"
  | "submissions"
  | "identify"
  | "masking"
  | "review"
  | "consensus"
  | "analysis"
  | "regrades"
  | "totals"
  | "publish";

interface TabDef {
  key: TabKey;
  label: string;
  hint?: string;
}

interface StageDef {
  label: string;
  tabs: TabDef[];
}

// Stage-click lands on the first pill; Grading→Review and Results→Totals keep
// the frequent Review↔Totals hop at one click each way.
const STAGES: StageDef[] = [
  { label: "Overview", tabs: [{ key: "overview", label: "Overview" }] },
  { label: "Problems", tabs: [{ key: "problems", label: "Problems" }] },
  {
    label: "Student work",
    tabs: [
      { key: "submissions", label: "Submissions", hint: "one PDF per student" },
      { key: "identify", label: "Identify", hint: "scanner pile (big PDF/zip)" },
      { key: "masking", label: "Masking" },
    ],
  },
  {
    label: "Grading",
    tabs: [
      { key: "review", label: "Review" },
      { key: "consensus", label: "Consensus" },
      { key: "analysis", label: "Analysis" },
    ],
  },
  {
    label: "Results",
    tabs: [
      { key: "totals", label: "Totals" },
      { key: "publish", label: "Publish" },
      { key: "regrades", label: "Regrade rounds" },
    ],
  },
];

const TABS: TabDef[] = STAGES.flatMap((s) => s.tabs);

export function AssessmentDetail() {
  const { id = "" } = useParams();
  const me = useMe();
  const canEdit = roleAtLeast(me.data?.user.role, "lecturer");
  // Tab state is URL-backed (?tab=key) so tabs are linkable and survive reloads;
  // unknown/absent param falls back to the Overview dashboard.
  const [searchParams, setSearchParams] = useSearchParams();
  const tabParam = searchParams.get("tab");
  const tab: TabKey = TABS.some((t) => t.key === tabParam) ? (tabParam as TabKey) : "overview";
  const setTab = (t: TabKey) => setSearchParams({ tab: t }, { replace: true });
  const activeStage = STAGES.find((s) => s.tabs.some((t) => t.key === tab)) ?? STAGES[0];
  const [addOpen, setAddOpen] = useState(false);
  const [editing, setEditing] = useState<Problem | null>(null);
  const [expandedId, setExpandedId] = useState<number | null>(null);

  const detail = useQuery({
    queryKey: ["assessment", id],
    queryFn: () => api.get<AssessmentDetailResponse>(`/api/assessments/${id}`),
    enabled: id !== "",
  });

  // The Regrade TA column is read-only visible to TA+ (the endpoint itself,
  // GET /api/assessments/{id}/ta-assignments, is TA+ readable) — only the PICKER
  // (PUT /api/problems/{id}/ta, a mutation) is gated lecturer+ (regrade v2 UI review
  // Finding 4: previously the whole column + fetch were hidden from TAs even though they
  // could read the same data via the API directly).
  const canView = roleAtLeast(me.data?.user.role, "ta");
  // GET /api/assessments/{id}/ta-assignments (regrade v2 gap 1, TA+) backs the column's
  // current-assignment display (read-only for TAs, editable via TAPicker for lecturer+) —
  // fetched once here rather than per-row. These hooks must stay above the
  // isPending/isError returns: hooks after an early return crash the whole page
  // with React #310 once the detail query resolves and the hook count grows.
  const taAssignments = useQuery({
    queryKey: ["ta-assignments", id],
    queryFn: () => api.get<TAAssignmentsResponse>(`/api/assessments/${id}/ta-assignments`),
    enabled: canView && id !== "",
    staleTime: 15_000,
  });
  const assignmentByProblemID = useMemo(
    () => new Map((taAssignments.data?.assignments ?? []).map((a) => [a.problem_id, a] as const)),
    [taAssignments.data],
  );

  if (detail.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (detail.isError) {
    return (
      <div className="mx-auto max-w-5xl">
        <Card title="Assessment">
          <p className="text-sm text-red-600">{detail.error.message}</p>
          <Link to="/assessments" className="mt-2 inline-block text-sm text-indigo-600 hover:underline">
            Back to assessments
          </Link>
        </Card>
      </div>
    );
  }

  const { assessment, problems } = detail.data;
  const cols = canView ? 7 : 5;

  return (
    <div className="mx-auto max-w-5xl space-y-4">
      <div className="flex items-start justify-between">
        <div>
          <Link to="/assessments" className="text-xs text-neutral-500 hover:text-neutral-800">
            ← Assessments
          </Link>
          <div className="mt-1 flex items-center gap-2.5">
            <AssessmentName id={id} name={assessment.name} canEdit={canEdit} />
            <Badge tone={assessment.kind === "exam" ? "indigo" : "neutral"}>{assessment.kind}</Badge>
            {assessment.archived && <Badge tone="amber">archived</Badge>}
          </div>
        </div>
        <a
          href={`/api/assessments/${id}/export.csv`}
          download
          className={buttonClassName("secondary", "px-2.5 py-1.5 text-xs")}
        >
          Export CSV
        </a>
      </div>

      <nav aria-label="Assessment stages">
        <div className="flex gap-1 overflow-x-auto whitespace-nowrap border-b border-neutral-200">
          {STAGES.map((s) => {
            const active = s === activeStage;
            return (
              <Link
                key={s.label}
                to={`/assessments/${id}?tab=${s.tabs[0].key}`}
                replace
                preventScrollReset
                aria-current={active ? "true" : undefined}
                className={cx(
                  "-mb-px shrink-0 border-b-2 px-2.5 py-2 text-sm font-medium transition-colors",
                  active
                    ? "border-indigo-600 text-indigo-700"
                    : "border-transparent text-neutral-500 hover:text-neutral-800",
                )}
              >
                {s.label}
              </Link>
            );
          })}
        </div>
        <div className="flex min-h-9 items-center justify-between gap-3">
          <div className="flex gap-1.5 overflow-x-auto whitespace-nowrap py-1.5">
            {activeStage.tabs.length > 1 &&
              activeStage.tabs.map((t) => (
                <Link
                  key={t.key}
                  to={`/assessments/${id}?tab=${t.key}`}
                  replace
                  preventScrollReset
                  title={t.hint}
                  aria-current={tab === t.key ? "page" : undefined}
                  className={cx(
                    "shrink-0 rounded-full px-3 py-1 text-xs font-medium transition-colors",
                    tab === t.key
                      ? "bg-indigo-600 text-white"
                      : "text-neutral-600 ring-1 ring-neutral-300 ring-inset hover:text-neutral-900",
                  )}
                >
                  {t.label}
                </Link>
              ))}
          </div>
          {activeStage.label === "Grading" && (
            <Link
              to={`/runs?launch=1&assessment_id=${id}`}
              className={buttonClassName("primary", "shrink-0 px-2.5 py-1 text-xs")}
            >
              Start AI grading
            </Link>
          )}
        </div>
      </nav>

      {tab === "overview" && <OverviewTab assessmentId={id} assessment={assessment} />}
      {tab === "problems" && (
        <div className="space-y-2">
          <div className="flex items-center justify-between">
            <h2 className="text-sm font-semibold text-neutral-900">Problems</h2>
            {canEdit && (
              <Button className="px-2.5 py-1 text-xs" onClick={() => setAddOpen(true)}>
                Add problem
              </Button>
            )}
          </div>
          <Table>
            <thead>
              <tr>
                <TH className="w-16">#</TH>
                <TH>Title</TH>
                <TH className="w-28 text-right">Max points</TH>
                <TH className="w-24 text-right">Position</TH>
                <TH className="w-20" />
                {canView && (
                  <TH className="w-48">
                    <span className="inline-flex items-center gap-1.5">
                      Regrade TA <HelpTip title="Regrade TA">{regradeTAHelp}</HelpTip>
                    </span>
                  </TH>
                )}
                {canEdit && <TH className="w-16" />}
              </tr>
            </thead>
            <tbody>
              {problems.length === 0 && (
                <tr>
                  <TD colSpan={cols} className="text-center text-neutral-400">
                    No problems yet.
                  </TD>
                </tr>
              )}
              {problems.map((p) => (
                <ProblemRow
                  key={p.id}
                  problem={p}
                  canEdit={canEdit}
                  canViewTA={canView}
                  cols={cols}
                  expanded={expandedId === p.id}
                  onToggle={() => setExpandedId(expandedId === p.id ? null : p.id)}
                  onEdit={() => setEditing(p)}
                  currentAssignment={assignmentByProblemID.get(p.id)}
                  assignmentsLoaded={taAssignments.isSuccess}
                />
              ))}
            </tbody>
          </Table>
        </div>
      )}
      {tab === "submissions" && <SubmissionsTab assessmentId={id} />}
      {tab === "identify" && <IdentifyTab assessmentId={id} />}
      {tab === "masking" && <MaskingTab assessmentId={id} />}
      {tab === "review" && <ReviewTab assessmentId={id} />}
      {tab === "consensus" && (
        <ConsensusTab assessmentId={id} onGoToReview={() => setTab("review")} />
      )}
      {tab === "regrades" && <RegradeRoundsTab assessmentId={id} />}
      {/* The LaTeX transcription export used to sit here under Totals; it now lives on
          the Overview tab as an always-present ladder card (spec §6.1) — one surface,
          one truth, and no second copy to fall out of sync. */}
      {tab === "totals" && (
        <TotalsCard assessmentId={id} finalSourceKind={assessment.final_source_kind} />
      )}
      {tab === "analysis" && (
        <AnalysisTab assessmentId={id} onGoToReview={() => setTab("review")} />
      )}
      {tab === "publish" && (
        <PublishTab
          assessmentId={id}
          assessmentName={assessment.name}
          onGoToReview={() => setTab("review")}
        />
      )}

      {addOpen && <ProblemDialog assessmentId={id} onClose={() => setAddOpen(false)} />}
      {editing && (
        <ProblemDialog assessmentId={id} problem={editing} onClose={() => setEditing(null)} />
      )}
    </div>
  );
}

// --- header name with inline rename -----------------------------------------------

function AssessmentName({ id, name, canEdit }: { id: string; name: string; canEdit: boolean }) {
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(name);

  const rename = useMutation({
    mutationFn: (newName: string) => api.patch(`/api/assessments/${id}`, { name: newName }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["assessment", id] });
      await queryClient.invalidateQueries({ queryKey: ["assessments"] });
      setEditing(false);
    },
  });

  if (!editing) {
    return (
      <>
        <h1 className="text-lg font-semibold text-neutral-900">{name}</h1>
        {canEdit && (
          <IconButton
            label="Rename assessment"
            onClick={() => {
              setValue(name);
              setEditing(true);
            }}
          >
            <Pencil />
          </IconButton>
        )}
      </>
    );
  }

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = value.trim();
    if (trimmed && trimmed !== name) rename.mutate(trimmed);
    else setEditing(false);
  };

  return (
    <form className="flex items-center gap-2" onSubmit={submit}>
      <Input
        autoFocus
        value={value}
        onChange={(e) => setValue(e.target.value)}
        className="w-64 py-1"
      />
      <Button type="submit" className="px-2.5 py-1 text-xs" disabled={rename.isPending}>
        Save
      </Button>
      <Button
        variant="secondary"
        className="px-2.5 py-1 text-xs"
        onClick={() => setEditing(false)}
      >
        Cancel
      </Button>
      {rename.isError && <p className="text-xs text-red-600">{rename.error.message}</p>}
    </form>
  );
}

// --- totals card (GET /api/assessments/{id}/totals; display only, D3: absent total
// until an official record exists — never render a silent 0) -----------------------

type TotalsSortKey = "student_id" | "total";

function TotalsCard({
  assessmentId,
  finalSourceKind,
}: {
  assessmentId: string;
  finalSourceKind?: "method" | "consensus";
}) {
  const [sortKey, setSortKey] = useState<TotalsSortKey>("student_id");
  const [sortDesc, setSortDesc] = useState(false);

  const totals = useQuery({
    queryKey: ["assessment-totals", assessmentId],
    queryFn: () =>
      api.get<AssessmentTotalsResponse>(`/api/assessments/${assessmentId}/totals`),
  });

  // D3: totals stay absent (—) until an official record exists, and nothing is
  // official before a final grading source is chosen — so grades-without-a-source
  // would otherwise look like a silent bug here. The totals response only counts
  // official grades (zero in exactly that scenario), so detect AI/human records
  // via the problems summary instead.
  const summary = useQuery({
    queryKey: ["problem-summaries", assessmentId],
    queryFn: () =>
      api.get<{ problems: ProblemSummary[] }>(
        `/api/assessments/${assessmentId}/problems/summary`,
      ),
  });
  const gradesExist = (summary.data?.problems ?? []).some(
    (p) => p.ai_graded + p.human_graded > 0,
  );

  const rows = useMemo(() => {
    const students = totals.data?.students ?? [];
    const sorted = [...students].sort((a, b) => {
      if (sortKey === "total") {
        const ca = a.total !== undefined ? toCents(a.total) : null;
        const cb = b.total !== undefined ? toCents(b.total) : null;
        // Students without an official total sort last regardless of direction.
        if (ca === null && cb === null) return 0;
        if (ca === null) return 1;
        if (cb === null) return -1;
        return sortDesc ? cb - ca : ca - cb;
      }
      return sortDesc
        ? b.student_id.localeCompare(a.student_id)
        : a.student_id.localeCompare(b.student_id);
    });
    return sorted;
  }, [totals.data, sortKey, sortDesc]);

  const toggleSort = (key: TotalsSortKey) => {
    if (key === sortKey) {
      setSortDesc((d) => !d);
    } else {
      setSortKey(key);
      setSortDesc(key === "total");
    }
  };

  const sortIndicator = (key: TotalsSortKey) => (key === sortKey ? (sortDesc ? " ▾" : " ▴") : "");

  return (
    <Card
      title={
        <span className="inline-flex items-center gap-1.5">
          Totals <HelpTip title="Totals">{totalsHelp}</HelpTip>
        </span>
      }
    >
      {totals.isPending && (
        <div className="flex justify-center py-6">
          <Spinner className="size-5" />
        </div>
      )}
      {totals.isError && <p className="text-sm text-red-600">{totals.error.message}</p>}
      {gradesExist && finalSourceKind === undefined && (
        <p className="mb-3 rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
          No final grading source chosen — totals stay empty until one is applied on the{" "}
          <Link
            to={`/assessments/${assessmentId}?tab=publish`}
            className="font-medium underline"
          >
            Publish tab
          </Link>
          .
        </p>
      )}
      {totals.isSuccess && (
        <Table>
          <thead>
            <tr>
              <TH>
                <button
                  type="button"
                  onClick={() => toggleSort("student_id")}
                  className="font-semibold hover:text-neutral-900"
                >
                  Student{sortIndicator("student_id")}
                </button>
              </TH>
              <TH>Name</TH>
              <TH className="w-24 text-right">Answers</TH>
              <TH className="w-24 text-right">Graded</TH>
              <TH className="w-28 text-right">
                <button
                  type="button"
                  onClick={() => toggleSort("total")}
                  className="font-semibold hover:text-neutral-900"
                >
                  Total{sortIndicator("total")}
                </button>
              </TH>
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 && (
              <tr>
                <TD colSpan={5} className="text-center text-neutral-400">
                  No students yet.
                </TD>
              </tr>
            )}
            {rows.map((row) => (
              <TotalsRow key={row.student_id} row={row} assessmentId={assessmentId} />
            ))}
          </tbody>
        </Table>
      )}
    </Card>
  );
}

function TotalsRow({
  row,
  assessmentId,
}: {
  row: AssessmentTotalRow;
  assessmentId: string;
}) {
  return (
    <tr>
      {/* The student cell is the entry point to the per-student page (spec
          2026-07-28-student-page-design.md); ?assessment= lands it pre-expanded on the
          exam being looked at right now. */}
      <TD className="font-medium tabular-nums">
        <Link
          to={`/students/${encodeURIComponent(row.student_id)}?assessment=${assessmentId}`}
          className="text-indigo-600 hover:underline"
        >
          {row.student_id}
        </Link>
      </TD>
      <TD>
        {row.name}
        {/* Roster-lifecycle (plan 2026-07-10, locked semantics e): withdrawn students
            stay visible in totals with an explicit marker — never silently dropped. */}
        {row.withdrawn && (
          <span title="Withdrawn — kept in totals with their history; excluded from new grading and publishing">
            <Badge tone="amber" className="ml-1.5">
              withdrawn
            </Badge>
          </span>
        )}
      </TD>
      <TD className="text-right tabular-nums">{row.answers}</TD>
      <TD className="text-right tabular-nums">{row.graded}</TD>
      <TD className="text-right tabular-nums">
        {row.total !== undefined ? row.total : <span className="text-neutral-400">—</span>}
      </TD>
    </tr>
  );
}

// --- problem rows ------------------------------------------------------------------

function ProblemRow({
  problem,
  canEdit,
  canViewTA,
  cols,
  expanded,
  onToggle,
  onEdit,
  currentAssignment,
  assignmentsLoaded,
}: {
  problem: Problem;
  canEdit: boolean;
  /** TA+ read visibility for the Regrade TA column (regrade v2 UI review Finding 4) —
   * distinct from canEdit, which gates only the mutating picker control. */
  canViewTA: boolean;
  cols: number;
  expanded: boolean;
  onToggle: () => void;
  onEdit: () => void;
  /** This problem's row from GET /api/assessments/{id}/ta-assignments, or undefined
   * while that fetch is still in flight (see assignmentsLoaded). */
  currentAssignment?: TAAssignmentRow;
  /** True once the assessment-wide ta-assignments fetch has resolved — distinguishes
   * "still loading" from "loaded, and this problem has no row" (shouldn't happen since
   * the backend returns every problem, but keeps the picker honest either way). */
  assignmentsLoaded: boolean;
}) {
  return (
    <>
      <tr onClick={onToggle} className="cursor-pointer hover:bg-neutral-50">
        <TD className="font-medium tabular-nums">
          <span className="mr-1.5 inline-block w-3 text-neutral-400">{expanded ? "▾" : "▸"}</span>
          {problem.number}
        </TD>
        <TD>{problem.title || <span className="text-neutral-400">untitled</span>}</TD>
        <TD className="text-right tabular-nums">{problem.max_points}</TD>
        <TD className="text-right tabular-nums">{problem.position}</TD>
        <TD className="text-right">
          <Link
            to={`/assessments/${problem.assessment_id}/problems/${problem.id}/review`}
            onClick={(e) => e.stopPropagation()}
            className={buttonClassName("ghost", "px-2 py-1 text-xs")}
          >
            Review
          </Link>
        </TD>
        {canViewTA && (
          <TD onClick={(e) => e.stopPropagation()}>
            {canEdit ? (
              <TAPicker
                problemId={problem.id}
                currentAssignment={currentAssignment}
                assignmentsLoaded={assignmentsLoaded}
              />
            ) : (
              <RegradeTADisplay
                currentAssignment={currentAssignment}
                assignmentsLoaded={assignmentsLoaded}
              />
            )}
          </TD>
        )}
        {canEdit && (
          <TD className="text-right">
            <IconButton
              label={`Edit problem ${problem.number}`}
              onClick={(e) => {
                e.stopPropagation();
                onEdit();
              }}
            >
              <Pencil />
            </IconButton>
          </TD>
        )}
      </tr>
      {expanded && (
        <tr>
          <TD colSpan={cols} className="bg-neutral-50/60 px-4 py-3">
            {problem.statement && (
              <p className="mb-3 text-sm whitespace-pre-wrap text-neutral-600">
                {problem.statement}
              </p>
            )}
            <ProblemPanel problem={problem} />
          </TD>
        </tr>
      )}
    </>
  );
}

// --- regrade TA read-only display (TA+ view, regrade v2 UI review Finding 4) -------

/**
 * Read-only counterpart to TAPicker for viewers who can read the assignment (TA+) but
 * cannot mutate it (lecturer+ only, via PUT /api/problems/{id}/ta). Mirrors TAPicker's
 * loading/unassigned states without a Select or mutation.
 */
function RegradeTADisplay({
  currentAssignment,
  assignmentsLoaded,
}: {
  currentAssignment?: TAAssignmentRow;
  assignmentsLoaded: boolean;
}) {
  if (!assignmentsLoaded) {
    return <Spinner className="size-3.5" />;
  }
  if (currentAssignment?.user_id != null) {
    return (
      <span className="text-xs text-neutral-700">
        {currentAssignment.user_name ?? "assigned (name unavailable)"}
      </span>
    );
  }
  return <span className="text-xs text-neutral-400">(no TA assigned)</span>;
}

// --- regrade TA picker (spec §6 D60: "at most one TA per problem", PUT-only) -------

/**
 * TA-per-problem assignment picker (regrade v2 spec §6 D60): PUT /api/problems/{id}/ta
 * {user_id}, lecturer+ (matches this row's canEdit gate). user_id: null unassigns.
 *
 * Data source is GET /api/graders (regrade v2 gap 2, lecturer+ — matches this row's
 * canEdit gate exactly, so no admin-only carve-out is needed): a minimal {id, name,
 * role} list of active TA-or-higher users, distinct from the admin-only GET /api/users.
 * Current assignment (regrade v2 gap 1) comes from the parent's ta-assignments fetch —
 * `currentAssignment` is this problem's row (user_id/user_name both null when
 * unassigned), `assignmentsLoaded` distinguishes "still loading" from "loaded, no
 * assignment" so the select doesn't briefly flash "(no TA assigned)" on first paint.
 */
function TAPicker({
  problemId,
  currentAssignment,
  assignmentsLoaded,
}: {
  problemId: number;
  currentAssignment?: TAAssignmentRow;
  assignmentsLoaded: boolean;
}) {
  // Local override: once this session successfully assigns/unassigns, trust that
  // result immediately rather than waiting for the ta-assignments query to refetch.
  const [localAssignedId, setLocalAssignedId] = useState<number | null | undefined>(undefined);
  const assignedId =
    localAssignedId !== undefined ? localAssignedId : (currentAssignment?.user_id ?? null);

  const graders = useQuery({
    queryKey: ["graders"],
    queryFn: () => api.get<GradersResponse>("/api/graders"),
    staleTime: 30_000,
  });
  // GET /api/graders already excludes inactive users and filters to TA-or-higher
  // server-side (internal/httpapi/regrade.go handleListGraders) — no client-side
  // re-filtering needed.
  const eligible = graders.data?.graders ?? [];

  const assign = useMutation({
    mutationFn: (userId: number | null) =>
      api.put<ProblemTAResponse>(`/api/problems/${problemId}/ta`, { user_id: userId }),
    onSuccess: (res) => setLocalAssignedId(res.user_id),
  });

  if (graders.isPending || (!assignmentsLoaded && localAssignedId === undefined)) {
    return <Spinner className="size-3.5" />;
  }
  if (graders.isError) {
    return <span className="text-xs text-red-600">{graders.error.message}</span>;
  }

  return (
    <div className="flex items-center gap-1.5">
      <Select
        className="py-1 text-xs"
        value={assignedId != null ? String(assignedId) : ""}
        disabled={assign.isPending}
        onChange={(e) => assign.mutate(e.target.value === "" ? null : Number(e.target.value))}
      >
        <option value="">(no TA assigned)</option>
        {eligible.map((g) => (
          <option key={g.id} value={String(g.id)}>
            {userLabel({ name: g.name, role: g.role })}
          </option>
        ))}
      </Select>
      {assign.isPending && <Spinner className="size-3.5 shrink-0" />}
      {assign.isError && (
        <span className="text-xs text-red-600">
          {assign.error instanceof ApiError ? assign.error.message : "assign failed"}
        </span>
      )}
    </div>
  );
}

// --- add / edit dialog ---------------------------------------------------------------

function ProblemDialog({
  assessmentId,
  problem,
  onClose,
}: {
  assessmentId: string;
  problem?: Problem;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const [number, setNumber] = useState(problem ? String(problem.number) : "");
  const [title, setTitle] = useState(problem?.title ?? "");
  const [statement, setStatement] = useState(problem?.statement ?? "");
  const [maxPoints, setMaxPoints] = useState(problem?.max_points ?? "");
  const [position, setPosition] = useState(problem ? String(problem.position) : "");
  const [confirmRenumber, setConfirmRenumber] = useState(false);

  // Renumber hazard (workflow guards): students' regrade emails reference problem
  // numbers, so changing one while a publish batch is live confuses reply threads.
  // has_live_batch comes from the publish preview (same cache key as PublishTab);
  // only fetched when editing an existing problem — Add can't renumber anything.
  const preview = useQuery({
    queryKey: ["publish-preview", assessmentId],
    queryFn: () => api.get<PublishPreview>(`/api/assessments/${assessmentId}/publish/preview`),
    enabled: problem !== undefined,
  });
  const hasLiveBatch = preview.data?.has_live_batch ?? false;

  const save = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = {
        number: parseInt(number, 10),
        title,
        statement,
        max_points: maxPoints.trim(),
      };
      if (position.trim() !== "") body.position = parseInt(position, 10);
      return problem
        ? api.patch<Problem>(`/api/problems/${problem.id}`, body)
        : api.post<Problem>(`/api/assessments/${assessmentId}/problems`, body);
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["assessment", assessmentId] });
      // problem_count on the list page changes too
      await queryClient.invalidateQueries({ queryKey: ["assessments"] });
      onClose();
    },
  });

  const valid = /^\d+$/.test(number.trim()) && maxPoints.trim() !== "";
  const renumbering = problem !== undefined && parseInt(number.trim(), 10) !== problem.number;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!valid) return;
    if (renumbering && hasLiveBatch) setConfirmRenumber(true);
    else save.mutate();
  };

  return (
    <Dialog open onClose={onClose} title={problem ? `Edit problem ${problem.number}` : "Add problem"}>
      <form className="space-y-3" onSubmit={submit}>
        <div className="grid grid-cols-3 gap-3">
          <Field label="Number">
            <Input
              required
              autoFocus={!problem}
              inputMode="numeric"
              placeholder="1"
              value={number}
              onChange={(e) => setNumber(e.target.value)}
            />
          </Field>
          <Field label="Max points">
            <Input
              required
              placeholder='e.g. "10" or "2.5"'
              value={maxPoints}
              onChange={(e) => setMaxPoints(e.target.value)}
            />
          </Field>
          <Field label="Position (optional)">
            <Input
              inputMode="numeric"
              placeholder="= number"
              value={position}
              onChange={(e) => setPosition(e.target.value)}
            />
          </Field>
        </div>
        <Field label="Title (optional)">
          <Input value={title} onChange={(e) => setTitle(e.target.value)} />
        </Field>
        <Field label="Statement (optional)">
          <Textarea rows={4} value={statement} onChange={(e) => setStatement(e.target.value)} />
        </Field>
        {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}
        <div className="flex justify-end gap-2 pt-1">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={save.isPending || !valid}>
            {save.isPending ? "Saving…" : problem ? "Save" : "Add"}
          </Button>
        </div>

        {confirmRenumber && (
          <Dialog open onClose={() => setConfirmRenumber(false)} title="Renumber this problem?">
            <p className="text-sm text-neutral-700">
              Students&apos; regrade emails reference problem numbers — renumbering
              mid-regrade-window will confuse threads. Continue?
            </p>
            {save.isError && <p className="mt-2 text-xs text-red-600">{save.error.message}</p>}
            <div className="mt-4 flex justify-end gap-2">
              <Button variant="secondary" onClick={() => setConfirmRenumber(false)}>
                Cancel
              </Button>
              <Button variant="danger" disabled={save.isPending} onClick={() => save.mutate()}>
                {save.isPending ? "Saving…" : "Continue"}
              </Button>
            </div>
          </Dialog>
        )}
      </form>
    </Dialog>
  );
}
