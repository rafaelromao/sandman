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
   The workflow then refreshes any pending release branch from current `main`,
   even when Release Please reports that its generated release content is
   unchanged. This includes merged test, documentation, and CI repairs without
   changing their SemVer effect. The refresh uses the release credential and
   an expected-head guard; conflicts or concurrent head changes fail visibly
   instead of force-updating the branch. A branch that already contains `main`
   is left alone, so an unchanged run does not restart validation unnecessarily.
3. Review the generated release change request. It is the authorization point
   for the next version and release notes. Release Please branch updates run the
   exhaustive `Full Regression - Linux` suite and the focused `Native
   Compatibility` suite (four parallel arch-specific jobs: `Native
   Compatibility (linux/amd64)` on `ubuntu-latest`, `Native
   Compatibility (darwin/amd64)` on `macos-15-intel`, `Native Compatibility
   (darwin/arm64)` on `macos-15`, and `Native Compatibility (linux/arm64)` on
   `ubuntu-24.04-arm`). Their results appear on the release change request.
   Reviewers must not merge a failed or incomplete suite. Check the SHA-bound
   scenario and prerequisite reports described in `testing.md`, including the
   independent unit/browser, smoke, integration and live-provider results.
   Release Please now calculates later RC versions from merged release-bearing
   Conventional Commits.
4. Merging the release change request triggers fresh CI, regression and native
   validation of the exact merged main SHA. Prior release-branch results cannot
   authorize publication of this new commit.
5. After the publication gate succeeds, the publisher creates an immutable tag
   and draft release, sets up the patched Go toolchain from `.go-version`, and
   runs GoReleaser `v2.10.1` with `release --clean --draft`. Finalization verifies
   all four archives and checksums before exposing the release. Go `1.25.0` from `go.mod`
   remains the language minimum; `.go-version` selects the patched build toolchain.

If Release fails at startup with no jobs, inspect its workflow annotations first.
The reusable CI call must grant `contents: read`, `actions: read` and
`pull-requests: read`; GitHub validates nested job permissions even when the
candidate condition would skip validation. A startup failure also prevents
Release Please maintenance from refreshing the pending branch. After merging a
workflow repair, dispatch Release on current `main` to refresh that branch;
rerunning an old run retains its old workflow source.

To diagnose a release, inspect the Release run's publication gate, the SHA-bound
validation reports, then the draft and GoReleaser output. A failed or incomplete
gate prevents tag and release creation. See recovery instructions below.

Automatic live validation uses OpenCode and requires only `OPENCODE_API_KEY`.
Claude provider E2Es are manual: dispatch `Full Regression - Linux` with
`run_claude` enabled. Only that optional job receives
`CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`); token-only authentication
works on a clean runner. Claude results do not gate automatic release publication.

The configured release targets are:

- `sandman_<version>_linux_amd64.tar.gz`
- `sandman_<version>_linux_arm64.tar.gz`
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

`CHANGELOG.md` is a curated, repository-owned changelog. It contains the
reconstructed `0.2.0` entry and the `Unreleased` section, and the pull-request
template asks contributors to update it for user-facing changes. Release Please
reads this file when preparing the bootstrap release, so its generated notes
must preserve the curated entry.

The RC1 bootstrap override was removed after `v1.0.0-rc.1` was verified.
Release Please now calculates subsequent RC versions automatically from merged
release-bearing Conventional Commits and the prerelease strategy. Maintainers
continue to own the curated changelog and release contributors should update it
in the same change request as user-facing behavior.

## Version Output

Both commands use the same build version and prefix it with `sandman`:

```console
$ sandman --version
sandman 1.0.0-rc.1
$ sandman version
sandman 1.0.0-rc.1
```

The sources of that value differ by build path:

| Build path | Version value |
|------------|---------------|
| Release binary | GoReleaser injects `.Version` into `main.version`; the release tag is `v1.0.0-rc.1`, while GoReleaser's version value is displayed without the tag's `v` prefix (for example `1.0.0-rc.1`) |
| `make build` | `VERSION` may be supplied explicitly; otherwise it comes from `git describe --tags --always --dirty`, so it can be `v1.0.0-rc.1`, a commit hash, or a dirty description |
| `go install ./cmd/sandman` | The Makefile is bypassed; a local checkout normally reports Go's `(devel)` build-info version, while a module install from a versioned source can report a pseudo-version; the final fallback is `dev` |
| `go install github.com/rafaelromao/sandman/cmd/sandman@v1.0.0-rc.1` | Go embeds the requested module version, normally `v1.0.0-rc.1` |

The intentional policy is to preserve the `v` prefix on Git tags and Go module
versions, while accepting the GoReleaser and local-build differences described
above. A release artifact's version output should therefore be checked with
`sandman --version`, not inferred from the archive filename alone.

The prompt guidance repeats the title/history distinction so agents do not
mistake the CI title gate for release calculation. Its wording was checked
against the regex in `AGENTS.md`, `.github/workflows/go.yml`, the Release Please
config, and the version command tests; no sandbox behavior change is claimed.

## Toolchain Determinism

The production release job explicitly installs the Go version declared by
`.go-version` and pins GoReleaser to `v2.10.1`. Both steps require the publication
gate to succeed and are skipped for an already public release. The pull-request
snapshot workflow shares these toolchain selections.

## First RC Verification

`v1.0.0-rc.1` has been verified: GitHub shows the tag and release as a
prerelease, all four platform archives and `checksums.txt` are present, and the
released binary reports version `1.0.0-rc.1`. Leave the manifest at the
Release Please-generated RC1 state; the next release-bearing merged history
determines the next version automatically.

### Validated-source publication and recovery

Release Please creates/refreshes PRs with `skip-github-release: true`, independently
of release validation failures. The Release workflow recognizes a candidate only
when the main commit changes `.release-please-manifest.json` from its first parent.
The immutable merged SHA, rather than release-branch evidence, runs the reusable
CI, regression and native workflows. These expensive checks remain outside the
ordinary PR path. The production release job explicitly installs the pinned Go
and GoReleaser versions after the publication gate succeeds.

Only after that gate succeeds does publication create an immutable tag and draft
release. GoReleaser uses `release.use_existing_draft: true` to upload to the prepared
draft, with `release.draft: true` and `--draft` retaining draft state; finalization downloads every asset and
verifies exactly the four platform archives against `checksums.txt` before making
the draft public. A failed validation creates no tag/release; failed packaging or
upload leaves a draft, never a complete public release. Existing tags must resolve
to the candidate SHA. Tags are never moved and assets are never overwritten.
Publication runs serialize without cancelling validation. Later main commits do
not change the candidate's SHA; strict up-to-date PR protection handles concurrent
merges before admission (see testing.md for live enforcement verification).

Rerun the failed Release run, or dispatch Release at the same main release commit.
A fully published release at that immutable tag is a no-op. An empty draft can be
retried. A partially uploaded draft fails closed: inspect the failed run, confirm
the tag SHA, then remove only that draft's partial assets and rerun. Never delete
or replace assets of an already public release. Do not dispatch a later unrelated
main commit expecting it to publish an earlier version; rerun the original run.
If a tag points elsewhere, stop and investigate rather than force-update it.
The manual recovery operation must retain the same reviewed source and rerun all
validation. GitHub Actions logs retain the run ID/attempt and SHA of each required
job; regression/native artifacts retain scenario, platform and tool evidence.
