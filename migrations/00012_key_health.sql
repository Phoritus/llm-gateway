-- +goose Up
-- +goose StatementBegin
ALTER TABLE provider_credentials
    ADD COLUMN check_due_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN checked_at   timestamptz,
    ADD COLUMN check_result text CHECK (check_result IN (
        'ok', 'key_rejected', 'quota_exhausted', 'rate_limited',
        'provider_timeout', 'provider_error', 'provider_unavailable'));
CREATE INDEX provider_credentials_check_due_idx ON provider_credentials (check_due_at);
-- +goose StatementEnd

-- +goose Down
DROP INDEX provider_credentials_check_due_idx;
ALTER TABLE provider_credentials DROP COLUMN check_result, DROP COLUMN checked_at, DROP COLUMN check_due_at;
