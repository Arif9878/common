package kafkaproto_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/kafka/kafkaproto"
	"github.com/Arif9878/common/go/testkit"
)

// wrappersProto declares the messages of google/protobuf/wrappers.proto in
// the same order, so StringValue is message 7 like in the Go type. The
// package differs so a real registry does not see a well-known type.
const wrappersProto = `syntax = "proto3";
package commontest.v1;
message DoubleValue { double value = 1; }
message FloatValue { float value = 1; }
message Int64Value { int64 value = 1; }
message UInt64Value { uint64 value = 1; }
message Int32Value { int32 value = 1; }
message UInt32Value { uint32 value = 1; }
message BoolValue { bool value = 1; }
message StringValue { string value = 1; }
message BytesValue { bytes value = 1; }
`

// fakeRegistry implements the two Schema Registry endpoints the package
// uses: register a schema, and get a subject's latest schema.
type fakeRegistry struct {
	mu       sync.Mutex
	ids      map[string]int // schema text -> ID
	subjects map[string][]sr.SubjectSchema
}

func newFakeRegistry(t *testing.T) (*fakeRegistry, *sr.Client) {
	f := &fakeRegistry{ids: map[string]int{}, subjects: map[string][]sr.SubjectSchema{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	rc, err := kafkaproto.NewRegistry(kafkaproto.Config{URLs: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	return f, rc
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/vnd.schemaregistry.v1+json")
	rest, ok := strings.CutPrefix(r.URL.Path, "/subjects/")
	subject, tail, _ := strings.Cut(rest, "/versions")
	switch {
	case ok && r.Method == http.MethodPost && tail == "":
		var s sr.Schema
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, ok := f.ids[s.Schema]
		if !ok {
			id = len(f.ids) + 1
			f.ids[s.Schema] = id
			f.subjects[subject] = append(f.subjects[subject],
				sr.SubjectSchema{Subject: subject, Version: len(f.subjects[subject]) + 1, ID: id, Schema: s})
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"id": id})
	case ok && r.Method == http.MethodGet && tail == "/latest":
		versions := f.subjects[subject]
		if len(versions) == 0 {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error_code": 40401, "message": "Subject not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(versions[len(versions)-1])
	default:
		http.NotFound(w, r)
	}
}

func TestRoundTripAndWireFormat(t *testing.T) {
	_, rc := newFakeRegistry(t)
	ctx := context.Background()
	serde, err := kafkaproto.New[*wrapperspb.StringValue](ctx, rc, "orders-value", kafkaproto.WithSchema(wrappersProto))
	if err != nil {
		t.Fatal(err)
	}
	b, err := serde.Serialize(wrapperspb.String("hello"))
	if err != nil {
		t.Fatal(err)
	}
	// 0, schema ID 1 (big endian), index [7] (count 1, then 7, zig-zag), payload.
	payload, _ := proto.Marshal(wrapperspb.String("hello"))
	want := append([]byte{0, 0, 0, 0, 1, 2, 14}, payload...)
	if !slices.Equal(b, want) {
		t.Errorf("encoded % x, want % x", b, want)
	}
	v, err := serde.Deserialize(b)
	if err != nil || v.GetValue() != "hello" {
		t.Fatalf("decoded %v, %v", v, err)
	}
}

func TestConsumerUsesLatestSchema(t *testing.T) {
	_, rc := newFakeRegistry(t)
	ctx := context.Background()
	if _, err := kafkaproto.New[*wrapperspb.StringValue](ctx, rc, "orders-value", kafkaproto.WithSchema(wrappersProto)); err != nil {
		t.Fatal(err)
	}
	// A second registration of the same schema keeps its ID.
	again, err := kafkaproto.New[*wrapperspb.StringValue](ctx, rc, "orders-value", kafkaproto.WithSchema(wrappersProto))
	if err != nil || again.ID() != 1 {
		t.Fatalf("re-register: id %d, %v", again.ID(), err)
	}
	latest, err := kafkaproto.New[*wrapperspb.StringValue](ctx, rc, "orders-value")
	if err != nil || latest.ID() != 1 {
		t.Fatalf("latest: id %d, %v", latest.ID(), err)
	}

	_, err = kafkaproto.New[*wrapperspb.StringValue](ctx, rc, "missing-value")
	if errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("unknown subject: err = %v, want invalid_argument", err)
	}
}

func TestDeserializeRejects(t *testing.T) {
	_, rc := newFakeRegistry(t)
	ctx := context.Background()
	strs, err := kafkaproto.New[*wrapperspb.StringValue](ctx, rc, "s-value", kafkaproto.WithSchema(wrappersProto))
	if err != nil {
		t.Fatal(err)
	}
	ints, err := kafkaproto.New[*wrapperspb.Int64Value](ctx, rc, "s-value", kafkaproto.WithSchema(wrappersProto))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ints.Serialize(wrapperspb.Int64(42))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := proto.Marshal(wrapperspb.String("x"))
	for name, value := range map[string][]byte{
		"another message type": b,
		"plain protobuf":       payload,
		"empty":                nil,
		"bad payload":          {0, 0, 0, 0, 1, 2, 14, 0xff, 0xff},
	} {
		if _, err := strs.Deserialize(value); errors.KindOf(err) != errors.InvalidArgument {
			t.Errorf("%s: err = %v, want invalid_argument", name, err)
		}
	}
}

func TestNestedMessageIndex(t *testing.T) {
	_, rc := newFakeRegistry(t)
	// DescriptorProto is message 2 of descriptor.proto; ExtensionRange is
	// its first nested message.
	serde, err := kafkaproto.New[*descriptorpb.DescriptorProto_ExtensionRange](context.Background(), rc, "n-value",
		kafkaproto.WithSchema("syntax = \"proto2\";"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := serde.Serialize(&descriptorpb.DescriptorProto_ExtensionRange{Start: proto.Int32(1)})
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0, 0, 0, 0, 1, 4, 4, 0}; !slices.Equal(b[:8], want) {
		t.Errorf("header % x, want % x (index [2 0])", b[:8], want)
	}
}

func TestNewRegistryConfig(t *testing.T) {
	if _, err := kafkaproto.NewRegistry(kafkaproto.Config{}); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("no URL: err = %v", err)
	}
	if got := kafkaproto.ValueSubject("orders"); got != "orders-value" {
		t.Errorf("ValueSubject = %q", got)
	}
	if got := kafkaproto.KeySubject("orders"); got != "orders-key" {
		t.Errorf("KeySubject = %q", got)
	}
}

// TestThroughKafka publishes and consumes Protobuf records, against the
// registry in SCHEMA_REGISTRY_TEST_URL if set (its subject is deleted at
// the end) and an in-process Kafka.
func TestThroughKafka(t *testing.T) {
	ctx := context.Background()
	subject := "commontest-kafkaproto-" + strings.ToLower(rand.Text()[:6]) + "-value"
	var rc *sr.Client
	if url := os.Getenv("SCHEMA_REGISTRY_TEST_URL"); url != "" {
		var err error
		rc, err = kafkaproto.NewRegistry(kafkaproto.Config{URLs: []string{url}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = rc.DeleteSubject(context.Background(), subject, sr.SoftDelete)
			_, _ = rc.DeleteSubject(context.Background(), subject, sr.HardDelete)
		})
	} else {
		_, rc = newFakeRegistry(t)
	}
	producerSerde, err := kafkaproto.New[*wrapperspb.StringValue](ctx, rc, subject, kafkaproto.WithSchema(wrappersProto))
	if err != nil {
		t.Fatal(err)
	}
	consumerSerde, err := kafkaproto.New[*wrapperspb.StringValue](ctx, rc, subject)
	if err != nil {
		t.Fatal(err)
	}
	if consumerSerde.ID() != producerSerde.ID() {
		t.Errorf("latest schema ID %d, registered %d", consumerSerde.ID(), producerSerde.ID())
	}

	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "orders"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	cfg := kafka.Config{Brokers: c.ListenAddrs()}
	logger, _ := testkit.NewLogger(t)
	p, err := kafka.NewProducer(ctx, cfg, kafka.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	for _, v := range []string{"a", "b", "c"} {
		r, err := kafka.Encode("orders", nil, wrapperspb.String(v), producerSerde)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Publish(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var got []string
	consumer, err := kafka.NewConsumer(ctx, cfg, "g", []string{"orders"},
		kafka.Typed(consumerSerde, func(_ context.Context, _ *kgo.Record, v *wrapperspb.StringValue) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, v.GetValue())
			return nil
		}), kafka.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- consumer.Run(ctx) }()
	t.Cleanup(func() { _ = consumer.Close(context.Background()); <-done })
	testkit.Eventually(t, 30*time.Second, "3 records", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 3
	})
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("consumed %v", got)
	}
}
