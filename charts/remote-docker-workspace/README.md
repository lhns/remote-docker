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
| `persistence.graph.size` | `50Gi` | images and containers |
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

## Uninstall

```bash
helm uninstall ws --namespace remote-docker
```

The PersistentVolumeClaims are left behind, as Helm leaves all volume claim
templates. Delete them deliberately:

```bash
kubectl delete pvc -n remote-docker -l app.kubernetes.io/name=remote-docker-workspace
```
