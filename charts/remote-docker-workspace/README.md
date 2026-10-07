# remote-docker-workspace Helm chart

A remote Docker workspace: a privileged pod running dockerd and an SSH agent,
reached from a laptop through an ordinary Ingress. Several replicas are several
independent workspaces; see [Several replicas](#several-replicas). Directories on the developer's
own machine are mounted into containers running here, over NFS through the
tunnel, so nothing is copied or synced.

## Install

```bash
helm install ws oci://ghcr.io/lhns/charts/remote-docker-workspace \
  --namespace remote-docker --create-namespace \
  --set ingress.host=ws.example.com
```

The chart's version is the release's, set when it is published; `Chart.yaml`'s
is a placeholder. Without `--version`, helm installs the newest release;
add `--version <x.y.z>` to pin one (the Releases page lists them).

The pod is privileged, because dockerd sets up its own bridge and iptables rules
and mounts NFS in its own namespace. On a cluster with Pod Security admission,
label the namespace or the pod is never admitted:

```bash
kubectl label namespace remote-docker pod-security.kubernetes.io/enforce=privileged
```

Mint a single-use enrolment token for an account:

```bash
kubectl exec -n remote-docker ws-remote-docker-workspace-0 -- \
  remote-dockerd token create --account alice
```

It prints one line. Run it on a machine with no Docker installed, and that
machine enrols its own key:

```bash
docker remote create ws --token rdt1.eyJ1Ijoi...
docker run --rm -v ${PWD}:/w alpine:3 ls /w      # /w is this machine's directory
```

Keys can also be enrolled by file, with `--set-file
authorizedKeys.alice=$HOME/.ssh/id_ed25519.pub`.

To manage accounts without `kubectl exec`, name admins, then mint the first
admin's token as above:

```bash
helm upgrade ws <chart> --reuse-values --set 'admins={alice}'
```

alice then runs `docker remote token create --account bob`, `remote user ls`
and `remote user rm bob` from her own machine. Any account can run
`remote token create` to enrol another machine of its own.

## Verify the chart and image (cosign keyless)

```bash
cosign verify ghcr.io/lhns/remote-docker-workspace:<x.y.z> \
  --certificate-identity-regexp '^https://github.com/lhns/remote-docker/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'

cosign verify ghcr.io/lhns/charts/remote-docker-workspace:<x.y.z> \
  --certificate-identity-regexp '^https://github.com/lhns/remote-docker/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

## Values

| key | default | |
|---|---|---|
| `image.repository` | `ghcr.io/lhns/remote-docker-workspace` | |
| `image.tag` | `""` | the chart's appVersion |
| `replicas` | `1` | independent workspaces; see Several replicas |
| `podAntiAffinity` | `hard` | `hard`: one replica per node; `soft`: prefer that; `none`. Ignored when `affinity` is set |
| `authorizedKeys` | `{}` | one entry per account; **the entry name is the account a client logs in as** (unix user `rd-<name>`) |
| `existingSecret` | `""` | use a Secret you manage instead |
| `publicURL` | `""` | the address an enrolment token's invite names; empty is `wss://<ingress.host>/`, or `ws://` without TLS, and with `replicas` above 1 the pod's own name |
| `admins` | `[]` | accounts that manage every account under `remote` (`WORKSPACE_ADMINS`, ADR 0053); they need not be enrolled yet |
| `perUserDind` | `true` | a dockerd per account (ADR 0019), or one shared (ADR 0012) |
| `dockerdArgs` | `--storage-driver=fuse-overlayfs` | see below |
| `dindImage` | `""` | the image an account's daemon runs; empty means this chart's |
| `persistence.graph.size` | `50Gi` | images and containers. Applies only when the claim is created; see Growing a volume |
| `persistence.graph.existingClaim` | `""` | mount a claim you own instead of generating one |
| `persistence.state.existingClaim` | `""` | the same, for the state volume |
| `persistence.state.size` | `1Gi` | host keys, the uid map, keys enrolled with a token, and tokens |
| `ephemeral.accounts` | `[]` | accounts whose every client process is a client of its own; see CI runners below |
| `ephemeral.maxClients` | `8` | clients at once per such account, counting those in their grace period |
| `ephemeral.grace` | `2m` | how long a client's port and volumes outlive its last connection |
| `ephemeral.cleanupContainers` | `false` | also remove an expired client's containers and compose networks |
| `metrics.enabled` | `false` | serve Prometheus metrics (`WORKSPACE_METRICS_ADDR`, ADR 0054) and add the `prometheus.io/scrape`, `/port` and `/path` pod annotations. No ServiceMonitor. **Unauthenticated and on every interface of the pod**: account names and activity are readable to anything that reaches the port, so admit only your Prometheus with a NetworkPolicy |
| `metrics.port` | `9090` | the metrics port, named `metrics` on the container |
| `env` | `{}` | extra agent environment; an entry here wins over the variable a value above renders |
| `ingress.enabled` | `true` | |
| `ingress.host` | `""` | **required** when the ingress is enabled |
| `service.type` | `ClusterIP` | SSH is not published; the ingress is the way in |
| `service.sessionAffinityTimeout` | `86400` | how long the Service keeps a client address on one replica after its last connection; 86400 is the most Kubernetes accepts |

`values.yaml` carries the reasoning for each; the ones worth knowing before you
install are below.

## The storage driver

`fuse-overlayfs` is the default because **overlay2 refuses to start** on Ceph- and
NFS-backed volumes, which is what many default StorageClasses hand out, and the
failure is a daemon that never comes up rather than a warning. On a cluster with
local or block volumes set `dockerdArgs: ""`, which is overlay2 and faster.

Whatever you choose, the per-account daemons inherit it, so the image they run
has to carry it. That is why `dindImage` defaults to this chart's own image:
stock `docker:dind` has no `fuse-overlayfs` and dies in a restart loop.

## CI runners

Autoscaled runners share one key. List the account under `ephemeral.accounts`
and each runner gets its own tunnel port and volumes, removed `grace` after it
goes away:

```yaml
authorizedKeys:
  ci: |
    ssh-ed25519 AAAA... ci@runners
ephemeral:
  accounts: [ci]
```

Nothing is rendered while the list is empty. The chart refuses to render a
`grace` that is not a duration, a `maxClients` below 1, and an account with no
entry in `authorizedKeys`, unless `existingSecret` is set, since that Secret is
not read at render time. The main README's "CI with autoscaled runners" has the
rest, including what a standalone `docker compose` needs.

## Both volumes are ReadWriteOnce, for different reasons

**The graph volume must be.** Two dockerds sharing one graph directory corrupt
it. Even on storage that offers ReadWriteMany, leave this alone.

**The state volume happens to be.** The agent is its only writer.
`ReadWriteMany` is safe there, and makes rescheduling onto another node quicker
because the volume need not detach first. Each replica has a claim of its own,
so it is never shared between replicas either.

Losing the state volume is not losing a cache: the SSH host keys change, so
every client that has connected before reports REMOTE HOST IDENTIFICATION HAS
CHANGED, every key enrolled with a token is revoked, and each account's uid
moves, which moves its reverse-tunnel port,
which strands the volumes named after the old one.

## Several replicas

`replicas: 3` runs three workspaces, not one workspace three times. Each replica
has its own:

- state volume: host key, uids, enrolment tokens and keys enrolled with one;
- graph volume, and each account's daemon with its images, containers and
  volumes;
- ephemeral runs, so `ephemeral.maxClients` counts per replica.

The keys Secret is the only thing they share, so an account enrolled there
exists on every replica, with nothing else in common. This is for spreading
load, such as autoscaled CI runners (main README, "CI with autoscaled
runners"), and not for failover: a replica that goes away takes its work with
it.

**A client must stay on the replica it started on.** Two ways:

- **The collective name**, `<fullname>.<ns>.svc`, as with one replica. The
  Service has `sessionAffinity: ClientIP`, so kube-proxy sends each client
  address to one replica for `service.sessionAffinityTimeout` (default and
  maximum 86400s) after its last connection. Clients behind one SNAT address
  share a replica, which is correct and unbalanced. A client idle for longer
  than the timeout, or one whose replica restarted, can be sent to another
  replica, which it refuses: each replica has its own host key, so a wrong
  landing fails loudly rather than reaching somebody else's workspace.
- **A replica's own name**, `<fullname>-N.<fullname>-headless.<ns>.svc`, from
  the headless Service the StatefulSet names. It also publishes SRV records,
  `_ssh._tcp` and `_ws._tcp`, listing every ready replica. The ports there are
  the agent's own, 2222 and 2280, since a headless name resolves to pod IPs.

**The ingress ignores the Service's affinity.** ingress-nginx balances across
the pods itself, so with `replicas` above 1 pin each client address there too:

```yaml
ingress:
  annotations:
    nginx.ingress.kubernetes.io/upstream-hash-by: "$remote_addr"
```

`NOTES.txt` warns when it is missing. Another controller needs its own
equivalent, and only ingress-nginx is tested.

**Enrolment is per replica.** A token enrols a key on the replica that minted it
and nowhere else, so with `replicas` above 1 and no `publicURL`, its invite
names that pod, `ssh://<pod>.<fullname>-headless.<ns>.svc:2222`, which only a
client inside the cluster reaches. An explicit `publicURL` is used by every
replica as it stands, which is right only if it reaches the minting one. For
accounts that should exist everywhere, enrol through the keys Secret.

**Scaling up moves some clients.** A new replica starts empty. Going from one
replica to several restarts the first pod, since its invite address becomes its
own name, and that ends the Service's affinity for every client; through the
ingress, adding a replica re-hashes a share of the clients. A client sent to a
replica it has not seen refuses its host key. Point it at its replica's own
name, or remove and recreate its workspace entry to start over wherever it
lands. Ephemeral CI runners start fresh each time and are not affected.

**One replica per node, by default.** `podAntiAffinity: hard` requires
replicas on different nodes (`kubernetes.io/hostname`), because the scheduler's
own spreading allows two or three on one node, and one node's failure or load
would then take several workspaces. With fewer schedulable nodes than
replicas, the extra replica stays Pending and says why. `soft` prefers
spreading and allows doubling up; `none` leaves it to the scheduler; an
explicit `affinity` replaces all of this.

A StatefulSet stops a pod before starting its replacement, which is what you
want, since two pods must never hold the same graph, and a node failure needs
the volume to detach before the pod can reschedule. Both are consequences of
one writer owning each replica's storage.

### Upgrading from 0.9.0 or earlier

The StatefulSet's `serviceName` is now the headless Service, and Kubernetes
cannot change it in place, so the chart refuses the upgrade and names the fix:

```bash
kubectl delete sts ws-remote-docker-workspace -n remote-docker --cascade=orphan
helm upgrade ws oci://ghcr.io/lhns/charts/remote-docker-workspace \
  -n remote-docker --reset-then-reuse-values
```

`--cascade=orphan` leaves the pod and both claims in place; the new StatefulSet
adopts them by name, `state-<fullname>-0` and `graph-<fullname>-0`, and replaces
the pod once. The host key, enrolled keys and images are kept, which CI checks
against the released 0.9.0 chart. Use `--reset-then-reuse-values` (Helm 3.14
and later) rather than `--reuse-values`: the latter renders with the old
chart's defaults, which lack `replicas` and the values beside it, and the chart
refuses it by name.

## Growing a volume

**Expanding the volume itself needs nothing from this chart.** A PVC's requested
size is mutable wherever the StorageClass sets `allowVolumeExpansion`, and with
a CSI driver that supports it the filesystem grows while the pod keeps running:

```bash
kubectl -n <ns> patch pvc graph-ws-remote-docker-workspace-0 -p '{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}'
```

If `.status.capacity.storage` reaches the new size, it was online. If it parks
with a `FileSystemResizePending` condition, the node-side resize is waiting for
a remount and the pod has to restart.

What that does NOT do is change `persistence.graph.size` here, and a
StatefulSet's `volumeClaimTemplates` are immutable, so this chart cannot be
updated to agree with it. **That matters on the day the claim is recreated** --
a restore, a rebuild, a new cluster -- because the template is what it is
recreated from, and it would silently come back at the old size.

`existingClaim` is the way out. Create the PVC yourself, point the chart at it,
and its size is an ordinary field you edit wherever you keep it:

```yaml
persistence:
  graph:
    existingClaim: workspace-graph
```

`size`, `storageClass` and `accessModes` are then ignored for that volume: they
describe a claim this chart no longer makes. The two volumes are independent, so
supplying one and generating the other is fine.

**Adopting it on a running install costs one restart.** Removing a
`volumeClaimTemplate` is as immutable as changing one, so the StatefulSet has to
be recreated. Delete it with `--cascade=orphan` to keep the pod and the claims,
let Helm rebuild it, and expect the pod to be replaced once as it converges.
Claims made by the old template are named `graph-<statefulset>-0` and
`state-<statefulset>-0`, the StatefulSet being `ws-remote-docker-workspace` for
a release named `ws`; naming those in `existingClaim` adopts them where they
are, with no data movement.

## Uninstall

```bash
helm uninstall ws --namespace remote-docker
```

The PersistentVolumeClaims are left behind, as Helm leaves all volume claim
templates. Delete them deliberately:

```bash
kubectl delete pvc -n remote-docker -l app.kubernetes.io/name=remote-docker-workspace
```
