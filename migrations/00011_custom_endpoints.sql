-- +goose Up
-- +goose StatementBegin
ALTER TABLE provider_credentials DROP CONSTRAINT provider_credentials_provider_check;
ALTER TABLE provider_credentials ADD CONSTRAINT provider_credentials_provider_check CHECK (provider IN (
    'openai', 'groq', 'cerebras', 'openrouter',
    'mistral', 'gemini', 'anthropic', 'mock', 'custom'));

ALTER TABLE provider_credentials ADD COLUMN base_url text
    CHECK ((provider = 'custom') = (base_url IS NOT NULL));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM route_targets WHERE credential_id IN (SELECT id FROM provider_credentials WHERE provider = 'custom');
DELETE FROM provider_credentials WHERE provider = 'custom';
ALTER TABLE provider_credentials DROP COLUMN base_url;
ALTER TABLE provider_credentials DROP CONSTRAINT provider_credentials_provider_check;
ALTER TABLE provider_credentials ADD CONSTRAINT provider_credentials_provider_check CHECK (provider IN (
    'openai', 'groq', 'cerebras', 'openrouter',
    'mistral', 'gemini', 'anthropic', 'mock'));
-- +goose StatementEnd
