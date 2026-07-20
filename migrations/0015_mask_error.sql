-- mask_error surface for the MaskPage job (D27 review, Finding 1). MaskPage was
-- the only new D27 River job without a terminal-error column: a deterministic
-- failure (e.g. a corrupt stored page JPEG that jpeg.Decode rejects on every
-- attempt) burned all 3 River attempts, the job was discarded, and the page
-- stayed masked=false with NO recorded cause — the D10 run gate blocked forever
-- and the review poll spun permanently with nothing visible to explain it.
--
-- This column records a short, PII-FREE static reason category on the final
-- failed attempt (never a dynamic message that could carry a path or student
-- content). A successful mask (or a no-op skip) clears it back to NULL, so the
-- "re-apply masks" retry path re-enqueues the page and a fixed page recovers.
-- +goose Up
ALTER TABLE answer_pages ADD COLUMN mask_error TEXT;

-- +goose Down
ALTER TABLE answer_pages DROP COLUMN mask_error;
