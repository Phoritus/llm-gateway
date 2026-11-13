-- +goose Up
-- +goose StatementBegin

-- One row per request that resolved a route. key_id has no foreign key: keys
-- are soft-deleted, and this is the hottest insert path in the schema. Alias,
-- vendor and model are plain text so the history outlives the route and
-- credential it names.
CREATE TABLE usage_events (
    id                bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id      uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    key_id            uuid        NOT NULL,
    route_alias       text        NOT NULL,
    vendor            text        NOT NULL,
    model             text        NOT NULL,
    status            integer     NOT NULL,
    outcome           text        NOT NULL,
    attempts          integer     NOT NULL,
    prompt_tokens     integer     NOT NULL,
    completion_tokens integer     NOT NULL,
    latency_ms        integer     NOT NULL,
    ttft_ms           integer     NOT NULL,
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
