-- Report-PDF publish attachments (spec §3, §9 — morning round D42/D46). A publish
-- batch can now attach the generated report PDF(s) to the outbound email: 'none' (the
-- existing behaviour, default — no attachment), 'compressed' (downscaled images, spec
-- §3), or 'original' (full-resolution page images). `zip` (spec §3 D45) swaps the
-- merged report PDF for a ZIP of per-problem JPEGs at the chosen quality — a fallback
-- for mail gateways or PDF viewers that choke on the PDF, sent INSTEAD OF the PDF, not
-- the PDF bundled inside a ZIP. Both fields are batch-level settings, chosen at publish time, so every
-- item in the batch shares them — GetPublishItemForResend (0023's sibling task) reads
-- them off the parent batch to reconstruct the same attachment behaviour for a
-- single-item resend.

-- +goose Up
ALTER TABLE publish_batches ADD COLUMN attachment TEXT NOT NULL DEFAULT 'none'
    CHECK (attachment IN ('none', 'compressed', 'original'));
ALTER TABLE publish_batches ADD COLUMN zip BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE publish_batches DROP COLUMN zip;
ALTER TABLE publish_batches DROP COLUMN attachment;
