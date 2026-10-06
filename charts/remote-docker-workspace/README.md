# remote-docker-workspace Helm chart

A remote Docker workspace: one privileged pod running dockerd and an SSH agent,
reached from a laptop through an ordinary Ingress. Directories on the developer's
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
cosign verify ghcr.io/lhns/remote-docker-workspace:0.2.1 \
  --certificate-identity-regexp '^https://github.com/lhns/remote-docker/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'

cosign verify ghcr.io/lhns/charts/remote-docker-workspace:0.2.1 \
  --certificate-identity-regexp '^https://github.com/lhns/remote-docker/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

## Values

| key | default | |
|---|---|---|
| `image.repository` | `ghcr.io/lhns/remote-docker-workspace` | |
| `image.tag` | `""` | the chart's appVersion |
| `authorizedKeys` | `{}` | one entry per account; **the entry name is the account a client logs in as** (unix user `rd-<name>`) |
| `existingSecret` | `""` | use a Secret you manage instead |
| `publicURL` | `""` | the address an enrolment token's invite names; empty is `wss://<ingress.host>/`, or `ws://` without TLS |
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
because the volume need not detach first — but it buys nothing while there is
one replica.

Losing the state volume is not losing a cache: the SSH host keys change, so
every client that has connected before reports REMOTE HOST IDENTIFICATION HAS
CHANGED, and each account's uid moves, which moves its reverse-tunnel port,
which strands the volumes named after the old one.

## One replica, and what follows

The workspace is a StatefulSet of one. `helm upgrade` therefore stops the old
pod before starting the new one — which is what you want, since two pods must
never hold the same graph — and a node failure needs the volume to detach before
the pod can reschedule. Neither is a bug to report; both are consequences of one
writer owning the storage.

## Growing a volume

**Expanding the volume itself needs nothing from this chart.** A PVC's requested
size is mutable wherever the StorageClass sets `allowVolumeExpansion`, and with
a CSI driver that supports it the filesystem grows while the pod keeps running:

```bash
kubectl -n <ns> patch pvc graph-<release>-0   -p '{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}'
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
Claims made by the old template are named `graph-<release>-0` and
`state-<release>-0`; naming those in `existingClaim` adopts them where they are,
with no data movement.

## Uninstall

```bash
helm uninstall ws --namespace remote-docker
```

The PersistentVolumeClaims are left behind, as Helm leaves all volume claim
templates. Delete them deliberately:

```bash
kubectl delete pvc -n remote-docker -l app.kubernetes.io/name=remote-docker-workspace
```
