package kafka_test

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/kafka/kafkatest"
	"github.com/Arif9878/common/go/resilience/retry"
	"github.com/Arif9878/common/go/testkit"
)

// retryEnv has a topic, two retry steps and a dead-letter topic.
func retryEnv(t *testing.T) (*kafkatest.Env, *kafka.Producer, string, []kafka.RetryTopic) {
	t.Helper()
	k := kafkatest.New(t, 1, "events", "events.retry-1", "events.retry-2", "events.dlq")
	group := k.Group("g")
	steps := []kafka.RetryTopic{
		{Topic: k.Topic("events.retry-1"), Delay: 300 * time.Millisecond},
		{Topic: k.Topic("events.retry-2"), Delay: 600 * time.Millisecond},
	}
	for _, s := range steps {
		k.TrackGroup(group + "." + s.Topic)
	}
	return k, newProducer(t, k.Config), group, steps
}

var once = kafka.WithRetry(retry.New(retry.WithMaxAttempts(1)))

type call struct {
	value, topic, attempt string
	offset                int64
	at                    time.Time
}

type calls struct {
	mu sync.Mutex
	l  []call
}

func (c *calls) add(r *kgo.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.l = append(c.l, call{string(r.Value), r.Topic, header(r, kafka.HeaderRetryAttempt), r.Offset, time.Now()})
}

func (c *calls) get() []call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.l)
}

func TestRetryTopicsDelayThenSuccess(t *testing.T) {
	k, p, group, steps := retryEnv(t)
	publish(t, p, k.Topic("events"), "a", "flaky", "b", "ok")

	var got calls
	c, err := kafka.NewConsumer(context.Background(), k.Config, group, []string{k.Topic("events")}, func(_ context.Context, r *kgo.Record) error {
		got.add(r)
		if string(r.Value) == "flaky" && header(r, kafka.HeaderRetryAttempt) == "" {
			return errors.Unavailable.New("inventory down")
		}
		return nil
	}, quiet, once, kafka.WithRetryTopics(p, steps...), kafka.WithDLQ(p, k.Topic("events.dlq")))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	testkit.Eventually(t, 30*time.Second, "three handler calls", func() bool { return len(got.get()) == 3 })

	l := got.get()
	if l[0].value != "flaky" || l[1].value != "ok" {
		t.Fatalf("calls = %+v: the failed record blocked the next one", l)
	}
	retried := l[2]
	if retried.value != "flaky" || retried.topic != k.Topic("events") || retried.offset != 0 || retried.attempt != "1" {
		t.Errorf("retried call = %+v, want flaky from %s offset 0, attempt 1", retried, k.Topic("events"))
	}
	if d := retried.at.Sub(l[0].at); d < steps[0].Delay {
		t.Errorf("retried after %s, before the %s delay", d, steps[0].Delay)
	}
}

func TestRetryTopicsExhaustedGoToDLQ(t *testing.T) {
	k, p, group, steps := retryEnv(t)
	publish(t, p, k.Topic("events"), "a", "broken")

	var got calls
	c, err := kafka.NewConsumer(context.Background(), k.Config, group, []string{k.Topic("events")}, func(_ context.Context, r *kgo.Record) error {
		got.add(r)
		return errors.Unavailable.New("inventory down")
	}, quiet, once, kafka.WithRetryTopics(p, steps...), kafka.WithDLQ(p, k.Topic("events.dlq")))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)

	dlq := consumeAll(t, k, k.Topic("events.dlq"), 1)
	if attempts := got.get(); len(attempts) != 3 || attempts[1].attempt != "1" || attempts[2].attempt != "2" {
		t.Errorf("handler calls = %+v, want the original and one per retry step", attempts)
	}
	r := dlq[0]
	if header(r, "dlq.original.topic") != k.Topic("events") || header(r, "dlq.original.offset") != "0" ||
		header(r, "dlq.error.kind") != "unavailable" || header(r, kafka.HeaderRetryAttempt) != "2" {
		t.Errorf("dlq headers = %v", r.Headers)
	}
	nb, _ := strconv.ParseInt(header(r, kafka.HeaderRetryNotBefore), 10, 64)
	if nb == 0 {
		t.Errorf("no %s header", kafka.HeaderRetryNotBefore)
	}
}

func TestRetryTopicsSkipNonRetryableErrors(t *testing.T) {
	k, p, group, steps := retryEnv(t)
	publish(t, p, k.Topic("events"), "a", "poison")

	var got calls
	c, err := kafka.NewConsumer(context.Background(), k.Config, group, []string{k.Topic("events")}, func(_ context.Context, r *kgo.Record) error {
		got.add(r)
		return errors.InvalidArgument.New("unknown event type")
	}, quiet, once, kafka.WithRetryTopics(p, steps...), kafka.WithDLQ(p, k.Topic("events.dlq")))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)

	dlq := consumeAll(t, k, k.Topic("events.dlq"), 1)
	if n := len(got.get()); n != 1 {
		t.Errorf("handler calls = %d, want 1: a non-retryable error must not be retried", n)
	}
	if header(dlq[0], kafka.HeaderRetryAttempt) != "" {
		t.Errorf("dlq record went through a retry topic: %v", dlq[0].Headers)
	}
}

func TestRetryTopicsConfigErrors(t *testing.T) {
	k := kafkatest.New(t, 1, "events")
	p := newProducer(t, k.Config)
	h := func(context.Context, *kgo.Record) error { return nil }
	for name, opt := range map[string]kafka.Option{
		"no producer":     kafka.WithRetryTopics(nil, kafka.RetryTopic{Topic: "r", Delay: time.Second}),
		"no delay":        kafka.WithRetryTopics(p, kafka.RetryTopic{Topic: "r"}),
		"no topic":        kafka.WithRetryTopics(p, kafka.RetryTopic{Delay: time.Second}),
		"consumed topic":  kafka.WithRetryTopics(p, kafka.RetryTopic{Topic: k.Topic("events"), Delay: time.Second}),
		"duplicate topic": kafka.WithRetryTopics(p, kafka.RetryTopic{Topic: "r", Delay: time.Second}, kafka.RetryTopic{Topic: "r", Delay: time.Minute}),
	} {
		if _, err := kafka.NewConsumer(context.Background(), k.Config, k.Group("g"), []string{k.Topic("events")}, h, quiet, opt); errors.KindOf(err) != errors.InvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", name, err)
		}
	}
}
