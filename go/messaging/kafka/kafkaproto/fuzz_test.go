package kafkaproto_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/messaging/kafka/kafkaproto"
)

// FuzzDeserialize feeds arbitrary record values to a consumer's Serde: it
// must not panic, every failure must be InvalidArgument (so the consumer
// dead-letters instead of retrying), and an accepted value must round-trip.
func FuzzDeserialize(f *testing.F) {
	_, rc := newFakeRegistry(f)
	serde, err := kafkaproto.New[*wrapperspb.StringValue](context.Background(), rc, "f-value", kafkaproto.WithSchema(wrappersProto))
	if err != nil {
		f.Fatal(err)
	}
	valid, _ := serde.Serialize(wrapperspb.String("hello"))
	f.Add(valid)
	f.Add([]byte{0, 0, 0, 0, 1, 0, 10, 1, 'x'})
	f.Add([]byte{0, 0, 0, 0, 1, 254, 255, 255, 255, 15})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := serde.Deserialize(b)
		if err != nil {
			if errors.KindOf(err) != errors.InvalidArgument {
				t.Fatalf("error of kind %v: %v", errors.KindOf(err), err)
			}
			return
		}
		again, err := serde.Serialize(v)
		if err != nil {
			t.Fatal(err)
		}
		w, err := serde.Deserialize(again)
		if err != nil || !proto.Equal(v, w) {
			t.Fatalf("round trip: %v %v", w, err)
		}
	})
}
