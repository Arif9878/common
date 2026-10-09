package dashboards_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// instrument is a metric instrument registered by the library.
type instrument struct {
	Name, Kind, Unit string
}

// thirdParty are instruments created by instrumentation the library wraps.
var thirdParty = []instrument{
	{"http.client.request.duration", "Float64Histogram", "s"}, // otelhttp, httpclient
	{"rpc.server.duration", "Float64Histogram", "ms"},         // otelgrpc, grpcserver
	{"rpc.client.duration", "Float64Histogram", "ms"},         // otelgrpc, grpcclient
	// redisotel, datastore/redis: connection pool.
	{"db.client.connections.usage", "Int64ObservableUpDownCounter", ""},
	{"db.client.connections.max", "Int64ObservableUpDownCounter", ""},
	{"db.client.connections.waits", "Int64ObservableCounter", ""},
	{"db.client.connections.timeouts", "Int64ObservableCounter", ""},
	{"db.client.connections.use_time", "Float64Histogram", "ms"},
	// kotel, messaging/kafka: client connections and traffic.
	{"messaging.kafka.connect_errors.count", "Int64Counter", "1"},
	{"messaging.kafka.write_errors.count", "Int64Counter", "1"},
	{"messaging.kafka.read_errors.count", "Int64Counter", "1"},
	{"messaging.kafka.produce_bytes.count", "Int64Counter", "by"},
	{"messaging.kafka.fetch_bytes.count", "Int64Counter", "by"},
	{"messaging.kafka.produce_records.count", "Int64Counter", "1"},
	{"messaging.kafka.fetch_records.count", "Int64Counter", "1"},
	// Go runtime metrics pushed over OTLP when Prometheus is off.
	{"go.goroutine.count", "Int64ObservableUpDownCounter", "{goroutine}"},
	{"go.memory.used", "Int64ObservableUpDownCounter", "By"},
}

// inventory scans the repository's Go source (every module) for
// meter.<Kind>("name", metric.WithUnit("unit")) calls.
func inventory(t *testing.T) []instrument {
	t.Helper()
	root := filepath.Join("..", "..") // the go/ directory
	var out []instrument
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "vendor" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isInstrumentKind(sel.Sel.Name) || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, _ := strconv.Unquote(lit.Value)
			in := instrument{Name: name, Kind: sel.Sel.Name}
			for _, a := range call.Args[1:] {
				if c, ok := a.(*ast.CallExpr); ok {
					if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "WithUnit" && len(c.Args) == 1 {
						if u, ok := c.Args[0].(*ast.BasicLit); ok {
							in.Unit, _ = strconv.Unquote(u.Value)
						}
					}
				}
			}
			out = append(out, in)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(out, thirdParty...)
}

func isInstrumentKind(name string) bool {
	for _, p := range []string{"Int64", "Float64"} {
		for _, k := range []string{"Counter", "UpDownCounter", "Histogram", "Gauge", "ObservableCounter", "ObservableUpDownCounter", "ObservableGauge"} {
			if name == p+k {
				return true
			}
		}
	}
	return false
}
