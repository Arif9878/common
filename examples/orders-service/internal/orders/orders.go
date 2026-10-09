// Package orders is the example service's domain: orders are created over
// HTTP, an "order created" event is published through the transactional
// outbox, and a Kafka consumer records a notification for each order.
package orders

import (
	"context"
	"embed"
	"io/fs"
	"strconv"
	"time"

	"go.uber.org/fx"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/errors"
	commonfx "github.com/Arif9878/common/go/fx"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/outbox"
)

// Topic receives the "order created" events.
const Topic = "orders.created"

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations are the service's goose migrations, applied at startup by
// commonfx.PostgresMigrations (one replica at a time).
var Migrations, _ = fs.Sub(migrations, "migrations")

// Module wires the domain into an fx application that has the commonfx
// Postgres, Outbox, EchoServer and Kafka modules.
var Module = fx.Options(
	commonfx.PostgresMigrations(Migrations),
	fx.Provide(NewService),
	fx.Invoke(RegisterRoutes),
	commonfx.KafkaConsumer("orders-notifier", []string{Topic}, NewNotifier),
)

// Order is an order as stored and returned by the API.
type Order struct {
	ID         int64     `json:"id"`
	CustomerID string    `json:"customer_id"`
	Amount     int64     `json:"amount"`
	CreatedAt  time.Time `json:"created_at"`
	Notified   bool      `json:"notified"`
}

// Created is the event published for every new order.
type Created struct {
	OrderID    int64  `json:"order_id"`
	CustomerID string `json:"customer_id"`
	Amount     int64  `json:"amount"`
}

// Service creates and reads orders.
type Service struct {
	db  *postgres.DB
	box *outbox.Outbox
}

// NewService returns the service.
func NewService(db *postgres.DB, box *outbox.Outbox) *Service {
	return &Service{db: db, box: box}
}

var errInvalidOrder = errors.WithPublicMessage(
	errors.InvalidArgument.New("invalid order"),
	"customer_id is required and amount must be positive")

// Create saves an order and its "order created" event in one transaction,
// so the event is published if and only if the order exists.
func (s *Service) Create(ctx context.Context, customerID string, amount int64) (Order, error) {
	if customerID == "" || amount <= 0 {
		return Order{}, errInvalidOrder // a 400 problem+json response
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Order{}, postgres.Classify(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // no-op after Commit

	o := Order{CustomerID: customerID, Amount: amount}
	err = tx.QueryRow(ctx, `INSERT INTO orders (customer_id, amount) VALUES ($1, $2) RETURNING id, created_at`,
		customerID, amount).Scan(&o.ID, &o.CreatedAt)
	if err != nil {
		return Order{}, postgres.Classify(err)
	}
	r, err := kafka.Encode(Topic, []byte(strconv.FormatInt(o.ID, 10)),
		Created{OrderID: o.ID, CustomerID: customerID, Amount: amount}, kafka.JSON[Created]{})
	if err != nil {
		return Order{}, err
	}
	if err := s.box.Write(ctx, tx, r); err != nil {
		return Order{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, postgres.Classify(err)
	}
	return o, nil
}

// Get returns the order with id, and whether its notification was sent.
func (s *Service) Get(ctx context.Context, id int64) (Order, error) {
	var o Order
	err := s.db.QueryRow(ctx, `SELECT o.id, o.customer_id, o.amount, o.created_at, n.order_id IS NOT NULL
FROM orders o LEFT JOIN notifications n ON n.order_id = o.id WHERE o.id = $1`, id).
		Scan(&o.ID, &o.CustomerID, &o.Amount, &o.CreatedAt, &o.Notified)
	if err != nil {
		return Order{}, postgres.Classify(err) // pgx.ErrNoRows becomes NotFound: a 404
	}
	return o, nil
}
