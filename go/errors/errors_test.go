package errors_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/Arif9878/common/go/errors"
)

type timeoutErr struct{ timeout bool }

func (e timeoutErr) Error() string { return "net op" }
func (e timeoutErr) Timeout() bool { return e.timeout }

type typedErr struct{ code int }

func (e *typedErr) Error() string { return fmt.Sprintf("typed %d", e.code) }

func TestKindOf(t *testing.T) {
	base := stderrors.New("boom")

	tests := []struct {
		name string
		err  error
		want errors.Kind
	}{
		{"nil", nil, errors.Unknown},
		{"plain", base, errors.Unknown},
		{"new", errors.NotFound.New("user"), errors.NotFound},
		{"wrap", errors.Unavailable.Wrap(base, "query"), errors.Unavailable},
		{"errorf", errors.InvalidArgument.Errorf("bad id %q", "x"), errors.InvalidArgument},
		{"fmt wrapped", fmt.Errorf("handler: %w", errors.Conflict.New("dup")), errors.Conflict},
		{"outermost wins", errors.Internal.Wrap(errors.NotFound.New("row"), "invariant"), errors.Internal},
		{"through public message", errors.WithPublicMessage(errors.Forbidden.New("acl"), "no"), errors.Forbidden},
		{"joined first classified", errors.Join(base, errors.RateLimited.New("quota"), errors.Internal.New("x")), errors.RateLimited},
		{"context canceled", context.Canceled, errors.Canceled},
		{"context deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), errors.Timeout},
		{"os deadline", os.ErrDeadlineExceeded, errors.Timeout},
		{"net timeout", &net.OpError{Op: "dial", Err: timeoutErr{timeout: true}}, errors.Timeout},
		{"net non-timeout", &net.OpError{Op: "dial", Err: timeoutErr{timeout: false}}, errors.Unknown},
		{"classified beats context", errors.Unavailable.Wrap(context.DeadlineExceeded, ""), errors.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errors.KindOf(tt.err); got != tt.want {
				t.Fatalf("KindOf = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWrapPreservesOriginal(t *testing.T) {
	sentinel := stderrors.New("sentinel")
	typed := &typedErr{code: 7}

	err := fmt.Errorf("outer: %w",
		errors.Unavailable.Wrap(errors.Join(sentinel, typed), "fetch"))

	if !errors.Is(err, sentinel) {
		t.Error("Is(sentinel) = false")
	}
	if got, ok := errors.AsType[*typedErr](err); !ok || got.code != 7 {
		t.Errorf("AsType = %v, %v", got, ok)
	}
	var target *typedErr
	if !errors.As(err, &target) || target != typed {
		t.Error("As did not find typed error")
	}

	errorf := errors.InvalidArgument.Errorf("parse: %w", sentinel)
	if !errors.Is(errorf, sentinel) {
		t.Error("Errorf with %w: Is(sentinel) = false")
	}
}

func TestNilHandling(t *testing.T) {
	// A typed nil returned as error would compare non-nil; these must return
	// an untyped nil.
	if err := errors.Internal.Wrap(nil, "x"); err != nil {
		t.Errorf("Wrap(nil) = %#v", err)
	}
	if err := errors.WithPublicMessage(nil, "x"); err != nil {
		t.Errorf("WithPublicMessage(nil) = %#v", err)
	}
	if got := errors.PublicMessage(nil); got != "" {
		t.Errorf("PublicMessage(nil) = %q", got)
	}
	if errors.IsRetryable(nil) {
		t.Error("IsRetryable(nil) = true")
	}
}

func TestErrorText(t *testing.T) {
	base := stderrors.New("dial tcp 10.0.0.1:5432: refused")

	tests := []struct {
		err  error
		want string
	}{
		{errors.NotFound.New("user not found"), "user not found"},
		{errors.NotFound.New(""), "not_found"},
		{errors.Unavailable.Wrap(base, "query user"), "query user: dial tcp 10.0.0.1:5432: refused"},
		{errors.Unavailable.Wrap(base, ""), "dial tcp 10.0.0.1:5432: refused"},
		{errors.InvalidArgument.Errorf("bad id %d", 3), "bad id 3"},
		{errors.WithPublicMessage(base, "try later"), base.Error()},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
}

func TestPublicMessage(t *testing.T) {
	secret := stderrors.New(`pq: password authentication failed for user "svc"`)

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"unclassified", secret, "internal error"},
		{"kind default", errors.Unavailable.Wrap(secret, "connect"), "service unavailable"},
		{"explicit", errors.WithPublicMessage(errors.NotFound.New("row 42 missing"), "order not found"), "order not found"},
		{"outermost explicit wins", errors.WithPublicMessage(errors.WithPublicMessage(secret, "inner"), "outer"), "outer"},
		{"explicit survives wrapping", fmt.Errorf("h: %w", errors.WithPublicMessage(secret, "bad input")), "bad input"},
		{"canceled", context.Canceled, "canceled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := errors.PublicMessage(tt.err)
			if got != tt.want {
				t.Fatalf("PublicMessage = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, "password") || strings.Contains(got, "42") {
				t.Fatalf("PublicMessage leaked internal text: %q", got)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	retryable := map[errors.Kind]bool{
		errors.Timeout:     true,
		errors.Unavailable: true,
		errors.RateLimited: true,
	}
	for k := errors.Unknown; k <= errors.Internal; k++ {
		if got := errors.IsRetryable(k.New("x")); got != retryable[k] {
			t.Errorf("IsRetryable(%v) = %v, want %v", k, got, retryable[k])
		}
	}
	if errors.IsRetryable(context.Canceled) {
		t.Error("canceled must not be retryable")
	}
	if !errors.IsRetryable(context.DeadlineExceeded) {
		t.Error("deadline exceeded should be retryable")
	}
}

func TestKindString(t *testing.T) {
	seen := map[string]errors.Kind{}
	for k := errors.Unknown; k <= errors.Internal; k++ {
		s := k.String()
		if s == "" || strings.ContainsAny(s, " -ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			t.Errorf("Kind(%d).String() = %q, want snake_case", k, s)
		}
		if prev, dup := seen[s]; dup {
			t.Errorf("Kind(%d) and Kind(%d) share name %q", prev, k, s)
		}
		seen[s] = k
	}
	if got := errors.Kind(255).String(); got != "unknown" {
		t.Errorf("out of range String() = %q", got)
	}
	if got := errors.PublicMessage(errors.Kind(255).New("x")); got != "internal error" {
		t.Errorf("out of range PublicMessage = %q", got)
	}
}

func BenchmarkKindOf(b *testing.B) {
	classified := fmt.Errorf("handler: %w", errors.NotFound.Wrap(stderrors.New("row"), "load"))
	unclassified := fmt.Errorf("handler: %w", fmt.Errorf("svc: %w", stderrors.New("x")))

	b.Run("classified", func(b *testing.B) {
		for b.Loop() {
			errors.KindOf(classified)
		}
	})
	b.Run("unclassified", func(b *testing.B) {
		for b.Loop() {
			errors.KindOf(unclassified)
		}
	})
}
