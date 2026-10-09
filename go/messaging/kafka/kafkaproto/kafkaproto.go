// Package kafkaproto serializes Protobuf messages for Kafka in the
// Confluent wire format, with schemas in a Confluent-compatible Schema
// Registry (Confluent, Redpanda, Apicurio in compatibility mode).
//
// A [Serde] implements kafka.Serializer and kafka.Deserializer, so it plugs
// into kafka.Encode and kafka.Typed:
//
//	//go:embed orders.proto
//	var ordersProto string
//
//	rc, err := kafkaproto.NewRegistry(cfg.SchemaRegistry)
//	...
//	serde, err := kafkaproto.New[*orderspb.OrderCreated](ctx, rc, kafkaproto.ValueSubject("orders.created"),
//		kafkaproto.WithSchema(ordersProto)) // producer: register (or find) the schema
//	...
//	r, err := kafka.Encode("orders.created", key, event, serde)
//
//	// consumer:
//	serde, err := kafkaproto.New[*orderspb.OrderCreated](ctx, rc, kafkaproto.ValueSubject("orders.created"))
//	consumer, err := kafka.NewConsumer(ctx, cfg.Kafka, group, topics, kafka.Typed(serde, handle))
//
// # Wire format
//
// Each value is a 0 byte, the schema ID (big-endian uint32), the message
// index (the message's position in its .proto file, as zig-zag varints),
// then the Protobuf encoding. This is what Confluent's serializers produce,
// so services in other languages can read and write the same topics.
//
// # Schemas
//
// With [WithSchema], New registers the .proto source under the subject; if
// the same schema is already registered, the registry returns its existing
// ID, and if it is incompatible with the subject's compatibility rule, New
// fails. Without it, New uses the subject's latest registered schema, for
// deployments where only CI registers schemas.
//
// Deserialize accepts any schema ID: Protobuf decoding does not need the
// writer's schema, and compatible schema versions decode into the same Go
// type. It rejects values whose message index names another message type,
// and values not in the wire format. Decoding errors have kind
// InvalidArgument, so the Kafka consumer does not retry them.
package kafkaproto

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/twmb/franz-go/pkg/sr"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/errors"
)

// Config configures the Schema Registry client. Environment variable names
// are relative; the service chooses the prefix, for example
// SCHEMA_REGISTRY_.
type Config struct {
	URLs     []string      `env:"URLS,required" envSeparator:"," validate:"min=1"`
	Username string        `env:"USERNAME"`
	Password config.Secret `env:"PASSWORD"`
	// Timeout bounds each registry request.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"10s"`
}

// NewRegistry returns a Schema Registry client for cfg. Requests are traced
// with the global tracer provider.
func NewRegistry(cfg Config) (*sr.Client, error) {
	if len(cfg.URLs) == 0 {
		return nil, errors.InvalidArgument.New("kafkaproto: schema registry needs at least one URL")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	opts := []sr.ClientOpt{
		sr.URLs(cfg.URLs...),
		sr.HTTPClient(&http.Client{Timeout: timeout, Transport: otelhttp.NewTransport(http.DefaultTransport)}),
	}
	if cfg.Username != "" {
		opts = append(opts, sr.BasicAuth(cfg.Username, cfg.Password.Reveal()))
	}
	rc, err := sr.NewClient(opts...)
	if err != nil {
		return nil, errors.InvalidArgument.Wrap(err, "kafkaproto: schema registry")
	}
	return rc, nil
}

// ValueSubject returns the subject of topic's record values under the
// default TopicNameStrategy: "<topic>-value".
func ValueSubject(topic string) string { return topic + "-value" }

// KeySubject returns the subject of topic's record keys: "<topic>-key".
func KeySubject(topic string) string { return topic + "-key" }

// Option configures [New].
type Option func(*options)

type options struct {
	schema *sr.Schema
}

// WithSchema registers schema, the .proto source declaring T, under the
// subject (or finds it if already registered). refs name the subjects of
// imported .proto files.
func WithSchema(schema string, refs ...sr.SchemaReference) Option {
	return func(o *options) {
		o.schema = &sr.Schema{Schema: schema, Type: sr.TypeProtobuf, References: refs}
	}
}

// Serde encodes and decodes messages of type T, a generated Protobuf
// message pointer such as *orderspb.OrderCreated. It is safe for
// concurrent use.
type Serde[T proto.Message] struct {
	id     int
	index  []int
	newT   func() T
	header sr.ConfluentHeader
}

// New returns a Serde for T under subject, registering the schema first
// with [WithSchema] or using the subject's latest schema otherwise.
func New[T proto.Message](ctx context.Context, rc *sr.Client, subject string, opts ...Option) (*Serde[T], error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	var zero T
	mt := zero.ProtoReflect().Type()
	s := &Serde[T]{
		index: messageIndex(mt.Descriptor()),
		newT:  func() T { return mt.New().Interface().(T) },
	}

	var err error
	if o.schema != nil {
		s.id, err = rc.RegisterSchema(ctx, subject, *o.schema, -1, -1)
	} else {
		var ss sr.SubjectSchema
		ss, err = rc.SchemaByVersion(ctx, subject, -1)
		if err == nil && ss.Type != sr.TypeProtobuf {
			err = errors.InvalidArgument.Errorf("kafkaproto: subject %s holds a %s schema, not Protobuf", subject, ss.Type)
		}
		s.id = ss.ID
	}
	if err != nil {
		return nil, errors.Join(errors.New("kafkaproto: schema for subject "+subject), classify(err))
	}
	return s, nil
}

// ID returns the schema ID that Serialize writes.
func (s *Serde[T]) ID() int { return s.id }

// Serialize encodes v in the Confluent wire format.
func (s *Serde[T]) Serialize(v T) ([]byte, error) {
	b, err := s.header.AppendEncode(nil, s.id, s.index)
	if err != nil {
		return nil, errors.InvalidArgument.Wrap(err, "kafkaproto: encode header")
	}
	b, err = proto.MarshalOptions{}.MarshalAppend(b, v)
	if err != nil {
		return nil, errors.InvalidArgument.Wrap(err, "kafkaproto: marshal")
	}
	return b, nil
}

// Deserialize decodes a value in the Confluent wire format into a new T.
func (s *Serde[T]) Deserialize(b []byte) (T, error) {
	var zero T
	_, rest, err := s.header.DecodeID(b)
	if err != nil {
		return zero, errors.InvalidArgument.Wrap(err, "kafkaproto: not in the Confluent wire format")
	}
	index, rest, err := s.header.DecodeIndex(rest, 64)
	if err != nil {
		return zero, errors.InvalidArgument.Wrap(err, "kafkaproto: message index")
	}
	if !slices.Equal(index, s.index) {
		return zero, errors.InvalidArgument.Errorf("kafkaproto: value is message %v of its schema, want %v (%T)", index, s.index, zero)
	}
	v := s.newT()
	if err := proto.Unmarshal(rest, v); err != nil {
		return zero, errors.InvalidArgument.Wrap(err, "kafkaproto: unmarshal")
	}
	return v, nil
}

// messageIndex returns the path to md in its file: its position among the
// file's top-level messages, then among each enclosing message's nested
// messages.
func messageIndex(md protoreflect.MessageDescriptor) []int {
	var path []int
	for d := protoreflect.Descriptor(md); ; d = d.Parent() {
		if _, ok := d.(protoreflect.FileDescriptor); ok {
			break
		}
		path = append(path, d.Index())
	}
	slices.Reverse(path)
	return path
}

// classify gives registry errors a kind: client errors (an unknown subject,
// an incompatible or invalid schema) are InvalidArgument, the rest
// Unavailable.
func classify(err error) error {
	if errors.KindOf(err) != errors.Unknown {
		return err
	}
	if re, ok := errors.AsType[*sr.ResponseError](err); ok && re.StatusCode >= 400 && re.StatusCode < 500 {
		return errors.InvalidArgument.Wrap(err, "schema registry")
	}
	return errors.Unavailable.Wrap(err, "schema registry")
}
