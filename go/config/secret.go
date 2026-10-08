package config

import (
	"fmt"
	"io"
	"log/slog"
	"reflect"
)

const redacted = "[REDACTED]"

// Secret is a configuration value that must not be logged or printed. It is
// parsed like a string. Formatting with any fmt verb, JSON or text
// marshalling, and logging with slog all produce "[REDACTED]".
type Secret string

// Reveal returns the secret value. Call it only where the value is used,
// such as when building a connection string.
func (s Secret) Reveal() string { return string(s) }

// String returns "[REDACTED]".
func (Secret) String() string { return redacted }

// GoString returns "[REDACTED]".
func (Secret) GoString() string { return redacted }

// Format writes "[REDACTED]" for every fmt verb. Without it, verbs that do
// not use String, such as %d, would print the underlying value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue returns "[REDACTED]".
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText returns "[REDACTED]".
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// MarshalJSON returns "[REDACTED]" as a JSON string.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// LogValue returns a slog value describing cfg, a struct or pointer to
// struct, for logging the effective configuration at startup. Structs
// become groups keyed by field name, so the logging package's key-based
// redaction also applies to fields such as Password that are not of type
// [Secret]. Secrets and other [slog.LogValuer] fields log their own value.
// Unexported fields are omitted.
func LogValue(cfg any) slog.Value {
	return logValue(reflect.ValueOf(cfg))
}

var (
	logValuerType   = reflect.TypeFor[slog.LogValuer]()
	stringerType    = reflect.TypeFor[interface{ String() string }]()
	textMarshalType = reflect.TypeFor[interface{ MarshalText() ([]byte, error) }]()
)

func logValue(v reflect.Value) slog.Value {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return slog.AnyValue(nil)
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return slog.AnyValue(nil)
	}

	t := v.Type()
	if v.Kind() != reflect.Struct ||
		t.Implements(logValuerType) || t.Implements(stringerType) || t.Implements(textMarshalType) {
		// Leaves, including structs that describe themselves (time.Time).
		return slog.AnyValue(v.Interface())
	}

	attrs := make([]slog.Attr, 0, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		attrs = append(attrs, slog.Attr{Key: f.Name, Value: logValue(v.Field(i))})
	}
	return slog.GroupValue(attrs...)
}
