package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAndRender(t *testing.T) {
	types, err := parseDir("testdata/sample", "example.com/m/sample")
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 2 || types[0].Name != "Config" || types[1].Name != "Nested" {
		t.Fatalf("types = %+v, want Config and Nested (OnlyPrefixes has no variables)", types)
	}
	doc := string(render(types))
	for _, want := range []string{
		"| `BROKERS` | `[]string (separated by \",\")` | **required** | Brokers are the seed brokers. |",
		"| `TIMEOUT` | `time.Duration` | `10s` | Timeout bounds each call \\| with a pipe. |",
		"| `PASSWORD` | `config.Secret` (secret) |  | Password is a secret. |",
		"| `LOG_*` | `Nested` |  | Variables of `Nested`, prefixed. |",
		"`example.com/m/sample`: Config configures the sample.",
		"[`sample.Config`](#sampleconfig)",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing %q in:\n%s", want, doc)
		}
	}
	for _, unwanted := range []string{"ignored", "INTERNAL", "second sentence"} {
		if strings.Contains(doc, unwanted) {
			t.Errorf("output contains %q", unwanted)
		}
	}
}

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sample"), 0o750); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile("testdata/sample/sample.go")
	_ = os.WriteFile(filepath.Join(dir, "sample", "sample.go"), src, 0o600) //nolint:gosec // a temporary directory
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n"), 0o600)

	if err := run(dir, "ENV.md", true); err == nil {
		t.Error("-check passed without the file")
	}
	if err := run(dir, "ENV.md", false); err != nil {
		t.Fatal(err)
	}
	if err := run(dir, "ENV.md", true); err != nil {
		t.Errorf("-check after writing: %v", err)
	}
}
