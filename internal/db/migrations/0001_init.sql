-- +goose Up
-- +goose StatementBegin
CREATE TABLE reminders (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    telegram_chat_id  bigint      NOT NULL,
    body              text        NOT NULL,
    scheduled_at      timestamptz NOT NULL,
    status            text        NOT NULL DEFAULT 'pending'
                                  CHECK (status IN ('pending', 'sent', 'failed')),
    locked_until      timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX reminders_due_idx
    ON reminders (scheduled_at)
    WHERE status = 'pending';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS reminders;
-- +goose StatementEnd