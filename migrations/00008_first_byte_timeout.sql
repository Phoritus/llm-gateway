-- +goose Up
-- +goose StatementBegin

ALTER TABLE route_targets ADD COLUMN first_byte_timeout_seconds integer
    CHECK (first_byte_timeout_seconds BETWEEN 1 AND 300);

-- +goose StatementEnd

-- +goose Down
ALTER TABLE route_targets DROP COLUMN first_byte_timeout_seconds;
