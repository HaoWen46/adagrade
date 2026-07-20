-- name: CreateProblem :one
INSERT INTO problems (assessment_id, number, title, statement, max_points, position)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetProblem :one
SELECT * FROM problems WHERE id = $1;

-- name: ListProblems :many
SELECT * FROM problems WHERE assessment_id = $1 ORDER BY position, number;

-- name: UpdateProblem :one
UPDATE problems
SET number = $2, title = $3, statement = $4, max_points = $5, position = $6, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteProblem :exec
DELETE FROM problems WHERE id = $1;

-- name: CountProblemRecords :one
SELECT count(*)
FROM grading_records gr
JOIN answers a ON a.id = gr.answer_id
WHERE a.problem_id = $1;

-- name: CountProblemPages :one
SELECT count(*)
FROM answer_pages ap
JOIN answers a ON a.id = ap.answer_id
WHERE a.problem_id = $1;
