-- +goose Up
CREATE TABLE IF NOT EXISTS kafka_outbox (
	id         bigserial   PRIMARY KEY,
	topic      text        NOT NULL,
	key        bytea,
	value      bytea,
	headers    jsonb       NOT NULL DEFAULT '[]',
	created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orders (
	id          bigserial   PRIMARY KEY,
	customer_id text        NOT NULL,
	amount      bigint      NOT NULL CHECK (amount > 0),
	created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS notifications (
	order_id bigint      PRIMARY KEY REFERENCES orders (id),
	sent_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE notifications, orders, kafka_outbox;
