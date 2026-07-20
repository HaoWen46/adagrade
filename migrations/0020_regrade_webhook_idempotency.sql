-- Webhook idempotency for inbound regrade replies (Phase 7 fix round 2, F1): Postmark
-- retries inbound webhook deliveries on timeout/non-2xx, and without a dedup key a
-- re-delivered payload creates a duplicate verified regrade row, burns rate-cap
-- budget, and double-sends the confirmation email. message_id is Postmark's per-
-- delivery MessageID (domain.InboundEmail.MessageID); nullable because not every
-- caller/fixture supplies one (e.g. rejected-before-parse paths never got this far
-- historically, and other providers may not set it), and a partial unique index
-- (WHERE message_id <> '') means multiple NULL/empty rows never collide with each
-- other -- only two rows sharing the same real message id do.

-- +goose Up
ALTER TABLE regrade_requests ADD COLUMN message_id TEXT;
CREATE UNIQUE INDEX regrade_requests_message_id_uniq ON regrade_requests (message_id) WHERE message_id <> '';

-- +goose Down
DROP INDEX regrade_requests_message_id_uniq;
ALTER TABLE regrade_requests DROP COLUMN message_id;
