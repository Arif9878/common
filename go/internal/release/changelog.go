package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// commit is one non-merge commit between two releases.
type commit struct {
	Hash    string
	Subject string
	Body    string
	PR      int // pull request that merged it, or 0 for a direct commit
}

// change is a commit parsed as a Conventional Commit
// (https://www.conventionalcommits.org): type(scope)!: description.
type change struct {
	Type, Scope, Description string
	Breaking                 bool
	Note                     string // BREAKING CHANGE footer, if any
	commit
}

var (
	conventional = regexp.MustCompile(`^([a-z]+)(?:\(([^)]+)\))?(!)?: (.+)$`)
	breakingNote = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE: `)
	mergedPR     = regexp.MustCompile(`^Merge pull request #(\d+) `)
	blankLines   = regexp.MustCompile(`\n{3,}`)
)

// changelogSections lists the commit types that appear in the changelog,
// in order. Other types (chore, ci, docs, test, build, style, refactor)
// are left out unless they are breaking.
var changelogSections = []struct{ typ, title string }{
	{"feat", "Features"},
	{"fix", "Fixes"},
	{"perf", "Performance"},
}

// parseChange parses c, reporting false when its subject is not a
// Conventional Commit.
func parseChange(c commit) (change, bool) {
	m := conventional.FindStringSubmatch(strings.TrimSpace(c.Subject))
	if m == nil {
		return change{}, false
	}
	ch := change{Type: m[1], Scope: m[2], Description: m[4], Breaking: m[3] == "!", commit: c}
	if loc := breakingNote.FindStringIndex(c.Body); loc != nil {
		ch.Breaking = true
		note := c.Body[loc[1]:]
		if end := strings.Index(note, "\n\n"); end >= 0 {
			note = note[:end]
		}
		ch.Note = strings.Join(strings.Fields(note), " ")
	}
	return ch, true
}

// releaseCommits returns the non-merge commits in from..to (all commits up
// to to when from is empty), oldest first, each with the number of the
// pull request whose merge brought it onto the first-parent history.
func releaseCommits(root, from, to string) ([]commit, error) {
	rng := to
	if from != "" {
		rng = from + ".." + to
	}
	const sep, end = "\x1f", "\x1e"
	out, err := output(root, "git", "log", "--reverse", "--topo-order", "--no-merges", "--format=%H"+sep+"%s"+sep+"%b"+end, rng)
	if err != nil {
		return nil, err
	}
	var commits []commit
	for rec := range strings.SplitSeq(out, end) {
		f := strings.SplitN(strings.TrimLeft(rec, "\n"), sep, 3)
		if len(f) != 3 {
			continue
		}
		commits = append(commits, commit{Hash: f[0], Subject: f[1], Body: strings.TrimSpace(f[2])})
	}

	// A pull request merged with a merge commit "Merge pull request #N"
	// brings in the commits reachable from its second parent only.
	merges, err := output(root, "git", "log", "--first-parent", "--merges", "--format=%H %P"+sep+"%s", rng)
	if err != nil {
		return nil, err
	}
	pr := map[string]int{}
	for line := range strings.SplitSeq(merges, "\n") {
		hashes, subject, _ := strings.Cut(line, sep)
		parents := strings.Fields(hashes)
		m := mergedPR.FindStringSubmatch(subject)
		if m == nil || len(parents) < 3 {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		in, err := output(root, "git", "rev-list", parents[1]+".."+parents[2])
		if err != nil {
			return nil, err
		}
		for _, h := range strings.Fields(in) {
			pr[h] = n
		}
	}
	for i := range commits {
		commits[i].PR = pr[commits[i].Hash]
	}
	return commits, nil
}

// changes parses commits, dropping those that are not Conventional
// Commits and the release tool's own commits.
func changes(commits []commit) []change {
	var out []change
	for _, c := range commits {
		ch, ok := parseChange(c)
		if !ok || (ch.Type == "chore" && ch.Scope == "release") {
			continue
		}
		out = append(out, ch)
	}
	return out
}

// renderChangelog returns the changelog section for version: breaking
// changes first, then one section per type in changelogSections. repo is
// the repository's web URL, used for links; date is YYYY-MM-DD.
func renderChangelog(repo, version, date string, chs []change) string {
	var b strings.Builder
	if semver.MatchString(version) {
		fmt.Fprintf(&b, "## [%s](%s/releases/tag/go/%s) - %s\n", version, repo, version, date)
	} else {
		fmt.Fprintf(&b, "## %s\n", version) // such as "Unreleased"
	}
	item := func(ch change, note bool) {
		if ch.Scope != "" {
			fmt.Fprintf(&b, "- **%s:** %s", ch.Scope, ch.Description)
		} else {
			fmt.Fprintf(&b, "- %s", ch.Description)
		}
		if ch.PR != 0 {
			fmt.Fprintf(&b, " ([#%d](%s/pull/%d))", ch.PR, repo, ch.PR)
		} else {
			fmt.Fprintf(&b, " ([%s](%s/commit/%s))", short(ch.Hash), repo, ch.Hash)
		}
		b.WriteString("\n")
		if note && ch.Note != "" {
			r := []rune(ch.Note)
			fmt.Fprintf(&b, "\n  %s%s\n\n", strings.ToUpper(string(r[0])), string(r[1:]))
		}
	}
	section := func(title string, keep func(change) bool, note bool) {
		first := true
		for _, ch := range chs {
			if !keep(ch) {
				continue
			}
			if first {
				fmt.Fprintf(&b, "\n### %s\n\n", title)
				first = false
			}
			item(ch, note)
		}
	}
	section("Breaking changes", func(ch change) bool { return ch.Breaking }, true)
	for _, s := range changelogSections {
		section(s.title, func(ch change) bool { return !ch.Breaking && ch.Type == s.typ }, false)
	}
	if !strings.Contains(b.String(), "\n### ") {
		b.WriteString("\nNo user-facing changes.\n")
	}
	return strings.TrimRight(blankLines.ReplaceAllString(b.String(), "\n\n"), "\n") + "\n"
}

const changelogHeader = `# Changelog

All modules of this repository are released together with one version
(see [RELEASING.md](RELEASING.md)). The release tool writes each section
from the Conventional Commits merged since the previous release.
`

// prependChangelog adds section above the newest release in the
// changelog at path, creating the file if needed.
func prependChangelog(path, section string) error {
	old, err := os.ReadFile(path) //nolint:gosec // path is the repository's changelog
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s := string(old)
	if s == "" {
		s = changelogHeader
	}
	head, rest, found := strings.Cut(s, "\n## ")
	if found {
		rest = "## " + rest
	}
	out := strings.TrimRight(head, "\n") + "\n\n" + section
	if rest != "" {
		out += "\n" + rest
	}
	return os.WriteFile(path, []byte(out), 0o600) //nolint:gosec // path is the repository's changelog
}

// checkBump reports an error when chs contain a breaking change that
// version, released after prev, is too small for: a new major version from
// v1 on, at least a new minor version before.
func checkBump(prev, version string, chs []change) error {
	if prev == "" {
		return nil
	}
	var breaking []string
	for _, ch := range chs {
		if ch.Breaking {
			breaking = append(breaking, ch.Subject)
		}
	}
	if len(breaking) == 0 {
		return nil
	}
	p, v := versionParts(prev), versionParts(version)
	ok := v[0] > p[0] || (p[0] == 0 && v[1] > p[1])
	if ok {
		return nil
	}
	need := "a new major version"
	if p[0] == 0 {
		need = "at least a new minor version"
	}
	return fmt.Errorf("%s needs %s after %s for breaking changes: %s", version, need, prev, strings.Join(breaking, "; "))
}

// repoURL returns the web URL of the repository from the core module path,
// such as https://github.com/Arif9878/common for github.com/Arif9878/common/go.
func repoURL(corePath string) string {
	return "https://" + strings.TrimSuffix(corePath, "/go")
}
