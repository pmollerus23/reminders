-- +goose Up
-- +goose StatementBegin
CREATE TABLE chat_proactive_log (
    telegram_chat_id bigint      PRIMARY KEY,
    last_sent_at     timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS chat_proactive_log;
-- +goose StatementEnd
