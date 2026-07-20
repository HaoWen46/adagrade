-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (email, display_name, role, active)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListUsers :many
SELECT * FROM users ORDER BY email;

-- name: ListAssignableGraders :many
-- Assignable graders for the TA-assignment picker (regrade v2 spec §6/§8 gap 2):
-- active users holding TA-or-higher role (ta|lecturer|admin). Deliberately excludes
-- inactive users (can't be logged-in assignees) and any lower role. This backs a
-- lecturer+ endpoint distinct from admin-only ListUsers, so the SELECT is scoped to
-- exactly the minimal fields the handler re-exposes (id, name, role) -- no email.
SELECT id, display_name, role FROM users
WHERE active AND role IN ('ta', 'lecturer', 'admin')
ORDER BY display_name, email;

-- name: UpdateUser :one
UPDATE users SET role = $2, active = $3, display_name = $4, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: CountActiveAdmins :one
SELECT count(*) FROM users WHERE role = 'admin' AND active;

-- name: UpsertActiveAdmin :one
INSERT INTO users (email, display_name, role, active)
VALUES ($1, '', 'admin', TRUE)
ON CONFLICT (email) DO UPDATE SET role = 'admin', active = TRUE, updated_at = now()
RETURNING *;
