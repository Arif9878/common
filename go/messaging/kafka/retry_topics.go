package kafka

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Arif9878/common/go/errors"
)

// RetryTopic is one step of delayed retries: records that failed are
// published to Topic and handled again Delay later.
type RetryTopic struct {
	Topic string
	Delay time.Duration
}

// Headers set on records published to a retry topic. The original
// coordinates are kept across steps; the dead-letter headers use them too.
const (
	HeaderRetryOriginalTopic     = "retry.original.topic"
	HeaderRetryOriginalPartition = "retry.original.partition"
	HeaderRetryOriginalOffset    = "retry.original.offset"
	HeaderRetryAttempt           = "retry.attempt"    // 1 in the first retry topic
	HeaderRetryNotBefore         = "retry.not-before" // Unix milliseconds
	HeaderRetryErrorKind         = "retry.error.kind"
	HeaderRetryError             = "retry.error"
)

// WithRetryTopics retries records that failed with a retryable error (see
// errors.IsRetryable) after a delay, without blocking their partition:
// such a record is published with p to the first step's topic and its
// offset committed, so the records after it continue. Delay later it is
// handled again; if it fails again it moves to the next step, and after
// the last one it is dead-lettered (WithDLQ), skipped (WithSkipOnFailure)
// or stops its retry topic's partition, like any record that failed for
// good. Records that fail with a non-retryable error skip the retry topics.
//
//	kafka.WithRetryTopics(producer,
//		kafka.RetryTopic{Topic: "billing.orders.retry-1m", Delay: time.Minute},
//		kafka.RetryTopic{Topic: "billing.orders.retry-10m", Delay: 10 * time.Minute}),
//	kafka.WithDLQ(producer, "billing.orders.dlq"),
//
// The topics must exist; give each consumer group its own, since another
// group's handler would retry records it did not fail. Each step is
// consumed by an internal consumer in the group "<group>.<topic>", started
// and closed with this one. Retried records reach the handler with their
// original Topic, Partition and Offset (so WithIdempotency keys match) and
// the retry headers above.
//
// Ordering: a retried record is handled after records that came after it.
// Don't use retry topics where records of one key must be handled in order.
func WithRetryTopics(p *Producer, steps ...RetryTopic) Option {
	return func(o *options) { o.retryProducer, o.retrySteps = p, steps }
}

func validateRetryTopics(o options, topics []string) error {
	if len(o.retrySteps) == 0 {
		return nil
	}
	if o.retryProducer == nil {
		return errors.InvalidArgument.New("kafka: WithRetryTopics needs a producer")
	}
	seen := map[string]bool{}
	for _, s := range o.retrySteps {
		switch {
		case s.Topic == "" || s.Delay <= 0:
			return errors.InvalidArgument.New("kafka: every RetryTopic needs a topic and a positive delay")
		case seen[s.Topic] || slices.Contains(topics, s.Topic) || s.Topic == o.dlqTopic:
			return errors.InvalidArgument.Errorf("kafka: retry topic %s is used twice", s.Topic)
		}
		seen[s.Topic] = true
	}
	return nil
}

// original returns the coordinates r was first consumed at.
func original(r *kgo.Record) (topic string, partition int32, offset int64) {
	topic, partition, offset = r.Topic, r.Partition, r.Offset
	if t := headerValue(r, HeaderRetryOriginalTopic); t != "" {
		topic = t
		if p, err := strconv.ParseInt(headerValue(r, HeaderRetryOriginalPartition), 10, 32); err == nil {
			partition = int32(p)
		}
		if o, err := strconv.ParseInt(headerValue(r, HeaderRetryOriginalOffset), 10, 64); err == nil {
			offset = o
		}
	}
	return topic, partition, offset
}

func headerValue(r *kgo.Record, key string) string {
	for i := len(r.Headers) - 1; i >= 0; i-- {
		if r.Headers[i].Key == key {
			return string(r.Headers[i].Value)
		}
	}
	return ""
}

// notBefore returns when r may be handled, or the zero time.
func notBefore(r *kgo.Record) time.Time {
	ms, err := strconv.ParseInt(headerValue(r, HeaderRetryNotBefore), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// retryRecords builds the records for step (0-based) from rs, which
// failed with err.
func retryRecords(rs []*kgo.Record, step int, s RetryTopic, err error, now time.Time) []*kgo.Record {
	kind, msg := errors.KindOf(err).String(), truncate(err.Error(), 1024)
	out := make([]*kgo.Record, len(rs))
	for i, r := range rs {
		topic, partition, offset := original(r)
		headers := slices.DeleteFunc(slices.Clone(r.Headers), func(h kgo.RecordHeader) bool {
			switch h.Key {
			case HeaderRetryOriginalTopic, HeaderRetryOriginalPartition, HeaderRetryOriginalOffset,
				HeaderRetryAttempt, HeaderRetryNotBefore, HeaderRetryErrorKind, HeaderRetryError:
				return true
			}
			return false
		})
		headers = append(headers,
			kgo.RecordHeader{Key: HeaderRetryOriginalTopic, Value: []byte(topic)},
			kgo.RecordHeader{Key: HeaderRetryOriginalPartition, Value: []byte(strconv.Itoa(int(partition)))},
			kgo.RecordHeader{Key: HeaderRetryOriginalOffset, Value: []byte(strconv.FormatInt(offset, 10))},
			kgo.RecordHeader{Key: HeaderRetryAttempt, Value: []byte(strconv.Itoa(step + 1))},
			kgo.RecordHeader{Key: HeaderRetryNotBefore, Value: []byte(strconv.FormatInt(ceilMilli(now.Add(s.Delay)), 10))},
			kgo.RecordHeader{Key: HeaderRetryErrorKind, Value: []byte(kind)},
			kgo.RecordHeader{Key: HeaderRetryError, Value: []byte(msg)},
		)
		out[i] = &kgo.Record{Topic: s.Topic, Key: r.Key, Value: r.Value, Headers: headers}
	}
	return out
}

// asOriginal wraps h so it sees records from a retry topic with their
// original topic, partition and offset. The consumer keeps the records it
// fetched for committing.
func asOriginal(h BatchHandler) BatchHandler {
	return func(ctx context.Context, rs []*kgo.Record) error {
		cp := make([]*kgo.Record, len(rs))
		for i, r := range rs {
			c := *r
			c.Topic, c.Partition, c.Offset = original(r)
			cp[i] = &c
		}
		return h(ctx, cp)
	}
}

// waitDue blocks until the last record of batch may be handled (records of
// a retry topic are due in order, as they share its delay). It reports
// false if the worker is stopped first.
func (c *Consumer) waitDue(w *worker, batch []*kgo.Record) bool {
	due := notBefore(batch[len(batch)-1])
	d := time.Until(due)
	if due.IsZero() || d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-w.stop:
		return false
	}
}

// ceilMilli returns t in Unix milliseconds, rounded up so a record is
// never retried before its delay.
func ceilMilli(t time.Time) int64 {
	ms := t.UnixMilli()
	if t.UnixNano()%int64(time.Millisecond) != 0 {
		ms++
	}
	return ms
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
