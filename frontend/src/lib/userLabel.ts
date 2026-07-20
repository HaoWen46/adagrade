// Shared "who is this" label for a user shown in the UI (audit B8): the old ad-hoc
// fallback chains rendered a bare `user #${id}` whenever display_name was empty — the
// common case, since the Add-user name field is optional — which is meaningless to a TA
// picking who to hand a problem to. The fallback chain is display_name (or a grader's
// `name` field, same concept under a different key) then email, which is always present
// and always identifies a real person; only once both are unavailable does this fall back
// to a neutral placeholder — never a raw numeric id.

export interface LabeledUser {
  display_name?: string | null;
  name?: string | null;
  email?: string | null;
  role?: string | null;
}

/** Human label for a user. Appends " (role)" only when `role` is supplied — callers that
 * already show role in its own column/badge should omit it here to avoid repeating it. */
export function userLabel(u: LabeledUser): string {
  const name = (u.display_name ?? u.name ?? "").trim();
  const base = name || (u.email ?? "").trim() || "name unavailable";
  return u.role ? `${base} (${u.role})` : base;
}
