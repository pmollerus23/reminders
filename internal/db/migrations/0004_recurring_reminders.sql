-- +goose Up
-- +goose StatementBegin
ALTER TABLE reminders ADD COLUMN recurrence text
    CHECK (recurrence IN ('hourly', 'daily', 'weekly', 'monthly'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE reminders DROP COLUMN recurrence;
-- +goose StatementEnd
