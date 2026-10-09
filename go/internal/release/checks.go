package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// checkRemote fails unless HEAD is the tip of main on remote, so a release
// never tags a stale or unpushed main.
func checkRemote(root, remote string) error {
	out, err := output(root, "git", "ls-remote", remote, "refs/heads/main")
	if err != nil {
		return err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return fmt.Errorf("%s has no main branch", remote)
	}
	head, err := output(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if fields[0] != head {
		return fmt.Errorf("local main (%s) is not %s's main (%s); pull or push first", short(head), remote, short(fields[0]))
	}
	return nil
}

// checkCI fails unless every GitHub check run on HEAD finished
// successfully (or was skipped), using gh.
func checkCI(root string) error {
	head, err := output(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	repo, err := output(root, "gh", "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner")
	if err != nil {
		return fmt.Errorf("find the GitHub repository (needs gh; pass -skip-ci to release without CI): %w", err)
	}
	js, err := output(root, "gh", "api", "repos/"+repo+"/commits/"+head+"/check-runs?per_page=100")
	if err != nil {
		return err
	}
	return ciVerdict(js, short(head))
}

func ciVerdict(js, commit string) error {
	var resp struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal([]byte(js), &resp); err != nil {
		return fmt.Errorf("decode check runs: %w", err)
	}
	if len(resp.CheckRuns) == 0 {
		return fmt.Errorf("commit %s has no CI checks yet; wait for CI", commit)
	}
	var problems []string
	for _, c := range resp.CheckRuns {
		switch {
		case c.Status != "completed":
			problems = append(problems, c.Name+" is "+c.Status)
		case c.Conclusion != "success" && c.Conclusion != "skipped" && c.Conclusion != "neutral":
			problems = append(problems, c.Name+" "+c.Conclusion)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("CI on %s is not green: %s", commit, strings.Join(problems, "; "))
	}
	return nil
}

// previousVersion returns the highest released version of m below
// version, ignoring pre-releases, or "" if m was never released.
func previousVersion(root string, m module, version string) (string, error) {
	out, err := output(root, "git", "tag", "-l", m.Dir+"/v*")
	if err != nil {
		return "", err
	}
	best := ""
	for _, tag := range strings.Fields(out) {
		v := strings.TrimPrefix(tag, m.Dir+"/")
		if strings.Contains(v, "/") || strings.Contains(v, "-") || !semver.MatchString(v) {
			continue // a nested module's tag, or a pre-release
		}
		if compareVersions(v, version) < 0 && (best == "" || compareVersions(v, best) > 0) {
			best = v
		}
	}
	return best, nil
}

// compareVersions compares release versions vX.Y.Z numerically.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	var p [3]int
	core, _, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	for i, s := range strings.SplitN(core, ".", 3) {
		p[i], _ = strconv.Atoi(s)
	}
	return p
}

// checkAPI runs gorelease on every module released before, failing when
// the API changes need a bigger version bump than version (for example an
// incompatible change in a patch release). gorelease's own diagnostics
// about the replace directives this repository uses for development are
// ignored; only its verdict on the version counts.
func checkAPI(root string, mods []module, version string) (deferred []module, err error) {
	var problems []string
	for _, m := range mods {
		base, err := previousVersion(root, m, version)
		if err != nil {
			return nil, err
		}
		if base == "" {
			fmt.Printf("  %-28s new module, no API check\n", m.Dir)
			continue
		}
		out, _ := combined(filepath.Join(root, m.Dir), "go", "run", "golang.org/x/exp/cmd/gorelease@"+goreleaseVersion,
			"-base="+base, "-version="+version)
		verdict, ok := apiVerdict(out, base, version)
		if !ok && len(m.Requires) > 0 && needsUnreleasedAPI(out, repoPrefix(mods)) {
			// gorelease ignores replace directives, so it builds m against
			// the released versions of the modules it requires, which lack
			// the API this release adds. Those modules are checked
			// themselves; m can only be checked once they are tagged.
			verdict, ok = "checked after tagging: uses API this release adds to "+strings.Join(m.Requires, ", "), true
			deferred = append(deferred, m)
			if c := section(out, "## incompatible changes"); c != "" {
				verdict, ok = "incompatible changes in the packages gorelease could load", !patchOnly(base, version)
			}
		}
		fmt.Printf("  %-28s %s → %s: %s\n", m.Dir, base, version, verdict)
		if !ok {
			problems = append(problems, m.Dir+": "+verdict+"\n"+section(out, "## incompatible changes"))
		}
	}
	if len(problems) > 0 {
		return nil, errors.New("API check failed (pass -skip-api-check to override):\n" + strings.Join(problems, "\n"))
	}
	return deferred, nil
}

// checkAPIAfterTag runs gorelease on the modules checkAPI deferred, once
// the release is tagged and pushed so the API they use can be downloaded.
// The release can no longer be stopped, so it reports problems instead of
// failing: an incompatible change the version doesn't allow needs a new
// release that restores the API, or one with a bigger version.
func checkAPIAfterTag(root string, deferred []module, version string) []string {
	var warnings []string
	for _, m := range deferred {
		base, err := previousVersion(root, m, version)
		if err != nil {
			warnings = append(warnings, m.Dir+": "+err.Error())
			continue
		}
		// Fetch this repository's modules straight from the remote: asked
		// seconds after the push, the module proxy and checksum database
		// would answer "not found" and cache that answer for everyone.
		out, _ := combinedEnv(filepath.Join(root, m.Dir), []string{"GOPRIVATE=" + repoPath(m)},
			"go", "run", "golang.org/x/exp/cmd/gorelease@"+goreleaseVersion, "-base="+base)
		verdict, ok := afterTagVerdict(out, base, version)
		fmt.Printf("  %-28s %s → %s: %s\n", m.Dir, base, version, verdict)
		if !ok {
			warnings = append(warnings, m.Dir+": "+verdict+"\n"+section(out, "## incompatible changes"))
		}
	}
	return warnings
}

// afterTagVerdict judges gorelease's report on a module that is already
// tagged. Without -version gorelease only lists the changes, so the
// policy of apiVerdict is applied to those lists.
func afterTagVerdict(out, base, version string) (string, bool) {
	if errs := section(out, "## errors in release version"); errs != "" || !strings.Contains(out, "# summary") {
		return "could not be checked:\n" + out, false
	}
	incompatible := section(out, "## incompatible changes") != ""
	switch {
	case incompatible && patchOnly(base, version):
		return "incompatible changes in a patch release", false
	case incompatible:
		return "incompatible changes (allowed by this minor or major version)", true
	case section(out, "## compatible changes") != "":
		return "compatible", true
	}
	return "no API changes", true
}

// goreleaseVersion pins gorelease, like the other tools in the Makefile.
const goreleaseVersion = "v0.0.0-20261007192929-f45ad48fbe92"

// apiVerdict applies the release policy to gorelease's output for base →
// version. gorelease follows semantic versioning, which allows anything in
// v0; this repository requires at least a minor version for incompatible
// changes before v1, as its release notes promise. New API in a patch
// release is reported but allowed.
func apiVerdict(out, base, version string) (string, bool) {
	summary, valid := goreleaseVerdict(out)
	if !valid {
		return summary, false
	}
	patch := patchOnly(base, version)
	switch {
	case section(out, "## incompatible changes") != "" && patch:
		return fmt.Sprintf("incompatible changes need at least v%d.%d.0", versionParts(version)[0], versionParts(version)[1]+1), false
	case section(out, "## compatible changes") != "" && patch:
		return "compatible, but adds API in a patch release (consider a minor version)", true
	case section(out, "## incompatible changes") != "":
		return "incompatible changes (allowed by this minor or major version)", true
	}
	return summary, true
}

// patchOnly reports whether version only bumps base's patch number.
func patchOnly(base, version string) bool {
	b, v := versionParts(base), versionParts(version)
	return b[0] == v[0] && b[1] == v[1]
}

// undefinedIdent matches a type-check error naming a missing identifier of
// another package, such as "undefined: errors.FieldError".
var undefinedIdent = regexp.MustCompile(`^\S+:\d+:\d+: undefined: \w+\.\w+$`)

// needsUnreleasedAPI reports whether gorelease failed only because the
// module uses packages or identifiers missing from the released versions
// of this repository's modules (whose paths start with prefix): every
// missing package is one of ours, and every type-check error is an
// undefined identifier of another package.
func needsUnreleasedAPI(out, prefix string) bool {
	found := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if _, pkg, ok := strings.Cut(line, "cannot find module providing package "); ok {
			if !strings.HasPrefix(pkg, prefix+"/") {
				return false
			}
			found = true
		}
	}
	errs := section(out, "## errors in release version")
	for _, line := range strings.Split(errs, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if !undefinedIdent.MatchString(line) {
			return false
		}
		found = true
	}
	return found
}

// repoPath returns the repository's import path prefix, such as
// github.com/Arif9878/common for the module github.com/Arif9878/common/go/fx
// in go/fx.
func repoPath(m module) string { return strings.TrimSuffix(m.Path, "/"+m.Dir) }

// repoPrefix returns the core module's path, the prefix of every module
// path of the repository.
func repoPrefix(mods []module) string {
	for _, m := range mods {
		if m.Dir == "go" {
			return m.Path
		}
	}
	return "\x00" // no core module: nothing matches
}

// goreleaseVerdict reads gorelease's summary: whether it considers version
// valid for the changes, and the line saying so.
func goreleaseVerdict(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "is a valid semantic version for this release"):
			return "compatible", true
		case strings.HasPrefix(line, "Suggested version:"),
			strings.Contains(line, "is not a valid semantic version"),
			strings.Contains(line, "cannot be released"):
			return line, false
		}
	}
	return "no verdict from gorelease:\n" + out, false
}

// section returns the lines under a gorelease heading such as
// "## incompatible changes", summed over packages, or "".
func section(out, heading string) string {
	var lines []string
	in := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
			in = strings.TrimSuffix(strings.TrimSpace(line), ":") == heading
		case in && strings.TrimSpace(line) != "":
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return strings.Join(lines, "\n")
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
