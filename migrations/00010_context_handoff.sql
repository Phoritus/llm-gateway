-- +goose Up
-- +goose StatementBegin

-- off sends every request as it came. reorder tries the targets whose known
-- context window fits the request first. trim also fits the conversation to a
-- target's window, dropping middle messages, when it is too long for it.
ALTER TABLE routes ADD COLUMN context_handoff text NOT NULL DEFAULT 'reorder'
    CHECK (context_handoff IN ('off', 'reorder', 'trim'));

-- What context handoff did to the request: null when nothing, otherwise
-- reordered or trimmed. Never any of the request's text.
ALTER TABLE usage_events ADD COLUMN context_handoff text
    CHECK (context_handoff IN ('reordered', 'trimmed'));

-- +goose StatementEnd

-- +goose Down
ALTER TABLE usage_events DROP COLUMN context_handoff;
ALTER TABLE routes DROP COLUMN context_handoff;
