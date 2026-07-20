// Per-problem panel: Rubric (versioned criteria editor) and Reference solutions
// (versioned textarea). Saving either always creates a NEW version — old versions
// stay immutable so existing grades keep pointing at what they were graded with.

import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import type {
  Problem,
  ProblemSummary,
  RubricResponse,
  RubricVersion,
  SolutionVersion,
  SolutionsResponse,
} from "../lib/types";
import { decimalEquals, sumDecimalStrings } from "../lib/decimal";
import { fmtDate } from "../lib/format";
import { Badge, Button, Dialog, Field, Input, Spinner, Textarea, cx } from "../components/ui";

export function ProblemPanel({ problem }: { problem: Problem }) {
  const [tab, setTab] = useState<"rubric" | "solutions">("rubric");

  return (
    <div className="rounded-lg border border-neutral-200 bg-white shadow-sm">
      <div className="flex gap-1 border-b border-neutral-200 px-2">
        {(["rubric", "solutions"] as const).map((t) => (
          <button
            key={t}
            type="button"
            onClick={() => setTab(t)}
            className={cx(
              "-mb-px border-b-2 px-2.5 py-2 text-sm font-medium transition-colors",
              tab === t
                ? "border-indigo-600 text-indigo-700"
                : "border-transparent text-neutral-500 hover:text-neutral-800",
            )}
          >
            {t === "rubric" ? "Rubric" : "Reference solutions"}
          </button>
        ))}
      </div>
      <div className="p-4">
        {tab === "rubric" ? (
          <RubricSection problem={problem} />
        ) : (
          <SolutionsSection problemId={problem.id} />
        )}
      </div>
    </div>
  );
}

// --- version history (shared by rubric + solutions) ---------------------------------

function VersionHistory({
  versions,
  selectedId,
  onSelect,
}: {
  versions: Array<{ id: number; version: number; created_at?: string }>;
  selectedId: number | null;
  onSelect: (id: number | null) => void;
}) {
  return (
    <div>
      <h3 className="mb-1.5 text-xs font-semibold tracking-wide text-neutral-500 uppercase">
        Versions
      </h3>
      {versions.length === 0 ? (
        <p className="text-xs text-neutral-400">None yet.</p>
      ) : (
        <ul className="space-y-0.5">
          {versions.map((v, i) => (
            <li key={v.id}>
              <button
                type="button"
                onClick={() => onSelect(i === 0 ? null : v.id)}
                className={cx(
                  "w-full rounded-md px-2 py-1 text-left text-xs transition-colors",
                  (i === 0 ? selectedId === null : selectedId === v.id)
                    ? "bg-indigo-50 font-medium text-indigo-700"
                    : "text-neutral-600 hover:bg-neutral-100",
                )}
              >
                v{v.version}
                {i === 0 && " (current)"}
                <span className="block text-[11px] text-neutral-400">{fmtDate(v.created_at)}</span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// --- rubric ---------------------------------------------------------------------------

function RubricSection({ problem }: { problem: Problem }) {
  const rubric = useQuery({
    queryKey: ["rubric", problem.id],
    queryFn: () => api.get<RubricResponse>(`/api/problems/${problem.id}/rubric`),
  });
  // null = editor on the current version; a version id = read-only historical view.
  const [viewId, setViewId] = useState<number | null>(null);

  if (rubric.isPending) return <Spinner />;
  if (rubric.isError) return <p className="text-sm text-red-600">{rubric.error.message}</p>;

  const { current, versions } = rubric.data;

  return (
    <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_10rem]">
      <div className="min-w-0">
        {viewId !== null ? (
          <RubricVersionView versionId={viewId} onBack={() => setViewId(null)} />
        ) : (
          <RubricEditor key={current?.id ?? "new"} problem={problem} current={current} />
        )}
      </div>
      <VersionHistory versions={versions} selectedId={viewId} onSelect={setViewId} />
    </div>
  );
}

interface CriterionDraft {
  description: string;
  points: string;
  partial_credit_notes: string;
}

const EMPTY_CRITERION: CriterionDraft = { description: "", points: "", partial_credit_notes: "" };

function RubricEditor({ problem, current }: { problem: Problem; current: RubricVersion | null }) {
  const queryClient = useQueryClient();
  const [notes, setNotes] = useState(current?.notes ?? "");
  const [increment, setIncrement] = useState(current?.score_increment ?? "0.5");
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [criteria, setCriteria] = useState<CriterionDraft[]>(
    current?.criteria?.map((c) => ({
      description: c.description,
      points: c.points,
      partial_credit_notes: c.partial_credit_notes,
    })) ?? [{ ...EMPTY_CRITERION }],
  );

  // Existing grades all point at rubric versions < the one this save creates, so
  // they stop counting toward official grades until re-graded under it (workflow
  // guards). The problems summary (same cache key ReviewTab/TotalsCard use, keyed by
  // the STRING assessment id) supplies the live graded count that arms the confirm.
  const summary = useQuery({
    queryKey: ["problem-summaries", String(problem.assessment_id)],
    queryFn: () =>
      api.get<{ problems: ProblemSummary[] }>(
        `/api/assessments/${problem.assessment_id}/problems/summary`,
      ),
  });
  const summaryRow = summary.data?.problems.find((p) => p.problem_id === problem.id);
  const gradedCount = summaryRow ? summaryRow.ai_graded + summaryRow.human_graded : 0;

  const save = useMutation({
    mutationFn: () =>
      api.post<RubricVersion>(`/api/problems/${problem.id}/rubric`, {
        notes,
        score_increment: increment.trim() || "0.5",
        criteria,
      }),
    onSuccess: () => {
      setConfirmOpen(false);
      return queryClient.invalidateQueries({ queryKey: ["rubric", problem.id] });
    },
  });

  const setCriterion = (i: number, patch: Partial<CriterionDraft>) =>
    setCriteria(criteria.map((c, j) => (j === i ? { ...c, ...patch } : c)));

  const sum = sumDecimalStrings(criteria.map((c) => c.points));
  const sumOk = sum !== null && decimalEquals(sum, problem.max_points);
  // A fresh editor (no rubric yet, nothing typed) shows a neutral placeholder
  // instead of shouting "invalid points" before the user has entered anything.
  const pristine = current === null && criteria.every((c) => c.points.trim() === "");
  const nextVersion = (current?.version ?? 0) + 1;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!sumOk) return;
    // Grades already exist under older versions — make the consequence explicit
    // (with the live count) before creating the version that sidelines them.
    if (gradedCount > 0) setConfirmOpen(true);
    else save.mutate();
  };

  return (
    <form className="space-y-3" onSubmit={submit}>
      <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
        Saving creates version {nextVersion}. Grades recorded under older versions stop counting
        toward official grades until re-graded (or manually re-entered) under v{nextVersion}.
      </p>

      {/* One block per criterion (same shape as the answer-view grade form): the
          description gets the full width with points beside it, partial-credit
          notes on their own line — the old four-column row split the width
          between two text fields, so wordy criteria were unreadable while
          being written. Each part self-labels via its placeholder. */}
      <div className="space-y-3">
        {criteria.map((c, i) => (
          <div key={i} className="border-b border-neutral-100 pb-3 last:border-0 last:pb-0">
            <div className="flex items-start gap-3">
              <Input
                placeholder="What earns these points"
                value={c.description}
                onChange={(e) => setCriterion(i, { description: e.target.value })}
                className="flex-1"
              />
              <div className="flex shrink-0 items-center gap-1">
                <Input
                  placeholder="2.5"
                  className="w-16 text-right tabular-nums"
                  value={c.points}
                  onChange={(e) => setCriterion(i, { points: e.target.value })}
                />
                <span className="text-xs text-neutral-400">pts</span>
              </div>
            </div>
            <div className="mt-1.5 flex items-center gap-2">
              <Input
                placeholder="partial credit (optional)"
                value={c.partial_credit_notes}
                onChange={(e) => setCriterion(i, { partial_credit_notes: e.target.value })}
                className="flex-1 py-1 text-xs"
              />
              <Button
                variant="ghost"
                className="shrink-0 px-2 py-1 text-xs"
                disabled={criteria.length === 1}
                onClick={() => setCriteria(criteria.filter((_, j) => j !== i))}
              >
                Remove
              </Button>
            </div>
          </div>
        ))}
        <div className="flex items-center justify-between pt-1">
          <Button
            variant="secondary"
            className="px-2.5 py-1 text-xs"
            onClick={() => setCriteria([...criteria, { ...EMPTY_CRITERION }])}
          >
            Add criterion
          </Button>
          <Badge tone={pristine ? "neutral" : sum === null ? "red" : sumOk ? "green" : "amber"}>
            {pristine
              ? `sum — / ${problem.max_points}`
              : sum === null
                ? "invalid points"
                : `sum ${sum} / ${problem.max_points}`}
          </Badge>
        </div>
      </div>

      <div className="grid grid-cols-[8rem_minmax(0,1fr)] gap-3">
        <Field label="Score increment">
          <Input
            className="text-right tabular-nums"
            value={increment}
            onChange={(e) => setIncrement(e.target.value)}
          />
        </Field>
        <Field label="Notes (optional)">
          <Input value={notes} onChange={(e) => setNotes(e.target.value)} />
        </Field>
      </div>

      {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}
      <div className="flex justify-end">
        <Button
          type="submit"
          disabled={save.isPending || !sumOk}
          title={sumOk ? undefined : "criterion points must sum to the problem max"}
        >
          {save.isPending ? "Saving…" : `Save as v${nextVersion}`}
        </Button>
      </div>

      {confirmOpen && (
        <Dialog open onClose={() => setConfirmOpen(false)} title={`Save rubric v${nextVersion}?`}>
          <p className="text-sm text-neutral-700">
            Saving creates version {nextVersion}.{" "}
            <strong className="tabular-nums">{gradedCount}</strong> grade
            {gradedCount === 1 ? "" : "s"} recorded under older versions{" "}
            {gradedCount === 1 ? "stops" : "stop"} counting toward official grades until re-graded
            (or manually re-entered) under v{nextVersion}.
          </p>
          {save.isError && <p className="mt-2 text-xs text-red-600">{save.error.message}</p>}
          <div className="mt-4 flex justify-end gap-2">
            <Button variant="secondary" onClick={() => setConfirmOpen(false)}>
              Cancel
            </Button>
            <Button variant="danger" disabled={save.isPending} onClick={() => save.mutate()}>
              {save.isPending ? "Saving…" : `Save v${nextVersion}`}
            </Button>
          </div>
        </Dialog>
      )}
    </form>
  );
}

function RubricVersionView({ versionId, onBack }: { versionId: number; onBack: () => void }) {
  const version = useQuery({
    queryKey: ["rubric-version", versionId],
    queryFn: () => api.get<RubricVersion>(`/api/rubric-versions/${versionId}`),
  });

  if (version.isPending) return <Spinner />;
  if (version.isError) return <p className="text-sm text-red-600">{version.error.message}</p>;

  const v = version.data;
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-sm text-neutral-600">
          Rubric <span className="font-medium text-neutral-900">v{v.version}</span> (read-only) ·
          created {fmtDate(v.created_at)}
        </p>
        <Button variant="secondary" className="px-2.5 py-1 text-xs" onClick={onBack}>
          Back to editor
        </Button>
      </div>
      <table className="w-full border-collapse text-left text-sm">
        <thead>
          <tr className="border-b border-neutral-200 text-xs text-neutral-500">
            <th className="py-1 pr-2 font-medium">Description</th>
            <th className="w-20 py-1 pr-2 text-right font-medium">Points</th>
            <th className="py-1 font-medium">Partial credit</th>
          </tr>
        </thead>
        <tbody>
          {(v.criteria ?? []).map((c) => (
            <tr key={c.id} className="border-b border-neutral-100">
              <td className="py-1.5 pr-2 text-neutral-800">{c.description}</td>
              <td className="py-1.5 pr-2 text-right tabular-nums">{c.points}</td>
              <td className="py-1.5 text-neutral-500">{c.partial_credit_notes || "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="text-xs text-neutral-500">
        Score increment: <span className="tabular-nums">{v.score_increment}</span>
        {v.notes && <> · Notes: {v.notes}</>}
      </p>
    </div>
  );
}

// --- reference solutions ----------------------------------------------------------------

function SolutionsSection({ problemId }: { problemId: number }) {
  const solutions = useQuery({
    queryKey: ["solutions", problemId],
    queryFn: () => api.get<SolutionsResponse>(`/api/problems/${problemId}/solutions`),
  });
  const [viewId, setViewId] = useState<number | null>(null);

  if (solutions.isPending) return <Spinner />;
  if (solutions.isError) return <p className="text-sm text-red-600">{solutions.error.message}</p>;

  const { current, versions } = solutions.data;

  return (
    <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_10rem]">
      <div className="min-w-0">
        {viewId !== null ? (
          <SolutionVersionView versionId={viewId} onBack={() => setViewId(null)} />
        ) : (
          <SolutionEditor key={current?.id ?? "new"} problemId={problemId} current={current} />
        )}
      </div>
      <VersionHistory versions={versions} selectedId={viewId} onSelect={setViewId} />
    </div>
  );
}

function SolutionEditor({
  problemId,
  current,
}: {
  problemId: number;
  current: SolutionVersion | null;
}) {
  const queryClient = useQueryClient();
  const [content, setContent] = useState(current?.content ?? "");
  const nextVersion = (current?.version ?? 0) + 1;

  const save = useMutation({
    mutationFn: () => api.post<SolutionVersion>(`/api/problems/${problemId}/solutions`, { content }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["solutions", problemId] }),
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (content.trim()) save.mutate();
  };

  return (
    <form className="space-y-3" onSubmit={submit}>
      <Textarea
        rows={8}
        placeholder="Reference solution for this problem…"
        value={content}
        onChange={(e) => setContent(e.target.value)}
        className="font-mono text-xs"
      />
      {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}
      <div className="flex items-center justify-end gap-3">
        <span className="text-xs text-neutral-400">Saving creates version {nextVersion}.</span>
        <Button type="submit" disabled={save.isPending || !content.trim()}>
          {save.isPending ? "Saving…" : `Save as v${nextVersion}`}
        </Button>
      </div>
    </form>
  );
}

function SolutionVersionView({ versionId, onBack }: { versionId: number; onBack: () => void }) {
  const version = useQuery({
    queryKey: ["solution-version", versionId],
    queryFn: () => api.get<SolutionVersion>(`/api/solution-versions/${versionId}`),
  });

  if (version.isPending) return <Spinner />;
  if (version.isError) return <p className="text-sm text-red-600">{version.error.message}</p>;

  const v = version.data;
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-sm text-neutral-600">
          Solution <span className="font-medium text-neutral-900">v{v.version}</span> (read-only) ·
          created {fmtDate(v.created_at)}
        </p>
        <Button variant="secondary" className="px-2.5 py-1 text-xs" onClick={onBack}>
          Back to editor
        </Button>
      </div>
      <pre className="rounded-md border border-neutral-200 bg-neutral-50 p-3 font-mono text-xs whitespace-pre-wrap text-neutral-800">
        {v.content ?? ""}
      </pre>
    </div>
  );
}
