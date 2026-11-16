-- +goose Up
-- +goose StatementBegin

-- auto caches a request only when its temperature is exactly 0, always caches
-- every cacheable request on the route, off caches none.
ALTER TABLE routes ADD COLUMN cache text NOT NULL DEFAULT 'auto' CHECK (cache IN ('auto', 'always', 'off'));

-- Counted from the events stored with cached = true, so a hit rate is read
-- from the rollup instead of from raw events that retention prunes.
ALTER TABLE usage_daily ADD COLUMN cache_hits bigint NOT NULL DEFAULT 0;

-- What a vendor's free tier allows one key on one model. A null limit is one
-- the vendor does not report to a request made with a free key. The rows are
-- data the upstream changes, so correcting one is an UPDATE, not a release.
CREATE TABLE free_quota_catalog (
    vendor text    NOT NULL,
    model  text    NOT NULL,
    rpm    integer CHECK (rpm > 0),
    rpd    integer CHECK (rpd > 0),
    tpm    integer CHECK (tpm > 0),
    tpd    integer CHECK (tpd > 0),
    PRIMARY KEY (vendor, model)
);

-- Groq's response headers give requests a day and tokens a minute only.
-- Gemini's OpenAI-compatible endpoint reports no limits. OpenRouter's 50
-- requests a day (GET /api/v1/key, free_model_daily_requests) count across
-- every :free model on the account.
INSERT INTO free_quota_catalog (vendor, model, rpm, rpd, tpm, tpd) VALUES
    ('groq',       'openai/gpt-oss-120b',                 NULL, 1000, 8000, NULL),
    ('groq',       'openai/gpt-oss-20b',                  NULL, 1000, 8000, NULL),
    ('groq',       'qwen/qwen3.8-27b',                    NULL, 1000, 8000, NULL),
    ('gemini',     'gemini-3.5-flash-lite',               NULL, NULL, NULL, NULL),
    ('openrouter', 'google/gemma-4-31b-it:free',          NULL,   50, NULL, NULL),
    ('openrouter', 'nvidia/nemotron-3.5-lightning:free',  NULL,   50, NULL, NULL);

-- +goose StatementEnd

-- +goose Down
DROP TABLE free_quota_catalog;
ALTER TABLE usage_daily DROP COLUMN cache_hits;
ALTER TABLE routes DROP COLUMN cache;
