package grpcserver_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/grpc/grpcserver"
)

type userKey struct{}

func TestBearer(t *testing.T) {
	auth := grpcserver.Bearer(func(ctx context.Context, token string) (context.Context, error) {
		if token != "good" {
			return nil, errors.Unauthorized.New("bad token")
		}
		return context.WithValue(ctx, userKey{}, "user-7"), nil
	})
	call := func(values ...string) (context.Context, error) {
		ctx := context.Background()
		if len(values) > 0 {
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", values[0]))
		}
		return auth(ctx, "/orders.v1.Orders/Get")
	}

	ctx, err := call("Bearer good")
	if err != nil || ctx.Value(userKey{}) != "user-7" {
		t.Fatalf("valid token: %v", err)
	}
	if _, err := call("bearer  good "); err != nil {
		t.Errorf("lower-case scheme and spaces: %v", err)
	}
	for name, values := range map[string][]string{"none": nil, "basic": {"Basic Zm9v"}, "empty": {"Bearer "}, "bad": {"Bearer bad"}} {
		if _, err := call(values...); errors.KindOf(err) != errors.Unauthorized {
			t.Errorf("%s: err = %v, want unauthorized", name, err)
		}
	}
}
