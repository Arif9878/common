package grpcstatus_test

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/grpc/grpcstatus"
)

// A server returns classified errors; grpcserver calls ToStatus, so the
// client receives the matching code and only the public message.
func ExampleToStatus() {
	err := errors.WithPublicMessage(errors.NotFound.New("orders: row o-1 missing in shard 3"), "order not found")
	st, _ := status.FromError(grpcstatus.ToStatus(err))
	fmt.Println(st.Code(), st.Message())
	// Output: NotFound order not found
}

// A client turns a status back into a classified error, so errors.KindOf,
// retries and circuit breakers work across services. grpcclient does this
// for every call.
func ExampleFromStatus() {
	err := grpcstatus.FromStatus(status.Error(codes.Unavailable, "inventory is restarting"))
	fmt.Println(errors.KindOf(err), errors.IsRetryable(err), status.Code(err))
	// Output: unavailable true Unavailable
}
