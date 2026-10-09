# Releasing

All Go modules of this repository are released together, with one version. Today these are `go` and `go/fx`; any integration module split out later is picked up automatically.

```sh
cd go
make release VERSION=v0.5.0           # dry run: prints the tags it would create
make release VERSION=v0.5.0 PUSH=1    # release
```

The tool (`internal/release`) runs these steps:

1. **Find and order the modules.** It finds every `go.mod` under `go/` and orders the modules so each comes after the modules it requires. Core comes first and `go/fx` last.
2. **Bump internal requirements.** A module that requires another module of this repository gets that requirement set to the new version, then `go mod tidy`. The tool commits this on `main`. Consumers ignore `replace` directives, so a published module must require released versions.
3. **Tag.** It tags every module at that commit: `go/v0.5.0`, `go/fx/v0.5.0`, and so on.
4. **Push and publish.** It pushes `main` and the tags, then creates a GitHub release per tag with generated notes. The core release is marked latest.

It refuses to run unless you're on a clean `main` and every tag is new. It also rejects v2 and later until the module paths carry the `/vN` suffix Go requires.

**After a release:**

- **Release notes.** Edit the generated notes on GitHub: name the behavior changes and how to upgrade.
- **Remote URL.** If pushing to `origin` doesn't work from your machine, pass a URL instead: `make release VERSION=… PUSH=1 REMOTE=https://github.com/Arif9878/common.git`.

**Versioning:**

- **Before v1.0:** a minor version may change behavior, and the release notes say how to upgrade.
- **Patch versions:** a patch only fixes bugs or updates dependencies.

**Dependency updates.** Dependabot opens weekly PRs for every module, plus one for GitHub Actions. It skips this repository's own modules, because the release tool bumps those.
