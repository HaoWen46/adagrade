-- Inbound regrade queue (spec §5-6, §8 — Phase 7). One row per verified-or-rejected
-- inbound reply, walked through the verification ladder in the webhook handler; the
-- ladder's rejections are recorded here too (status rejected/*) so nothing is silently
-- dropped and the no-backscatter property is auditable after the fact.

-- +goose Up
CREATE TABLE regrade_requests (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    publish_item_id BIGINT REFERENCES publish_items (id), -- NULL if the token didn't even parse
    student_id BIGINT REFERENCES students (id),
    assessment_id BIGINT REFERENCES assessments (id),
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    from_email TEXT NOT NULL,
    spf_verdict TEXT, -- warn-not-block (D33 rung 4): recorded, not enforced in v0
    dkim_verdict TEXT,
    subject TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL DEFAULT '', -- student content: same PII class as transcriptions, never logged
    status TEXT NOT NULL DEFAULT 'received' CHECK (status IN (
        'received', 'under_review', 'resolved_upheld', 'resolved_regraded',
        'rejected_bad_token', 'rejected_superseded', 'rejected_sender_mismatch',
        'rejected_rate_limited'
    )),
    resolver_id BIGINT REFERENCES users (id),
    resolution_note TEXT NOT NULL DEFAULT '',
    resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Queue view: filter by assessment + status.
CREATE INDEX regrade_requests_assessment_status_idx ON regrade_requests (assessment_id, status);
-- The rate cap (D33 rung 5): count verified requests per (student, assessment).
CREATE INDEX regrade_requests_student_assessment_idx ON regrade_requests (student_id, assessment_id);

-- +goose Down
DROP TABLE regrade_requests;
