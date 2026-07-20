-- +goose Up
-- Image intake (D22) added the invalid_image quarantine outcome after the
-- original quarantine table was created. Widen the persisted reason contract so
-- malformed image uploads reach the durable review queue instead of degrading to
-- "quarantine record failed" after their blob has already been stored.
ALTER TABLE upload_quarantine DROP CONSTRAINT upload_quarantine_reason_check;
ALTER TABLE upload_quarantine ADD CONSTRAINT upload_quarantine_reason_check
    CHECK (reason IN ('unknown_student', 'invalid_pdf', 'invalid_image', 'duplicate_in_batch'));

-- +goose Down
-- Preserve rows across rollback: invalid_image is the image-specific spelling of
-- the older unreadable-document category, so narrow it before restoring the old
-- CHECK rather than deleting quarantine history.
UPDATE upload_quarantine SET reason = 'invalid_pdf' WHERE reason = 'invalid_image';
ALTER TABLE upload_quarantine DROP CONSTRAINT upload_quarantine_reason_check;
ALTER TABLE upload_quarantine ADD CONSTRAINT upload_quarantine_reason_check
    CHECK (reason IN ('unknown_student', 'invalid_pdf', 'duplicate_in_batch'));
