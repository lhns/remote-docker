# 0056 — The agent prunes unused images and build cache, by age and under disk pressure

- Status: Accepted
- Date: 2026-10-19
- Current answer: one loop in the agent, over the workspace's own daemon and
  every running account daemon. Images no container has used for
  `WORKSPACE_PRUNE_AFTER` (7d) go, removed by reference and never forced; build
  cache unused that long goes; above `WORKSPACE_PRUNE_ABOVE` (80%) cache and
  then images go oldest first. Never volumes, never a daemon started to be
  pruned.

## What forced it

- A workspace's graph volume fills over time. Nothing here pruned, and with a daemon per account (ADR 0019) an operator has no one place
  to prune from.
- Docker's own `image prune --filter until=` measures an image's build or pull
  date. A base image built a year ago and used this morning is not old, so it
  cannot express "unused for a week".
- Age alone does not cap the disk: the data that filled the volume was newer
  than any sensible age.
- Volumes cannot be a target. `rd-*` share volumes hold pointers, and an
  `rd-*-cache` volume behind a union looks unused to the daemon because it is
  bound by path (ADR 0044) while holding a container's unsynced writes.

## The decision

- **In the agent, not a chart CronJob.** Only the agent knows each account's
  daemon (`daemons.Targets`), and compose and VM deployments get it too. It is
  on by default everywhere, including a VM whose daemon is the machine's own
  dockerd.
- **Which daemons.** A daemon per account: each enrolled account through
  `Targets.Lookup`, never `Ensure`, so a stopped daemon is not started to be
  pruned; plus the parent daemon, for images only. The parent's containers are
  the account daemons and are never touched, and `WORKSPACE_DIND_IMAGE` is never
  a candidate: with no account left nothing references it, and losing it means a
  re-pull, or a failure behind a mirror. A shared daemon gets every rule.
- **Image age is when a container last used it.** The agent keeps, per daemon, a
  map of image id to last seen in `<state>/prune/<account>.json`, saved by
  renaming a complete file into place. An image is seen when a container,
  running or stopped, references it at a look (every 5 minutes), or a container
  `create` event since the previous look names it. An event names the image as
  typed, resolved through the current tag, digest and id map.
  An image met for the first time, a missing record and an unreadable one all
  mean now, so an upgrade or a lost file only delays removal.
- **Removal is by reference and never forced.** `docker image rm <repo:tag>` for
  every tag, the digest references for an untagged image, the id for a bare
  one. `-f` would remove images that stopped containers use; without it Docker
  refuses them, and an image counts as pruned only when every reference went.
- **Build cache**: `docker builder prune --force --filter until=<age>`. In
  moby `until` is the canonical name of `unused-for` and becomes BuildKit's
  KeepDuration, so it measures last use (`builder/builder-next/builder.go`,
  read 2026-10-19). The parent's cache is not pruned: nothing builds there.
- **Stopped containers** only with `WORKSPACE_PRUNE_CONTAINERS`: stopped longer
  than the age by `State.FinishedAt`, or `Created` for one that never started,
  `docker rm` without `-v`. Never on the parent of a daemon per account.
- **Pressure.** Every 5 minutes the agent measures, with statfs, the filesystem
  of the parent's `DockerRootDir`, which holds every account's `-lib` volume.
  Above the threshold: build cache unused for an hour on every daemon, then
  unused images across all daemons, oldest last-seen first, re-measuring after
  each, until below. An image seen within the last hour is never removed, so one
  just pulled for a build in progress stays. If usage is still above with
  nothing left, one warning per crossing.
- **Reporting.** One info line per daemon per sweep that removed something.
  Bytes are the statfs free-space delta, never `docker system df`, which walks
  every layer and is slow on a large store over network storage. The counter
  `remote_docker_prune_reclaimed_bytes_total{account,reason}` follows ADR 0054.
- **Settings**: `WORKSPACE_PRUNE_AFTER` (Go duration or whole days; `0` or `off`
  disables), `WORKSPACE_PRUNE_ABOVE` (percent; `off` disables),
  `WORKSPACE_PRUNE_CONTAINERS`; a bad value stops the start. `..._INTERVAL` and
  `..._FLOOR` are test hooks. Whole days (`7d`) are parsed by one helper that
  `WORKSPACE_EPHEMERAL_GRACE` shares.
- **Packages.** `agent/internal/prune` is the policy and the record codec, pure
  and table-tested; `dockercli.Pruning` runs the docker commands; the loop is
  `cmd/remote-dockerd/prune.go`.

## What it costs

- **On by default means an image somebody still wanted can be pulled again.**
  The first sweep after an upgrade counts everything as used, so age pruning
  starts a full period later, but the disk rule applies at once.
- **A container created between listing and removal can lose some tags.** The
  removal runs by reference, tag by tag, and the daemon refuses only the last
  one. Accepted: the image is then still present under its remaining names.
- **The event buffer is best effort.** Moby keeps only the last 256 events
  (`eventsLimit`, `daemon/events/events.go`, v28.5.2, read 2026-10-19; re-check with
  `gh api repos/moby/moby/contents/daemon/events/events.go?ref=v28.5.2 --jq .content | base64 -d | grep eventsLimit`), so
  `--rm` runs between two looks can be missed. The look is every 5 minutes, and
  an image used that way and then idle for the whole age is the case it can
  still lose.
- **Space is measured on the parent's data directory.** It is right in a
  container and on a VM, where the agent shares that filesystem; elsewhere the
  rule reports it cannot measure, once, and the age rules still run.
- **Pressure can remove what age would have kept.** That is its purpose; the
  floor of an hour is the only protection for an image nothing uses yet.
- **A volume is never reclaimed.** A full disk of volumes is the operator's to
  resolve, and the warning says nothing is left to prune.
