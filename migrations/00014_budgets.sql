-- +goose Up
-- +goose StatementBegin

-- What a vendor charges for a model, in USD per million tokens, which is also
-- micro-USD per token. Like free_quota_catalog the rows are data the upstream
-- changes, so correcting one is an UPDATE, and each says when and where it was
-- read. Six decimals: the gateway reads a price as whole millionths of a
-- micro-USD per token, so a finer price would be rounded anyway.
CREATE TABLE model_prices (
    vendor                    text          NOT NULL,
    model                     text          NOT NULL,
    input_usd_per_mtok        numeric(12,6) NOT NULL CHECK (input_usd_per_mtok >= 0),
    output_usd_per_mtok       numeric(12,6) NOT NULL CHECK (output_usd_per_mtok >= 0),
    cached_input_usd_per_mtok numeric(12,6) CHECK (cached_input_usd_per_mtok >= 0),
    checked_on                date          NOT NULL,
    source                    text          NOT NULL,
    PRIMARY KEY (vendor, model)
);

-- Standard tier, the lowest prompt-length tier where a vendor has several.
-- gemini-4-carbon rises to 4.00/20.00 on 2026-12-01, and gemini-3.8-flash and
-- gemini-3.6-flash to 1.50/7.50 on 2027-01-01, by Google's own page.
INSERT INTO model_prices (vendor, model, input_usd_per_mtok, output_usd_per_mtok, cached_input_usd_per_mtok, checked_on, source) VALUES
    ('openai',    'gpt-6.2-sol',           2.00, 10.00, 0.10,  '2026-11-24', 'https://developers.openai.com/api/docs/pricing'),
    ('openai',    'gpt-6.2-astra',         8.00, 40.00, 0.40,  '2026-11-24', 'https://developers.openai.com/api/docs/pricing'),
    ('openai',    'gpt-5.6-terra',         2.00, 12.00, 0.20,  '2026-11-24', 'https://developers.openai.com/api/docs/pricing'),
    ('openai',    'gpt-5.6-luna',          0.20,  1.20, 0.02,  '2026-11-24', 'https://developers.openai.com/api/docs/pricing'),
    ('openai',    'gpt-5.4-mini',          0.75,  4.50, 0.075, '2026-11-24', 'https://developers.openai.com/api/docs/pricing'),
    ('openai',    'gpt-5-mini',            0.25,  2.00, 0.025, '2026-11-24', 'https://developers.openai.com/api/docs/pricing'),
    ('anthropic', 'claude-fable-5-2',      8.00, 40.00, 0.20,  '2026-11-24', 'https://platform.claude.com/docs/en/about-claude/pricing'),
    ('anthropic', 'claude-opus-5-5',       4.00, 20.00, 0.20,  '2026-11-24', 'https://platform.claude.com/docs/en/about-claude/pricing'),
    ('anthropic', 'claude-sonnet-5-5',     2.00, 10.00, 0.10,  '2026-11-24', 'https://platform.claude.com/docs/en/about-claude/pricing'),
    ('anthropic', 'claude-haiku-5-5',      0.10,  0.50, 0.01,  '2026-11-24', 'https://platform.claude.com/docs/en/about-claude/pricing'),
    ('gemini',    'gemini-4-carbon',       2.00, 10.00, 0.10,  '2026-11-24', 'https://aistudio.google.com/docs/pricing'),
    ('gemini',    'gemini-3.8-flash',      0.75,  3.75, 0.075, '2026-11-24', 'https://aistudio.google.com/docs/pricing'),
    ('gemini',    'gemini-3.6-flash',      0.75,  3.75, 0.075, '2026-11-24', 'https://aistudio.google.com/docs/pricing'),
    ('gemini',    'gemini-3.5-flash-lite', 0.30,  2.50, 0.03,  '2026-11-24', 'https://aistudio.google.com/docs/pricing'),
    ('groq',      'openai/gpt-oss-120b',   0.15,  0.60, NULL,  '2026-11-24', 'https://console.groq.com/docs/models'),
    ('groq',      'openai/gpt-oss-20b',    0.075, 0.30, NULL,  '2026-11-24', 'https://console.groq.com/docs/models');

-- Null is a request this gateway could not price: an unpriced model, or one
-- recorded with budgets off. A request that reached no target costs 0.
ALTER TABLE usage_events ADD COLUMN cost_micro_usd bigint;
ALTER TABLE usage_daily
    ADD COLUMN cost_micro_usd bigint NOT NULL DEFAULT 0,
    ADD COLUMN unpriced       bigint NOT NULL DEFAULT 0;

ALTER TABLE gateway_keys
    ADD COLUMN budget_micro_usd bigint CHECK (budget_micro_usd >= 0),
    ADD COLUMN budget_action    text   CHECK (budget_action IN ('block', 'warn')),
    ADD CONSTRAINT gateway_keys_budget_whole CHECK ((budget_micro_usd IS NULL) = (budget_action IS NULL));

-- +goose StatementEnd

-- +goose Down
ALTER TABLE gateway_keys DROP CONSTRAINT gateway_keys_budget_whole,
    DROP COLUMN budget_action, DROP COLUMN budget_micro_usd;
ALTER TABLE usage_daily DROP COLUMN unpriced, DROP COLUMN cost_micro_usd;
ALTER TABLE usage_events DROP COLUMN cost_micro_usd;
DROP TABLE model_prices;
