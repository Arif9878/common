// Package otelres builds the OpenTelemetry resource shared by the tracing
// and metrics packages.
package otelres

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// New returns a resource describing the service. Attributes from
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES are applied first; non-empty
// arguments override them.
func New(ctx context.Context, service, environment, version string) (*resource.Resource, error) {
	var attrs []attribute.KeyValue
	if service != "" {
		attrs = append(attrs, semconv.ServiceName(service))
	}
	if environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentNameKey.String(environment))
	}
	if version != "" {
		attrs = append(attrs, semconv.ServiceVersion(version))
	}

	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
		resource.WithAttributes(attrs...),
		resource.WithSchemaURL(semconv.SchemaURL),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}
	return resource.Merge(resource.Default(), res)
}
