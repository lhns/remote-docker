# remote-docker-workspace Helm chart

A remote Docker workspace: one privileged pod running dockerd and an SSH agent,
reached from a laptop through an ordinary Ingress. Directories on the developer's
own machine are mounted into containers running here, over NFS through the
tunnel, so nothing is copied or synced.

## Install

```bash
helm install ws oci://ghcr.io/lhns/charts/remote-docker-workspace \
  --version 0.2.1 \
  --namespace remote-docker --create-namespace \
  --set ingress.host=ws.example.com \
  --set-file authorizedKeys.alice=$HOME/.ssh/id_ed25519.pub
```

The pod is privileged, because dockerd sets up its own bridge and iptables rules
and mounts NFS in its own namespace. On a cluster with Pod Security admission,
label the namespace or the pod is never admitted:

```bash
kubectl label namespace remote-docker pod-security.kubernetes.io/enforce=privileged
```

Then, from a machine with no Docker installed:

```bash
remote-docker remote enroll                      # prints the key to enrol above
remote-docker remote create dev --host wss://ws.example.com --user alice
docker run --rm -v ${PWD}:/w alpine:3 ls /w      # /w is this machine's directory
```

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
| `authorizedKeys` | `{}` | one entry per account; **the entry name is the unix account** |
| `existingSecret` | `""` | use a Secret you manage instead |
| `perUserDind` | `true` | a dockerd per account (ADR 0019), or one shared (ADR 0012) |
| `dockerdArgs` | `--storage-driver=fuse-overlayfs` | see below |
| `dindImage` | `""` | the image an account's daemon runs; empty means this chart's |
| `persistence.graph.size` | `50Gi` | images and containers. Applies only when the claim is created; see Growing a volume |
| `persistence.graph.existingClaim` | `""` | mount a claim you own instead of generating one |
| `persistence.state.existingClaim` | `""` | the same, for the state volume |
| `persistence.state.size` | `1Gi` | host keys and the uid map |
| `ingress.enabled` | `true` | |
| `ingress.host` | `""` | **required** when the ingress is enabled |
| `service.type` | `ClusterIP` | SSH is not published; the ingress is the way in |

`values.yaml` carries the reasoning for each; the three worth knowing before you
install are below.

## The storage driver

`fuse-overlayfs` is the default because **overlay2 refuses to start** on Ceph- and
NFS-backed volumes, which is what many default StorageClasses hand out, and the
failure is a daemon that never comes up rather than a warning. On a cluster with
local or block volumes set `dockerdArgs: ""`, which is overlay2 and faster.

Whatever you choose, the per-account daemons inherit it, so the image they run
has to carry it. That is why `dindImage` defaults to this chart's own image:
stock `docker:dind` has no `fuse-overlayfs` and dies in a restart loop.

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
