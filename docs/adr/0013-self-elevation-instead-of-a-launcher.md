# 0013. Self-elevation instead of a launcher container

- Status: Accepted
- Date: 2026-08-07

## Context

The workspace must be privileged: it runs its own dockerd, sets up its own
bridge and iptables rules, and mounts NFS in its own namespace (ADR 0001).
Docker Swarm cannot run privileged tasks.

The established workaround is a **launcher container** — a Swarm service that
starts the real container through the host's Docker socket, outside Swarm's
orchestration. `deploy/swarm-launcher.yml` used
`registry.gitlab.com/egos-tech/swarm-launcher` for exactly this.

It works, and it costs a third-party image on the trust path of every
deployment. That image can start privileged containers on the node; a
compromise or an unwelcome update there is a compromise of the host. It is also
another thing to pull, pin, and explain.

`lhns/docker-vpn-gateway` solves the same problem with an `elevate` script: the
service starts unprivileged and relaunches *itself* privileged through the host
socket. The launcher and the payload are the same image, so there is no extra
trust path.

## Decision

Build elevation into the agent. The Swarm service runs our own image
unprivileged with `command: elevate`; it inspects itself through the host's
Docker socket and starts a privileged copy of itself outside Swarm, forwarding
signals and exiting with the child's status.

Three details carry it.

**The child joins the task's network namespace** (`--network container:<self>`).
This is not an optimisation — it is the only reason the scheme works. Swarm
publishes the port into the *task's* namespace, and a container started outside
Swarm has no published port of its own. This is exactly what the launcher
achieved with `LAUNCH_NETWORK_MODE: container:{{.Task.Name}}`.

**The child must not inherit the host's Docker socket.** A privileged container
holding it gives every enrolled workspace user root on the node — they have
access to the inner daemon by design, and the inner daemon would then reach the
outer one. It would also collide with dind's own socket at the same path. So
the deployment mounts it at `/var/run/host-docker.sock`, and the plan drops any
mount of it — matched by destination *and* by name, so a deployment that puts
it somewhere unexpected is still caught.

A blanket `--volumes-from` would have been shorter and would have carried the
socket straight through. Mounts are therefore replicated explicitly.

**We identify ourselves from a Swarm template.** `WORKSPACE_SELF:
"{{.Task.Name}}"` — Swarm expands templates in environment values, and Docker
accepts a name wherever it accepts an id. `docker-vpn-gateway` parses
`/proc/self/mountinfo`, which we keep only as a fallback for plain Docker.

## Consequences

- No third-party image on the trust path. One image, built here, does both
  jobs.
- **The host Docker socket mount is the whole trust boundary**, and it should
  be read that way: whoever can deploy this stack can already start privileged
  containers on the node. Elevation does not widen that — it avoids a second
  image doing it.
- The privileged container is **not** managed by Swarm. It is `--rm`, signals
  are forwarded so stopping the task stops it, and a stale one is removed by
  name before launching. Without the signal forwarding a stopped task would
  leave the child holding the published port and the replacement task would
  fail to bind it — a failure that looks like a port conflict and is not.
- Guarded against recursion: the child carries `WORKSPACE_ELEVATED=1` and
  elevation refuses when it is set. A misconfiguration that would otherwise
  fork containers until the node fell over costs one environment variable to
  prevent.
- The planning logic is a pure function (`elevate.Plan`), so the rules above
  are unit tested without a daemon. The difference between a correct and a
  catastrophic invocation here is one flag, and it should be visible in a test.
- The image's build context is now the repository root rather than `image/`,
  since the agent is built from source. `docker build -f image/Dockerfile .`
- Swarm itself is not exercised in CI. The mechanism under test is `docker
  run`, which is; the Swarm wiring needs a real deployment to prove.
