-- +goose Up
-- +goose StatementBegin
ALTER TABLE provider_credentials DROP CONSTRAINT provider_credentials_provider_check;
ALTER TABLE provider_credentials ADD CONSTRAINT provider_credentials_provider_check CHECK (provider IN (
    'openai', 'groq', 'cerebras', 'openrouter',
    'mistral', 'gemini', 'anthropic', 'mock'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM route_targets WHERE credential_id IN (SELECT id FROM provider_credentials WHERE provider = 'mock');
DELETE FROM provider_credentials WHERE provider = 'mock';
ALTER TABLE provider_credentials DROP CONSTRAINT provider_credentials_provider_check;
ALTER TABLE provider_credentials ADD CONSTRAINT provider_credentials_provider_check CHECK (provider IN (
    'openai', 'groq', 'cerebras', 'openrouter',
    'mistral', 'gemini', 'anthropic'));
-- +goose StatementEnd
