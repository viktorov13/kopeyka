CREATE TABLE telegram_conversations (
    user_id BIGINT PRIMARY KEY CHECK (user_id > 0 AND user_id <= 9007199254740991),
    history JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(history) = 'array'),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
