# Releasing

All Go modules of this repository are released together, with one version: the core module `go`, the integration modules (`go/messaging/kafka`, `go/datastore/postgres`, …) and `go/fx`. A new module is picked up automatically.

```sh
cd go
make release VERSION=v0.5.0           # dry run: prints the tags it would create
make release VERSION=v0.5.0 PUSH=1    # release
```

The tool (`internal/release`) runs these steps:

1. **Find and order the modules.** It finds every `go.mod` under `go/` and orders the modules so each comes after the modules it requires. Core comes first; for example `go/datastore/postgres` comes before `go/idempotency/pgstore`, which requires it.
2. **Bump internal requirements.** A module that requires another module of this repository gets that requirement set to the new version, then `go mod tidy`. The tool commits this on `main`. Consumers ignore `replace` directives, so a published module must require released versions.
3. **Tag.** It tags every module at that commit: `go/v0.5.0`, `go/messaging/kafka/v0.5.0`, `go/fx/v0.5.0`, and so on.
4. **Push and publish.** It pushes `main` and the tags, then creates a GitHub release per tag with generated notes. The core release is marked latest.

Before changing anything, the tool runs these checks, and the dry run shows their result:

| Check | Fails when |
|---|---|
| Working tree, branch and tags | the tree has changes, you're not on `main`, or a tag already exists |
| `main` matches the remote | local `main` is behind or ahead of the remote's `main` (pull or push first) |
| CI on HEAD | a GitHub check on the commit failed, is still running, or hasn't started (needs `gh`; `-skip-ci` turns it off) |
| API compatibility | `gorelease` finds incompatible API changes and the version only bumps the patch number. Before v1.0, incompatible changes need at least a minor version. New API in a patch release is reported but allowed. Modules without a previous release are skipped. `-skip-api-check` turns it off. |

It also rejects v2 and later until the module paths carry the `/vN` suffix Go requires.

`-trailer "Co-Authored-By: Name <email>"` adds a trailer to the release commit; the flag can be repeated.

**After a release:**

- **Release notes.** Edit the generated notes on GitHub: name the behavior changes and how to upgrade.
- **Remote URL.** If pushing to `origin` doesn't work from your machine, pass a URL instead: `make release VERSION=… PUSH=1 REMOTE=https://github.com/Arif9878/common.git`.

**Versioning:**

- **Before v1.0:** a minor version may change behavior, and the release notes say how to upgrade.
- **Patch versions:** a patch only fixes bugs or updates dependencies.

**Dependency updates.** Dependabot opens weekly PRs for every module, plus one for GitHub Actions. It skips this repository's own modules, because the release tool bumps those.

**Developing across modules.** Every `go.mod` has `replace` directives pointing at the other modules in this repository, so a change in core is visible to the integration modules at once. When adding a module, copy that `replace` block into its `go.mod` and add the new module to every other `go.mod`'s block.
