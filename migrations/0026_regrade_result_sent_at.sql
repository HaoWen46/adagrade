-- Send-failure recovery path (regrade v2 whole-branch review, Finding F1).
--
-- handleSendResult's atomic flip-before-send (D59, migration 0025 era) is the correct
-- race arbiter for send-once: it must win the resolve BEFORE calling the email
-- provider, or two concurrent sends could both pass a pre-check read. But that
-- ordering means a provider send failure AFTER the flip leaves the request resolved
-- with no email ever delivered and, until now, no way to tell "resolved because we
-- sent" apart from "resolved but the send failed" -- and no route to retry. The
-- student's result email #N never arrives and the chain is permanently dead for that
-- request.
--
-- result_sent_at (nullable) is the delivery marker: set only after a SUCCESSFUL
-- provider send (both the original send-result call and the new resend-result path,
-- migration-adjacent handler change). NULL after a resolve-but-send-failed row is
-- exactly the "needs recovery" signal the new POST /api/regrades/{id}/resend-result
-- route gates on.

-- +goose Up
ALTER TABLE regrade_requests ADD COLUMN result_sent_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE regrade_requests DROP COLUMN result_sent_at;
