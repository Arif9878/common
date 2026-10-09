// Command release tags a release of every Go module in this repository.
//
// All modules are released together with the same version. A module that
// requires another module of the repository (go/fx requires go, and so
// will the integration modules) must require the version being released,
// because consumers ignore its replace directive. release therefore:
//
//  1. finds the modules (every go.mod under go/) and orders them so a
//     module comes after the modules it requires;
//  2. sets each in-repository requirement to the new version, runs go mod
//     tidy, and commits that change on main;
//  3. tags every module at that commit (go/vX.Y.Z, go/fx/vX.Y.Z, …);
//  4. with -push, pushes main and the tags, and with -github-release,
//     creates a GitHub release per tag with generated notes (the core
//     module's marked latest).
//
// Without -push it only prints the plan and changes nothing:
//
//	cd go && go run ./internal/release -version v0.5.0          # dry run
//	cd go && go run ./internal/release -version v0.5.0 -push    # release
//
// It refuses to run unless the working tree is clean, on main, at the tip
// of the remote's main, with green CI on that commit (checked with gh), and
// every tag is new. It also runs gorelease on each module released before
// and refuses a version too small for the API changes, such as a patch
// release with an incompatible change. -skip-ci and -skip-api-check turn
// those two checks off. -trailer adds a trailer, such as a Co-Authored-By
// line, to the release commit.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

func main() {
	var o opts
	flag.StringVar(&o.version, "version", "", "version to release, such as v0.5.0")
	flag.BoolVar(&o.push, "push", false, "commit, tag and push (default: print the plan only)")
	flag.StringVar(&o.remote, "remote", "origin", "git remote or URL to push to")
	flag.BoolVar(&o.ghRelease, "github-release", false, "with -push, create GitHub releases with generated notes (needs gh)")
	flag.BoolVar(&o.skipCI, "skip-ci", false, "release without checking that CI passed on the commit")
	flag.BoolVar(&o.skipAPI, "skip-api-check", false, "release without checking API compatibility with gorelease")
	flag.Func("trailer", "add a trailer line, such as a Co-Authored-By line, to the release commit (repeatable)",
		func(s string) error { o.trailers = append(o.trailers, s); return nil })
	flag.Parse()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

var semver = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// module is one go.mod in the repository.
type module struct {
	Path     string   // module path
	Dir      string   // directory relative to the repository root, such as "go/fx"
	Requires []string // module paths of other repository modules it requires
}

// Tag returns the module's tag for version.
func (m module) Tag(version string) string { return m.Dir + "/" + version }

type opts struct {
	version   string
	push      bool
	remote    string
	ghRelease bool
	skipCI    bool
	skipAPI   bool
	trailers  []string
}

func run(o opts) error {
	version, push, remote, ghRelease := o.version, o.push, o.remote, o.ghRelease
	if !semver.MatchString(version) {
		return fmt.Errorf("-version %q is not a semantic version like v0.5.0", version)
	}
	root, err := output("", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	mods, err := findModules(root)
	if err != nil {
		return err
	}
	if major := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 2)[0]; major != "0" && major != "1" {
		for _, m := range mods {
			if !strings.HasSuffix(m.Path, "/v"+major) {
				return fmt.Errorf("%s needs the module path suffix /v%s first (Go's semantic import versioning)", m.Path, major)
			}
		}
	}
	if mods, err = order(mods); err != nil {
		return err
	}
	fmt.Printf("Releasing %s:\n", version)
	for _, m := range mods {
		fmt.Printf("  %-20s %s", m.Tag(version), m.Path)
		if len(m.Requires) > 0 {
			fmt.Printf("  (requires %s at %s)", strings.Join(m.Requires, ", "), version)
		}
		fmt.Println()
	}
	checks := []struct {
		name string
		skip bool
		fn   func() error
	}{
		{"working tree, branch and tags", false, func() error { return preflight(root, mods, version) }},
		{"main matches " + remote, false, func() error { return checkRemote(root, remote) }},
		{"CI on HEAD", o.skipCI, func() error { return checkCI(root) }},
		{"API compatibility (gorelease)", o.skipAPI, func() error { return checkAPI(root, mods, version) }},
	}
	var failed []string
	fmt.Println("\nChecks:")
	for i, c := range checks {
		if c.skip {
			fmt.Printf("  - %s: skipped\n", c.name)
			continue
		}
		if err := c.fn(); err != nil {
			fmt.Printf("  ✗ %s: %v\n", c.name, err)
			failed = append(failed, c.name)
			if i == 0 {
				break // the other checks assume a clean main
			}
			continue
		}
		fmt.Printf("  ✓ %s\n", c.name)
	}
	if len(failed) > 0 {
		if !push {
			fmt.Println("\nDry run; a release would fail now.")
			return nil
		}
		return fmt.Errorf("checks failed: %s", strings.Join(failed, ", "))
	}
	if !push {
		fmt.Println("\nDry run: nothing changed. Run again with -push to release.")
		return nil
	}

	undo := func(err error) error {
		return fmt.Errorf("%w\nundo local changes with: git checkout -- . && git tag -d <tags created>", err)
	}
	for _, m := range mods {
		if len(m.Requires) == 0 {
			continue
		}
		dir := filepath.Join(root, m.Dir)
		for _, req := range m.Requires {
			if _, err := output(dir, "go", "mod", "edit", "-require="+req+"@"+version); err != nil {
				return undo(err)
			}
		}
		if _, err := output(dir, "go", "mod", "tidy"); err != nil {
			return undo(err)
		}
	}
	if dirty, err := output(root, "git", "status", "--porcelain"); err != nil {
		return err
	} else if dirty != "" {
		msg := "chore(release): require " + version + " between modules"
		if len(o.trailers) > 0 {
			msg += "\n\n" + strings.Join(o.trailers, "\n")
		}
		if _, err := output(root, "git", "commit", "-qam", msg); err != nil {
			return err
		}
	}
	var tags []string
	for _, m := range mods {
		tag := m.Tag(version)
		if _, err := output(root, "git", "tag", "-a", tag, "-m", tag); err != nil {
			return err
		}
		tags = append(tags, tag)
	}
	refs := append([]string{"push", remote, "main"}, prefixed("refs/tags/", tags)...)
	if _, err := output(root, "git", refs...); err != nil {
		return fmt.Errorf("push (tags are created locally; delete them with git tag -d before retrying): %w", err)
	}
	fmt.Println("Pushed main and", strings.Join(tags, ", "))

	if ghRelease {
		for i, tag := range tags {
			latest := "--latest=false"
			if i == 0 {
				latest = "--latest"
			}
			if _, err := output(root, "gh", "release", "create", tag, "--verify-tag", "--generate-notes",
				"--title", tag, latest); err != nil {
				return err
			}
		}
		fmt.Println("Created GitHub releases; edit their notes on GitHub.")
	}
	return nil
}

// findModules returns the modules under go/ in root, skipping vendor and
// testdata directories.
func findModules(root string) ([]module, error) {
	var mods []module
	err := filepath.WalkDir(filepath.Join(root, "go"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "vendor" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if d.Name() != "go.mod" {
			return nil
		}
		js, err := output(filepath.Dir(path), "go", "mod", "edit", "-json")
		if err != nil {
			return err
		}
		var mf struct {
			Module  struct{ Path string }
			Require []struct{ Path string }
		}
		if err := json.Unmarshal([]byte(js), &mf); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		m := module{Path: mf.Module.Path, Dir: filepath.ToSlash(rel)}
		for _, r := range mf.Require {
			m.Requires = append(m.Requires, r.Path) // filtered to repository modules below
		}
		mods = append(mods, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for _, m := range mods {
		paths[m.Path] = true
	}
	for i := range mods {
		mods[i].Requires = slices.DeleteFunc(mods[i].Requires, func(p string) bool { return !paths[p] })
	}
	return mods, nil
}

// order sorts modules so each comes after the modules it requires, and
// alphabetically by directory otherwise. It fails on a cycle.
func order(mods []module) ([]module, error) {
	slices.SortFunc(mods, func(a, b module) int { return strings.Compare(a.Dir, b.Dir) })
	byPath := map[string]module{}
	for _, m := range mods {
		byPath[m.Path] = m
	}
	var out []module
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(m module) error
	visit = func(m module) error {
		switch state[m.Path] {
		case 1:
			return fmt.Errorf("modules require each other in a cycle through %s", m.Path)
		case 2:
			return nil
		}
		state[m.Path] = 1
		for _, r := range m.Requires {
			if err := visit(byPath[r]); err != nil {
				return err
			}
		}
		state[m.Path] = 2
		out = append(out, m)
		return nil
	}
	for _, m := range mods {
		if err := visit(m); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func preflight(root string, mods []module, version string) error {
	branch, err := output(root, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch != "main" {
		return fmt.Errorf("on branch %q; release from main", branch)
	}
	if dirty, err := output(root, "git", "status", "--porcelain"); err != nil {
		return err
	} else if dirty != "" {
		return errors.New("the working tree has changes; commit or stash them first")
	}
	for _, m := range mods {
		if _, err := output(root, "git", "rev-parse", "-q", "--verify", "refs/tags/"+m.Tag(version)); err == nil {
			return fmt.Errorf("tag %s already exists", m.Tag(version))
		}
	}
	return nil
}

func prefixed(prefix string, s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = prefix + v
	}
	return out
}

// output runs a command in dir and returns its trimmed standard output.
func output(dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), name, args...) //nolint:gosec // release tooling runs fixed commands
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// combined runs a command in dir and returns its standard output and
// error together, also when it fails.
func combined(dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), name, args...) //nolint:gosec // release tooling runs fixed commands
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
