-- name: CreateGradingMethod :one
INSERT INTO grading_methods (name) VALUES ($1) RETURNING *;

-- name: ListGradingMethods :many
SELECT * FROM grading_methods
WHERE (sqlc.arg(include_archived)::bool OR archived_at IS NULL)
ORDER BY name;

-- name: GetGradingMethod :one
SELECT * FROM grading_methods WHERE id = $1;

-- name: SetMethodArchived :one
UPDATE grading_methods
SET archived_at = CASE WHEN sqlc.arg(archived)::bool THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: CreateMethodVersion :one
INSERT INTO grading_method_versions (method_id, version, config, created_by)
VALUES (
    $1,
    (SELECT COALESCE(MAX(version), 0) + 1 FROM grading_method_versions WHERE method_id = $1),
    $2, $3
)
RETURNING *;

-- name: LatestMethodVersion :one
SELECT * FROM grading_method_versions WHERE method_id = $1 ORDER BY version DESC LIMIT 1;

-- name: GetMethodVersion :one
SELECT * FROM grading_method_versions WHERE id = $1;

-- name: ListMethodVersions :many
SELECT * FROM grading_method_versions WHERE method_id = $1 ORDER BY version DESC;

-- name: CountGradingMethods :one
SELECT count(*) FROM grading_methods;

-- name: LatestPromptTemplate :one
SELECT * FROM prompt_template_versions WHERE name = $1 ORDER BY version DESC LIMIT 1;

-- name: GetPromptTemplateVersion :one
SELECT * FROM prompt_template_versions WHERE id = $1;

-- name: CreatePromptTemplateVersion :one
INSERT INTO prompt_template_versions (name, version, system_template, user_template, created_by)
VALUES (
    $1,
    (SELECT COALESCE(MAX(version), 0) + 1 FROM prompt_template_versions WHERE name = $1),
    $2, $3, $4
)
RETURNING *;
