# 0035 — The workspace on Kubernetes

- Status: Accepted; extends [ADR 0025](0025-the-agent-as-a-guest.md) and
  [ADR 0034](0034-ssh-inside-a-websocket.md)
- Date: 2026-08-14, amended 2026-10-05 (ephemeral values, enrolment) and
  2026-10-07 (several replicas)
- Current answer: a StatefulSet of `replicas` independent workspaces, one per
  node by default, behind the headless Service it names. Clients stay on a
  replica through `sessionAffinity: ClientIP` on the collective Service (and an
  ingress annotation for ingress-nginx); nothing in the client chooses one.

> The ingress is the way in, so the deployment needs no load balancer and no
> node port. Everything else follows from each pod owning its storage.

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

**A StatefulSet, not a Deployment.** Two pods must never hold the same graph
directory. A rolling Deployment starts the replacement before the old pod is
gone; a StatefulSet terminates first, and its volume claim templates give each
volume a name that outlives the pod.

**Several replicas are several workspaces (2026-10-07).** Asked for so
autoscaled CI runners (ADR 0050) are not bound to one pod and one node.

- Each replica owns its state and graph volumes: host key, uids, tokens,
  enrolled keys, accounts' daemons, ephemeral runs. Only the keys Secret is
  shared. Nothing is replicated, so this spreads load and is not failover.
- `serviceName` is `<fullname>-headless` (`clusterIP: None`,
  `publishNotReadyAddresses: false`, ports `ssh` 2222 and `ws` 2280, the
  agent's own since the name resolves to pod IPs): a stable name per pod and
  `_ssh._tcp`/`_ws._tcp` SRV records.
- **Stickiness is the Service's, not the client's.** The collective Service
  has `sessionAffinity: ClientIP`, timeout 86400s (the API's maximum), so a
  client that already uses the collective name needs no change. Rejected:
  DNS or SRV selection in the client (a `dns+srv://` address). It would put
  replica choice and its persistence in every client for what kube-proxy
  already does. Costs: clients behind one SNAT address share a replica, and
  one idle past the timeout or whose replica restarted may be re-pinned.
- **ingress-nginx ignores Service affinity**, so the chart documents
  `nginx.ingress.kubernetes.io/upstream-hash-by: "$remote_addr"` and NOTES
  warns without it. Not refused at render: other controllers have their own.
- **Host keys stay per replica.** A client trusts the first key it sees for a
  name, so a wrong landing is refused rather than served by another
  workspace. A shared key would make that silent.
- **An invite names its own pod.** A token redeems only where it was minted,
  so with `replicas` above 1 and no `publicURL`, `WORKSPACE_PUBLIC_URL` is
  `ssh://$(POD_NAME).<headless>.<ns>.svc:2222`, expanded by the kubelet. It is
  reachable only inside the cluster; enrolment for everybody goes through the
  Secret.
- **`podAntiAffinity: hard` by default**, on `kubernetes.io/hostname`. The
  scheduler's default spreading allows several replicas on one node, which
  defeats the point. `soft` and `none` exist; an explicit `affinity` wins.
- **No byte-identical render at one replica.** The headless Service, the
  affinity and `POD_NAME` are always there, so there is one code path.

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

**No Role, no ClusterRole, and no token in the pod.** The agent never talks to
the Kubernetes API. It runs a daemon, provisions unix accounts and serves SSH,
and a ServiceAccount with nothing bound to it is the whole of what it needs.
`automountServiceAccountToken: false` goes with that, because an enrolled
account gets a shell in this container as its own uid and a projected token is
mounted at mode 0644: without it, a key enrolled to run containers also holds
whatever the namespace's default ServiceAccount can do.

**The chart names the image for the per-account daemons.** Those daemons inherit
the workspace's storage driver, and stock `docker:dind` does not carry
fuse-overlayfs. Compose and Swarm discover the right image through `elevate`, by
inspecting the container they are running in; a pod cannot do that, so the chart
passes `WORKSPACE_DIND_IMAGE`. Found by the cluster test, in a restart loop that
said `exec: "fuse-overlayfs": executable file not found in $PATH`.

**Ephemeral accounts (ADR 0050) are values checked at render time.**
`ephemeral.*` renders the four `WORKSPACE_EPHEMERAL_*` variables only while an
account is listed, yields to an `env` entry naming the same variable, and
refuses a bad grace, a `maxClients` below 1 or an account with no key, rather
than leaving an agent that will not start.

## Consequences

- **Upgrading from 0.9.0 or earlier needs one manual step (2026-10-07).**
  `serviceName` is immutable, so the chart `lookup`s the StatefulSet and
  refuses, naming `kubectl delete sts <name> --cascade=orphan`; the new
  StatefulSet adopts the pod and the claims `state-<sts>-0` and
  `graph-<sts>-0`. CI installs the released 0.9.0 chart and checks the claim
  UIDs and the host key survive. `--reuse-values` renders with the old chart's
  defaults, so it is refused in favour of `--reset-then-reuse-values`.
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
- **Enrolling a device needs no `helm upgrade` (2026-10-05).** `kubectl exec
  <statefulset>-0 -- remote-dockerd token create --account alice` mints a token
  whose invite names `publicURL`, by default `wss://<ingress.host>/`, passed as
  `WORKSPACE_PUBLIC_URL` (ADR 0051). The redeemed key lands in
  `enrolled_keys.d` on the state volume; the `authorizedKeys` Secret stays the
  operator's and read-only. Still no Kubernetes API: the token is a file on that
  volume. CI redeems one through the ingress and checks the Secret is unchanged.
