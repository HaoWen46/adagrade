-- Second provider wire protocol: openai-compat (OpenRouter, OpenAI, and other
-- Chat-Completions-compatible gateways) alongside anthropic-compat (D11 v1).

-- +goose Up
ALTER TABLE llm_providers DROP CONSTRAINT llm_providers_kind_check;
ALTER TABLE llm_providers ADD CONSTRAINT llm_providers_kind_check
    CHECK (kind IN ('anthropic-compat', 'openai-compat'));

-- +goose Down
DELETE FROM llm_providers WHERE kind = 'openai-compat';
ALTER TABLE llm_providers DROP CONSTRAINT llm_providers_kind_check;
ALTER TABLE llm_providers ADD CONSTRAINT llm_providers_kind_check
    CHECK (kind IN ('anthropic-compat'));
