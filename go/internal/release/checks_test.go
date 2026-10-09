package main

import (
	"os/exec"
	"testing"
)

func TestCIVerdict(t *testing.T) {
	green := `{"check_runs":[{"name":"test","status":"completed","conclusion":"success"},
		{"name":"guard","status":"completed","conclusion":"skipped"}]}`
	if err := ciVerdict(green, "abc"); err != nil {
		t.Errorf("green CI: %v", err)
	}
	for name, js := range map[string]string{
		"failed":  `{"check_runs":[{"name":"vuln","status":"completed","conclusion":"failure"}]}`,
		"running": `{"check_runs":[{"name":"test","status":"in_progress","conclusion":null}]}`,
		"none":    `{"check_runs":[]}`,
	} {
		if err := ciVerdict(js, "abc"); err == nil {
			t.Errorf("%s CI accepted", name)
		}
	}
}

func TestAPIVerdict(t *testing.T) {
	const valid = "\n# summary\nv0.6.2 (with tag go/v0.6.2) is a valid semantic version for this release\n"
	incompat := "# github.com/x/requestid\n## incompatible changes\nMaxLen: removed\n" + valid
	compat := "# github.com/x/requestid\n## compatible changes\nNew: added\n" + valid
	for _, tc := range []struct {
		name, out, base, version string
		ok                       bool
	}{
		{"no changes", valid, "v0.6.1", "v0.6.2", true},
		{"incompatible in a patch", incompat, "v0.6.1", "v0.6.2", false},
		{"incompatible in a minor", incompat, "v0.6.1", "v0.7.0", true},
		{"new API in a patch", compat, "v0.6.1", "v0.6.2", true},
		{"gorelease rejects", "# summary\nSuggested version: v2.0.0\n", "v1.2.0", "v1.3.0", false},
		{"no verdict", "go: cannot find module", "v0.6.1", "v0.6.2", false},
	} {
		verdict, ok := apiVerdict(tc.out, tc.base, tc.version)
		if ok != tc.ok {
			t.Errorf("%s: %q ok=%v, want %v", tc.name, verdict, ok, tc.ok)
		}
	}
	if got := section(incompat+"# github.com/x/other\n## incompatible changes\nF: changed\n", "## incompatible changes"); got != "MaxLen: removed\nF: changed" {
		t.Errorf("section = %q", got)
	}
}

func TestPreviousVersion(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // fixed test commands
		cmd.Dir = root
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "x")
	for _, tag := range []string{"go/v0.5.0", "go/v0.10.0", "go/v0.6.1", "go/v0.11.0-rc.1", "go/fx/v0.9.0", "go/v1.0.0"} {
		git("tag", tag)
	}
	core := module{Dir: "go"}
	for version, want := range map[string]string{"v0.11.0": "v0.10.0", "v0.6.2": "v0.6.1", "v0.5.0": "", "v2.0.0": "v1.0.0"} {
		if got, err := previousVersion(root, core, version); err != nil || got != want {
			t.Errorf("previous of %s = %q, %v; want %q (fx tags and pre-releases ignored)", version, got, err, want)
		}
	}
	if got, _ := previousVersion(root, module{Dir: "go/new"}, "v0.6.0"); got != "" {
		t.Errorf("new module previous = %q", got)
	}
}

func TestNeedsUnreleasedAPI(t *testing.T) {
	const prefix = "github.com/Arif9878/common/go"
	tests := map[string]struct {
		out  string
		want bool
	}{
		"missing package of ours": {`gorelease: preparing to load packages for github.com/Arif9878/common/go/fx: looking for missing dependencies: go: -d flag is deprecated. -d=true is a no-op
go: gorelease-load-module imports
	github.com/Arif9878/common/go/fx imports
	github.com/Arif9878/common/go/validation: cannot find module providing package github.com/Arif9878/common/go/validation
exit status 1`, true},
		"missing outside package": {`go: gorelease-load-module imports
	github.com/Arif9878/common/go/fx imports
	example.com/other: cannot find module providing package example.com/other`, false},
		"undefined identifiers": {`# github.com/Arif9878/common/go/transport/grpc/grpcstatus
## errors in release version:
grpcstatus/grpcstatus.go:163:42: undefined: errors.FieldError
grpcstatus/grpcstatus.go:107:22: undefined: errors.Fields

# summary
v0.7.0 is not a valid semantic version for this release.`, true},
		"other type errors": {`## errors in release version:
x/x.go:1:2: cannot use s (variable of type string) as int value
x/x.go:3:4: undefined: errors.Fields`, false},
		"plain incompatible change": {"## incompatible changes\nFoo: removed\n\n# summary\nSuggested version: v0.8.0", false},
	}
	for name, tc := range tests {
		if got := needsUnreleasedAPI(tc.out, prefix); got != tc.want {
			t.Errorf("%s: needsUnreleasedAPI = %v, want %v", name, got, tc.want)
		}
	}
}

func TestAfterTagVerdict(t *testing.T) {
	const summary = "\n# summary\nCannot suggest a release version.\nCan only suggest a release version when compared against the most recent version of this major: v0.7.0.\n"
	tests := map[string]struct {
		out, version string
		ok           bool
	}{
		"compatible":              {"# example.com/m/grpcserver\n## compatible changes\nBearer: added\n" + summary, "v0.7.0", true},
		"no changes":              {summary, "v0.7.0", true},
		"incompatible in a minor": {"## incompatible changes\nFoo: removed\n" + summary, "v0.7.0", true},
		"incompatible in a patch": {"## incompatible changes\nFoo: removed\n" + summary, "v0.6.2", false},
		"load errors":             {"## errors in release version:\nx.go:1:2: undefined: errors.Fields\n" + summary, "v0.7.0", false},
		"gorelease failed to run": {"go: downloading …\nexit status 1", "v0.7.0", false},
	}
	for name, tc := range tests {
		if _, ok := afterTagVerdict(tc.out, "v0.6.1", tc.version); ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", name, ok, tc.ok)
		}
	}
}

func TestRepoPath(t *testing.T) {
	m := module{Path: "github.com/Arif9878/common/go/messaging/kafka", Dir: "go/messaging/kafka"}
	if got := repoPath(m); got != "github.com/Arif9878/common" {
		t.Errorf("repoPath = %q", got)
	}
}
