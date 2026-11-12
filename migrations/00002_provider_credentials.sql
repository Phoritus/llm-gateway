-- +goose Up
-- +goose StatementBegin

CREATE TABLE provider_credentials (
    id           uuid        PRIMARY KEY,
    workspace_id uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    provider     text        NOT NULL CHECK (provider IN (
                     'openai', 'groq', 'cerebras', 'openrouter',
                     'mistral', 'gemini', 'anthropic')),
    name         text        NOT NULL DEFAULT '',
    key_id       text        NOT NULL,
    ciphertext   bytea       NOT NULL,
    last4        text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX provider_credentials_workspace_idx ON provider_credentials (workspace_id, created_at);

-- +goose StatementEnd

-- +goose Down
DROP TABLE provider_credentials;
