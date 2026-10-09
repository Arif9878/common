package logging_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/Arif9878/common/go/observability/logging"
)

func logOnce(t *testing.T, cfg logging.Config) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger, err := logging.New(cfg, logging.WithWriter(&buf))
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestServiceIdentityFromOTelEnvironment(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "orders")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.version=1.4.2,deployment.environment.name=production")

	rec := logOnce(t, logging.Config{})
	if rec[logging.KeyService] != "orders" || rec[logging.KeyVersion] != "1.4.2" || rec[logging.KeyEnvironment] != "production" {
		t.Errorf("record = %v, want the identity from OTEL_*", rec)
	}

	// Explicit configuration overrides the variables, field by field.
	rec = logOnce(t, logging.Config{Service: "orders-worker", Version: "1.5.0"})
	if rec[logging.KeyService] != "orders-worker" || rec[logging.KeyVersion] != "1.5.0" || rec[logging.KeyEnvironment] != "production" {
		t.Errorf("record = %v, want explicit service and version, environment from OTEL_*", rec)
	}
}

func TestNoServiceIdentity(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	rec := logOnce(t, logging.Config{})
	for _, k := range []string{logging.KeyService, logging.KeyEnvironment, logging.KeyVersion} {
		if _, ok := rec[k]; ok {
			t.Errorf("%s present without any configuration: %v", k, rec)
		}
	}
}
