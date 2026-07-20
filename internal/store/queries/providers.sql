-- name: ListProviders :many
SELECT * FROM llm_providers ORDER BY name;

-- name: ListEnabledProviders :many
SELECT * FROM llm_providers WHERE enabled ORDER BY name;

-- name: GetProvider :one
SELECT * FROM llm_providers WHERE id = $1;

-- name: GetProviderByName :one
SELECT * FROM llm_providers WHERE name = $1;

-- name: CreateProvider :one
INSERT INTO llm_providers (name, kind, base_url, api_key_ciphertext, api_key_hint, models, requests_per_second, burst)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: UpdateProvider :one
UPDATE llm_providers
SET base_url = $2,
    api_key_ciphertext = $3,
    api_key_hint = $4,
    models = $5,
    requests_per_second = $6,
    burst = $7,
    enabled = $8,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetProviderModels :exec
UPDATE llm_providers SET models = $2, updated_at = now() WHERE id = $1;

-- name: MarkProviderVerified :exec
UPDATE llm_providers SET last_verified_at = now(), updated_at = now() WHERE id = $1;

-- name: DeleteProvider :exec
DELETE FROM llm_providers WHERE id = $1;

-- name: CountProviders :one
SELECT count(*) FROM llm_providers;

-- name: CountMethodVersionsUsingProvider :one
SELECT count(*) FROM grading_method_versions WHERE config ->> 'provider' = sqlc.arg(provider)::text;
