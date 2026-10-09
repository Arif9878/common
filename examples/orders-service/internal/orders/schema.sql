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
