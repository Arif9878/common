// Package otlptest is an in-process OTLP/HTTP receiver for tests: it
// records the requests exporters send, decoded.
package otlptest

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Request is one export received.
type Request struct {
	Path    string
	Header  http.Header
	Gzipped bool
	Traces  *coltracepb.ExportTraceServiceRequest
	Metrics *colmetricspb.ExportMetricsServiceRequest
	Logs    *collogspb.ExportLogsServiceRequest
}

// Receiver records OTLP/HTTP exports.
type Receiver struct {
	URL string // base URL, such as http://127.0.0.1:1234

	mu   sync.Mutex
	reqs []Request
}

// New starts a receiver that stops when the test ends.
func New(t testing.TB) *Receiver {
	t.Helper()
	rc := &Receiver{}
	srv := httptest.NewServer(http.HandlerFunc(rc.serve))
	t.Cleanup(srv.Close)
	rc.URL = srv.URL
	return rc
}

func (rc *Receiver) serve(w http.ResponseWriter, r *http.Request) {
	body := io.Reader(r.Body)
	gz := r.Header.Get("Content-Encoding") == "gzip"
	if gz {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body = zr
	}
	b, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req := Request{Path: r.URL.Path, Header: r.Header.Clone(), Gzipped: gz}
	var msg proto.Message
	switch r.URL.Path {
	case "/v1/traces":
		req.Traces = &coltracepb.ExportTraceServiceRequest{}
		msg = req.Traces
	case "/v1/metrics":
		req.Metrics = &colmetricspb.ExportMetricsServiceRequest{}
		msg = req.Metrics
	case "/v1/logs":
		req.Logs = &collogspb.ExportLogsServiceRequest{}
		msg = req.Logs
	default:
		http.NotFound(w, r)
		return
	}
	if err := proto.Unmarshal(b, msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rc.mu.Lock()
	rc.reqs = append(rc.reqs, req)
	rc.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

// Requests returns the exports received on path ("/v1/traces", …).
func (rc *Receiver) Requests(path string) []Request {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []Request
	for _, r := range rc.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}
