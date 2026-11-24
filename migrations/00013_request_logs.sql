-- +goose Up
-- +goose StatementBegin

CREATE TABLE request_logs (
    id                 uuid        PRIMARY KEY,
    workspace_id       uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    key_id             uuid        NOT NULL,
    route_alias        text        NOT NULL,
    vendor             text        NOT NULL,
    model              text        NOT NULL,
    status             integer     NOT NULL,
    outcome            text        NOT NULL,
    created_at         timestamptz NOT NULL,
    request            text        NOT NULL,
    response           text        NOT NULL,
    request_truncated  boolean     NOT NULL,
    response_truncated boolean     NOT NULL
);

CREATE INDEX request_logs_workspace_created_idx ON request_logs (workspace_id, created_at DESC, id DESC);
CREATE INDEX request_logs_created_idx ON request_logs (created_at);

-- +goose StatementEnd

-- +goose Down
DROP TABLE request_logs;
