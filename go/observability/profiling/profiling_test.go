package profiling_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Arif9878/common/go/observability/profiling"
)

func TestRegister(t *testing.T) {
	mux := http.NewServeMux()
	profiling.Register(mux)
	for path, want := range map[string]string{
		"/debug/pprof/":                  "goroutine",
		"/debug/pprof/heap?debug=1":      "heap profile",
		"/debug/pprof/goroutine?debug=1": "goroutine profile",
		"/debug/pprof/cmdline":           "profiling.test",
		"/debug/pprof/profile?seconds=1": "", // binary profile
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d, body without %q", path, rec.Code, want)
		}
	}
}
