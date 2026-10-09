package otlp_test

import (
	"strings"
	"testing"

	"github.com/Arif9878/common/go/observability/otlp"
)

func TestParseHeaders(t *testing.T) {
	h, err := otlp.ParseHeaders(" api-key = abc , Authorization=Basic%20dXNlcjpwdw== ")
	if err != nil {
		t.Fatal(err)
	}
	if h["api-key"] != "abc" || h["Authorization"] != "Basic dXNlcjpwdw==" || len(h) != 2 {
		t.Errorf("headers = %v", h)
	}
	if h, err := otlp.ParseHeaders(""); err != nil || h != nil {
		t.Errorf("empty: %v %v", h, err)
	}
	for _, bad := range []string{"novalue", "=v", "k=%zz-secret"} {
		_, err := otlp.ParseHeaders(bad)
		if err == nil {
			t.Errorf("%q: no error", bad)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: error reveals the value: %v", bad, err)
		}
	}
}

func TestValidateAndProtocol(t *testing.T) {
	for _, c := range []otlp.Config{{Protocol: "udp"}, {Compression: "zstd"}, {Headers: "x"}, {Timeout: -1}} {
		if c.Validate() == nil {
			t.Errorf("%+v: valid", c)
		}
	}
	for _, tc := range []struct{ cfg, env, want string }{
		{"", "", otlp.ProtocolGRPC},
		{"", "http/protobuf", otlp.ProtocolHTTP},
		{"grpc", "http/protobuf", otlp.ProtocolGRPC},
		{"HTTP", "", otlp.ProtocolHTTP},
	} {
		if got, err := (otlp.Config{Protocol: tc.cfg}).ResolvedProtocol(tc.env); err != nil || got != tc.want {
			t.Errorf("protocol %q env %q = %q, %v; want %q", tc.cfg, tc.env, got, err, tc.want)
		}
	}
}
