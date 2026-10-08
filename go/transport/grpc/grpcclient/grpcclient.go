// Package grpcclient creates gRPC client connections with the standard
// interceptors: tracing and metrics, a default deadline, request-ID
// propagation, error classification and optional retries.
//
//	conn, err := grpcclient.New("dns:///orders.internal:9090",
//		grpcclient.WithRetry(grpcclient.RetryPolicy{MaxAttempts: 3}))
//	if err != nil {
//		return err
//	}
//	shutdown.Register(graceful.CloseDeps, "orders-grpc", func(context.Context) error { return conn.Close() })
//	orders := orderspb.NewOrdersClient(conn)
//
// New returns the native *grpc.ClientConn (created with grpc.NewClient,
// which connects lazily); generated clients and every grpc.DialOption
// (through [WithDialOptions]) work as usual.
//
// # Transport security
//
// TLS with the system roots is the default. Use [WithTLS] for a private CA
// or mTLS, and [WithInsecure] only for plaintext inside a trusted network
// or for tests; it must be chosen explicitly.
//
// # Every call
//
//   - A span and rpc.client.call.duration metrics (otelgrpc), with W3C
//     trace context sent in metadata.
//   - A deadline of WithTimeout (10s by default) when the context has none,
//     so no call can hang forever.
//   - The request ID from the context sent as x-request-id metadata.
//   - Errors classified by status code with grpcstatus.FromStatus, so
//     errors.KindOf, errors.IsRetryable, the retry package and circuit
//     breakers understand them. status.Code(err) still works.
//
// # Retries
//
// Off unless [WithRetry] is given; then gRPC's own retry implementation is
// configured through the service config (with exponential backoff and
// jitter, and server pushback). Only enable it for methods that are safe
// to repeat: by default only UNAVAILABLE is retried, which usually means
// the request was not processed.
package grpcclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/requestid"
	"github.com/Arif9878/common/go/transport/grpc/grpcstatus"
)

// RetryPolicy configures gRPC's built-in retries. Zero fields use the
// defaults shown.
type RetryPolicy struct {
	// MaxAttempts includes the first attempt; gRPC caps it at 5. Default 3.
	MaxAttempts int
	// InitialBackoff and MaxBackoff bound the randomized delays. Defaults
	// 100ms and 2s.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// Codes are the retried status codes. Default: Unavailable only.
	Codes []codes.Code
}

func (p RetryPolicy) serviceConfig() (string, error) {
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 3
	}
	if p.MaxAttempts < 2 || p.MaxAttempts > 5 {
		return "", errors.InvalidArgument.New("grpcclient: RetryPolicy.MaxAttempts must be between 2 and 5")
	}
	if p.InitialBackoff == 0 {
		p.InitialBackoff = 100 * time.Millisecond
	}
	if p.MaxBackoff == 0 {
		p.MaxBackoff = 2 * time.Second
	}
	if len(p.Codes) == 0 {
		p.Codes = []codes.Code{codes.Unavailable}
	}
	retryable := make([]uint32, len(p.Codes)) // the service config accepts numeric codes
	for i, c := range p.Codes {
		retryable[i] = uint32(c)
	}
	sc := map[string]any{
		"methodConfig": []any{map[string]any{
			"name": []any{map[string]any{}}, // every method
			"retryPolicy": map[string]any{
				"maxAttempts":          p.MaxAttempts,
				"initialBackoff":       fmt.Sprintf("%.3fs", p.InitialBackoff.Seconds()),
				"maxBackoff":           fmt.Sprintf("%.3fs", p.MaxBackoff.Seconds()),
				"backoffMultiplier":    2,
				"retryableStatusCodes": retryable,
			},
		}},
	}
	b, err := json.Marshal(sc)
	return string(b), err
}

// Option configures [New].
type Option func(*options)

type options struct {
	creds       credentials.TransportCredentials
	timeout     time.Duration
	retry       *RetryPolicy
	meterProv   metric.MeterProvider
	tracerProv  trace.TracerProvider
	propagators propagation.TextMapPropagator
	dialOpts    []grpc.DialOption
}

// WithTLS uses TLS with cfg, for example with a private CA or a client
// certificate for mTLS.
func WithTLS(cfg *tls.Config) Option {
	return func(o *options) { o.creds = credentials.NewTLS(cfg) }
}

// WithInsecure disables transport security.
func WithInsecure() Option { return func(o *options) { o.creds = insecure.NewCredentials() } }

// WithTimeout sets the deadline applied to calls whose context has none.
// The default is 10 seconds; 0 disables it. Streams are not affected.
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// WithRetry enables gRPC's built-in retries with p.
func WithRetry(p RetryPolicy) Option { return func(o *options) { o.retry = &p } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option { return func(o *options) { o.meterProv = mp } }

// WithTracerProvider sets the tracer provider. The default is the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProv = tp }
}

// WithPropagators sets how trace context is written to metadata. The
// default is the global propagator.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(o *options) { o.propagators = p }
}

// WithDialOptions adds native grpc.DialOption values. They are applied
// after the package's options, so they can override them.
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(o *options) { o.dialOpts = append(o.dialOpts, opts...) }
}

// New creates a client connection to target (see grpc.NewClient for the
// syntax, such as "dns:///host:port"). It does not connect; the first call
// does.
func New(target string, opts ...Option) (*grpc.ClientConn, error) {
	o := options{
		creds:       credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12}),
		timeout:     10 * time.Second,
		meterProv:   otel.GetMeterProvider(),
		tracerProv:  otel.GetTracerProvider(),
		propagators: otel.GetTextMapPropagator(),
	}
	for _, opt := range opts {
		opt(&o)
	}

	dial := []grpc.DialOption{
		grpc.WithTransportCredentials(o.creds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(o.tracerProv),
			otelgrpc.WithMeterProvider(o.meterProv),
			otelgrpc.WithPropagators(o.propagators),
		)),
		grpc.WithChainUnaryInterceptor(unaryInterceptor(o.timeout)),
		grpc.WithChainStreamInterceptor(streamInterceptor),
	}
	if o.retry != nil {
		sc, err := o.retry.serviceConfig()
		if err != nil {
			return nil, err
		}
		dial = append(dial, grpc.WithDefaultServiceConfig(sc))
	}
	conn, err := grpc.NewClient(target, append(dial, o.dialOpts...)...)
	if err != nil {
		return nil, errors.InvalidArgument.Wrap(err, "grpcclient: "+target)
	}
	return conn, nil
}

func withRequestID(ctx context.Context) context.Context {
	if id, ok := requestid.FromContext(ctx); ok {
		if md, _ := metadata.FromOutgoingContext(ctx); len(md.Get("x-request-id")) == 0 {
			return metadata.AppendToOutgoingContext(ctx, "x-request-id", id)
		}
	}
	return ctx
}

func unaryInterceptor(timeout time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := ctx.Deadline(); !ok && timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		return grpcstatus.FromStatus(invoker(withRequestID(ctx), method, req, reply, cc, opts...))
	}
}

func streamInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	cs, err := streamer(withRequestID(ctx), desc, cc, method, opts...)
	if err != nil {
		return nil, grpcstatus.FromStatus(err)
	}
	return &classifiedStream{ClientStream: cs}, nil
}

// classifiedStream classifies errors from stream operations. io.EOF, the
// normal end of a stream, is passed through unchanged.
type classifiedStream struct{ grpc.ClientStream }

func (s *classifiedStream) RecvMsg(m any) error { return classifyStreamErr(s.ClientStream.RecvMsg(m)) }
func (s *classifiedStream) SendMsg(m any) error { return classifyStreamErr(s.ClientStream.SendMsg(m)) }

func classifyStreamErr(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	return grpcstatus.FromStatus(err)
}
