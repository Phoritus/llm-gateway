-- +goose Up
-- +goose StatementBegin

-- A workspace's own upstream API keys, sealed with AES-256-GCM. ciphertext is
-- nonce || GCM output exactly as crypto.Sealed packs it, so there is no
-- separate nonce column. key_id names the master key it was sealed under,
-- which is what a rotation sweep selects on.
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
CREATE INDEX provider_credentials_key_id_idx ON provider_credentials (key_id);

-- +goose StatementEnd

-- +goose Down
DROP TABLE provider_credentials;
