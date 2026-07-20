-- Core auth + audit tables (spec §4, §10).

-- +goose Up
CREATE TABLE users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email TEXT NOT NULL UNIQUE, -- normalized: lowercase+trim (internal/auth.NormalizeEmail)
    display_name TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL CHECK (role IN ('admin', 'lecturer', 'ta')),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- scs/pgxstore session storage: server-side sessions, instant revocation (spec §2).
CREATE TABLE sessions (
    token TEXT PRIMARY KEY,
    data BYTEA NOT NULL,
    expiry TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions (expiry);

-- Append-only audit trail for create/delete/override/publish (plan §10).
CREATE TABLE audit_log (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_user_id BIGINT REFERENCES users (id),
    action TEXT NOT NULL,
    target_kind TEXT NOT NULL,
    target_id TEXT NOT NULL DEFAULT '',
    detail JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_target_idx ON audit_log (target_kind, target_id);

-- +goose Down
DROP TABLE audit_log;
DROP TABLE sessions;
DROP TABLE users;
