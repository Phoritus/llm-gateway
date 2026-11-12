-- +goose Up
-- +goose StatementBegin

CREATE TABLE gateway_keys (
    id           uuid        PRIMARY KEY,
    workspace_id uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    name         text        NOT NULL DEFAULT '',
    prefix       text        NOT NULL,
    hash         bytea       NOT NULL UNIQUE,
    last_used_at timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX gateway_keys_workspace_idx ON gateway_keys (workspace_id, created_at);

-- +goose StatementEnd

-- +goose Down
DROP TABLE gateway_keys;
