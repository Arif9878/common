// Package secret defines a provider-neutral secret type and the interface
// business code depends on to obtain secrets.
//
//	type Deps struct {
//		Secrets secret.Provider // Vault in production, secret.Static in tests
//	}
//
//	s, err := deps.Secrets.Get(ctx, "database/creds/orders")
//	user, pass := s.Field("username"), s.Field("password")
//
// A [Secret] never reveals its values through fmt, JSON, text marshalling
// or slog: all of them produce "[REDACTED]". Read values with
// [Secret.Field] where they are used.
//
// Implementations live in subpackages (secret/vault). Errors use the
// errors package kinds: NotFound for a missing secret, Unauthorized or
// Forbidden for access problems, Unavailable or Timeout for transient
// failures, InvalidArgument for a malformed secret. Error messages never
// contain secret values.
package secret

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/Arif9878/common/go/errors"
)

// Provider retrieves secrets by key. Keys are provider-specific paths such
// as "kv/data/payments/api" or "database/creds/orders".
type Provider interface {
	Get(ctx context.Context, key string) (Secret, error)
}

// ProviderFunc adapts a function to [Provider].
type ProviderFunc func(ctx context.Context, key string) (Secret, error)

// Get calls f.
func (f ProviderFunc) Get(ctx context.Context, key string) (Secret, error) { return f(ctx, key) }

// ErrNotFound is returned by providers for unknown keys. Its kind is
// NotFound.
var ErrNotFound = errors.NotFound.New("secret not found")

const redacted = "[REDACTED]"

// Secret is an immutable set of named secret values with metadata. The
// zero value has no fields.
type Secret struct {
	fields map[string]string

	// Version identifies the secret's revision, if the provider has one.
	// Equal non-empty versions mean equal values.
	Version string
	// ExpiresAt is when the secret stops being valid (a lease or
	// certificate end), or zero if it does not expire.
	ExpiresAt time.Time
	// LeaseID identifies a renewable or revocable lease, if any.
	LeaseID string
}

// New returns a secret with a copy of fields.
func New(fields map[string]string) Secret {
	return Secret{fields: maps.Clone(fields)}
}

// Field returns the value of the named field, or "" if absent.
func (s Secret) Field(name string) string { return s.fields[name] }

// Lookup returns the value of the named field and whether it is present.
func (s Secret) Lookup(name string) (string, bool) {
	v, ok := s.fields[name]
	return v, ok
}

// Fields returns the field names, sorted. It never returns values.
func (s Secret) Fields() []string { return slices.Sorted(maps.Keys(s.fields)) }

// Require returns an error of kind InvalidArgument naming the fields of
// names that are missing or empty.
func (s Secret) Require(names ...string) error {
	var missing []string
	for _, n := range names {
		if s.fields[n] == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return errors.InvalidArgument.Errorf("secret: missing fields %v", missing)
	}
	return nil
}

// TTL returns the time left until ExpiresAt, or 0 if the secret does not
// expire. An expired secret returns a negative duration.
func (s Secret) TTL() time.Duration {
	if s.ExpiresAt.IsZero() {
		return 0
	}
	return time.Until(s.ExpiresAt)
}

// String returns a description without values.
func (s Secret) String() string {
	return fmt.Sprintf("secret(fields=%v, version=%q) %s", s.Fields(), s.Version, redacted)
}

// GoString returns the same as String.
func (s Secret) GoString() string { return s.String() }

// Format writes String for every verb, so no fmt verb prints values.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

// LogValue logs field names and metadata, never values.
func (s Secret) LogValue() slog.Value {
	attrs := []slog.Attr{slog.Any("fields", s.Fields())}
	if s.Version != "" {
		attrs = append(attrs, slog.String("version", s.Version))
	}
	if !s.ExpiresAt.IsZero() {
		attrs = append(attrs, slog.Time("expires_at", s.ExpiresAt))
	}
	return slog.GroupValue(attrs...)
}

// MarshalJSON returns "[REDACTED]".
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText returns "[REDACTED]".
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Static is a Provider serving fixed secrets, for tests and local
// development. Unknown keys return [ErrNotFound].
type Static map[string]Secret

// Get returns the secret stored under key.
func (s Static) Get(_ context.Context, key string) (Secret, error) {
	sec, ok := s[key]
	if !ok {
		return Secret{}, fmt.Errorf("secret %q: %w", key, ErrNotFound)
	}
	return sec, nil
}
