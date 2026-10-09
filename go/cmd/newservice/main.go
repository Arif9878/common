// Command newservice creates a new service on the common library: an fx
// application with configuration from the environment, logs, traces and
// metrics, the admin server with health probes, an Echo API with one
// example route and its test, a Dockerfile and a Kubernetes deployment.
//
//	go run github.com/Arif9878/common/go/cmd/newservice@latest -module github.com/acme/billing
//	go run github.com/Arif9878/common/go/cmd/newservice@latest -module github.com/acme/billing -postgres
//
// The service requires the version of the library newservice was run at,
// so run it at the version to start from. Then:
//
//	cd billing && go mod tidy && go test ./...
//
// Flags:
//
//	-module   module path of the new service (required)
//	-dir      directory to create; default: the module path's last element
//	-name     service name, used for the binary, image and Kubernetes
//	          objects; default: the module path's last element
//	-postgres add PostgreSQL with goose migrations
//	-version  library version to require; default: newservice's own
package main

import (
	"bytes"
	"embed"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"text/template"
)

//go:embed all:template
var templates embed.FS

const corePath = "github.com/Arif9878/common/go"

// params fill the templates.
type params struct {
	Module   string // module path
	Name     string // service name: lower-case letters, digits and '-'
	Ident    string // Name as an SQL identifier
	Version  string // common library version
	Postgres bool
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func main() {
	var p params
	var dir string
	flag.StringVar(&p.Module, "module", "", "module path of the new service (required)")
	flag.StringVar(&dir, "dir", "", "directory to create (default: the module path's last element)")
	flag.StringVar(&p.Name, "name", "", "service name (default: the module path's last element)")
	flag.BoolVar(&p.Postgres, "postgres", false, "add PostgreSQL with goose migrations")
	flag.StringVar(&p.Version, "version", "", "common library version to require (default: this command's)")
	flag.Parse()
	if err := run(p, dir); err != nil {
		fmt.Fprintln(os.Stderr, "newservice:", err)
		os.Exit(1)
	}
}

func run(p params, dir string) error {
	if p.Module == "" {
		return errors.New("-module is required, such as -module github.com/acme/billing")
	}
	last := path.Base(p.Module)
	if p.Name == "" {
		p.Name = strings.ToLower(last)
	}
	if !nameRE.MatchString(p.Name) {
		return fmt.Errorf("service name %q: use lower-case letters, digits and '-', starting with a letter (or pass -name)", p.Name)
	}
	p.Ident = strings.ReplaceAll(p.Name, "-", "_")
	if p.Version == "" {
		p.Version = ownVersion()
		if p.Version == "" {
			return errors.New("cannot tell which library version this is (run it with go run …/cmd/newservice@vX.Y.Z, or pass -version)")
		}
	}
	if dir == "" {
		dir = last
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s exists and is not empty", dir)
	}
	files, err := render(p)
	if err != nil {
		return err
	}
	for name, content := range files {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("Created %s in %s, on the common library %s. Next:\n\n  cd %s && go mod tidy && go test ./...\n", p.Module, dir, p.Version, dir)
	return nil
}

// render returns the service's files by path. Go files are gofmt-ed;
// PostgreSQL-only files are left out without p.Postgres.
func render(p params) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(templates, "template", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(name, "template/"), ".tmpl")
		if rel == "gitignore" {
			rel = ".gitignore" // embed skips dot files outside all:, and go mod skips them too
		}
		if !p.Postgres && strings.Contains(rel, "migrations/") {
			return nil
		}
		src, err := templates.ReadFile(name)
		if err != nil {
			return err
		}
		t, err := template.New(rel).Option("missingkey=error").Parse(string(src))
		if err != nil {
			return err
		}
		var b bytes.Buffer
		if err := t.Execute(&b, p); err != nil {
			return err
		}
		content := b.Bytes()
		if strings.HasSuffix(rel, ".go") {
			if content, err = format.Source(content); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		}
		out[rel] = content
		return nil
	})
	return out, err
}

// ownVersion returns the version of the common library module this
// command was built from, or "" in a development build.
func ownVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Path != corePath || bi.Main.Version == "" || bi.Main.Version == "(devel)" {
		return ""
	}
	return bi.Main.Version
}
