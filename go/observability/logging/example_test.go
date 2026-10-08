package logging_test

import (
	"context"
	"log/slog"
	"os"

	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/requestid"
)

func Example() {
	logger, err := logging.New(logging.Config{Service: "orders", Version: "1.4.0"})
	if err != nil {
		panic(err)
	}

	// Typically done by transport middleware and the Kafka consumer.
	ctx := requestid.NewContext(context.Background(), "req-7f3a")
	ctx = logging.ContextWithAttrs(ctx, slog.String(logging.KeyTopic, "orders.created"))

	logger.InfoContext(ctx, "order accepted", "order_id", "o-1", "card_token", "tok_abc")
	// Prints one JSON line containing service, version, order_id,
	// card_token="[REDACTED]", request_id and topic.
}

func ExampleNewHandler() {
	// Wrap any slog.Handler, for example to add fields to a test logger.
	h := logging.NewHandler(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}), logging.WithRedactKeys("iban"))

	slog.New(h).Info("payout", "iban", "DE89370400440532013000", "amount", 10)
	// Output:
	// level=INFO msg=payout iban=[REDACTED] amount=10
}
