CREATE TABLE IF NOT EXISTS expenses (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL CHECK (user_id > 0 AND user_id <= 9007199254740991),
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0 AND amount_minor <= 99999999999),
    category TEXT NOT NULL CHECK (category IN ('еда', 'транспорт', 'жильё', 'развлечения', 'здоровье', 'покупки', 'другое')),
    spent_at DATE NOT NULL,
    comment TEXT NOT NULL DEFAULT '' CHECK (char_length(comment) <= 500),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS expenses_user_date_idx ON expenses (user_id, spent_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS expenses_date_idx ON expenses (spent_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS expenses_user_created_idx ON expenses (user_id, created_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS telegram_updates (
    update_id BIGINT PRIMARY KEY,
    response TEXT NOT NULL DEFAULT '',
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
