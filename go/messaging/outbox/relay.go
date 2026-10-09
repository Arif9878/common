package outbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/observability/logging"
)

// RelayOption configures [Outbox.NewRelay].
type RelayOption func(*relayOptions)

type relayOptions struct {
	batchSize    int
	pollInterval time.Duration
	retryDelay   time.Duration
	maxHold      time.Duration
	logger       *slog.Logger
	meterProv    metric.MeterProvider
}

// WithBatchSize bounds the records published per round. Default 500.
func WithBatchSize(n int) RelayOption { return func(o *relayOptions) { o.batchSize = n } }

// WithPollInterval sets how often the relay checks the table when no
// notification arrives, and how often a standby replica tries to become
// the relay. Default 1s.
func WithPollInterval(d time.Duration) RelayOption {
	return func(o *relayOptions) { o.pollInterval = d }
}

// WithRetryDelay sets the wait after a failed round (Kafka or PostgreSQL
// unavailable) before trying again. Default 1s.
func WithRetryDelay(d time.Duration) RelayOption { return func(o *relayOptions) { o.retryDelay = d } }

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) RelayOption { return func(o *relayOptions) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) RelayOption {
	return func(o *relayOptions) { o.meterProv = mp }
}

// Relay publishes outbox records to Kafka. Create it with
// [Outbox.NewRelay], start [Relay.Run] once, and stop it with
// [Relay.Stop].
type Relay struct {
	table    string
	db       *postgres.DB
	producer *kafka.Producer
	o        relayOptions

	stopOnce sync.Once
	stop     chan struct{}   // closed by Stop: finish the current round, then return
	work     context.Context // cancelled when Stop's deadline passes
	abort    context.CancelFunc
	done     chan struct{}

	published metric.Int64Counter
	lag       metric.Float64Histogram
	failures  metric.Int64Counter
	active    metric.Int64UpDownCounter
}

// NewRelay returns a relay publishing this outbox's records with producer.
// The producer's defaults (acks from all replicas, idempotent writes) keep
// the records of a partition in order.
func (o *Outbox) NewRelay(db *postgres.DB, producer *kafka.Producer, opts ...RelayOption) *Relay {
	ro := relayOptions{
		batchSize:    500,
		pollInterval: time.Second,
		retryDelay:   time.Second,
		maxHold:      time.Minute,
		logger:       slog.Default(),
		meterProv:    otel.GetMeterProvider(),
	}
	for _, opt := range opts {
		opt(&ro)
	}
	r := &Relay{table: o.table, db: db, producer: producer, o: ro,
		stop: make(chan struct{}), done: make(chan struct{})}
	r.work, r.abort = context.WithCancel(context.Background())

	meter := ro.meterProv.Meter("github.com/Arif9878/common/go/messaging/outbox")
	r.published, _ = meter.Int64Counter("outbox.records.published",
		metric.WithDescription("Outbox records published to Kafka and deleted, by topic."))
	r.lag, _ = meter.Float64Histogram("outbox.publish.lag", metric.WithUnit("s"),
		metric.WithDescription("Time from writing an outbox record to its publication."))
	r.failures, _ = meter.Int64Counter("outbox.relay.failures",
		metric.WithDescription("Relay rounds that failed (Kafka or PostgreSQL unavailable) and will be retried."))
	r.active, _ = meter.Int64UpDownCounter("outbox.relay.active",
		metric.WithDescription("1 while this process is the relay holding the outbox lock."))
	return r
}

// Run relays records until ctx ends or Stop is called, then returns nil.
// Failures (Kafka or PostgreSQL unavailable) are logged, counted in
// outbox.relay.failures and retried; they do not end Run. Its signature
// matches graceful.Manager.Go.
func (r *Relay) Run(ctx context.Context) error {
	defer close(r.done)
	stopAbort := context.AfterFunc(ctx, r.abort) // ctx ending cancels the round in flight
	defer stopAbort()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopCancel := context.AfterFunc(r.work, cancel)
	defer stopCancel()
	attrs := metric.WithAttributes(attribute.String("table", r.table))
	for {
		held, err := r.session(ctx, attrs)
		if err != nil && ctx.Err() == nil {
			r.failures.Add(ctx, 1, attrs)
			r.o.logger.WarnContext(ctx, "outbox relay: session failed; retrying", logging.Err(err))
		}
		wait := r.o.pollInterval
		if err != nil {
			wait = r.o.retryDelay
		} else if held {
			wait = 0 // gave up the lock on purpose (maxHold); take it again
		}
		if !r.sleep(ctx, wait) {
			return nil
		}
	}
}

// Stop makes Run finish its current round and return. If ctx ends first,
// the round is cancelled and Stop returns ctx's error without waiting
// further: a round blocked on an unreachable Kafka cluster can outlast any
// deadline, because the idempotent producer does not give up on records
// that may have been sent. Records of an unfinished round stay in the
// outbox and are published by the next relay. Its signature matches
// graceful.Hook.
func (r *Relay) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() { close(r.stop) })
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		r.abort()
		return ctx.Err()
	}
}

func (r *Relay) stopping() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

// sleep waits d, returning false if the relay should stop.
func (r *Relay) sleep(ctx context.Context, d time.Duration) bool {
	if r.stopping() || ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.stop:
		return false
	case <-ctx.Done():
		return false
	}
}

// session takes the relay lock on a dedicated connection and relays until
// stopped, an error, or maxHold. It reports whether it held the lock.
func (r *Relay) session(ctx context.Context, attrs metric.MeasurementOption) (bool, error) {
	conn, err := r.db.Acquire(ctx)
	if err != nil {
		return false, postgres.Classify(err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, "outbox:"+r.table).Scan(&locked); err != nil {
		return false, postgres.Classify(err)
	}
	if !locked {
		return false, nil // another replica is the relay
	}
	r.active.Add(ctx, 1, attrs)
	defer r.active.Add(context.WithoutCancel(ctx), -1, attrs)
	defer unlock(conn, r.table)
	r.o.logger.DebugContext(ctx, "outbox relay: acquired the lock", "table", r.table)

	if _, err := conn.Exec(ctx, `LISTEN `+pgx.Identifier{r.table}.Sanitize()); err != nil {
		return true, postgres.Classify(err)
	}
	until := time.Now().Add(r.o.maxHold)
	for time.Now().Before(until) {
		n, err := r.round()
		if err != nil {
			return true, err
		}
		if r.stopping() || ctx.Err() != nil {
			return true, nil
		}
		if n == r.o.batchSize {
			continue // more are waiting
		}
		if err := r.waitForWork(ctx, conn); err != nil {
			return true, err
		}
		if r.stopping() || ctx.Err() != nil {
			return true, nil
		}
	}
	return true, nil
}

func unlock(conn *pgxpool.Conn, table string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A broken connection is discarded by the pool, which also ends the
	// session and its lock.
	_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, "outbox:"+table)
	_, _ = conn.Exec(ctx, `UNLISTEN *`)
}

// waitForWork returns when a notification arrives, the poll interval
// passes, or the relay is stopped.
func (r *Relay) waitForWork(ctx context.Context, conn *pgxpool.Conn) error {
	wctx, cancel := context.WithTimeout(ctx, r.o.pollInterval)
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-wctx.Done():
		}
	}()
	_, err := conn.Conn().WaitForNotification(wctx)
	if err != nil && wctx.Err() != nil {
		return nil // timeout or stop
	}
	return postgres.Classify(err)
}

// round publishes up to batchSize records and deletes them, returning how
// many it published.
func (r *Relay) round() (int, error) {
	// The round runs on r.work, not ctx: Stop lets it finish while its
	// deadline allows.
	work := r.work
	if work.Err() != nil {
		return 0, nil
	}
	rows, err := r.db.Query(work, `SELECT id, topic, key, value, headers, created_at FROM `+r.table+
		` ORDER BY id LIMIT $1`, r.o.batchSize)
	if err != nil {
		return 0, postgres.Classify(err)
	}
	var (
		ids     []int64
		records []*kgo.Record
		created []time.Time
	)
	for rows.Next() {
		var (
			id  int64
			rec kgo.Record
			hs  []byte
			at  time.Time
		)
		if err := rows.Scan(&id, &rec.Topic, &rec.Key, &rec.Value, &hs, &at); err != nil {
			rows.Close()
			return 0, postgres.Classify(err)
		}
		var headers []header
		if err := json.Unmarshal(hs, &headers); err != nil {
			rows.Close()
			return 0, errors.Internal.Wrap(err, "outbox: decode headers")
		}
		for _, h := range headers {
			rec.Headers = append(rec.Headers, kgo.RecordHeader{Key: h.Key, Value: h.Value})
		}
		ids, records, created = append(ids, id), append(records, &rec), append(created, at)
	}
	if err := rows.Err(); err != nil {
		return 0, postgres.Classify(err)
	}
	if len(records) == 0 {
		return 0, nil
	}

	if err := r.producer.Publish(work, records...); err != nil {
		return 0, errors.Join(errors.New("outbox: publish"), err)
	}
	if _, err := r.db.Exec(work, `DELETE FROM `+r.table+` WHERE id = ANY($1)`, ids); err != nil {
		// Published but not deleted: the next round publishes them again.
		return 0, errors.Join(errors.New("outbox: delete published records"), postgres.Classify(err))
	}
	now := time.Now()
	for i, rec := range records {
		a := metric.WithAttributes(attribute.String("topic", rec.Topic))
		r.published.Add(work, 1, a)
		r.lag.Record(work, now.Sub(created[i]).Seconds(), a)
	}
	return len(records), nil
}
