package kafka_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/tenant"
	"github.com/Arif9878/common/go/testkit"
)

func TestTenantFromProducer(t *testing.T) {
	prop := kafka.WithPropagators(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	k := newKafka(t, 1, "events")
	p := newProducer(t, k.cfg, prop)

	ctx := tenant.NewContext(context.Background(), "acme")
	if err := p.Publish(ctx, &kgo.Record{Topic: k.T("events"), Value: []byte("with tenant")}); err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), &kgo.Record{Topic: k.T("events"), Value: []byte("without")}); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		opts []kafka.Option
		want string
	}{
		"WithTenant":  {[]kafka.Option{kafka.WithTenant()}, "acme"},
		"not trusted": {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			tenants := map[string]string{}
			bags := map[string]string{}
			c, err := kafka.NewConsumer(context.Background(), k.cfg, k.G("g-"+name), []string{k.T("events")}, func(ctx context.Context, r *kgo.Record) error {
				mu.Lock()
				defer mu.Unlock()
				tenants[string(r.Value)], _ = tenant.FromContext(ctx)
				bags[string(r.Value)] = baggage.FromContext(ctx).Member(tenant.BaggageKey).Value()
				return nil
			}, append([]kafka.Option{quiet, prop}, tc.opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			running(t, c)
			testkit.Eventually(t, 30*time.Second, "both records", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(tenants) == 2
			})
			mu.Lock()
			defer mu.Unlock()
			if tenants["with tenant"] != tc.want || tenants["without"] != "" {
				t.Errorf("tenants = %v, want %q for the first record only", tenants, tc.want)
			}
			if bags["with tenant"] != "acme" {
				t.Errorf("handler context lacks the producer's baggage: %v", bags)
			}
		})
	}
}
