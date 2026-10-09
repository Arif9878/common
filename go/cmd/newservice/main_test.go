package main

import (
	"bufio"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejects(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		p   params
		dir string
	}{
		"no module":     {params{Version: "v1.0.0"}, filepath.Join(dir, "a")},
		"bad name":      {params{Module: "example.com/Billing_API", Version: "v1.0.0"}, filepath.Join(dir, "b")},
		"no version":    {params{Module: "example.com/billing"}, filepath.Join(dir, "c")},
		"non-empty dir": {params{Module: "example.com/billing", Version: "v1.0.0"}, nonEmpty(t)},
	} {
		if err := run(tc.p, tc.dir); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func nonEmpty(t *testing.T) string {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRenderVariants(t *testing.T) {
	plain, err := render(params{Module: "example.com/billing", Name: "billing", Ident: "billing", Version: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	withPG, err := render(params{Module: "example.com/billing", Name: "billing", Ident: "billing", Version: "v1.2.3", Postgres: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"go.mod", "main.go", "main_test.go", "internal/api/api.go", "Dockerfile", "k8s/deployment.yaml", "README.md", ".gitignore"} {
		if plain[f] == nil {
			t.Errorf("missing %s", f)
		}
	}
	if _, ok := plain["internal/api/migrations/00001_init.sql"]; ok {
		t.Error("migrations without -postgres")
	}
	if _, ok := withPG["internal/api/migrations/00001_init.sql"]; !ok {
		t.Error("no migrations with -postgres")
	}
	if strings.Contains(string(plain["main.go"]), "postgres") || !strings.Contains(string(withPG["main.go"]), "commonfx.Postgres()") {
		t.Error("main.go does not follow -postgres")
	}
	if !strings.Contains(string(plain["go.mod"]), "github.com/Arif9878/common/go/fx v1.2.3") {
		t.Errorf("go.mod:\n%s", plain["go.mod"])
	}
}

// TestGeneratedServiceBuilds generates both variants against this
// repository's modules (through replace directives), and builds, vets and
// tests them (the PostgreSQL variant's test is only compiled).
func TestGeneratedServiceBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("builds generated modules")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	mods := repoModules(t, repo)
	for _, pg := range []bool{false, true} {
		name := "plain"
		if pg {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "svc")
			if err := run(params{Module: "example.com/acme/billing-api", Version: "v0.0.0", Postgres: pg}, dir); err != nil {
				t.Fatal(err)
			}
			for path, d := range mods {
				gocmd(t, dir, "mod", "edit", "-replace="+path+"="+d)
			}
			gocmd(t, dir, "mod", "tidy")
			gocmd(t, dir, "vet", "./...")
			if pg {
				// Compile the test only: running it would migrate the
				// shared test database.
				gocmd(t, dir, "test", "-count=1", "-run", "^$", "./...")
				return
			}
			gocmd(t, dir, "test", "-count=1", "./...")
		})
	}
}

// repoModules returns the module path and directory of every module under
// repo.
func repoModules(t *testing.T, repo string) map[string]string {
	t.Helper()
	mods := map[string]string{}
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || d.Name() == "template" || strings.HasPrefix(d.Name(), ".") && p != repo) {
			return filepath.SkipDir
		}
		if d.Name() != "go.mod" {
			return nil
		}
		f, err := os.Open(p) //nolint:gosec // a file of this repository
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if mod, ok := strings.CutPrefix(sc.Text(), "module "); ok {
				mods[strings.TrimSpace(mod)] = filepath.Dir(p)
				break
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return mods
}

func gocmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "go", args...) //nolint:gosec // the go command with test-chosen arguments
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "POSTGRES_TEST_URL=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
