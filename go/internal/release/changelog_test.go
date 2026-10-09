package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseChange(t *testing.T) {
	tests := []struct {
		subject, body string
		ok            bool
		want          change
	}{
		{subject: "feat(fx): add Scheduler", ok: true, want: change{Type: "feat", Scope: "fx", Description: "add Scheduler"}},
		{subject: "fix: close the body", ok: true, want: change{Type: "fix", Description: "close the body"}},
		{subject: "refactor!: remove the legacy packages", ok: true, want: change{Type: "refactor", Description: "remove the legacy packages", Breaking: true}},
		{
			subject: "feat(kafka): rename Consumer options",
			body:    "Longer text.\n\nBREAKING CHANGE: WithGroup is now\nWithGroupID.\n\nCo-Authored-By: someone",
			ok:      true,
			want:    change{Type: "feat", Scope: "kafka", Description: "rename Consumer options", Breaking: true, Note: "WithGroup is now WithGroupID."},
		},
		{subject: "fix: x", body: "BREAKING-CHANGE: y", ok: true, want: change{Type: "fix", Description: "x", Breaking: true, Note: "y"}},
		{subject: "Update README.md"},
		{subject: "feat:missing space"},
		{subject: "Feat: capitalized type"},
	}
	for _, tt := range tests {
		got, ok := parseChange(commit{Subject: tt.subject, Body: tt.body})
		if ok != tt.ok {
			t.Errorf("parseChange(%q) ok = %v, want %v", tt.subject, ok, tt.ok)
			continue
		}
		got.commit = commit{}
		if ok && got != tt.want {
			t.Errorf("parseChange(%q) = %+v, want %+v", tt.subject, got, tt.want)
		}
	}
}

func TestRenderChangelog(t *testing.T) {
	commits := []commit{
		{Hash: "aaaaaaaaaaaaaaaa", Subject: "feat(fx): add Scheduler", PR: 42},
		{Hash: "bbbbbbbbbbbbbbbb", Subject: "docs: explain the lock"},
		{Hash: "cccccccccccccccc", Subject: "fix: close the body"},
		{Hash: "dddddddddddddddd", Subject: "refactor!: remove the legacy packages", Body: "BREAKING CHANGE: pin v0.6 to keep them.", PR: 45},
		{Hash: "eeeeeeeeeeeeeeee", Subject: "chore(release): v0.6.1"},
		{Hash: "ffffffffffffffff", Subject: "perf(cache): skip the codec for []byte", PR: 43},
		{Hash: "0000000000000000", Subject: "not conventional"},
	}
	got := renderChangelog("https://github.com/o/r", "v0.7.0", "2026-10-09", changes(commits))
	want := `## [v0.7.0](https://github.com/o/r/releases/tag/go/v0.7.0) - 2026-10-09

### Breaking changes

- remove the legacy packages ([#45](https://github.com/o/r/pull/45))

  Pin v0.6 to keep them.

### Features

- **fx:** add Scheduler ([#42](https://github.com/o/r/pull/42))

### Fixes

- close the body ([ccccccc](https://github.com/o/r/commit/cccccccccccccccc))

### Performance

- **cache:** skip the codec for []byte ([#43](https://github.com/o/r/pull/43))
`
	if got != want {
		t.Errorf("renderChangelog:\n%s\nwant:\n%s", got, want)
	}

	empty := renderChangelog("https://github.com/o/r", "Unreleased", "", changes(commits[1:2]))
	if empty != "## Unreleased\n\nNo user-facing changes.\n" {
		t.Errorf("no changes rendered as %q", empty)
	}
}

func TestPrependChangelog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := prependChangelog(path, "## v0.1.0\n\n- one\n"); err != nil {
		t.Fatal(err)
	}
	if err := prependChangelog(path, "## v0.2.0\n\n- two\n"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // a file in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	want := changelogHeader + "\n## v0.2.0\n\n- two\n\n## v0.1.0\n\n- one\n"
	if string(got) != want {
		t.Errorf("changelog:\n%s\nwant:\n%s", got, want)
	}
}

func TestCheckBump(t *testing.T) {
	breaking := []change{{commit: commit{Subject: "refactor!: remove x"}, Breaking: true}}
	feature := []change{{commit: commit{Subject: "feat: add x"}}}
	tests := []struct {
		prev, version string
		chs           []change
		ok            bool
	}{
		{"v0.6.1", "v0.6.2", breaking, false},
		{"v0.6.1", "v0.7.0", breaking, true},
		{"v0.6.1", "v1.0.0", breaking, true},
		{"v1.2.0", "v1.3.0", breaking, false},
		{"v1.2.0", "v2.0.0", breaking, true},
		{"v1.2.0", "v1.2.1", feature, true}, // gorelease checks new API
		{"", "v0.1.0", breaking, true},
	}
	for _, tt := range tests {
		if err := checkBump(tt.prev, tt.version, tt.chs); (err == nil) != tt.ok {
			t.Errorf("checkBump(%s, %s) = %v, want ok %v", tt.prev, tt.version, err, tt.ok)
		}
	}
}

func TestReleaseCommits(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := output(dir, "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "feat: before the release")
	git("tag", "go/v0.1.0")
	git("commit", "-q", "--allow-empty", "-m", "fix: direct on main")
	git("switch", "-q", "-c", "topic")
	git("commit", "-q", "--allow-empty", "-m", "feat(x): from a pull request")
	git("switch", "-q", "main")
	git("merge", "-q", "--no-ff", "topic", "-m", "Merge pull request #7 from o/topic")

	commits, err := releaseCommits(dir, "go/v0.1.0", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range commits {
		got = append(got, fmt.Sprintf("%s #%d", c.Subject, c.PR))
	}
	if want := []string{"fix: direct on main #0", "feat(x): from a pull request #7"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("commits = %q, want %q", got, want)
	}
	all, err := releaseCommits(dir, "", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("all history has %d commits, want 3", len(all))
	}
}
