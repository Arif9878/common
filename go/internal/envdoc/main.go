// Command envdoc writes ENVIRONMENT.md: every configuration struct of the
// repository's modules (types with fields tagged env:"…") and the
// variables it reads, with their types, defaults and documentation, taken
// from the source.
//
//	cd go && go run ./internal/envdoc          # rewrite ENVIRONMENT.md
//	cd go && go run ./internal/envdoc -check   # fail if it is out of date
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

func main() {
	check := flag.Bool("check", false, "fail if ENVIRONMENT.md is not up to date instead of writing it")
	flag.Parse()
	if err := run(".", "ENVIRONMENT.md", *check); err != nil {
		fmt.Fprintln(os.Stderr, "envdoc:", err)
		os.Exit(1)
	}
}

func run(dir, out string, check bool) error {
	types, err := collect(dir)
	if err != nil {
		return err
	}
	doc := render(types)
	if check {
		old, err := os.ReadFile(filepath.Join(dir, out)) //nolint:gosec // the repository's own file
		if err != nil || !bytes.Equal(old, doc) {
			return errors.New(out + " is out of date; run: make envdoc")
		}
		return nil
	}
	return os.WriteFile(filepath.Join(dir, out), doc, 0o600)
}

// configType is a struct with environment-tagged fields.
type configType struct {
	Pkg     string // import path
	PkgName string // package name, such as commonfx
	Name    string
	Doc     string
	Fields  []field
}

type field struct {
	Name     string // variable name, relative to the prefix the service chooses
	Type     string
	Default  string
	Required bool
	Secret   bool
	Doc      string
	Nested   string // for envPrefix fields: the nested type, such as "logging.Config"
}

// collect parses every non-test package under dir (skipping internal,
// vendor and testdata directories) and returns its configuration types.
func collect(dir string) ([]configType, error) {
	var types []configType
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		switch d.Name() {
		case "vendor", "testdata", "internal":
			return filepath.SkipDir
		}
		pkg, err := importPath(dir, path)
		if err != nil {
			return err
		}
		ts, err := parseDir(path, pkg)
		types = append(types, ts...)
		return err
	})
	slices.SortFunc(types, func(a, b configType) int {
		return strings.Compare(a.Pkg+"."+a.Name, b.Pkg+"."+b.Name)
	})
	return types, err
}

// importPath returns the import path of the package in dir, from the
// nearest go.mod at or above it within root.
func importPath(root, dir string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, "go.mod")) //nolint:gosec // go.mod files of the walked tree
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if mod, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					rel, err := filepath.Rel(d, dir)
					if err != nil {
						return "", err
					}
					if rel == "." {
						return mod, nil
					}
					return mod + "/" + filepath.ToSlash(rel), nil
				}
			}
		}
		if d == root || d == filepath.Dir(d) {
			return "", fmt.Errorf("%s: no go.mod", dir)
		}
	}
}

func parseDir(dir, pkg string) ([]configType, error) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var types []configType
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(f.Name.Name, "_test") || f.Name.Name == "main" {
			continue
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				doc := ts.Doc
				if doc == nil {
					doc = gd.Doc
				}
				ct := configType{Pkg: pkg, PkgName: f.Name.Name, Name: ts.Name.Name, Doc: firstSentence(doc.Text())}
				lastDoc := ""
				for _, fl := range st.Fields.List {
					for _, f := range fieldsOf(fl) {
						// "Service, Environment and Version describe …" documents
						// the fields that follow it too.
						if f.Doc == "" && len(fl.Names) > 0 && mentions(lastDoc, fl.Names[0].Name) {
							f.Doc = lastDoc
						}
						if f.Doc != "" {
							lastDoc = f.Doc
						}
						ct.Fields = append(ct.Fields, f)
					}
				}
				if slices.ContainsFunc(ct.Fields, func(f field) bool { return f.Nested == "" }) {
					types = append(types, ct)
				}
			}
		}
	}
	return types, nil
}

func fieldsOf(fl *ast.Field) []field {
	if fl.Tag == nil || len(fl.Names) == 0 || !fl.Names[0].IsExported() {
		return nil
	}
	raw, err := strconv.Unquote(fl.Tag.Value)
	if err != nil {
		return nil
	}
	tag := reflect.StructTag(raw)
	doc := fl.Doc.Text()
	if doc == "" {
		doc = fl.Comment.Text()
	}
	doc = strings.Join(strings.Fields(doc), " ")
	typ := exprString(fl.Type)

	if prefix, ok := tag.Lookup("envPrefix"); ok {
		return []field{{Name: prefix + "*", Type: typ, Doc: doc, Nested: typ}}
	}
	env, ok := tag.Lookup("env")
	if !ok || env == "" || env == "-" {
		return nil
	}
	name, opts, _ := strings.Cut(env, ",")
	f := field{Name: name, Type: typ, Doc: doc, Secret: typ == "config.Secret" || typ == "Secret"}
	f.Required = slices.Contains(strings.Split(opts, ","), "required")
	f.Default, _ = tag.Lookup("envDefault")
	if sep, ok := tag.Lookup("envSeparator"); ok && strings.HasPrefix(typ, "[]") {
		f.Type += " (separated by \"" + sep + "\")"
	}
	return []field{f}
}

func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return exprString(t.X)
	case *ast.ArrayType:
		return "[]" + exprString(t.Elt)
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	}
	return "?"
}

// firstSentence returns the first sentence of a doc comment, on one line.
func firstSentence(doc string) string {
	doc = strings.Join(strings.Fields(doc), " ")
	if i := strings.Index(doc, ". "); i >= 0 {
		return doc[:i+1]
	}
	return doc
}

func render(types []configType) []byte {
	var b bytes.Buffer
	b.WriteString(`# Environment variables

<!-- Generated by internal/envdoc from the source; run make envdoc after changing a configuration struct. -->

Every configuration struct reads environment variables through ` + "`config.Load`" + `. The names below are relative: the service chooses a prefix for each struct with an ` + "`envPrefix`" + ` tag, so ` + "`ENDPOINT`" + ` in ` + "`otlp.Config`" + ` is ` + "`OTLP_ENDPOINT`" + ` under ` + "`envPrefix:\"OTLP_\"`" + `:

` + "```go" + `
type Config struct {
	OTLP  otlp.Config  ` + "`envPrefix:\"OTLP_\"`" + `
	Kafka kafka.Config ` + "`envPrefix:\"KAFKA_\"`" + `
}
` + "```" + `

Secrets (` + "`config.Secret`" + `) never appear in logs or in printed configuration. Packages that use the OpenTelemetry SDK also read the standard ` + "`OTEL_*`" + ` variables; see the README.

`)
	b.WriteString("| Package | Struct |\n|---|---|\n")
	for _, t := range types {
		fmt.Fprintf(&b, "| `%s` | [`%s`](#%s) |\n", short(t.Pkg), shortType(t), anchor(t))
	}
	for _, t := range types {
		fmt.Fprintf(&b, "\n## %s\n\n", shortType(t))
		fmt.Fprintf(&b, "`%s`", t.Pkg)
		if t.Doc != "" {
			fmt.Fprintf(&b, ": %s", t.Doc)
		}
		b.WriteString("\n\n| Variable | Type | Default | Description |\n|---|---|---|---|\n")
		for _, f := range t.Fields {
			def := "`" + f.Default + "`"
			switch {
			case f.Required:
				def = "**required**"
			case f.Default == "":
				def = ""
			}
			typ := "`" + f.Type + "`"
			if f.Secret {
				typ += " (secret)"
			}
			desc := escape(f.Doc)
			if f.Nested != "" {
				desc = strings.TrimSpace(desc + " Variables of `" + f.Nested + "`, prefixed.")
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", f.Name, typ, def, desc)
		}
	}
	return b.Bytes()
}

func short(pkg string) string { return strings.TrimPrefix(pkg, "github.com/Arif9878/common/go/") }

func shortType(t configType) string { return t.PkgName + "." + t.Name }

// anchor is GitHub's heading anchor for shortType(t).
func anchor(t configType) string {
	return strings.ToLower(strings.NewReplacer(".", "", " ", "-").Replace(shortType(t)))
}

func escape(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// mentions reports whether doc names the Go identifier name as a word in
// its first sentence.
func mentions(doc, name string) bool {
	for _, w := range strings.FieldsFunc(firstSentence(doc), func(r rune) bool {
		return r == ' ' || r == ',' || r == '.' || r == '(' || r == ')'
	}) {
		if w == name {
			return true
		}
	}
	return false
}
