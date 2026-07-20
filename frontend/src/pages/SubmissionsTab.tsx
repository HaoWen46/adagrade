// Submissions tab: multi-PDF upload (filename = <student_id>.pdf) with per-file
// results, the roster reconciliation report (rows expand into a per-student
// submission viewer — browsing/manual grading is never gated on grading state),
// and quarantine assignment (D13).

import { useRef, useState, type FormEvent } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, apiUpload, ApiError } from "../lib/api";
import type {
  DirectUploadResult,
  DirectUploadRow,
  IngestFileResult,
  IngestReport,
  QuarantineEntry,
  Student,
  StudentSubmissionView,
} from "../lib/types";
import {
  Badge,
  Button,
  Card,
  IconButton,
  Input,
  Spinner,
  TD,
  TH,
  Table,
  type BadgeTone,
} from "../components/ui";
import { RotateCcw, X } from "../components/icons";
import { SafeImage } from "../components/SafeImage";
import { HelpTip } from "../components/HelpTip";
import { forceReplaceGradedHelp, mappedExpectedHelp } from "../lib/helpContent";
import { quarantineReasonLabel } from "../lib/labels";

const ROW_STATUS_TONES: Record<DirectUploadRow["status"], BadgeTone> = {
  pending: "neutral",
  ingested: "green",
  quarantined: "amber",
  rejected: "red",
  error: "red",
};

const SYNC_REJECTED_TONE: BadgeTone = "red";

export function SubmissionsTab({ assessmentId }: { assessmentId: string }) {
  return (
    <div className="space-y-4">
      <UploadCard assessmentId={assessmentId} />
      <ReconciliationReport assessmentId={assessmentId} />
    </div>
  );
}

// --- upload -------------------------------------------------------------------------

function UploadCard({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();
  const fileRef = useRef<HTMLInputElement>(null);
  const [files, setFiles] = useState<File[]>([]);
  const [force, setForce] = useState(false);
  // Sync-rejected rows from the upload response itself (no upload_id, never appear
  // in the polled /uploads list) — shown inline immediately, cleared on next submit.
  const [syncRejected, setSyncRejected] = useState<DirectUploadResult[]>([]);

  const uploads = useDirectUploads(assessmentId);

  const upload = useMutation({
    mutationFn: (fs: File[]) => {
      const form = new FormData();
      for (const f of fs) form.append("files", f);
      if (force) form.append("force", "1");
      return apiUpload<{ results: DirectUploadResult[] }>(
        `/api/assessments/${assessmentId}/submissions`,
        form,
      );
    },
    onSuccess: async (data) => {
      setSyncRejected(data.results.filter((r) => r.status === "rejected"));
      await queryClient.invalidateQueries({ queryKey: ["direct-uploads", assessmentId] });
      setFiles([]);
      if (fileRef.current) fileRef.current.value = "";
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (files.length > 0) {
      setSyncRejected([]);
      upload.mutate(files);
    }
  };

  return (
    <Card title="Upload submissions (PDF, one per student)">
      <p className="mb-3 text-xs text-neutral-400">
        Have one big scanner PDF instead of per-student files? Use the{" "}
        <a href="?tab=identify" className="font-medium underline">
          Identify tab
        </a>
        .
      </p>
      <form className="flex flex-wrap items-center gap-3" onSubmit={submit}>
        <input
          ref={fileRef}
          type="file"
          multiple
          accept=".pdf,application/pdf"
          onChange={(e) => setFiles(Array.from(e.target.files ?? []))}
          className="flex-1 text-sm text-neutral-600 file:mr-3 file:rounded-md file:border file:border-solid file:border-neutral-300 file:bg-white file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-neutral-800 hover:file:bg-neutral-50"
        />
        <label className="flex items-center gap-1.5 text-sm text-neutral-600">
          <input
            type="checkbox"
            checked={force}
            onChange={(e) => setForce(e.target.checked)}
            className="size-3.5 accent-indigo-600"
          />
          force replace graded{" "}
          <HelpTip title="Force replace graded">{forceReplaceGradedHelp}</HelpTip>
        </label>
        <Button type="submit" disabled={files.length === 0 || upload.isPending}>
          {upload.isPending
            ? "Uploading…"
            : `Upload${files.length > 0 ? ` ${files.length} file${files.length === 1 ? "" : "s"}` : ""}`}
        </Button>
      </form>
      <p className="mt-2 text-xs text-neutral-400">
        Filenames must be <span className="font-mono">&lt;student_id&gt;.pdf</span>; anything
        unmatched lands in quarantine below. Pages map to problems in order; a PDF with more pages
        than configured problems is rejected so no page is discarded. Uploads are staged
        immediately and processed in the background — statuses below update automatically.
      </p>

      {upload.isError && <p className="mt-3 text-sm text-red-600">{upload.error.message}</p>}
      <UploadResults rejected={syncRejected} rows={uploads.data?.uploads ?? []} pending={uploads.isPending} />
    </Card>
  );
}

/**
 * Polls GET .../uploads every 2s only while some row is still "pending" — once
 * every row has reached a terminal status (ingested/quarantined/rejected/error)
 * polling stops, matching BatchListCard's processing_files gate (polling forever
 * was a past Critical finding).
 */
function useDirectUploads(assessmentId: string) {
  const queryClient = useQueryClient();
  const wasPendingRef = useRef(false);
  return useQuery({
    queryKey: ["direct-uploads", assessmentId],
    queryFn: () => api.get<{ uploads: DirectUploadRow[] }>(`/api/assessments/${assessmentId}/uploads`),
    refetchInterval: (query) => {
      const rows = query.state.data?.uploads ?? [];
      const stillPending = rows.some((u) => u.status === "pending");
      if (wasPendingRef.current && !stillPending) {
        // Transitioned from having pending rows to none: the reconciliation report
        // and problem summaries may now reflect newly-ingested submissions.
        void queryClient.invalidateQueries({ queryKey: ["ingest-report", assessmentId] });
        void queryClient.invalidateQueries({ queryKey: ["problem-summaries", assessmentId] });
      }
      wasPendingRef.current = stillPending;
      return stillPending ? 2000 : false;
    },
  });
}

function UploadResults({
  rejected,
  rows,
  pending,
}: {
  rejected: DirectUploadResult[];
  rows: DirectUploadRow[];
  pending: boolean;
}) {
  if (rejected.length === 0 && rows.length === 0) {
    return pending ? (
      <div className="mt-3 flex justify-center py-4">
        <Spinner className="size-5" />
      </div>
    ) : null;
  }
  return (
    <div className="mt-3">
      <Table>
        <thead>
          <tr>
            <TH>File</TH>
            <TH className="w-28">Status</TH>
            <TH>Reason</TH>
          </tr>
        </thead>
        <tbody>
          {rejected.map((r, i) => (
            <tr key={`sync-${i}`}>
              <TD className="font-mono text-xs">{r.filename}</TD>
              <TD>
                <Badge tone={SYNC_REJECTED_TONE}>rejected</Badge>
              </TD>
              <TD className="text-neutral-500">
                {r.reason ? quarantineReasonLabel(r.reason) : "—"}
              </TD>
            </tr>
          ))}
          {rows.map((r) => (
            <tr key={r.id}>
              <TD className="font-mono text-xs">{r.filename}</TD>
              <TD>
                <span className="inline-flex items-center gap-1.5">
                  <Badge tone={ROW_STATUS_TONES[r.status]}>{r.status}</Badge>
                  {r.status === "pending" && <Spinner className="size-3" />}
                </span>
              </TD>
              <TD className="text-neutral-500">
                {r.reason ? quarantineReasonLabel(r.reason) : "—"}
              </TD>
            </tr>
          ))}
        </tbody>
      </Table>
    </div>
  );
}

// --- reconciliation report ------------------------------------------------------------

function ReconciliationReport({ assessmentId }: { assessmentId: string }) {
  const [expanded, setExpanded] = useState<string | null>(null);
  const report = useQuery({
    queryKey: ["ingest-report", assessmentId],
    queryFn: () => api.get<IngestReport>(`/api/assessments/${assessmentId}/ingest/report`),
  });

  if (report.isPending) {
    return (
      <div className="flex justify-center py-10">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (report.isError) {
    return (
      <Card>
        <p className="text-sm text-red-600">{report.error.message}</p>
      </Card>
    );
  }

  const { students, quarantine } = report.data;
  // Same "has work" rule as Overview's step 2 (audit B2): a submission OR a mapped
  // scan page counts. Kept in sync so this header and Overview never disagree about
  // the same assessment.
  const submitted = students.filter((s) => s.submission_id !== undefined).length;
  const viaScansOnly = students.filter(
    (s) => s.submission_id === undefined && s.mapped_pages > 0,
  ).length;
  const haveWork = submitted + viaScansOnly;

  return (
    <div className="space-y-4">
      <div className="space-y-2">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-semibold text-neutral-900">Reconciliation</h3>
          <span className="text-xs text-neutral-500 tabular-nums">
            {haveWork}/{students.length} have work
            {viaScansOnly > 0 ? ` · ${viaScansOnly} via scans` : ""} · click a row to view pages
          </span>
        </div>
        <Table>
          <thead>
            <tr>
              <TH className="w-32">Student ID</TH>
              <TH>Name</TH>
              <TH>Submission</TH>
              <TH className="w-24 text-right">PDF pages</TH>
              <TH className="w-36 text-right">
                <span className="inline-flex items-center gap-1.5">
                  Mapped/expected{" "}
                  <HelpTip title="Mapped vs. expected">{mappedExpectedHelp}</HelpTip>
                </span>
              </TH>
            </tr>
          </thead>
          <tbody>
            {students.length === 0 && (
              <tr>
                <TD colSpan={5} className="text-center text-neutral-400">
                  No students on the roster yet.
                </TD>
              </tr>
            )}
            {students.map((s) => {
              const mismatch = s.submission_id !== undefined && s.mapped_pages !== s.expected_pages;
              const isOpen = expanded === s.student_id;
              return (
                <StudentRows
                  key={s.student_id}
                  assessmentId={assessmentId}
                  row={s}
                  mismatch={mismatch}
                  open={isOpen}
                  onToggle={() => setExpanded(isOpen ? null : s.student_id)}
                />
              );
            })}
          </tbody>
        </Table>
      </div>

      <div className="space-y-2">
        <h3 className="text-sm font-semibold text-neutral-900">Quarantine</h3>
        {quarantine.length === 0 ? (
          <Card>
            <p className="text-sm text-neutral-400">Nothing in quarantine.</p>
          </Card>
        ) : (
          <Table>
            <thead>
              <tr>
                <TH>File</TH>
                <TH>Reason</TH>
                <TH className="w-72">Assign to student</TH>
              </tr>
            </thead>
            <tbody>
              {quarantine.map((q) => (
                <QuarantineRow key={q.id} entry={q} assessmentId={assessmentId} />
              ))}
            </tbody>
          </Table>
        )}
      </div>
    </div>
  );
}

// --- per-student submission viewer -----------------------------------------------------

function StudentRows({
  assessmentId,
  row,
  mismatch,
  open,
  onToggle,
}: {
  assessmentId: string;
  row: IngestReport["students"][number];
  mismatch: boolean;
  open: boolean;
  onToggle: () => void;
}) {
  return (
    <>
      <tr className="cursor-pointer hover:bg-neutral-50" onClick={onToggle}>
        <TD className="font-medium tabular-nums">
          <span className="mr-1.5 inline-block w-3 text-neutral-400">{open ? "▾" : "▸"}</span>
          {row.student_id}
        </TD>
        <TD>{row.name}</TD>
        <TD>
          {row.submission_id !== undefined ? (
            <div className="space-y-1.5">
              <span className="flex items-center gap-2">
                <Badge tone="green">✓</Badge>
                <span className="font-mono text-xs text-neutral-500">{row.filename}</span>
              </span>
              {/* Retract lives here (HCI audit): this is the real control the
                  Identify-tab parked/promoted surfaces tell TAs to use. Its own
                  clicks stopPropagation so operating it never toggles the row. */}
              <div onClick={(e) => e.stopPropagation()}>
                <RetractControl
                  assessmentId={assessmentId}
                  submissionId={row.submission_id}
                  studentId={row.student_id}
                  filename={row.filename}
                />
              </div>
            </div>
          ) : row.mapped_pages > 0 ? (
            // No per-student PDF, but scan pages were assigned + promoted for this
            // student on the Identify tab (audit B2) — neutral, not "missing": the
            // work exists, it just arrived through the other intake path.
            <div className="space-y-1.5">
              <Badge tone="neutral">via scans</Badge>
              <p className="text-xs text-neutral-500">
                {row.mapped_pages} page{row.mapped_pages === 1 ? "" : "s"} mapped from Identify
              </p>
            </div>
          ) : (
            <Badge tone="red">missing</Badge>
          )}
        </TD>
        <TD className="text-right tabular-nums">{row.page_count ?? "—"}</TD>
        <TD
          className={
            mismatch ? "text-right font-medium tabular-nums text-red-600" : "text-right tabular-nums"
          }
        >
          {row.mapped_pages}/{row.expected_pages}
        </TD>
      </tr>
      {open && (
        <tr>
          <TD colSpan={5} className="bg-neutral-50/70 p-0">
            <StudentSubmissionPanel assessmentId={assessmentId} studentId={row.student_id} />
          </TD>
        </tr>
      )}
    </>
  );
}

// StudentSubmissionPanel shows one student's uploaded pages for this assessment —
// viewable and manually gradable regardless of grading state.
function StudentSubmissionPanel({
  assessmentId,
  studentId,
}: {
  assessmentId: string;
  studentId: string;
}) {
  const view = useQuery({
    queryKey: ["student-submission", assessmentId, studentId],
    queryFn: () =>
      api.get<StudentSubmissionView>(
        `/api/assessments/${assessmentId}/students/${encodeURIComponent(studentId)}/submission`,
      ),
  });

  if (view.isPending) {
    return (
      <div className="flex justify-center py-6">
        <Spinner className="size-5" />
      </div>
    );
  }
  if (view.isError) {
    return <p className="px-4 py-3 text-sm text-red-600">{view.error.message}</p>;
  }

  const { submission, answers } = view.data;
  return (
    <div className="space-y-3 px-4 py-3">
      <div className="flex items-center gap-3 text-sm">
        {submission ? (
          <>
            <a
              href={`/api/submissions/${submission.id}/pdf`}
              target="_blank"
              rel="noreferrer"
              className="font-medium text-indigo-600 hover:underline"
            >
              Open source PDF ↗
            </a>
            <span className="text-xs text-neutral-400">
              {submission.filename} · {submission.page_count} page
              {submission.page_count === 1 ? "" : "s"}
            </span>
          </>
        ) : (
          <span className="text-neutral-400">
            No submission uploaded — answers can still be graded manually (e.g. absent = 0).
          </span>
        )}
      </div>

      {answers.length === 0 && (
        <p className="text-sm text-neutral-400">
          No answers exist yet — they are created when submissions are first ingested.
        </p>
      )}
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {answers.map((a) => (
          <div key={a.answer_id} className="rounded-md border border-neutral-200 bg-white p-2.5">
            <div className="mb-2 flex items-center justify-between gap-2">
              <span className="text-sm font-medium text-neutral-800">
                Problem {a.problem_number}
                {a.problem_title ? ` — ${a.problem_title}` : ""}
              </span>
              <span className="flex items-center gap-1.5">
                {a.has_official && <Badge tone="green">official</Badge>}
                {a.record_count > 0 && <Badge tone="neutral">{a.record_count} rec</Badge>}
              </span>
            </div>
            {a.pages.length === 0 ? (
              <p className="mb-2 text-xs text-neutral-400">No pages mapped.</p>
            ) : (
              <div className="mb-2 flex flex-wrap gap-1.5">
                {a.pages.map((p) => (
                  <Link key={p.page_id} to={`/answers/${a.answer_id}`}>
                    <SafeImage
                      src={`/api/answer-pages/${p.page_id}/image`}
                      alt={`page ${p.page_index + 1}`}
                      className="h-24 w-20 rounded border border-neutral-200 object-contain hover:border-indigo-400"
                    />
                  </Link>
                ))}
              </div>
            )}
            <Link
              to={`/answers/${a.answer_id}`}
              className="text-xs font-medium text-indigo-600 hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              View & grade →
            </Link>
          </div>
        ))}
      </div>
    </div>
  );
}

// isNeedsForce recognizes the one rejection that force can override: the graded-
// without-force guard (internal/ingest/ingest.go: "student already has grading
// records; re-upload requires force"). Unlike retract's structured 409
// needs_force flag, the assign endpoint answers HTTP 200 with a FileResult, so
// the reason text is the only signal. Published answers are never forceable.
function isNeedsForce(res: IngestFileResult): boolean {
  return res.status === "rejected" && (res.reason ?? "").includes("requires force");
}

// QuarantineRow (HCI audit fixes): roster-mismatch entries are assigned through
// selection-only search — free text never submits (a one-digit typo onto a valid
// neighboring roster ID used to silently file the PDF under the wrong student).
// Unreadable entries cannot be repaired by assignment, so they instead explain
// the replacement flow and offer an audited dismiss action. The assign endpoint
// answers HTTP 200 for non-ingested outcomes too, so the FileResult body is
// inspected and the graded guard reveals a Retract-style force confirm.
function QuarantineRow({ entry, assessmentId }: { entry: QuarantineEntry; assessmentId: string }) {
  const queryClient = useQueryClient();
  const assignable = entry.reason === "unknown_student" || entry.reason === "duplicate_in_batch";
  const [rosterQuery, setRosterQuery] = useState("");
  const [selected, setSelected] = useState<Student | null>(null);
  const [searchFocused, setSearchFocused] = useState(false);
  // Last non-ingested FileResult, rendered inline under the form.
  const [result, setResult] = useState<IngestFileResult | null>(null);

  const roster = useQuery({
    queryKey: ["students"],
    queryFn: () => api.get<{ students: Student[] }>("/api/students"),
    enabled: assignable,
  });

  const assign = useMutation({
    mutationFn: (vars: { studentId: string; force: boolean }) =>
      api.post<IngestFileResult>(`/api/quarantine/${entry.id}/assign`, {
        student_id: vars.studentId,
        force: vars.force,
      }),
    onSuccess: async (res) => {
      if (res.status === "ingested") {
        setResult(null);
        await queryClient.invalidateQueries({ queryKey: ["ingest-report", assessmentId] });
        await queryClient.invalidateQueries({ queryKey: ["problem-summaries", assessmentId] });
      } else {
        // rejected | quarantined: the entry is still open — surface the reason.
        setResult(res);
        if (res.status === "quarantined") {
          await Promise.all([
            queryClient.invalidateQueries({ queryKey: ["ingest-report", assessmentId] }),
            queryClient.invalidateQueries({ queryKey: ["workflow-warnings", assessmentId] }),
          ]);
        }
      }
    },
  });

  const dismiss = useMutation({
    mutationFn: () => api.post<{ dismissed: boolean }>(`/api/quarantine/${entry.id}/dismiss`, {}),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["ingest-report", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["workflow-warnings", assessmentId] }),
      ]);
    },
  });

  const select = (s: Student) => {
    setSelected(s);
    setRosterQuery("");
    setResult(null);
    assign.reset();
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    // Selection-only: without a roster click there is nothing to submit.
    if (!selected || assign.isPending) return;
    setResult(null);
    assign.mutate({ studentId: selected.student_id, force: false });
  };

  const needsForce = selected !== null && result !== null && isNeedsForce(result);

  const q = rosterQuery.trim().toLowerCase();
  const matches = (roster.data?.students ?? []).filter(
    (s) =>
      !s.withdrawn &&
      (q === "" || s.student_id.toLowerCase().includes(q) || s.name.toLowerCase().includes(q)),
  );
  const listOpen = !selected && (searchFocused || q !== "");

  if (!assignable) {
    return (
      <tr>
        <TD className="font-mono text-xs">{entry.filename}</TD>
        <TD className="text-neutral-500">{quarantineReasonLabel(entry.reason)}</TD>
        <TD>
          <div className="space-y-1.5">
            <p className="text-xs text-neutral-500">
              Upload a readable replacement separately, then dismiss this entry.
            </p>
            <IconButton
              variant="danger"
              label={`Dismiss ${entry.filename} — its audit record is kept`}
              disabled={dismiss.isPending}
              onClick={() => {
                if (confirm("Dismiss this unreadable file from quarantine? Its audit record is kept.")) {
                  dismiss.mutate();
                }
              }}
            >
              {dismiss.isPending ? <Spinner className="size-3.5" /> : <X />}
            </IconButton>
            {dismiss.isError && <p className="text-xs text-red-600">{dismiss.error.message}</p>}
          </div>
        </TD>
      </tr>
    );
  }

  return (
    <tr>
      <TD className="font-mono text-xs">{entry.filename}</TD>
      <TD className="text-neutral-500">{quarantineReasonLabel(entry.reason)}</TD>
      <TD>
        {needsForce ? (
          // Mirror of RetractControl's force escalation: same copy shape, same
          // two-button confirm. The search form is hidden while this is open so
          // the re-POST can only target the student the guard fired for.
          <div className="space-y-1.5 rounded-md border border-neutral-200 bg-white p-2">
            <p className="text-xs text-amber-800">
              <span className="font-mono">{selected.student_id}</span> — {selected.name} already
              has grades recorded. Replace their submission anyway? The grades are kept but
              flagged as based on a replaced image, so you know to re-check them.
            </p>
            <div className="flex items-center gap-2">
              <Button
                variant="danger"
                className="px-2.5 py-1 text-xs"
                disabled={assign.isPending}
                onClick={() => assign.mutate({ studentId: selected.student_id, force: true })}
              >
                {assign.isPending ? "Assigning…" : "Assign anyway"}
              </Button>
              <Button
                variant="secondary"
                className="px-2.5 py-1 text-xs"
                disabled={assign.isPending}
                onClick={() => {
                  setResult(null);
                  assign.reset();
                }}
              >
                Cancel
              </Button>
            </div>
          </div>
        ) : (
          <div className="space-y-1">
            <form className="flex flex-wrap items-center gap-2" onSubmit={submit}>
              <Input
                placeholder="search roster (id or name)"
                value={rosterQuery}
                onChange={(e) => {
                  setRosterQuery(e.target.value);
                  setSelected(null); // typing filters; only a click selects
                }}
                onFocus={() => setSearchFocused(true)}
                onBlur={() => setSearchFocused(false)}
                className="w-44 py-1 text-xs"
              />
              {selected && (
                <span className="text-xs">
                  <span className="font-mono font-medium tabular-nums">{selected.student_id}</span>
                  <span className="text-neutral-500"> — {selected.name}</span>
                </span>
              )}
              <Button
                type="submit"
                variant="secondary"
                className="px-2.5 py-1 text-xs"
                disabled={!selected || assign.isPending}
              >
                {assign.isPending ? "Assigning…" : "Assign"}
              </Button>
            </form>
            {listOpen &&
              (roster.isPending ? (
                <Spinner className="size-4" />
              ) : roster.isError ? (
                <p className="text-xs text-red-600">{roster.error.message}</p>
              ) : (
                <div className="max-h-32 w-64 space-y-0.5 overflow-y-auto rounded-md border border-neutral-200 bg-white p-1">
                  {matches.slice(0, 8).map((s) => (
                    <button
                      key={s.id}
                      type="button"
                      // Keep focus in the input so blur doesn't unmount the
                      // list before this button's click lands.
                      onMouseDown={(e) => e.preventDefault()}
                      onClick={() => select(s)}
                      className="flex w-full items-center justify-between gap-2 rounded px-2 py-1 text-left text-xs hover:bg-neutral-50"
                    >
                      <span className="font-mono tabular-nums">{s.student_id}</span>
                      <span className="truncate text-neutral-500">{s.name}</span>
                    </button>
                  ))}
                  {matches.length === 0 && (
                    <p className="px-2 py-1 text-xs text-neutral-400">No roster matches.</p>
                  )}
                </div>
              ))}
            {result && (
              <p className="text-xs text-red-600">
                {result.status === "quarantined" ? "Still quarantined: " : "Rejected: "}
                {result.reason ? quarantineReasonLabel(result.reason) : "no reason given"}
              </p>
            )}
            {assign.isError && <p className="text-xs text-red-600">{assign.error.message}</p>}
          </div>
        )}
      </TD>
    </tr>
  );
}

// --- retract (HCI audit) ---------------------------------------------------------------
// The real POST /api/submissions/{id}/retract control the ParkedCard / OrphanQueue /
// MatrixCard surfaces point at. Two-step like ParkedCard's force pattern: a plain
// confirm first (force:false); a graded-without-force 409 (needs_force:true) reveals the
// "retract anyway" escalation and re-POSTs force:true; a published block (409 without
// needs_force) or any other error just shows its message with no force path. Retract
// unassigns the live submission and deletes its pages, so on success the reconciliation
// report and the scan matrix/pages both change — invalidate all of them.
function RetractControl({
  assessmentId,
  submissionId,
  studentId,
  filename,
}: {
  assessmentId: string;
  submissionId: number;
  studentId: string;
  filename?: string;
}) {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [needsForce, setNeedsForce] = useState(false);

  const retract = useMutation({
    mutationFn: (force: boolean) =>
      api.post<{ retracted: true }>(`/api/submissions/${submissionId}/retract`, { force }),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["ingest-report", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["student-submission", assessmentId, studentId] }),
        queryClient.invalidateQueries({ queryKey: ["problem-summaries", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["scan-pages", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["scan-matrix", assessmentId] }),
      ]);
      setOpen(false);
      setNeedsForce(false);
    },
    onError: (err) => {
      // Only the graded-without-force guard offers a force path; the published
      // block (409, needs_force absent) and every other failure show their
      // message alone — force cannot override a published answer.
      setNeedsForce(
        err instanceof ApiError &&
          err.status === 409 &&
          (err.details as { needs_force?: boolean } | undefined)?.needs_force === true,
      );
    },
  });

  const close = () => {
    setOpen(false);
    setNeedsForce(false);
    retract.reset();
  };

  if (!open) {
    return (
      <IconButton
        variant="danger"
        label={`Retract ${filename ?? "this submission"} — deletes its pages (any existing grades are kept, flagged for re-check)`}
        onClick={() => {
          retract.reset();
          setNeedsForce(false);
          setOpen(true);
        }}
      >
        <RotateCcw />
      </IconButton>
    );
  }

  return (
    <div className="space-y-1.5 rounded-md border border-neutral-200 bg-white p-2">
      {needsForce ? (
        <p className="text-xs text-amber-800">
          This student already has grades recorded. Retract anyway? The grades are kept but flagged
          as based on a replaced image, so you know to re-check them.
        </p>
      ) : (
        <p className="text-xs text-neutral-600">
          Retracting unassigns this submission and deletes its pages, freeing the student × problem
          cell for a scan page or a re-upload.
        </p>
      )}

      {retract.isError && !needsForce && (
        <p className="text-xs text-red-600">{retract.error.message}</p>
      )}

      <div className="flex items-center gap-2">
        <Button
          variant="danger"
          className="px-2.5 py-1 text-xs"
          disabled={retract.isPending}
          onClick={() => retract.mutate(needsForce)}
        >
          {retract.isPending
            ? "Retracting…"
            : needsForce
              ? "Retract anyway"
              : "Confirm retract"}
        </Button>
        <Button
          variant="secondary"
          className="px-2.5 py-1 text-xs"
          disabled={retract.isPending}
          onClick={close}
        >
          Cancel
        </Button>
      </div>
    </div>
  );
}
