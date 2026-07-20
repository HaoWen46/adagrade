-- +goose Up
-- Text-render-loss flag (pdfium CJK fix, 2026-07-12). The WASM pdfium build
-- silently renders glyphs from non-embedded CID/CJK fonts as nothing while the
-- PDF's text layer stays intact, so typeset content can vanish from the image
-- the AI grades with no error anywhere. render.ProbeTextLoss detects this at
-- render time; these columns persist the per-page count of suspect runs so a
-- workflow warning can surface affected pages. 0 = clean or no text layer
-- (bitmap scans always probe 0).
ALTER TABLE answer_pages ADD COLUMN text_loss_runs INTEGER NOT NULL DEFAULT 0;
ALTER TABLE scan_pages ADD COLUMN text_loss_runs INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE answer_pages DROP COLUMN text_loss_runs;
ALTER TABLE scan_pages DROP COLUMN text_loss_runs;
