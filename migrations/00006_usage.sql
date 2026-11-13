-- +goose Up
-- +goose StatementBegin

-- One row per request that resolved a route. id is minted by the gateway when
-- it records the request, so a batch written twice stores each event once.
-- key_id has no foreign key, and alias, vendor and model are plain text, so
-- the history outlives the key, route and credential it names.
CREATE TABLE usage_events (
    id                uuid        PRIMARY KEY,
    workspace_id      uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    key_id            uuid        NOT NULL,
    route_alias       text        NOT NULL,
    vendor            text        NOT NULL,
    model             text        NOT NULL,
    status            integer     NOT NULL,
    outcome           text        NOT NULL,
    attempts          integer     NOT NULL,
    -- bigint: the token counts are whatever the upstream reported, and one
    -- row out of range would fail the batch it is written in.
    prompt_tokens     bigint      NOT NULL,
    completion_tokens bigint      NOT NULL,
    latency_ms        bigint      NOT NULL,
    ttft_ms           bigint      NOT NULL,
    cached            boolean     NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL
);

CREATE INDEX usage_events_workspace_created_idx ON usage_events (workspace_id, created_at DESC);
-- Retention deletes by age across every workspace.
CREATE INDEX usage_events_created_idx ON usage_events (created_at);

-- The dashboard's read side, kept for good. day is the UTC date of created_at.
CREATE TABLE usage_daily (
    workspace_id      uuid   NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    day               date   NOT NULL,
    route_alias       text   NOT NULL,
    vendor            text   NOT NULL,
    model             text   NOT NULL,
    requests          bigint NOT NULL,
    errors            bigint NOT NULL,
    prompt_tokens     bigint NOT NULL,
    completion_tokens bigint NOT NULL,
    latency_ms_sum    bigint NOT NULL,
    PRIMARY KEY (workspace_id, day, route_alias, vendor, model)
);

-- +goose StatementEnd

-- +goose Down
DROP TABLE usage_daily;
DROP TABLE usage_events;
