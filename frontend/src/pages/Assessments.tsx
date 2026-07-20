// Assessments list: create, archive/unarchive, drill into detail.

import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import { roleAtLeast, useMe } from "../lib/auth";
import type { Assessment } from "../lib/types";
import { fmtDate } from "../lib/format";
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
} from "../components/ui";
import { Archive as ArchiveIcon } from "../components/icons";

export function Assessments() {
  const me = useMe();
  const canEdit = roleAtLeast(me.data?.user.role, "lecturer");
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [includeArchived, setIncludeArchived] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  // Archive is confirm-gated (workflow guards): it looks like a lifecycle action but
  // only hides the row — nothing running (batches, regrade windows, runs) is stopped.
  const [archiving, setArchiving] = useState<Assessment | null>(null);

  const list = useQuery({
    queryKey: ["assessments", includeArchived],
    queryFn: () =>
      api.get<{ assessments: Assessment[] }>(
        includeArchived ? "/api/assessments?include_archived=1" : "/api/assessments",
      ),
  });

  const archive = useMutation({
    mutationFn: (a: Assessment) =>
      api.post<Assessment>(`/api/assessments/${a.id}/archive`, { archived: !a.archived }),
    onSuccess: () => {
      setArchiving(null);
      return queryClient.invalidateQueries({ queryKey: ["assessments"] });
    },
  });

  return (
    <div className="mx-auto max-w-4xl space-y-4">
      <div className="flex items-center justify-between gap-3">
        <h1 className="text-lg font-semibold text-neutral-900">Assessments</h1>
        <div className="flex items-center gap-4">
          <label className="flex items-center gap-1.5 text-sm text-neutral-600">
            <input
              type="checkbox"
              checked={includeArchived}
              onChange={(e) => setIncludeArchived(e.target.checked)}
              className="size-3.5 accent-indigo-600"
            />
            Include archived
          </label>
          {canEdit && <Button onClick={() => setCreateOpen(true)}>New assessment</Button>}
        </div>
      </div>

      {archive.isError && <p className="text-xs text-red-600">{archive.error.message}</p>}

      {list.isPending ? (
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      ) : list.isError ? (
        <Card>
          <p className="text-sm text-red-600">{list.error.message}</p>
        </Card>
      ) : (
        <Table>
          <thead>
            <tr>
              <TH>Name</TH>
              <TH>Kind</TH>
              <TH className="text-right">Problems</TH>
              <TH>Created</TH>
              <TH>Status</TH>
              {canEdit && <TH className="w-24" />}
            </tr>
          </thead>
          <tbody>
            {list.data.assessments.length === 0 && (
              <tr>
                <TD colSpan={canEdit ? 6 : 5} className="text-center text-neutral-400">
                  No assessments yet.
                </TD>
              </tr>
            )}
            {list.data.assessments.map((a) => (
              <tr
                key={a.id}
                onClick={() => void navigate(`/assessments/${a.id}`)}
                className="cursor-pointer hover:bg-neutral-50"
              >
                <TD className="font-medium">
                  {/* B12: a real Link, not just a row onClick — gives keyboard focus,
                      middle/cmd-click "open in new tab", and an actual accessibility-tree
                      entry. stopPropagation so clicking the link text doesn't also fire
                      the row's own onClick navigate() underneath it. */}
                  <Link
                    to={`/assessments/${a.id}`}
                    className="hover:underline"
                    onClick={(e) => e.stopPropagation()}
                  >
                    {a.name}
                  </Link>
                </TD>
                <TD>
                  <Badge tone={a.kind === "exam" ? "indigo" : "neutral"}>{a.kind}</Badge>
                </TD>
                <TD className="text-right tabular-nums">{a.problem_count ?? 0}</TD>
                <TD className="text-neutral-500">{fmtDate(a.created_at)}</TD>
                <TD>{a.archived ? <Badge tone="amber">archived</Badge> : <Badge tone="green">active</Badge>}</TD>
                {canEdit && (
                  <TD className="text-right">
                    <IconButton
                      label={a.archived ? `Unarchive ${a.name}` : `Archive ${a.name}`}
                      // No pending Spinner swap here (unlike Methods/Providers): the
                      // archive mutation is shared by every row, so a swap would show
                      // all rows spinning at once. disabled alone is the honest state.
                      disabled={archive.isPending}
                      onClick={(e) => {
                        e.stopPropagation();
                        // Unarchive is harmless (it only re-lists the row) — only
                        // archiving goes through the confirm dialog.
                        if (a.archived) archive.mutate(a);
                        else setArchiving(a);
                      }}
                    >
                      <ArchiveIcon />
                    </IconButton>
                  </TD>
                )}
              </tr>
            ))}
          </tbody>
        </Table>
      )}

      {createOpen && <CreateAssessmentDialog onClose={() => setCreateOpen(false)} />}
      {archiving && (
        <Dialog open onClose={() => setArchiving(null)} title={`Archive ${archiving.name}?`}>
          <p className="text-sm text-neutral-700">
            Archiving only hides this assessment from the default list. Publish batches, regrade
            windows, and runs continue unaffected.
          </p>
          {archive.isError && <p className="mt-2 text-xs text-red-600">{archive.error.message}</p>}
          <div className="mt-4 flex justify-end gap-2">
            <Button variant="secondary" onClick={() => setArchiving(null)}>
              Cancel
            </Button>
            <Button disabled={archive.isPending} onClick={() => archive.mutate(archiving)}>
              {archive.isPending ? "Archiving…" : "Archive"}
            </Button>
          </div>
        </Dialog>
      )}
    </div>
  );
}

function CreateAssessmentDialog({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [kind, setKind] = useState("exam");
  const [name, setName] = useState("");

  const create = useMutation({
    mutationFn: () => api.post<Assessment>("/api/assessments", { kind, name: name.trim() }),
    // Drop the operator straight into the new (empty) assessment so the next step —
    // adding problems — is right there, rather than back on the roster hunting for
    // the row they just created.
    onSuccess: async (created) => {
      await queryClient.invalidateQueries({ queryKey: ["assessments"] });
      onClose();
      void navigate(`/assessments/${created.id}`);
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (name.trim()) create.mutate();
  };

  return (
    <Dialog open onClose={onClose} title="New assessment">
      <form className="space-y-3" onSubmit={submit}>
        <Field label="Kind">
          <Select value={kind} onChange={(e) => setKind(e.target.value)} className="w-full">
            <option value="exam">exam</option>
            <option value="assignment">assignment</option>
          </Select>
        </Field>
        <Field label="Name">
          <Input
            required
            autoFocus
            placeholder="e.g. Midterm 1"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </Field>
        {create.isError && <p className="text-xs text-red-600">{create.error.message}</p>}
        <div className="flex justify-end gap-2 pt-1">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={create.isPending || !name.trim()}>
            {create.isPending ? "Creating…" : "Create"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
