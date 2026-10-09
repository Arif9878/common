package otlp_test

import (
	"strings"
	"testing"

	"github.com/Arif9878/common/go/observability/otlp"
)

// FuzzParseHeaders checks that parsing never panics and that an error
// never contains a header value, since values are API keys.
func FuzzParseHeaders(f *testing.F) {
	f.Add("api-key", "s3cr3t-value", ",")
	f.Add("Authorization", "Basic%20dXNlcjpwdw==", ";")
	f.Add("", "%zzvalue", "=")
	f.Fuzz(func(t *testing.T, key, value, extra string) {
		input := key + "=" + value + extra
		h, err := otlp.ParseHeaders(input)
		if err == nil {
			for k := range h {
				if strings.TrimSpace(k) == "" {
					t.Fatalf("empty header name from %q", input)
				}
			}
			return
		}
		if len(value) >= 8 && !strings.Contains(key, value) && strings.Contains(err.Error(), value) {
			t.Fatalf("error %q reveals the value %q", err, value)
		}
	})
}
