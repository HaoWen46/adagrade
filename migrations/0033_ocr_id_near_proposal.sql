-- +goose Up
-- Near-miss ID proposals (roster-anchored OCR, 2026-07-12). An OCR'd student
-- ID with no exact roster hit whose normalized form sits at edit distance 1
-- from EXACTLY ONE active roster ID now surfaces as a human-confirmed orphan
-- proposal, proposal_source 'ocr_id_near' (internal/scan/match.go). This is
-- proposal-only by construction: auto-assign still requires the exact-ID +
-- exact-name agreement rung — the wrong-student firewall is unchanged.
ALTER TABLE scan_pages DROP CONSTRAINT scan_pages_proposal_source_check;
ALTER TABLE scan_pages ADD CONSTRAINT scan_pages_proposal_source_check
    CHECK (proposal_source IN ('ocr_agree', 'ocr_id', 'ocr_id_near', 'ocr_name', 'ocr_disagree'));

-- +goose Down
-- Rows carrying the new value lose their provenance tag (the proposed
-- student/problem pre-fill itself remains; the UI's default copy covers a
-- proposal with no source).
UPDATE scan_pages SET proposal_source = NULL WHERE proposal_source = 'ocr_id_near';
ALTER TABLE scan_pages DROP CONSTRAINT scan_pages_proposal_source_check;
ALTER TABLE scan_pages ADD CONSTRAINT scan_pages_proposal_source_check
    CHECK (proposal_source IN ('ocr_agree', 'ocr_id', 'ocr_name', 'ocr_disagree'));
