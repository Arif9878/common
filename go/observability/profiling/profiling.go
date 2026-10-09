// Package profiling serves Go's runtime profiles (net/http/pprof) on a
// mux of your choice, for the admin port rather than the public API:
//
//	admin := http.NewServeMux()
//	profiling.Register(admin) // /debug/pprof/…
//
// With commonfx, set ADMIN_PPROF=true instead. Profiles can then be taken
// on demand (go tool pprof http://host:9090/debug/pprof/profile) or
// collected continuously by Grafana Alloy's pyroscope.scrape into Grafana
// Pyroscope, without an SDK in the service; the README shows the Alloy
// configuration.
//
// Never register it on a public listener: profiles reveal the program's
// internals, and CPU and trace profiles cost CPU while they run.
package profiling

import (
	"net/http"
	"net/http/pprof"
)

// Register adds the pprof handlers under /debug/pprof/ to mux.
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/pprof/", pprof.Index) // heap, goroutine, allocs, block, mutex, threadcreate
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
}
