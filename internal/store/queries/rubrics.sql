-- Rubric + reference-solution versions are insert-only (DECISIONS D5): new content
-- is always version MAX+1 for the problem; nothing is ever mutated.

-- name: LatestRubricVersion :one
SELECT * FROM rubric_versions WHERE problem_id = $1 ORDER BY version DESC LIMIT 1;

-- name: ListRubricVersions :many
SELECT * FROM rubric_versions WHERE problem_id = $1 ORDER BY version DESC;

-- name: GetRubricVersion :one
SELECT * FROM rubric_versions WHERE id = $1;

-- name: CreateRubricVersion :one
INSERT INTO rubric_versions (problem_id, version, notes, score_increment, created_by)
VALUES (
    $1,
    (SELECT COALESCE(MAX(version), 0) + 1 FROM rubric_versions WHERE problem_id = $1),
    $2, $3, $4
)
RETURNING *;

-- name: CreateRubricCriterion :one
INSERT INTO rubric_criteria (rubric_version_id, position, description, points, partial_credit_notes)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListRubricCriteria :many
SELECT * FROM rubric_criteria WHERE rubric_version_id = $1 ORDER BY position;

-- name: LatestSolutionVersion :one
SELECT * FROM reference_solution_versions WHERE problem_id = $1 ORDER BY version DESC LIMIT 1;

-- name: ListSolutionVersions :many
SELECT * FROM reference_solution_versions WHERE problem_id = $1 ORDER BY version DESC;

-- name: GetSolutionVersion :one
SELECT * FROM reference_solution_versions WHERE id = $1;

-- name: CreateSolutionVersion :one
INSERT INTO reference_solution_versions (problem_id, version, content, created_by)
VALUES (
    $1,
    (SELECT COALESCE(MAX(version), 0) + 1 FROM reference_solution_versions WHERE problem_id = $1),
    $2, $3
)
RETURNING *;
