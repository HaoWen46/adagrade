-- Local-OCR rung on scan identification (DECISIONS D24): records which engine
-- produced a scan_file's OCR fields — 'local' for the on-device reader, or the
-- provider name when the cloud-VLM rung answered instead. Nullable: files
-- identified before this migration, or never OCR'd at all, carry no engine.

-- +goose Up
ALTER TABLE scan_files ADD COLUMN ocr_engine TEXT;

-- +goose Down
ALTER TABLE scan_files DROP COLUMN ocr_engine;
