# 0057 — Orphaned account daemons are reported, and removed only when asked

- Status: Accepted
- Date: 2026-10-19
- Current answer: with a daemon per account, an account that is no longer
  enrolled but still owns a daemon container or graph volume is an orphan. By
  default the agent logs it and exposes a gauge. `WORKSPACE_PRUNE_ORPHANS=delete`
  removes the daemon container `WORKSPACE_PRUNE_ORPHANS_AFTER` (30d) after the
  agent first saw the account gone, and the graph volume only with
  `WORKSPACE_PRUNE_ORPHAN_VOLUMES=true`. `WORKSPACE_PRUNE_DRY_RUN=true` logs
  instead of removing anything pruning would.

## What forced it

- ADR 0056 prunes what a daemon holds, not the daemon. Removing an account's key
  file leaves its `rd-dind-<account>` container and `-lib` volume (ADR 0019)
  behind; the volume is the largest object on the graph disk, and nothing
  collects it.
- That volume also holds the account's compose volumes, databases included. It
  is the one thing here that cannot be rebuilt, so removing it must be a
  separate, deliberate switch.
- An agent that has not read its keys, or lost them, sees every account as
  gone.

## The decision

- **An orphan** is a footprint (daemon container of this workspace, or a graph
  volume carrying the `remote-docker.daemon` label) whose account is in neither
  the operator's keys directories nor the enrolled one: the same set
  `accounts.Store` serves.
- **The clock** is the first time the agent saw the account gone, kept in
  `<state>/prune/_orphans.json` and written by rename. The key file leaves no
  date, and a container's stop time says nothing about when the account left. An
  upgrade starts the clock at the upgrade, and an account that returns drops out,
  so a second departure starts a new grace.
- **Reported, not removed, by default.** Report mode is one warning per orphan
  per agent run, plus `remote_docker_orphaned_daemons{account}`. Delete mode uses
  `Targets.Reset` (ADR 0053), which already removes the container and, with
  purge, only a volume carrying this project's labels.
- **A volume without a container of this workspace is only ever reported.**
  Volumes carry no workspace label, so on a parent daemon shared by two
  workspaces it could be the other one's.
- **A running daemon is removed too** once the grace is over: nobody can reach
  it, and its containers' published ports are reachable only through the
  account.
- **Nothing is done with no enrolled account at all**, with a warning.
- **Dry run** is a decorator over the docker calls plus one check before the
  orphan removal, so it follows exactly the code a real run takes. Volumes are
  still never a target of image pruning.

## What it costs

- **Delete mode with `WORKSPACE_PRUNE_ORPHAN_VOLUMES` destroys data** for an
  account that was removed by mistake and not restored within the grace. The
  default is off, and the chart documents it next to the value.
- **A deployment that removes then re-adds keys across a longer window than the
  grace loses the daemon.** Pick a grace longer than any such window.
- **The grace is not the account's age.** An account removed long before an
  upgrade is reported from the upgrade and removed one grace later.
- **A shared daemon has no orphans to find**, and the check is not run.
