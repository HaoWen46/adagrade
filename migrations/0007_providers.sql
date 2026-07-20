-- LLM providers become app-managed data (DECISIONS D11 v1 revision): base URL,
-- model suggestions, and rate limits editable in the UI; the API key stored
-- AES-GCM-encrypted under the machine-local master key (D16). Method configs keep
-- referencing providers by name.

-- +goose Up
CREATE TABLE llm_providers (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL DEFAULT 'anthropic-compat' CHECK (kind IN ('anthropic-compat')),
    base_url TEXT NOT NULL,
    api_key_ciphertext BYTEA NOT NULL,
    api_key_hint TEXT NOT NULL DEFAULT '', -- e.g. "…ab12", shown in the UI
    models TEXT [] NOT NULL DEFAULT '{}', -- suggested model ids for pickers
    requests_per_second REAL NOT NULL DEFAULT 1 CHECK (requests_per_second > 0),
    burst INT NOT NULL DEFAULT 2 CHECK (burst >= 1),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    last_verified_at TIMESTAMPTZ, -- last successful "Test" call
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE llm_providers;
