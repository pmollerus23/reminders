-- +goose Up
-- +goose StatementBegin
ALTER TABLE reminders DROP CONSTRAINT reminders_status_check;
ALTER TABLE reminders ADD CONSTRAINT reminders_status_check
    CHECK (status IN ('pending', 'processing', 'sent', 'failed'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE reminders DROP CONSTRAINT reminders_status_check;
ALTER TABLE reminders ADD CONSTRAINT reminders_status_check
    CHECK (status IN ('pending', 'sent', 'failed'));
-- +goose StatementEnd