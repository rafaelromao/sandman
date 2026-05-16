# Sandbox Modes

Sandman provides two sandbox strategies for executing agents:

- **Worktree** — git worktree isolation only
- **Container-backed** (default) — Docker or Podman container with filesystem and process isolation

## Worktree

```bash
sandman run --sandbox worktree 42
```

In worktree mode, each `AgentRun` gets a dedicated git worktree at `.sandman/worktrees/`. The agent process runs directly on the host with no additional isolation beyond the worktree checkout. This is the lightest-weight option but provides no filesystem or process isolation.

- One worktree per `AgentRun`
- No container runtime required
- Suitable for trusted agents or local testing
- `container_capacity` and `max_containers` have no effect

## Container-backed

```bash
sandman run --sandbox podman 42 43   # default
sandman run --sandbox docker 42 43
```

In container mode, each `AgentRun` executes inside a Docker or Podman container. Sandman manages a pool of containers per batch, each hosting one or more worktrees.

### Container scheduling

Container scheduling is governed by two config fields:

- **`container_capacity`** — maximum concurrent `AgentRun`s inside one `ContainerSandbox`
- **`max_containers`** — maximum number of `ContainerSandbox` instances

#### How it works

1. When a batch starts, Sandman creates containers as needed to accommodate the active `AgentRun`s
2. Within each container, up to `container_capacity` runs execute concurrently
3. If all containers are at capacity and `max_containers` has been reached, additional runs queue until a slot frees up
4. When a container becomes idle (all its runs finish), it may be reused by later eligible runs in the same batch
5. All containers are stopped automatically when the batch completes

#### Auto mode (`max_containers: 0`)

```yaml
container_capacity: 4
max_containers: 0
```

Sandman creates the minimum number of containers needed for the currently active `AgentRun`s. For example, with 6 active runs and `container_capacity=4`:

- Container 1 hosts runs 1-4
- Container 2 hosts runs 5-6

No containers sit idle. Additional containers are created or removed as runs start and finish, up to the minimum needed.

#### Fixed pool (`max_containers: N`)

```yaml
container_capacity: 2
max_containers: 3
```

Sandman creates up to 3 containers, each hosting up to 2 concurrent runs (max 6 concurrent). If the batch has 8 runnable issues, the last 2 queue until capacity frees up.

#### Capacity of 1 (`container_capacity: 1`)

```yaml
container_capacity: 1
```

Each container hosts exactly one `AgentRun`. This provides the strongest per-run isolation at the cost of higher overhead.

### Setup requirements

- Podman or Docker installed on the host
- The sandbox user must have permission to run containers
- Container images are built from `.sandman/Dockerfile` (created by `sandman init`)

### Trade-offs

| Aspect | Worktree | Container |
|--------|----------|-----------|
| Setup | None | Requires container runtime |
| Isolation | None beyond git | Filesystem and process isolation |
| Overhead | Minimal | Container startup and resource usage |
| Sharing | Runs directly on host | Config dirs and files mounted from host |
| Auth | Supports keychain auth | File-based auth only (keychain rejected) |
