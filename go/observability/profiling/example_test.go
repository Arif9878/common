package profiling_test

import (
	"net/http"
	"time"

	"github.com/Arif9878/common/go/observability/profiling"
)

// Serve pprof on the internal admin port, never on the public API.
// commonfx.AdminServer does this when ADMIN_PPROF=true.
func ExampleRegister() {
	admin := http.NewServeMux()
	profiling.Register(admin)
	// go tool pprof http://localhost:9090/debug/pprof/profile?seconds=30
	_ = &http.Server{Addr: ":9090", Handler: admin, ReadHeaderTimeout: 5 * time.Second}
}
