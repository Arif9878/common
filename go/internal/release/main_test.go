package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeMod(t *testing.T, root, dir, content string) {
	t.Helper()
	p := filepath.Join(root, dir, "go.mod")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFindAndOrderModules(t *testing.T) {
	root := t.TempDir()
	writeMod(t, root, "go", "module example.com/r/go\n\ngo 1.26\n\nrequire golang.org/x/net v0.60.0\n")
	writeMod(t, root, "go/fx", "module example.com/r/go/fx\n\ngo 1.26\n\nrequire (\n\texample.com/r/go v0.4.0\n\texample.com/r/go/messaging/kafka v0.4.0\n)\n")
	writeMod(t, root, "go/messaging/kafka", "module example.com/r/go/messaging/kafka\n\ngo 1.26\n\nrequire example.com/r/go v0.4.0\n")
	writeMod(t, root, "go/vendor/x", "module example.com/vendored\n")
	writeMod(t, root, "go/x/testdata/m", "module example.com/testdata\n")

	mods, err := findModules(root)
	if err != nil {
		t.Fatal(err)
	}
	mods, err = order(mods)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, m := range mods {
		dirs = append(dirs, m.Dir)
	}
	if want := []string{"go", "go/messaging/kafka", "go/fx"}; !slices.Equal(dirs, want) {
		t.Fatalf("order = %v, want %v", dirs, want)
	}
	if got := mods[2].Requires; !slices.Equal(got, []string{"example.com/r/go", "example.com/r/go/messaging/kafka"}) {
		t.Errorf("fx requires %v; external modules must be dropped", got)
	}
	if len(mods[0].Requires) != 0 {
		t.Errorf("core requires %v, want none", mods[0].Requires)
	}
	if got := mods[2].Tag("v1.2.3"); got != "go/fx/v1.2.3" {
		t.Errorf("tag = %q", got)
	}
}

func TestOrderRejectsCycles(t *testing.T) {
	_, err := order([]module{
		{Path: "a", Dir: "go/a", Requires: []string{"b"}},
		{Path: "b", Dir: "go/b", Requires: []string{"a"}},
	})
	if err == nil {
		t.Fatal("no error for a cycle")
	}
}

func TestVersionFormat(t *testing.T) {
	for v, ok := range map[string]bool{"v0.5.0": true, "v1.0.0-rc.1": true, "0.5.0": false, "v0.5": false, "": false} {
		if semver.MatchString(v) != ok {
			t.Errorf("%q valid = %v", v, !ok)
		}
	}
}

func TestFindExamples(t *testing.T) {
	root := t.TempDir()
	writeMod(t, root, "go", "module example.com/r/go\n\ngo 1.26\n")
	writeMod(t, root, "examples/svc", "module example.com/r/examples/svc\n\ngo 1.26\n\nrequire (\n\texample.com/r/go v0.4.0\n\tgolang.org/x/net v0.60.0\n)\n")
	writeMod(t, root, "examples/standalone", "module example.com/r/examples/standalone\n\ngo 1.26\n\nrequire golang.org/x/net v0.60.0\n")

	mods, err := findModules(root)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := findExamples(root, mods)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].Dir != "examples/svc" || !slices.Equal(ex[0].Requires, []string{"example.com/r/go"}) {
		t.Errorf("examples = %+v, want examples/svc requiring example.com/r/go", ex)
	}
}
