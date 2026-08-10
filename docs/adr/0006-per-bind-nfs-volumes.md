# 0006. Per-bind NFS volumes, not one workspace mount

- Status: Accepted; the `~/workspace` convenience mount is superseded by
  [ADR 0018](0018-one-way-to-do-each-thing.md)
- Date: 2026-08-07
- Supersedes part of the consequences of [ADR 0003](0003-client-serves-workspace-mounts.md)

## Context

The original design mounts the client's **working directory**, once, at
`~/workspace` inside the workspace container, and relies on the daemon sharing
that mount namespace so bind mounts resolve without translation.

Two problems, one fatal.

**Bind sources outside the served root cannot be expressed at all.**
`-v D:\data:/data`, or a Compose file referencing `../shared`, has nowhere to
resolve to. The single mount is not merely inconvenient here; it is the limit
of what the design can say. This is the fatal one.

**A replacement mount is invisible to running containers.** This was discovered
by experiment during the original build, and it is worth restating precisely
because it is counter-intuitive. Mount propagation (`rslave`) carries mounts
made *inside* the workspace directory into the container. It does not carry a
new mount placed *at* the workspace directory itself — that event belongs to
the parent's peer group, which the container never joined. The container
silently keeps the old, now-empty mount. It does not error; it sees an empty
directory.

The original response was to avoid remounting: make `workspace-mount`
idempotent, derive the tunnel port from the uid so it is stable across
reconnects, and have `--force` print a warning that every container using the
workspace must be restarted. That is a correct response to the constraint, and
`test/propagation.sh` pins both behaviours. But both clients then call
`workspace-mount --force` unconditionally when starting a session, because a
fresh NFS server has freshly generated file handles — so the warning fires on
a path where nobody reads it.

## Decision

Do not mount the share into the workspace's namespace at all. Instead, the
proxy converts **each bind mount into its own Docker volume**, backed by NFS
through Docker's built-in `local` driver:

```yaml
driver: local
opts:
  type:   nfs
  o:      addr=127.0.0.1,port=30000,mountport=30000,nfsvers=3,nolock,soft,…
  device: ":/m/<share id>"
```

No volume plugin is involved; the `local` driver has wrapped `mount(8)` for
this purpose for years. The daemon performs the mount in its own namespace when
the container starts, on the loopback address where the reverse tunnel listens.

A single convenience mount at `~/workspace` is kept, for the interactive shell
only. (That mount and the shell are both gone as of ADR 0018. The rest of this
record stands -- in particular the `rslave` finding below, which was found by
experiment and outlives the mount it justified.)

## Consequences

- **Bind sources anywhere on the client now work** — another drive, above the
  working directory, or unrelated to it. Combined with ADR 0007 this is what
  makes the client a general Docker endpoint rather than a working-directory
  tool.
- **The propagation problem stops existing.** The mount belongs to the
  container, not to a shared parent, so nothing has to propagate anywhere. A
  container that needs a fresh share gets one by restarting — which is an
  ordinary operation with an obvious mental model, unlike a remount that
  running containers cannot observe.
- `workspace-mount`, `workspace-umount`, `sudoers.workspace` and the `--force`
  warning are no longer needed for containers. What remains of them serves only
  the interactive shell.
- **Do not reintroduce the host-namespace mount to "simplify" this.** The
  paragraph above about `rslave` is the reason. It was found by experiment, it
  costs real debugging time to rediscover, and the failure mode is an empty
  directory rather than an error.
- `test/propagation.sh` still governs the `~/workspace` shell mount and is
  retained, with its stated purpose narrowed to that.
- The daemon now mounts NFS once per container rather than once per session, so
  a broken tunnel surfaces as a container that fails to start with a mount
  error. That is a clearer failure than a container that starts and sees
  nothing, but it is a different one, and it needs its own test.
- Volumes we create must be identifiable so they can be garbage collected, and
  a volume we did not create must never be removed. Hence the `rd-` prefix and
  `IsManagedVolume`.
- **"In use" cannot be asked of the daemon alone.** It counts a volume as in
  use once a container names it, which is strictly after the volume is created
  -- so every rewrite passes through a window where the volume exists and looks
  collectable. The collector runs in exactly that window, because the
  connection it rides on is opened lazily by the request creating the volume.
  Losing the race is silent: a missing named volume is RECREATED by the daemon
  as an empty local one, so the container starts with an empty directory
  instead of the project. `rewrite.Guard` closes it -- the share registry is
  the second source of truth, and one lock spans registering a share and
  creating its volume. Found by `test/integration.sh` section 16, after the
  test for something else had been written.
