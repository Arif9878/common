# Releasing

All Go modules of this repository are released together, with one version: the core module `go`, the integration modules (`go/messaging/kafka`, `go/datastore/postgres`, …) and `go/fx`. A new module is picked up automatically.

```sh
cd go
make release VERSION=v0.5.0           # dry run: prints the tags it would create
make release VERSION=v0.5.0 PUSH=1    # release
```

The tool (`internal/release`) runs these steps:

1. **Find and order the modules.** It finds every `go.mod` under `go/` and orders the modules so each comes after the modules it requires. Core comes first; for example `go/datastore/postgres` comes before `go/idempotency/pgstore`, which requires it.
2. **Bump internal requirements and write the changelog.** A module that requires another module of this repository gets that requirement set to the new version, then `go mod tidy`. Consumers ignore `replace` directives, so a published module must require released versions. The modules under `examples/` get the same requirement bump (they are not tagged), so `go mod tidy` stays clean there. The tool also adds a section for the version to [`CHANGELOG.md`](CHANGELOG.md) (see below) and commits both on `main`.
3. **Tag.** It tags every module at that commit: `go/v0.5.0`, `go/messaging/kafka/v0.5.0`, `go/fx/v0.5.0`, and so on.
4. **Push and publish.** It pushes `main` and the tags, then creates a GitHub release per tag. The core release is marked latest and uses the changelog section as its notes; the other releases point to it.

Before changing anything, the tool runs these checks, and the dry run shows their result:

| Check | Fails when |
|---|---|
| Working tree, branch and tags | the tree has changes, you're not on `main`, or a tag already exists |
| `main` matches the remote | local `main` is behind or ahead of the remote's `main` (pull or push first) |
| CI on HEAD | a GitHub check on the commit failed, is still running, or hasn't started (needs `gh`; `-skip-ci` turns it off) |
| API compatibility | `gorelease` finds incompatible API changes and the version only bumps the patch number. Before v1.0, incompatible changes need at least a minor version. New API in a patch release is reported but allowed. Modules without a previous release are skipped. gorelease ignores `replace`, so a module using API that its in-repository requirements add in the same release (for example `go/fx` using a new core package) can't be loaded against them; its check runs after the tags are pushed, when that API can be downloaded. The release can't be stopped at that point, so a problem found there is reported (and the tool exits non-zero) to be fixed in a new release. `-skip-api-check` turns it off. |
| Version fits the commits | a commit since the previous release is breaking and the version is too small: before v1.0 it needs a new minor version, from v1 on a new major version |

It also rejects v2 and later until the module paths carry the `/vN` suffix Go requires.

`-trailer "Co-Authored-By: Name <email>"` adds a trailer to the release commit; the flag can be repeated.

**Changelog.** The section is written from the [Conventional Commits](https://www.conventionalcommits.org) merged since the previous release, so write commit subjects as `type(scope): description`:

| Commit | Section |
|---|---|
| `feat(fx): add Scheduler` | Features |
| `fix(kafka): …` | Fixes |
| `perf(cache): …` | Performance |
| `refactor!: …`, or any type with a `BREAKING CHANGE: <how to upgrade>` footer | Breaking changes, with the footer as the upgrade note |
| `docs`, `test`, `ci`, `chore`, `build`, `refactor`, `style` | left out |

The `commits` workflow fails a pull request whose title or commit subjects don't follow this format. Each entry links to the pull request that merged it, or to the commit for a direct push. `make changelog` prints the section for what's merged so far; the dry run of `make release` prints it too. To reword an entry, edit `CHANGELOG.md` and the GitHub release after releasing.

**After a release:**

- **Release notes.** Check the notes on GitHub, and add upgrade steps the commit footers don't cover.
- **Remote URL.** If pushing to `origin` doesn't work from your machine, pass a URL instead: `make release VERSION=… PUSH=1 REMOTE=https://github.com/Arif9878/common.git`.

**Versioning:**

- **Before v1.0:** a minor version may change behavior, and the release notes say how to upgrade.
- **Patch versions:** a patch only fixes bugs or updates dependencies.

**Dependency updates.** Dependabot opens weekly PRs for every module, plus one for GitHub Actions. It skips this repository's own modules, because the release tool bumps those.

**Developing across modules.** Every `go.mod` has `replace` directives pointing at the other modules in this repository, so a change in core is visible to the integration modules at once. When adding a module, copy that `replace` block into its `go.mod` and add the new module to every other `go.mod`'s block.
