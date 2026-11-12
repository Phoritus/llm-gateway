-- +goose Up
-- +goose StatementBegin

ALTER TABLE provider_credentials ADD CONSTRAINT provider_credentials_id_workspace_key UNIQUE (id, workspace_id);

CREATE TABLE routes (
    id           uuid        PRIMARY KEY,
    workspace_id uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    alias        text        NOT NULL,
    free_first   boolean     NOT NULL DEFAULT false,
    max_hops     integer     NOT NULL DEFAULT 3 CHECK (max_hops BETWEEN 1 AND 10),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, alias),
    UNIQUE (id, workspace_id)
);

CREATE TABLE route_targets (
    route_id      uuid    NOT NULL,
    workspace_id  uuid    NOT NULL,
    position      integer NOT NULL CHECK (position >= 0),
    credential_id uuid    NOT NULL,
    model         text    NOT NULL,
    PRIMARY KEY (route_id, position),
    FOREIGN KEY (route_id, workspace_id) REFERENCES routes (id, workspace_id) ON DELETE CASCADE,
    FOREIGN KEY (credential_id, workspace_id) REFERENCES provider_credentials (id, workspace_id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX route_targets_credential_idx ON route_targets (credential_id);
-- Deferred, not RESTRICT: deleting a workspace cascades to its credentials and,
-- separately, to its routes' targets. An immediate check fires before the
-- second cascade has run, so a workspace that had a route could never be deleted.

-- +goose StatementEnd

-- +goose Down
DROP TABLE route_targets;
DROP TABLE routes;
ALTER TABLE provider_credentials DROP CONSTRAINT provider_credentials_id_workspace_key;
