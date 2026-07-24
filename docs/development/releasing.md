# Releasing Sandman

This guide is the canonical release procedure for maintainers. It describes the
automation committed in this repository; it does not replace the focused work
on release authorization ([#2389](https://github.com/rafaelromao/sandman/issues/2389)),
the first-release bootstrap ([#2390](https://github.com/rafaelromao/sandman/issues/2390)),
Go prerequisites ([#2391](https://github.com/rafaelromao/sandman/issues/2391)), or
binary installation ([#2392](https://github.com/rafaelromao/sandman/issues/2392)).

## Release Flow

1. Merge a change request into `main` after the required CI checks pass.
2. The `Release` workflow runs on the push to `main`. Release Please reads
   `release-please-config.json` and `.release-please-manifest.json`, then opens
   or updates a release change request when the commit history contains a
   release-bearing change.
3. Review the generated release change request. It is the authorization point
   for the next version and release notes. Merge it through the normal branch
   protection rules.
4. Release Please creates the version tag and published GitHub Release when
   the release change request is merged. The initial manifest version is
   `1.0.0`, so the first tag is expected to be `v1.0.0` after the bootstrap
   work in [#2390](https://github.com/rafaelromao/sandman/issues/2390).
5. When Release Please reports `release_created == 'true'`, the same workflow
   checks out the workflow's triggering commit with full history and runs
   GoReleaser with `release --clean`. GoReleaser uploads the archives and
   `checksums.txt` to the GitHub Release. The workflow does not explicitly pass
   the generated tag to `actions/checkout`.

To diagnose a release, inspect the Release workflow run, the Release Please
change request and its generated tag, then the GoReleaser step and the assets
on the GitHub Release page. GoReleaser is conditional: a failed or skipped
Release Please release means it does not run.

The configured release targets are:

- `sandman_<version>_linux_amd64.tar.gz`
- `sandman_<version>_darwin_amd64.tar.gz`
- `sandman_<version>_darwin_arm64.tar.gz`
- `checksums.txt`

The exact archive names are produced by `.goreleaser.yml`; use the assets on
the published release rather than guessing a version string.

## Versioning Policy

Release Please uses the Conventional Commits in merged commit history to
decide whether a release is needed and which SemVer component changes. The
repository's current policy is:

| Commit | Version effect |
|--------|----------------|
| `feat:` | Minor release |
| `fix:` or `perf:` | Patch release |
| `feat!:` / `fix!:` / `perf!:` | Major release |
| `docs:`, `chore:`, `refactor:`, `test:`, `build:`, `ci:`, `revert:` | No release bump |

Use a `!` after the type or scope for a breaking `feat`, `fix`, or `perf`.
Conventional Commits also defines a `BREAKING CHANGE:` footer. Release Please
can use either marker when parsing a release-bearing commit; either marker
changes that commit's effect to a major release. For this repository, use the
accepted `feat!`, `fix!`, or `perf!` title form as well so the commit history,
change-request title, and CI policy agree. The title gate does not accept
arbitrary breaking variants such as `docs!:`.

This is separate from pull-request title validation. The
`semantic-pull-request` job validates the title of every change request using
the regex in [`AGENTS.md`](../../AGENTS.md#branching-and-versioning-rules),
including non-release-bearing types. That check does not inspect the complete
commit history and does not itself create a release. Release Please evaluates
the merged history after changes land on `main`.

The exact title policy is also summarized in
[`CONTRIBUTING.md`](../../CONTRIBUTING.md#conventional-commits) and enforced by
[`.github/workflows/go.yml`](../../.github/workflows/go.yml).

## Changelog Ownership

`CHANGELOG.md` is currently a curated, repository-owned changelog. It contains
the bootstrapped `1.0.0` entry and the `Unreleased` section, and the pull-request
template asks contributors to update it for user-facing changes. Release
Please is configured with `changelog-path: /dev/null`, so it must not be
described as the writer of `CHANGELOG.md`.

The first-release decision and any one-time bootstrap behavior belong to
[#2390](https://github.com/rafaelromao/sandman/issues/2390). Until that work
changes this policy, maintainers own the curated changelog and release
contributors should update it in the same change request as user-facing
behavior. Do not duplicate the bootstrap procedure here.

## Version Output

Both commands use the same build version and prefix it with `sandman`:

```console
$ sandman --version
sandman v1.0.0
$ sandman version
sandman v1.0.0
```

The sources of that value differ by build path:

| Build path | Version value |
|------------|---------------|
| Release binary | GoReleaser injects `.Version` into `main.version`; the release tag is `v1.0.0`, while GoReleaser's version value is displayed without the tag's `v` prefix (for example `1.0.0`) |
| `make build` | `VERSION` may be supplied explicitly; otherwise it comes from `git describe --tags --always --dirty`, so it can be `v1.0.0`, a commit hash, or a dirty description |
| `go install ./cmd/sandman` | The Makefile is bypassed; a local checkout normally reports Go's `(devel)` build-info version, while a module install from a versioned source can report a pseudo-version; the final fallback is `dev` |
| `go install github.com/rafaelromao/sandman/cmd/sandman@v1.0.0` | Go embeds the requested module version, normally `v1.0.0` |

The intentional policy is to preserve the `v` prefix on Git tags and Go module
versions, while accepting the GoReleaser and local-build differences described
above. A release artifact's version output should therefore be checked with
`sandman --version`, not inferred from the archive filename alone.

The prompt guidance repeats the title/history distinction so agents do not
mistake the CI title gate for release calculation. Its wording was checked
against the regex in `AGENTS.md`, `.github/workflows/go.yml`, the Release Please
config, and the version command tests; no sandbox behavior change is claimed.

## Maintainer Follow-ups

The release jobs currently rely on the runner's environment for Go rather than
having an explicit `actions/setup-go` step in the release job, and both release
workflows use the floating GoReleaser `version: latest`. Follow
[#2397](https://github.com/rafaelromao/sandman/issues/2397) to add explicit Go
setup and pin a reviewed GoReleaser version before treating the release workflow
as fully hermetic. Those changes are deliberately not made by this issue.
