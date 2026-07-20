// Users: allowlist management (admin only). Role/active edits apply immediately —
// sessions resolve the live user row on every request.

import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import { roleAtLeast, useMe } from "../lib/auth";
import type { AuditEntry, AuditListResponse, UserRow } from "../lib/types";
import { fmtDate } from "../lib/format";
import { Badge, Button, Card, Field, Input, Select, Spinner, TD, TH, Table } from "../components/ui";
import { HelpTip } from "../components/HelpTip";
import { userRolesHelp } from "../lib/helpContent";
import { userLabel } from "../lib/userLabel";

const ROLES = ["ta", "lecturer", "admin"] as const;

export function Users() {
  const me = useMe();

  if (!roleAtLeast(me.data?.user.role, "admin")) {
    return (
      <div className="mx-auto max-w-4xl space-y-4">
        <h1 className="text-lg font-semibold text-neutral-900">Users</h1>
        <Card>
          <p className="text-sm text-neutral-500">Admins only.</p>
        </Card>
      </div>
    );
  }
  return <UsersAdmin selfId={me.data?.user.id} />;
}

function UsersAdmin({ selfId }: { selfId: number | undefined }) {
  const queryClient = useQueryClient();

  const users = useQuery({
    queryKey: ["users"],
    queryFn: () => api.get<{ users: UserRow[] }>("/api/users"),
  });

  const update = useMutation({
    mutationFn: ({ id, ...patch }: { id: number; role?: string; active?: boolean }) =>
      api.patch<UserRow>(`/api/users/${id}`, patch),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["users"] }),
  });

  return (
    <div className="mx-auto max-w-4xl space-y-4">
      <h1 className="text-lg font-semibold text-neutral-900">Users</h1>

      <CreateUserCard />

      {update.isError && <p className="text-xs text-red-600">{update.error.message}</p>}

      {users.isPending ? (
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      ) : users.isError ? (
        <Card>
          <p className="text-sm text-red-600">{users.error.message}</p>
        </Card>
      ) : (
        <Table>
          <thead>
            <tr>
              <TH>Email</TH>
              <TH>Name</TH>
              <TH className="w-32">
                <span className="inline-flex items-center gap-1.5">
                  Role <HelpTip title="Roles">{userRolesHelp}</HelpTip>
                </span>
              </TH>
              <TH className="w-24">Active</TH>
            </tr>
          </thead>
          <tbody>
            {users.data.users.map((u) => (
              <tr key={u.id} className="hover:bg-neutral-50">
                <TD className="font-medium">
                  {u.email}
                  {u.id === selfId && (
                    <Badge tone="indigo" className="ml-2">
                      you
                    </Badge>
                  )}
                </TD>
                <TD>
                  {u.display_name ? (
                    u.display_name
                  ) : (
                    // No display name set (the Add-user name field is optional) — fall
                    // back to the email, which is always present and always identifies
                    // a real person, never a bare "user #N" (audit B8).
                    <span className="text-neutral-500">{userLabel({ email: u.email })}</span>
                  )}
                </TD>
                <TD>
                  <Select
                    value={u.role}
                    disabled={update.isPending}
                    onChange={(e) => update.mutate({ id: u.id, role: e.target.value })}
                    className="w-28"
                  >
                    {ROLES.map((r) => (
                      <option key={r} value={r}>
                        {r}
                      </option>
                    ))}
                  </Select>
                </TD>
                <TD>
                  <label className="flex items-center gap-1.5 text-sm text-neutral-600">
                    <input
                      type="checkbox"
                      checked={u.active}
                      disabled={update.isPending}
                      onChange={(e) => update.mutate({ id: u.id, active: e.target.checked })}
                      className="size-3.5 accent-indigo-600"
                    />
                    {u.active ? "active" : "inactive"}
                  </label>
                </TD>
              </tr>
            ))}
          </tbody>
        </Table>
      )}

      <AuditSection />
    </div>
  );
}

// --- audit log (admin-only, trust spec §6, D39) -------------------------------------------

const AUDIT_PAGE_SIZE = 50;

function AuditSection() {
  const [targetKind, setTargetKind] = useState("");
  const [targetId, setTargetId] = useState("");
  const [action, setAction] = useState("");
  const [actor, setActor] = useState("");
  const [offset, setOffset] = useState(0);

  // Filters reset paging back to the first page — otherwise a narrower filter could
  // land on an offset past the end of its own (smaller) result set.
  const setFilter = (setter: (v: string) => void) => (v: string) => {
    setter(v);
    setOffset(0);
  };

  const params = new URLSearchParams();
  if (targetKind.trim()) params.set("target_kind", targetKind.trim());
  if (targetId.trim()) params.set("target_id", targetId.trim());
  if (action.trim()) params.set("action", action.trim());
  if (actor.trim()) params.set("actor", actor.trim());
  params.set("limit", String(AUDIT_PAGE_SIZE));
  params.set("offset", String(offset));

  const audit = useQuery({
    queryKey: ["audit", targetKind, targetId, action, actor, offset],
    queryFn: () => api.get<AuditListResponse>(`/api/audit?${params.toString()}`),
  });

  const entries = audit.data?.entries ?? [];

  return (
    <Card title="Audit log">
      <div className="mb-3 flex flex-wrap items-end gap-3">
        <Field label="Target kind">
          <Input
            placeholder="run, user, …"
            value={targetKind}
            onChange={(e) => setFilter(setTargetKind)(e.target.value)}
            className="w-32"
          />
        </Field>
        <Field label="Target ID">
          <Input
            placeholder="123"
            value={targetId}
            onChange={(e) => setFilter(setTargetId)(e.target.value)}
            className="w-24"
          />
        </Field>
        <Field label="Action">
          <Input
            placeholder="run.launch, …"
            value={action}
            onChange={(e) => setFilter(setAction)(e.target.value)}
            className="w-36"
          />
        </Field>
        <Field label="Actor user ID">
          <Input
            placeholder="123"
            value={actor}
            onChange={(e) => setFilter(setActor)(e.target.value)}
            className="w-24"
          />
        </Field>
      </div>

      {audit.isPending ? (
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      ) : audit.isError ? (
        <p className="text-sm text-red-600">{audit.error.message}</p>
      ) : (
        <>
          <Table>
            <thead>
              <tr>
                <TH className="w-40">Time</TH>
                <TH>Actor</TH>
                <TH className="w-32">Action</TH>
                <TH className="w-28">Target kind</TH>
                <TH className="w-24">Target ID</TH>
                <TH>Detail</TH>
              </tr>
            </thead>
            <tbody>
              {entries.length === 0 ? (
                <tr>
                  <TD colSpan={6} className="text-center text-neutral-400">
                    No audit entries match these filters.
                  </TD>
                </tr>
              ) : (
                entries.map((e) => <AuditRow key={e.id} entry={e} />)
              )}
            </tbody>
          </Table>
          <div className="mt-3 flex items-center justify-between">
            <p className="text-xs text-neutral-500">
              Showing {entries.length === 0 ? 0 : offset + 1}
              {"–"}
              {offset + entries.length}
            </p>
            <div className="flex gap-2">
              <Button
                variant="secondary"
                className="px-2.5 py-1 text-xs"
                title="Previous page"
                aria-label="Previous page"
                disabled={offset === 0}
                onClick={() => setOffset((o) => Math.max(0, o - AUDIT_PAGE_SIZE))}
              >
                ←
              </Button>
              <Button
                variant="secondary"
                className="px-2.5 py-1 text-xs"
                title="Next page"
                aria-label="Next page"
                disabled={entries.length < AUDIT_PAGE_SIZE}
                onClick={() => setOffset((o) => o + AUDIT_PAGE_SIZE)}
              >
                →
              </Button>
            </div>
          </div>
        </>
      )}
    </Card>
  );
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  return (
    <tr className="hover:bg-neutral-50 align-top">
      <TD className="text-xs whitespace-nowrap text-neutral-500 tabular-nums">
        {fmtDate(entry.created_at)}
      </TD>
      <TD className="text-xs">
        {entry.actor_email || <span className="text-neutral-400">system</span>}
      </TD>
      <TD className="text-xs font-medium">{entry.action}</TD>
      <TD className="text-xs text-neutral-600">{entry.target_kind}</TD>
      <TD className="text-xs tabular-nums">{entry.target_id}</TD>
      <TD>
        {entry.detail === undefined ? (
          <span className="text-xs text-neutral-400">—</span>
        ) : (
          <details>
            <summary className="cursor-pointer text-xs text-neutral-500 hover:text-neutral-800">
              detail
            </summary>
            <pre className="mt-1 max-h-40 max-w-md overflow-auto rounded-md border border-neutral-200 bg-neutral-50 p-2 font-mono text-xs whitespace-pre-wrap text-neutral-700">
              {JSON.stringify(entry.detail, null, 2)}
            </pre>
          </details>
        )}
      </TD>
    </tr>
  );
}

function CreateUserCard() {
  const queryClient = useQueryClient();
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [role, setRole] = useState<string>("ta");

  const create = useMutation({
    mutationFn: () =>
      api.post<UserRow>("/api/users", {
        email: email.trim(),
        role,
        display_name: displayName.trim(),
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["users"] });
      setEmail("");
      setDisplayName("");
      setRole("ta");
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (email.trim()) create.mutate();
  };

  return (
    <Card title="Add user">
      <form className="flex items-end gap-3" onSubmit={submit}>
        <Field label="Email" className="flex-1">
          <Input
            type="email"
            required
            placeholder="someone@ntu.edu.tw"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </Field>
        <Field label="Display name" className="flex-1">
          <Input value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
        </Field>
        <Field label="Role">
          <Select value={role} onChange={(e) => setRole(e.target.value)} className="w-28">
            {ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </Select>
        </Field>
        <Button type="submit" disabled={create.isPending || !email.trim()}>
          {create.isPending ? "Adding…" : "Add"}
        </Button>
      </form>
      {create.isError && <p className="mt-2 text-xs text-red-600">{create.error.message}</p>}
    </Card>
  );
}
