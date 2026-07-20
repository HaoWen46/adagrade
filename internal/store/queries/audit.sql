-- name: InsertAudit :exec
INSERT INTO audit_log (actor_user_id, action, target_kind, target_id, detail)
VALUES ($1, $2, $3, $4, $5);

-- name: ListAuditForTarget :many
SELECT * FROM audit_log WHERE target_kind = $1 AND target_id = $2 ORDER BY id DESC LIMIT $3;

-- ListAudit is the admin audit-log read path (trust spec §6, D39): all filters
-- optional, newest-first, paginated. actor_email joins users for display since
-- actor_user_id alone isn't human-readable in the UI.
-- name: ListAudit :many
SELECT al.*, u.email AS actor_email
FROM audit_log al
LEFT JOIN users u ON u.id = al.actor_user_id
WHERE (sqlc.narg(target_kind)::text IS NULL OR al.target_kind = sqlc.narg(target_kind))
  AND (sqlc.narg(target_id)::text IS NULL OR al.target_id = sqlc.narg(target_id))
  AND (sqlc.narg(action)::text IS NULL OR al.action = sqlc.narg(action))
  AND (sqlc.narg(actor_user_id)::bigint IS NULL OR al.actor_user_id = sqlc.narg(actor_user_id))
ORDER BY al.id DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: CountAudit :one
SELECT count(*)
FROM audit_log al
WHERE (sqlc.narg(target_kind)::text IS NULL OR al.target_kind = sqlc.narg(target_kind))
  AND (sqlc.narg(target_id)::text IS NULL OR al.target_id = sqlc.narg(target_id))
  AND (sqlc.narg(action)::text IS NULL OR al.action = sqlc.narg(action))
  AND (sqlc.narg(actor_user_id)::bigint IS NULL OR al.actor_user_id = sqlc.narg(actor_user_id));
