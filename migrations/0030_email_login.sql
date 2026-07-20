-- Email magic-link login for allowlisted staff.

-- +goose Up
CREATE TABLE login_tokens (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX login_tokens_user_idx ON login_tokens (user_id);
CREATE INDEX login_tokens_expires_idx ON login_tokens (expires_at);

-- +goose Down
DROP TABLE login_tokens;
