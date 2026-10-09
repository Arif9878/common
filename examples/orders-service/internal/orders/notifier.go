package orders

import (
	"context"
	"log/slog"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/messaging/kafka"
)

// NewNotifier returns the handler of "order created" events. A real service
// would send an e-mail or push notification; this one records it.
//
// The consumer is idempotent at two levels: commonfx.KafkaIdempotency skips
// events already handled (keyed by the outbox's event-id header, in
// Redis), and the insert ignores a notification that exists, for the rare
// redelivery after a crash between the insert and Redis recording it.
func NewNotifier(db *postgres.DB, logger *slog.Logger) kafka.Handler {
	return kafka.Typed(kafka.JSON[Created]{}, func(ctx context.Context, _ *kgo.Record, ev Created) error {
		_, err := db.Exec(ctx, `INSERT INTO notifications (order_id) VALUES ($1) ON CONFLICT (order_id) DO NOTHING`, ev.OrderID)
		if err != nil {
			return postgres.Classify(err) // Unavailable is retried; the record is not committed until it succeeds
		}
		logger.InfoContext(ctx, "order notification sent", "order_id", ev.OrderID, "customer_id", ev.CustomerID)
		return nil
	})
}
