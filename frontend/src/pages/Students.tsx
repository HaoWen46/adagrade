// Students: roster list + CSV import (lecturer/admin). A parse error anywhere
// rejects the whole import (D13) — the per-line errors are listed verbatim
// (including the non-UTF-8 Excel hint and duplicate id/email guards). A
// successful import reports the roster diff (roster-lifecycle plan 2026-07-10):
// actives missing from the CSV and withdrawn students present in it, with
// explicit bulk withdraw/reinstate proposals — sync is never automatic.

import { useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api, apiUpload } from "../lib/api";
import { roleAtLeast, useMe } from "../lib/auth";
import type {
  BulkStudentStatusResult,
  ImportReport,
  Student,
  StudentDeleteBlockedError,
  StudentDeleteResult,
} from "../lib/types";
import {
  Badge,
  Button,
  Card,
  Dialog,
  Field,
  IconButton,
  Input,
  Pager,
  Spinner,
  TD,
  TH,
  Table,
  cx,
} from "../components/ui";
import { Trash, UserCheck, UserMinus } from "../components/icons";
import { HelpTip } from "../components/HelpTip";
import { rosterCsvHelp, withdrawHelp } from "../lib/helpContent";
import { studentBlockingKindLabel } from "../lib/labels";

const PAGE_SIZE = 50;

interface LineError {
  line: number;
  msg: string;
}

/** Pulls the {line, msg} list out of the 400 roster-rejected body, if present. */
function lineErrors(err: unknown): LineError[] {
  if (!(err instanceof ApiError) || err.details === null || typeof err.details !== "object") {
    return [];
  }
  const errors = (err.details as { errors?: unknown }).errors;
  if (!Array.isArray(errors)) return [];
  return errors.filter(
    (e): e is LineError =>
      e !== null &&
      typeof e === "object" &&
      typeof (e as { line?: unknown }).line === "number" &&
      typeof (e as { msg?: unknown }).msg === "string",
  );
}

export function Students() {
  const me = useMe();
  const queryClient = useQueryClient();
  const canImport = roleAtLeast(me.data?.user.role, "lecturer");
  // B15: hard delete is admin-only (server-enforced, requireRole(RoleAdmin) — this only
  // hides the control) — same pattern as PublishTab's canUnpublish / Users page gate.
  const canDelete = roleAtLeast(me.data?.user.role, "admin");

  const students = useQuery({
    queryKey: ["students"],
    queryFn: () => api.get<{ students: Student[] }>("/api/students"),
  });

  // 250+ rosters: the full list still arrives in one response (ordered by
  // student_id server-side); search + paging are purely client-side.
  const [search, setSearch] = useState("");
  const [page, setPage] = useState(0);

  const all = students.data?.students ?? [];
  const needle = search.trim().toLowerCase();
  const filtered = needle === "" ? all : all.filter((s) => s.student_id.toLowerCase().startsWith(needle));
  const pageCount = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  // Clamp instead of setState-in-render: a shrinking result set can leave
  // `page` past the end, so render the last valid page.
  const safePage = Math.min(page, pageCount - 1);
  const rows = filtered.slice(safePage * PAGE_SIZE, (safePage + 1) * PAGE_SIZE);

  // D23: lecturer+ can withdraw/reinstate a student; excluded from future
  // materialize/ingest-report/matching once withdrawn, existing history kept.
  // Withdraw goes through a confirm dialog; reinstate (the undo) stays one click.
  const [confirmWithdraw, setConfirmWithdraw] = useState<Student | null>(null);
  // B15: erroneous/test roster entries (e.g. old smoke-test students) with no real
  // artifacts can be hard-deleted, admin-only — a typed-confirm dialog owns its own
  // mutation (DeleteStudentDialog below) so it resets cleanly between students.
  const [confirmDelete, setConfirmDelete] = useState<Student | null>(null);
  const setWithdrawn = useMutation({
    mutationFn: ({ id, withdrawn }: { id: number; withdrawn: boolean }) =>
      api.patch<Student>(`/api/students/${id}`, { withdrawn }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["students"] });
      setConfirmWithdraw(null);
    },
  });

  return (
    <div className="mx-auto max-w-4xl space-y-4">
      <h1 className="text-lg font-semibold text-neutral-900">Students</h1>

      {canImport && <ImportCard />}

      {setWithdrawn.isError && <p className="text-xs text-red-600">{setWithdrawn.error.message}</p>}

      {/* Kept outside the pending/error/table ternary so it stays mounted (and
          keeps focus) during refetches. Search resets paging back to the first
          page — same convention as the Users.tsx audit filters. */}
      <Input
        className="w-56"
        placeholder="Search by student ID…"
        value={search}
        onChange={(e) => {
          setSearch(e.target.value);
          setPage(0);
        }}
      />

      {students.isPending ? (
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      ) : students.isError ? (
        <Card>
          <p className="text-sm text-red-600">{students.error.message}</p>
        </Card>
      ) : (
        <>
          <Table>
            <thead>
              <tr>
                <TH>Student ID</TH>
                <TH>Name</TH>
                <TH>Email</TH>
                <TH className="w-32">
                  <span className="inline-flex items-center gap-1.5">
                    Status <HelpTip title="Active vs. withdrawn">{withdrawHelp}</HelpTip>
                  </span>
                </TH>
                {canImport && <TH className={canDelete ? "w-40" : "w-24"} />}
              </tr>
            </thead>
            <tbody>
              {all.length === 0 ? (
                <tr>
                  <TD colSpan={canImport ? 5 : 4} className="text-center text-neutral-400">
                    No students yet — import a roster CSV.
                  </TD>
                </tr>
              ) : filtered.length === 0 ? (
                <tr>
                  <TD colSpan={canImport ? 5 : 4} className="text-center text-neutral-400">
                    No students match this ID.
                  </TD>
                </tr>
              ) : null}
              {rows.map((s) => (
                <tr key={s.id} className={cx("hover:bg-neutral-50", s.withdrawn && "bg-neutral-50 text-neutral-400")}>
                  <TD className="font-medium tabular-nums">{s.student_id}</TD>
                  <TD>{s.name}</TD>
                  <TD className="text-neutral-500">{s.email}</TD>
                  <TD>
                    {s.withdrawn ? (
                      <Badge tone="amber">Withdrawn</Badge>
                    ) : (
                      <Badge tone="green">Active</Badge>
                    )}
                  </TD>
                  {canImport && (
                    <TD>
                      <div className="flex items-center gap-1">
                        <IconButton
                          label={
                            s.withdrawn
                              ? `Reinstate ${s.student_id}`
                              : `Withdraw ${s.student_id} — keeps history, excluded from new runs/publishes`
                          }
                          disabled={setWithdrawn.isPending}
                          onClick={() =>
                            s.withdrawn
                              ? setWithdrawn.mutate({ id: s.id, withdrawn: false })
                              : setConfirmWithdraw(s)
                          }
                        >
                          {s.withdrawn ? <UserCheck /> : <UserMinus />}
                        </IconButton>
                        {/* B15: admin-only hard delete — hidden for ta/lecturer (the
                            server independently enforces requireRole(RoleAdmin)). */}
                        {canDelete && (
                          <IconButton
                            variant="danger"
                            label={`Delete ${s.student_id} — only possible while no artifacts reference them`}
                            onClick={() => setConfirmDelete(s)}
                          >
                            <Trash />
                          </IconButton>
                        )}
                      </div>
                    </TD>
                  )}
                </tr>
              ))}
            </tbody>
          </Table>

          <div className="flex items-center justify-between">
            <p className="text-xs text-neutral-500 tabular-nums">
              {needle !== ""
                ? `${filtered.length} of ${all.length} students`
                : `${all.length} students`}
            </p>
            <Pager page={safePage} pageCount={pageCount} onPage={setPage} />
          </div>
        </>
      )}

      {confirmWithdraw && (
        <Dialog
          open
          onClose={() => setConfirmWithdraw(null)}
          title={`Withdraw ${confirmWithdraw.student_id}?`}
        >
          <div className="space-y-2 text-sm text-neutral-600">
            <p>
              This marks <span className="font-medium text-neutral-800">{confirmWithdraw.name}</span>{" "}
              ({confirmWithdraw.student_id}) as withdrawn — <strong>nothing is deleted</strong>.
            </p>
            <ul className="list-inside list-disc space-y-1">
              <li>Existing submissions, grades, and history are all kept.</li>
              <li>
                The student is excluded from future grading preparation, ingest reports, and scan
                matching.
              </li>
              <li>You can reinstate them at any time from this same list.</li>
            </ul>
          </div>
          {setWithdrawn.isError && (
            <p className="mt-2 text-xs text-red-600">{setWithdrawn.error.message}</p>
          )}
          <div className="mt-4 flex justify-end gap-2">
            <Button variant="secondary" onClick={() => setConfirmWithdraw(null)}>
              Cancel
            </Button>
            <Button
              variant="danger"
              disabled={setWithdrawn.isPending}
              onClick={() => setWithdrawn.mutate({ id: confirmWithdraw.id, withdrawn: true })}
            >
              {setWithdrawn.isPending ? "Withdrawing…" : "Withdraw"}
            </Button>
          </div>
        </Dialog>
      )}

      {confirmDelete && (
        <DeleteStudentDialog student={confirmDelete} onClose={() => setConfirmDelete(null)} />
      )}
    </div>
  );
}

/**
 * B15 typed-confirm hard-delete dialog (matches PublishTab's typed-name Publish/Unpublish
 * dialogs — the existing destructive idiom). Owns its own mutation + confirm-text state so
 * it starts fresh every time it mounts (one per `s.id`, unmounted on close via the
 * `confirmDelete &&` guard above) rather than needing manual resets between students.
 *
 * DELETE /api/students/{id} only succeeds when nothing but bare, never-submitted answer
 * rows reference the student (task 7 report) — a 409 names every real artifact kind found;
 * this renders that explanation and points at Withdraw as the reversible alternative,
 * exactly as the brief asks.
 */
function DeleteStudentDialog({ student, onClose }: { student: Student; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [confirmText, setConfirmText] = useState("");

  const del = useMutation({
    mutationFn: () => api.del<StudentDeleteResult>(`/api/students/${student.id}`),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["students"] });
      onClose();
    },
  });

  const blocked =
    del.error instanceof ApiError && del.error.status === 409 && del.error.details
      ? (del.error.details as StudentDeleteBlockedError)
      : null;

  const idMatches = confirmText.trim() === student.student_id;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (idMatches) del.mutate();
  };

  return (
    <Dialog open onClose={onClose} title={`Delete ${student.student_id}?`}>
      <form className="space-y-3" onSubmit={submit}>
        <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800">
          This permanently removes{" "}
          <span className="font-medium">{student.name}</span> ({student.student_id}) from the
          roster. Unlike Withdraw, <strong>this cannot be undone</strong> — it only succeeds
          when the student has no real submissions, grades, or other history; use Withdraw
          instead for anyone who actually took the class.
        </div>

        {blocked ? (
          <div className="space-y-1.5 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
            <p>
              Can&apos;t delete — this student has real records:{" "}
              {blocked.blocking.map(studentBlockingKindLabel).join(", ")}.
            </p>
            <p>
              Use <strong>Withdraw</strong> instead — it keeps all history but excludes them
              from future grading, publishing, and matching.
            </p>
          </div>
        ) : (
          del.isError && <p className="text-xs text-red-600">{del.error.message}</p>
        )}

        <Field
          label={
            <>
              Type the student ID (<strong>{student.student_id}</strong>) to confirm
            </>
          }
        >
          <Input
            autoFocus
            value={confirmText}
            onChange={(e) => setConfirmText(e.target.value)}
            placeholder={student.student_id}
          />
        </Field>

        <div className="flex justify-end gap-2 pt-1">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" disabled={!idMatches || del.isPending}>
            {del.isPending ? "Deleting…" : "Delete permanently"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function ImportCard() {
  const queryClient = useQueryClient();
  const fileRef = useRef<HTMLInputElement>(null);
  const [file, setFile] = useState<File | null>(null);

  const importRoster = useMutation({
    mutationFn: (f: File) => {
      const form = new FormData();
      form.append("file", f);
      return apiUpload<ImportReport>("/api/students/import", form);
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["students"] });
      setFile(null);
      if (fileRef.current) fileRef.current.value = "";
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (file) importRoster.mutate(file);
  };

  const errors = lineErrors(importRoster.error);

  return (
    <Card
      title={
        <span className="inline-flex items-center gap-1.5">
          Import roster (CSV) <HelpTip title="Roster CSV format">{rosterCsvHelp}</HelpTip>
        </span>
      }
    >
      <form className="flex items-center gap-3" onSubmit={submit}>
        <input
          ref={fileRef}
          type="file"
          accept=".csv,text/csv"
          onChange={(e) => setFile(e.target.files?.[0] ?? null)}
          className="flex-1 text-sm text-neutral-600 file:mr-3 file:rounded-md file:border file:border-solid file:border-neutral-300 file:bg-white file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-neutral-800 hover:file:bg-neutral-50"
        />
        <Button type="submit" disabled={!file || importRoster.isPending}>
          {importRoster.isPending ? "Importing…" : "Import"}
        </Button>
      </form>

      {importRoster.isSuccess && (
        <>
          <p className="mt-3 text-sm text-green-700">
            Imported: {importRoster.data.added} added · {importRoster.data.updated} updated ·{" "}
            {importRoster.data.unchanged} unchanged · {importRoster.data.total} total.
          </p>
          {/* Remount per import (submittedAt changes) so toggle/applied state resets. */}
          <RosterDiffPanel key={importRoster.submittedAt} report={importRoster.data} />
        </>
      )}
      {importRoster.isError && (
        <div className="mt-3 space-y-1">
          <p className="text-sm text-red-600">{importRoster.error.message}</p>
          {errors.length > 0 && (
            <ul className="list-inside list-disc text-xs text-red-600">
              {errors.map((e, i) => (
                <li key={i}>
                  line {e.line}: {e.msg}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </Card>
  );
}

type BulkAction = "withdraw" | "reinstate";

/** Renders the import's roster diff with explicit bulk-sync proposals. The import
 * itself never mutates withdrawn state; each action here goes through a confirm
 * dialog and hits POST /api/students/bulk-{withdraw,reinstate}. */
function RosterDiffPanel({ report }: { report: ImportReport }) {
  const queryClient = useQueryClient();
  const missing = report.missing_active ?? [];
  const present = report.withdrawn_present ?? [];
  const emailChanged = report.email_changed ?? 0;
  const nameChanged = report.name_changed ?? 0;

  const [confirm, setConfirm] = useState<BulkAction | null>(null);
  // action -> updated count, once applied (replaces the button; no re-fire).
  const [applied, setApplied] = useState<Partial<Record<BulkAction, number>>>({});

  const bulk = useMutation({
    mutationFn: ({ action, ids }: { action: BulkAction; ids: string[] }) =>
      api.post<BulkStudentStatusResult>(`/api/students/bulk-${action}`, { student_ids: ids }),
    onSuccess: async (res, vars) => {
      await queryClient.invalidateQueries({ queryKey: ["students"] });
      setApplied((a) => ({ ...a, [vars.action]: res.updated }));
      setConfirm(null);
    },
  });

  if (missing.length === 0 && present.length === 0 && emailChanged === 0 && nameChanged === 0) {
    return null;
  }

  return (
    <div className="mt-3 space-y-3">
      {missing.length > 0 && (
        <div className="rounded-md border border-amber-200 bg-amber-50 p-3">
          <p className="text-sm font-medium text-amber-900">
            {missing.length} active {missing.length === 1 ? "student is" : "students are"} not in
            this CSV
          </p>
          <p className="mt-1 text-xs text-amber-800">
            Usually add/drop or 停修. Nothing was changed — withdraw them explicitly if they left
            the class.
          </p>
          <details className="mt-2">
            <summary className="cursor-pointer text-xs font-medium text-amber-900">
              Show the {missing.length} {missing.length === 1 ? "ID" : "IDs"}
            </summary>
            <p className="mt-1 break-all font-mono text-xs text-amber-800">{missing.join(", ")}</p>
          </details>
          {applied.withdraw !== undefined ? (
            <p className="mt-2 text-xs font-medium text-amber-900">
              Withdrew {applied.withdraw} {applied.withdraw === 1 ? "student" : "students"}.
            </p>
          ) : (
            <Button
              type="button"
              variant="danger"
              className="mt-2 px-2 py-1 text-xs"
              disabled={bulk.isPending}
              onClick={() => setConfirm("withdraw")}
            >
              Withdraw all {missing.length}
            </Button>
          )}
        </div>
      )}

      {present.length > 0 && (
        <div className="rounded-md border border-amber-200 bg-amber-50 p-3">
          <p className="text-sm font-medium text-amber-900">
            {present.length} withdrawn {present.length === 1 ? "student is" : "students are"} in
            this CSV
          </p>
          <p className="mt-1 text-xs text-amber-800">
            Likely retaking the course. They stay excluded from grading and publishing until you
            reinstate them.
          </p>
          <details className="mt-2">
            <summary className="cursor-pointer text-xs font-medium text-amber-900">
              Show the {present.length} {present.length === 1 ? "ID" : "IDs"}
            </summary>
            <p className="mt-1 break-all font-mono text-xs text-amber-800">{present.join(", ")}</p>
          </details>
          {applied.reinstate !== undefined ? (
            <p className="mt-2 text-xs font-medium text-amber-900">
              Reinstated {applied.reinstate} {applied.reinstate === 1 ? "student" : "students"}.
            </p>
          ) : (
            <Button
              type="button"
              className="mt-2 px-2 py-1 text-xs"
              disabled={bulk.isPending}
              onClick={() => setConfirm("reinstate")}
            >
              Reinstate all {present.length}
            </Button>
          )}
        </div>
      )}

      {(emailChanged > 0 || nameChanged > 0) && (
        <div className="rounded-md border border-neutral-200 bg-neutral-50 p-3 text-xs text-neutral-600">
          <p>
            {emailChanged} {emailChanged === 1 ? "email" : "emails"} changed · {nameChanged}{" "}
            {nameChanged === 1 ? "name" : "names"} changed.
          </p>
          {emailChanged > 0 && (
            <p className="mt-1">
              Heads-up: any open regrade threads for these students were started from the old
              address — new grade and result emails go to the updated address.
            </p>
          )}
        </div>
      )}

      {bulk.isError && <p className="text-xs text-red-600">{bulk.error.message}</p>}

      {confirm === "withdraw" && (
        <Dialog open onClose={() => setConfirm(null)} title={`Withdraw ${missing.length} students?`}>
          <p className="text-sm text-neutral-600">
            These students were removed from the class list. Withdrawing keeps their history but
            excludes them from future grading and publishing.
          </p>
          {bulk.isError && <p className="mt-2 text-xs text-red-600">{bulk.error.message}</p>}
          <div className="mt-4 flex justify-end gap-2">
            <Button variant="secondary" onClick={() => setConfirm(null)}>
              Cancel
            </Button>
            <Button
              variant="danger"
              disabled={bulk.isPending}
              onClick={() => bulk.mutate({ action: "withdraw", ids: missing })}
            >
              {bulk.isPending ? "Withdrawing…" : `Withdraw all ${missing.length}`}
            </Button>
          </div>
        </Dialog>
      )}

      {confirm === "reinstate" && (
        <Dialog
          open
          onClose={() => setConfirm(null)}
          title={`Reinstate ${present.length} students?`}
        >
          <p className="text-sm text-neutral-600">
            These students appear in the class list again — usually retakers. Reinstating includes
            them in new grading runs, publish batches, and grade emails again; their existing
            history was never deleted.
          </p>
          {bulk.isError && <p className="mt-2 text-xs text-red-600">{bulk.error.message}</p>}
          <div className="mt-4 flex justify-end gap-2">
            <Button variant="secondary" onClick={() => setConfirm(null)}>
              Cancel
            </Button>
            <Button
              disabled={bulk.isPending}
              onClick={() => bulk.mutate({ action: "reinstate", ids: present })}
            >
              {bulk.isPending ? "Reinstating…" : `Reinstate all ${present.length}`}
            </Button>
          </div>
        </Dialog>
      )}
    </div>
  );
}
