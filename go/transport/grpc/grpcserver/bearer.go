package grpcserver

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/Arif9878/common/go/errors"
)

var errNoBearer = errors.WithPublicMessage(errors.Unauthorized.New("grpcserver: no bearer token"), "missing bearer token")

// Bearer returns an Authenticator that reads the "authorization" metadata,
// "Bearer <token>", and passes the token to verify, which returns the
// context with the caller's identity. With jwtauth:
//
//	grpcserver.WithAuth(grpcserver.Bearer(verifier.Authenticate))
//
// A call without a bearer token fails with Unauthorized
// (codes.Unauthenticated).
func Bearer(verify func(ctx context.Context, token string) (context.Context, error)) Authenticator {
	return func(ctx context.Context, _ string) (context.Context, error) {
		for _, v := range metadata.ValueFromIncomingContext(ctx, "authorization") {
			scheme, token, ok := strings.Cut(strings.TrimSpace(v), " ")
			if token = strings.TrimSpace(token); ok && strings.EqualFold(scheme, "Bearer") && token != "" {
				return verify(ctx, token)
			}
		}
		return nil, errNoBearer
	}
}
