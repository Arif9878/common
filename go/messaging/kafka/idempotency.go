package kafka

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/idempotency"
	"github.com/Arif9878/common/go/observability/logging"
)

// Idempotency configures [WithIdempotency].
type Idempotency struct {
	// Store keeps the processed keys. Use a store shared by all replicas,
	// such as redisstore.New(rdb) or pgstore.New(db).
	Store idempotency.Store
	// Key returns a record's idempotency key, or "" to always process the
	// record. The default, [OffsetKey], catches redelivery of the same
	// record; use [HeaderKey] to also catch an event published twice.
	Key func(group string, r *kgo.Record) string
	// Lease is how long a claim lasts before another consumer may take the
	// record over. It must exceed the handler's worst-case duration,
	// retries included. Default 5 minutes.
	Lease time.Duration
	// TTL is how long processed keys are remembered. Default 24 hours.
	TTL time.Duration
}

// WithIdempotency makes the consumer skip records that were already
// processed, using cfg.Store to remember them. It turns at-least-once
// delivery (a record is redelivered after a crash, rebalance or failed
// commit) into processing each record once, within the limits below.
//
// Before calling the handler, the consumer claims each record's key. A key
// already completed is a duplicate: the record is not passed to the
// handler, is committed, and is counted in kafka.consumer.duplicates. A key
// claimed by another consumer that is still running fails the call with
// [idempotency.ErrInProgress] (retryable), so the record is tried again
// later. After the handler succeeds, its records' keys are completed; when
// it fails, their claims are released so a later delivery can retry them.
// For a [BatchHandler], the batch it receives excludes duplicates, and the
// records reported processed by a [BatchError] are completed.
//
// Limits are those of the idempotency package: a crash after the handler's
// side effects but before the keys are completed, a handler running longer
// than Lease, or a redelivery after TTL processes the record again. Make
// handlers idempotent for the side effects where that matters.
//
// With Redis:
//
//	rdb, err := redis.New(ctx, cfg.Redis)
//	...
//	kafka.WithIdempotency(kafka.Idempotency{Store: redisstore.New(rdb)})
func WithIdempotency(cfg Idempotency) Option {
	return func(o *options) {
		if cfg.Key == nil {
			cfg.Key = OffsetKey
		}
		if cfg.Lease <= 0 {
			cfg.Lease = 5 * time.Minute
		}
		if cfg.TTL <= 0 {
			cfg.TTL = 24 * time.Hour
		}
		o.idem = &cfg
	}
}

// OffsetKey identifies a record by consumer group, topic, partition and
// offset. It catches redelivery of the same record. It does not catch the
// same event published twice (those are two records), and keys would
// repeat if the topic were deleted and recreated within TTL.
func OffsetKey(group string, r *kgo.Record) string {
	return "kafka:" + group + ":" + r.Topic + ":" + strconv.Itoa(int(r.Partition)) + ":" + strconv.FormatInt(r.Offset, 10)
}

// HeaderKey returns a key function that uses the value of the record
// header name, such as an event ID set by the producer, scoped by consumer
// group. It also catches the same event published twice. Records without
// the header are identified by [OffsetKey].
func HeaderKey(name string) func(group string, r *kgo.Record) string {
	return func(group string, r *kgo.Record) string {
		for _, h := range r.Headers {
			if h.Key == name && len(h.Value) > 0 {
				return "kafka:" + group + ":" + name + ":" + string(h.Value)
			}
		}
		return OffsetKey(group, r)
	}
}

type claim struct {
	key   string
	token string // "" when the record has no key
}

// idempotent wraps h so it only sees records not processed before.
func (c *Consumer) idempotent(h BatchHandler) BatchHandler {
	cfg := c.o.idem
	return func(ctx context.Context, rs []*kgo.Record) error {
		todo := make([]*kgo.Record, 0, len(rs))
		claims := make([]claim, 0, len(rs))
		pos := make([]int, 0, len(rs)) // index in rs of each todo record
		release := func(cs []claim) {
			for _, cl := range cs {
				if cl.token == "" {
					continue
				}
				if err := cfg.Store.Release(context.WithoutCancel(ctx), cl.key, cl.token); err != nil && !errors.Is(err, idempotency.ErrLeaseLost) {
					c.o.logger.WarnContext(ctx, "kafka idempotency: release claim", "key", cl.key, logging.Err(err))
				}
			}
		}

		duplicates := 0
		inBatch := make(map[string]bool, len(rs))
		for i, r := range rs {
			key := cfg.Key(c.group, r)
			if key == "" {
				todo, claims, pos = append(todo, r), append(claims, claim{}), append(pos, i)
				continue
			}
			if inBatch[key] {
				// Same key earlier in this batch: that record is handled now,
				// and this one is only committed if it is.
				duplicates++
				continue
			}
			inBatch[key] = true
			token, claimed, existing, err := cfg.Store.Begin(ctx, key, cfg.Lease)
			switch {
			case err != nil:
				release(claims)
				return errors.Join(errors.New("kafka idempotency: begin"), err)
			case claimed:
				todo, claims, pos = append(todo, r), append(claims, claim{key, token}), append(pos, i)
			case existing.State == idempotency.Completed:
				duplicates++
			default:
				release(claims)
				return idempotency.ErrInProgress
			}
		}
		if duplicates > 0 {
			c.duplicates.Add(ctx, int64(duplicates), metric.WithAttributes(attribute.String("topic", rs[0].Topic)))
			c.o.logger.DebugContext(ctx, "kafka records skipped as duplicates", slog.Int("count", duplicates))
		}
		if len(todo) == 0 {
			return nil
		}

		done := false
		defer func() {
			if !done { // the handler panicked; the consumer recovers it
				release(claims)
			}
		}()
		err := h(ctx, todo)
		done = true
		if err == nil {
			c.complete(ctx, claims)
			return nil
		}
		be, ok := errors.AsType[*BatchError](err)
		if !ok || be.Processed <= 0 || be.Processed >= len(todo) {
			release(claims)
			return err
		}
		c.complete(ctx, claims[:be.Processed])
		release(claims[be.Processed:])
		// The consumer counts processed records in rs, which also holds the
		// duplicates skipped before the first unprocessed record.
		return &BatchError{Processed: pos[be.Processed], Err: be.Err}
	}
}

// complete records claims as processed. A failure is logged, not
// returned: the records were processed, and failing them now would process
// them again.
func (c *Consumer) complete(ctx context.Context, claims []claim) {
	cfg := c.o.idem
	for _, cl := range claims {
		if cl.token == "" {
			continue
		}
		if err := cfg.Store.Complete(context.WithoutCancel(ctx), cl.key, cl.token, nil, cfg.TTL); err != nil {
			c.o.logger.WarnContext(ctx, "kafka idempotency: complete key; the record may be processed again",
				"key", cl.key, logging.Err(err))
		}
	}
}
