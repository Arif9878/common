package grpc_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/grpc/grpcclient"
	"github.com/Arif9878/common/go/transport/grpc/grpcserver"
)

// TestWithTLS calls a TLS server whose certificate comes from a private CA:
// WithTLS with that CA succeeds, the default system roots don't.
func TestWithTLS(t *testing.T) {
	ca := httptest.NewTLSServer(nil) // only for its certificate, valid for example.com
	ca.Close()
	cert := ca.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := grpcserver.New(grpcserver.WithLogger(quiet), grpcserver.WithServerOptions(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}))))
	srv.RegisterService(&echoDesc, &echoServer{})
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	dialer := grpcclient.WithDialOptions(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}))

	call := func(opts ...grpcclient.Option) error {
		conn, err := grpcclient.New("passthrough:///example.com", append(opts, dialer)...)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out := new(wrapperspb.StringValue)
		if err := conn.Invoke(ctx, "/test.Echo/Echo", wrapperspb.String("hi"), out); err != nil {
			return err
		}
		if out.GetValue() != "hi" {
			t.Errorf("echo = %q", out.GetValue())
		}
		return nil
	}
	if err := call(grpcclient.WithTLS(&tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12})); err != nil {
		t.Errorf("with the private CA: %v", err)
	}
	if err := call(); err == nil {
		t.Error("the default TLS config trusted a private CA")
	}
}

// TestStreamOpenError checks that an error opening a stream is classified
// like a unary error.
func TestStreamOpenError(t *testing.T) {
	e := setup(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.conn.NewStream(ctx, &echoDesc.Streams[0], "/test.Echo/Count")
	if status.Code(err) != codes.Canceled || errors.KindOf(err) != errors.Canceled {
		t.Errorf("NewStream on a canceled context: %v (code %s, kind %s)", err, status.Code(err), errors.KindOf(err))
	}
}

func TestRetryPolicyValidation(t *testing.T) {
	for _, p := range []grpcclient.RetryPolicy{
		{MaxAttempts: 1},
		{MaxAttempts: 6},
	} {
		if _, err := grpcclient.New("passthrough:///x", grpcclient.WithInsecure(), grpcclient.WithRetry(p)); errors.KindOf(err) != errors.InvalidArgument {
			t.Errorf("RetryPolicy %+v: err = %v, want InvalidArgument", p, err)
		}
	}
	conn, err := grpcclient.New("passthrough:///x", grpcclient.WithInsecure(),
		grpcclient.WithRetry(grpcclient.RetryPolicy{Codes: []codes.Code{codes.Unavailable, codes.ResourceExhausted}}))
	if err != nil {
		t.Fatalf("custom retry codes: %v", err)
	}
	_ = conn.Close()
}
