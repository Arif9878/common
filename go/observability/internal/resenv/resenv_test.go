package resenv_test

import (
	"testing"

	"github.com/Arif9878/common/go/observability/internal/resenv"
)

func TestLookup(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=from-attrs, service.version=1.4.2,"+
		"deployment.environment=old,deployment.environment.name=prod%20eu,broken,=x,k8s.pod.name=orders-7d9")
	got := resenv.Lookup()
	if want := (resenv.Identity{Service: "from-attrs", Environment: "prod eu", Version: "1.4.2"}); got != want {
		t.Errorf("Lookup() = %+v, want %+v", got, want)
	}

	t.Setenv("OTEL_SERVICE_NAME", "orders")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=ignored,deployment.environment=staging")
	got = resenv.Lookup()
	if want := (resenv.Identity{Service: "orders", Environment: "staging"}); got != want {
		t.Errorf("Lookup() = %+v, want %+v (OTEL_SERVICE_NAME wins; old environment key)", got, want)
	}

	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	if got := resenv.Lookup(); got != (resenv.Identity{}) {
		t.Errorf("Lookup() without variables = %+v", got)
	}
}
