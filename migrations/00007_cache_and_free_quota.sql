-- +goose Up
-- +goose StatementBegin

-- auto caches a request only when its temperature is exactly 0, always caches
-- every cacheable request on the route, off caches none.
ALTER TABLE routes ADD COLUMN cache text NOT NULL DEFAULT 'auto' CHECK (cache IN ('auto', 'always', 'off'));

-- Counted from the events stored with cached = true, so a hit rate is read
-- from the rollup instead of from raw events that retention prunes.
ALTER TABLE usage_daily ADD COLUMN cache_hits bigint NOT NULL DEFAULT 0;

-- What a vendor's free tier allows one key on one model. A null limit is one
-- the vendor does not publish. The rows are data the upstream changes, so
-- correcting one is an UPDATE here, not a release.
CREATE TABLE free_quota_catalog (
    vendor text    NOT NULL,
    model  text    NOT NULL,
    rpm    integer CHECK (rpm > 0),
    rpd    integer CHECK (rpd > 0),
    tpm    integer CHECK (tpm > 0),
    tpd    integer CHECK (tpd > 0),
    PRIMARY KEY (vendor, model)
);

-- As published by each vendor at the time of writing, and not verified
-- against live keys. Where a vendor counts a limit across the whole account
-- rather than per model (OpenRouter's :free models, Mistral's free plan), the
-- row is that account-wide figure, so several such models on one key are
-- over-counted as free.
INSERT INTO free_quota_catalog (vendor, model, rpm, rpd, tpm, tpd) VALUES
    ('groq',       'llama-3.3-70b-versatile',                 30,  1000,   12000,  100000),
    ('groq',       'llama-3.1-8b-instant',                    30, 14400,    6000,  500000),
    ('cerebras',   'llama-3.3-70b',                           30, 14400,   60000, 1000000),
    ('cerebras',   'llama3.1-8b',                             30, 14400,   60000, 1000000),
    ('gemini',     'gemini-2.5-flash',                        10,   250,  250000,    NULL),
    ('gemini',     'gemini-2.5-flash-lite',                   15,  1000,  250000,    NULL),
    ('openrouter', 'meta-llama/llama-3.3-70b-instruct:free',  20,    50,    NULL,    NULL),
    -- Mistral publishes one request a second, written here as 60 a minute.
    ('mistral',    'mistral-small-latest',                    60,  NULL,  500000,    NULL);

-- +goose StatementEnd

-- +goose Down
DROP TABLE free_quota_catalog;
ALTER TABLE usage_daily DROP COLUMN cache_hits;
ALTER TABLE routes DROP COLUMN cache;
