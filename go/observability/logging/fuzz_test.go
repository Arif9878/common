package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Arif9878/common/go/observability/logging"
)

// FuzzRedaction logs a value under a sensitive key (with any prefix and
// suffix, at the top level and in a group) and checks it never appears in
// the output.
func FuzzRedaction(f *testing.F) {
	f.Add("db_", "Password", "", "hunter2-hunter2")
	f.Add("", "api-key", "_v2", "sk_live_0123456789")
	f.Add("x.", "Authorization", "", "Bearer abc.def.ghi")
	f.Fuzz(func(t *testing.T, prefix, sensitive, suffix, value string) {
		switch strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(sensitive)) {
		case "password", "secret", "token", "apikey", "authorization", "cookie", "privatekey", "credential":
		default:
			return
		}
		if len(value) < 8 {
			return // too short to be distinctive
		}
		var buf bytes.Buffer
		logger, err := logging.New(logging.Config{}, logging.WithWriter(&buf))
		if err != nil {
			t.Fatal(err)
		}
		key := prefix + sensitive + suffix
		logger.Info("event", key, value, slog.Group("nested", key, value))
		logger.With(key, value).Info("with")
		// Decode the records and look at values only: keys are logged
		// as they are and may contain anything.
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("invalid JSON %q: %v", line, err)
			}
			if leaked(rec, value) {
				t.Fatalf("value logged under key %q:\n%s", key, line)
			}
		}
	})
}

func leaked(v any, value string) bool {
	switch v := v.(type) {
	case string:
		return strings.Contains(v, value)
	case map[string]any:
		for _, e := range v {
			if leaked(e, value) {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if leaked(e, value) {
				return true
			}
		}
	}
	return false
}
