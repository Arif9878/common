package otelres

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

func TestNew(t *testing.T) {
	tests := map[string]struct {
		env                           map[string]string
		service, environment, version string
		want                          map[attribute.Key]string
	}{
		"arguments": {
			service: "orders", environment: "prod", version: "1.2.3",
			want: map[attribute.Key]string{semconv.ServiceNameKey: "orders", semconv.DeploymentEnvironmentNameKey: "prod", semconv.ServiceVersionKey: "1.2.3"},
		},
		"environment variables": {
			env:  map[string]string{"OTEL_SERVICE_NAME": "billing", "OTEL_RESOURCE_ATTRIBUTES": "service.version=9,deployment.environment.name=staging,team=pay"},
			want: map[attribute.Key]string{semconv.ServiceNameKey: "billing", semconv.ServiceVersionKey: "9", semconv.DeploymentEnvironmentNameKey: "staging", "team": "pay"},
		},
		"arguments override environment variables": {
			env:     map[string]string{"OTEL_SERVICE_NAME": "billing", "OTEL_RESOURCE_ATTRIBUTES": "service.version=9,team=pay"},
			service: "orders", version: "1.2.3",
			want: map[attribute.Key]string{semconv.ServiceNameKey: "orders", semconv.ServiceVersionKey: "1.2.3", "team": "pay"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("OTEL_SERVICE_NAME", "")
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			res, err := New(context.Background(), tc.service, tc.environment, tc.version)
			if err != nil {
				t.Fatal(err)
			}
			for k, want := range tc.want {
				if got, ok := res.Set().Value(k); !ok || got.String() != want {
					t.Errorf("%s = %q, want %q", k, got.String(), want)
				}
			}
			if _, ok := res.Set().Value(semconv.TelemetrySDKNameKey); !ok {
				t.Error("telemetry.sdk.name missing")
			}
			if res.SchemaURL() != semconv.SchemaURL {
				t.Errorf("schema URL = %q", res.SchemaURL())
			}
		})
	}
}

func TestNewInvalidEnvironment(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "not-a-pair")
	if _, err := New(context.Background(), "orders", "", ""); err == nil {
		t.Error("malformed OTEL_RESOURCE_ATTRIBUTES accepted")
	}
}
