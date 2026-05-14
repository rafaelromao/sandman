# ADR-0007: BuildToolsPreset and pinned init scaffolding

## Status

proposed

## Context

Sandman's current `sandman init` flow treats scaffold choice as language detection and mostly maps that choice to a base image like `golang:latest` or `python:latest`. The generated `.sandman/Dockerfile` does not yet encode the richer contract we want to rely on in container-backed sandboxes: a shared baseline tool layer, mainstream stack tooling, correct built-in Agent Provider installation, and pinned versions that can be reproduced later.

That gap has concrete costs. Mixed image families make shared setup brittle, built-in Agent Provider installation sources are inconsistent, and `latest` tags hide what version was actually chosen. We also need a clear explanation for why Sandman prefers scaffold-time recipes, `mise` as the first fallback for missing toolchains, and metadata-based drift validation instead of treating `.sandman/config.yaml` and `.sandman/Dockerfile` as two independent sources of truth.

## Decision

1. We will rename the scaffold-time concept from language selection to **BuildToolsPreset**. `sandman init` will use `--build-tools` and will remove `--lang` and `--from-image`.
2. Sandman will ship first-class **BuildToolsPreset** recipes for `generic`, `go`, `node`, `python`, `rust`, `java`, `ruby`, `dotnet`, and `elixir`.
3. Each **BuildToolsPreset** will generate a Debian-family `.sandman/Dockerfile` with a shared baseline tool layer, preset-specific tooling, `mise`, and the selected built-in Agent Provider installed from a normalized, versioned source.
4. `sandman init` will resolve exact pinned versions at scaffold time. It may accept logical `--tool-version` selectors and interactive choices, prefer repo-declared version hints when present, use live latest/LTS lookups when possible, and fall back to a bundled version catalog when offline or lookup fails.
5. Scaffolded Dockerfiles are user-owned after init. Sandman may stamp metadata comments for the selected Agent Provider and **BuildToolsPreset**, validate drift only when those markers exist, and treat metadata-free Dockerfiles as opaque custom files.
6. Sandman will enforce the supported `Agent Provider x BuildToolsPreset` matrix with real container build coverage in CI.

## Consequences

### Positive

- Scaffolded container images become reproducible because Sandman writes exact pins instead of floating `latest` values.
- Sandman can offer a stronger out-of-box container experience without hard-coding one fixed set of language base images.
- The Dockerfile contract becomes clearer: scaffold once, then either keep Sandman metadata for validation or remove it and fully own the file.
- Built-in Agent Providers get one normalized installation model across supported presets.

### Negative

- First container builds will be heavier and slower because every preset includes a shared baseline and `mise`.
- Sandman must maintain version-source metadata, compatibility rules, and an offline fallback catalog.
- The initial preset list is narrower than the older language detector list, so some stacks move to the `generic` path until explicit support is added.

### Neutral

- Container-backed sandboxing still builds from `.sandman/Dockerfile`; this ADR changes how that file is scaffolded, not the runtime source of truth.
- Advanced users still retain a manual escape hatch by editing or replacing the scaffolded Dockerfile.
