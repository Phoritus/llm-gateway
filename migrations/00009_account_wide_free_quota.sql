-- +goose Up
-- +goose StatementBegin

ALTER TABLE free_quota_catalog ADD COLUMN per_account boolean NOT NULL DEFAULT false;

-- OpenRouter's 50 free requests a day (GET /api/v1/key,
-- free_model_daily_requests) cover every :free model on the key.
UPDATE free_quota_catalog SET per_account = true WHERE vendor = 'openrouter' AND model LIKE '%:free';

-- +goose StatementEnd

-- +goose Down
ALTER TABLE free_quota_catalog DROP COLUMN per_account;
