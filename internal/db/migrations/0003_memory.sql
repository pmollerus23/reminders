-- +goose Up
-- +goose StatementBegin
CREATE TABLE chat_turns (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    telegram_chat_id bigint      NOT NULL,
    role             text        NOT NULL CHECK (role IN ('user', 'assistant')),
    content          text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);

-- Ordered by most-recent-first to make "load last N turns" cheap.
CREATE INDEX chat_turns_chat_created_idx
    ON chat_turns (telegram_chat_id, created_at DESC);

CREATE TABLE chat_summaries (
    telegram_chat_id   bigint      PRIMARY KEY,
    summary            text        NOT NULL,
    summarized_through timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE chat_facts (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    telegram_chat_id bigint      NOT NULL,
    -- No CHECK constraint on kind: the vocabulary lives in Go, not the DB,
    -- so adding a new Kind constant doesn't require a migration.
    kind             text        NOT NULL,
    content          jsonb       NOT NULL,
    source           text        NOT NULL CHECK (source IN ('user', 'inferred')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX chat_facts_chat_kind_idx
    ON chat_facts (telegram_chat_id, kind);

CREATE INDEX chat_facts_content_gin_idx
    ON chat_facts USING GIN (content);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS chat_facts;
DROP TABLE IF EXISTS chat_summaries;
DROP TABLE IF EXISTS chat_turns;
-- +goose StatementEnd
