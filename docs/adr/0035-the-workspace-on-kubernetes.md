# 0035 — The workspace on Kubernetes

- Status: Accepted; extends [ADR 0025](0025-the-agent-as-a-guest.md) and
  [ADR 0034](0034-ssh-inside-a-websocket.md)
- Date: 2026-08-14

> The ingress is the way in, so the deployment needs no load balancer and no
> node port. Everything else follows from one pod owning its storage.

## What forced it

A workspace could be deployed with compose, with Swarm, or as a systemd unit on
a VM. On Kubernetes there was nothing, so anybody running one there wrote
manifests from the compose file and guessed at the parts that matter: which
volumes must persist, why the pod is privileged, and what happens when two pods
touch one graph directory.

Exposing it used to be the hard part. SSH on 2222 needs a TCP load balancer or a
node port, and many clusters offer neither to an ordinary namespace. ADR 0034
removed that: the tunnel is an HTTP upgrade, so an Ingress carries it like
anything else on 443.

## The decisions

**A StatefulSet of one, not a Deployment.** Two pods must never hold the same
graph directory. A rolling Deployment starts the replacement before the old pod
is gone; a StatefulSet terminates first, and its volume claim templates give
each volume a name that outlives the pod.

**Two volumes, and only one of the access modes is a rule.** The graph directory
is ReadWriteOnce because sharing it corrupts it. The state directory is
ReadWriteOnce because the agent is its only writer, and ReadWriteMany is safe
there if the storage prefers it. Both are configurable and the values say which
is which, because a reader cannot tell a rule from a default by looking.

**Nothing to reserve a path from.** The agent accepts the WebSocket upgrade on
any path (ADR 0034), so the Ingress takes the whole host and it does not matter
whether the controller strips a prefix.

**The Ingress is on by default and requires a host.** A workspace nobody can
reach is not a deployment, and an Ingress with no host matches every request
that arrives at the controller — so the chart refuses to render rather than
install that.

**Privileged, with no unprivileged mode to fall back to.** dockerd sets up its
own bridge and iptables rules and mounts NFS in its own namespace. A chart
cannot label a namespace it does not own, so `NOTES.txt` prints the Pod Security
label rather than letting the pod be rejected with no explanation.

**No Role and no ClusterRole.** The agent never talks to the Kubernetes API. It
runs a daemon, provisions unix accounts and serves SSH, and a ServiceAccount
with nothing bound to it is the whole of what it needs.

**The chart names the image for the per-account daemons.** Those daemons inherit
the workspace's storage driver, and stock `docker:dind` does not carry
fuse-overlayfs. Compose and Swarm discover the right image through `elevate`, by
inspecting the container they are running in; a pod cannot do that, so the chart
passes `WORKSPACE_DIND_IMAGE`. Found by the cluster test, in a restart loop that
said `exec: "fuse-overlayfs": executable file not found in $PATH`.

## Consequences

- **`helm upgrade` stops the pod before starting the new one**, and a node
  failure waits for the volume to detach. Both follow from one writer owning the
  storage, and both are worth knowing before an outage rather than during one.
- **The default storage driver is a guess about the cluster.** fuse-overlayfs is
  chosen because overlay2 refuses to start on Ceph- and NFS-backed volumes,
  which many default StorageClasses hand out, and that failure is a daemon that
  never comes up. On local or block storage `dockerdArgs: ""` is faster.
- **This is proven end to end, on every pull request.** kind, ingress-nginx, the
  chart, and the client reading a file from the runner inside a container in the
  cluster. That dockerd runs inside a pod inside a kind node at all was measured
  before the chart was written, because the alternative was a chart described as
  tested on the strength of `helm template`.
- **Only ingress-nginx is proven.** The chart's WebSocket annotations for other
  controllers are suggestions, and the values say so.
