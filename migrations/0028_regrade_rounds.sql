-- +goose Up
-- Regrade rounds (round-based grading, phase 2): replies after the exam's
-- regrade deadline are recorded-but-rejected, and an adjudicated "regraded"
-- verdict ADOPTS a record as that turn's overlay layer — the effective grade
-- of an answer is the latest adopted record, else the round-0 official.

ALTER TABLE regrade_requests DROP CONSTRAINT regrade_requests_status_check;
ALTER TABLE regrade_requests ADD CONSTRAINT regrade_requests_status_check CHECK (status IN (
    'received', 'under_review', 'resolved_upheld', 'resolved_regraded',
    'rejected_bad_token', 'rejected_superseded', 'rejected_sender_mismatch',
    'rejected_rate_limited', 'rejected_deadline'
));

-- The adopted record for a regraded sub-item: normally the round method's AI
-- record, or a manually attached record when that method failed (source +
-- fallback, same rule as round 0). NULL for upheld/unadjudicated sub-items.
ALTER TABLE regrade_request_problems
    ADD COLUMN adopted_record_id BIGINT REFERENCES grading_records (id);

-- +goose Down
ALTER TABLE regrade_request_problems DROP COLUMN adopted_record_id;
ALTER TABLE regrade_requests DROP CONSTRAINT regrade_requests_status_check;
ALTER TABLE regrade_requests ADD CONSTRAINT regrade_requests_status_check CHECK (status IN (
    'received', 'under_review', 'resolved_upheld', 'resolved_regraded',
    'rejected_bad_token', 'rejected_superseded', 'rejected_sender_mismatch',
    'rejected_rate_limited'
));
