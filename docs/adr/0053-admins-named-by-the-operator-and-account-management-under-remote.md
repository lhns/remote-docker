# 0053 — Admins named by the operator, and account management under remote

- Status: Accepted
- Date: 2026-10-05, amended 2026-10-06 (`user rm --purge`; token withdrawal
  before the keys; `key rm` of an operator key in `authorize`; 2026-10-07,
  `WORKSPACE_USER_TOKENS`)
- Amended 2026-10-07: `WORKSPACE_USER_TOKENS=false` (default true) makes
  `token.create` admin-only. It is a fact on `check` (`AdminOnlyTokens`),
  decided in `authorize` before the quota. `token.ls` and `token.rm` of one's
  own tokens, `key.*` and the operator's `remote-dockerd token create` are
  unchanged.

## What forced it

After ADR 0051 only the operator mints tokens, inside the workspace
(`kubectl exec … remote-dockerd token create`). Adding your own second
machine, removing somebody, or seeing who is enrolled all needed a shell in the
workspace as root. Most of that belongs to the people using it.

## The decision

- **`WORKSPACE_ADMINS` names the admins**, comma-separated, each folded like a
  key file's name. Read at start; only the operator edits it. A name need not
  be enrolled yet, which is how the first admin arrives: name alice, mint
  `remote-dockerd token create --account alice`, alice redeems it.
- **`workspace-enrol`**, in `core/enrol` with the redeem (ADR 0021's rule):
  JSON `Request` in, one `Reply` out, on a connection that authenticated as an
  account. The caller is that account (`sessionAccount`), never a field.
  `route` dispatches it before `handleSession`, so it is the same on every
  platform.
- **One pure policy**, `authorize(check)` in `agent/internal/sshd/enrolpolicy.go`,
  and one table test of it. Each operation gathers the facts, asks, then acts.

### Operations

| op | CLI | non-admin | admin |
|---|---|---|---|
| `whoami` | | yes | yes |
| `token.create` | `remote token create [--account X \| --unbound] [--expires] [--note]` | bound to self only, none if `WORKSPACE_USER_TOKENS=false` | anybody, or unbound |
| `token.ls` / `token.rm` | `remote token ls`, `remote token rm <id>` | tokens bound to self | all |
| `user.ls` | `remote user ls` | no | yes |
| `user.rm` | `remote user rm <account> [--purge] [-f]` | no | see below |
| `key.ls` / `key.add` / `key.rm` | `remote key ls\|add\|rm [--account X]` | own keys | anybody's |

A refusal is one line and a fix: `only an admin can create a token for another
account (you are bob)` / `fix: ask an admin, or the operator: \`remote-dockerd
token create --account carol\``. The exception is `token rm` of a token the
caller may not remove: that is `no token <id>`, as for one that does not exist,
since saying "another account's" would confirm the id.

### What even an admin cannot do

| refused | override |
|---|---|
| `user rm` of yourself | none: ask another admin |
| removing the last admin who holds a key, by `user rm` or `key rm` | none: an operator would have to edit `WORKSPACE_ADMINS` to recover |
| `user rm` of an account an operator directory enrols | none: remove it there (ADR 0052) |
| `key rm` of a key an operator directory holds | none, likewise |
| `key rm` of your connected key, or your last key | `-f` |
| `user rm` while the account's daemon runs containers, or cannot say | `-f` |

### `user rm`

1. authorize, then ask `Daemons.Running` (`-f` skips it);
2. revoke every token bound to the account, which would otherwise bring it
   back, including one a redemption has claimed: that redemption then fails
   at `Claim.Done` and takes its key out again. Before step 3, so a
   redemption that completes first has its key removed with the file;
3. `RemoveAccountFile`, which syncs, and the sweep closes the account's
   connections (ADR 0028);
4. in per-account mode, drop the account's ephemeral runs (ADR 0050);
5. `Daemons.Reset(purge=false)`: the daemon container goes.

Kept: `rd-dind-<account>-lib`, the home directory, the unix user, the uid and
the port records. A new bound token for the name brings the account back as it
was; an unbound one never takes the name (the uidmap keeps it).

- Removing an admin still named in `WORKSPACE_ADMINS` succeeds and says so,
  `fix: remove bob from WORKSPACE_ADMINS`: the name keeps the rights for
  whoever is enrolled under it next.
- `daemons.Targets` gained `Running` and `Reset` for this, and `Shared`
  implements them, so no use site asks which mode it is in (ADR 0019). On the
  shared daemon (ADR 0012) `Running` is 0, `Reset` does nothing, and the reply
  says the daemon's containers, images and volumes were not touched.
- A step after step 3 that fails is a notice in the reply; the keys are
  already revoked.

### `user rm --purge`

The same checks and steps, `-f` included, with step 5 as
`Daemons.Reset(purge=true)`: `rd-dind-<account>-lib` goes too, only if it
carries both the managed and the account label. Then:

1. `Store.Purge`: the home directory, only if it is the `Home` the store
   recorded for that account, a real directory owned by its uid and with
   nothing mounted inside; never a path a daemon reported. Then
   `Provisioner.Remove`, `userdel` by uid, of a user `Ensure` would adopt;
2. `Ports.Forget`: the account's `clientports` lines.

The uidmap entry is KEPT, so the name and the uid are never reused, and a
later bound token gets the same uid on a clean slate: an empty daemon, a new
home. On the shared daemon both steps still run.

### The invite from a client

`remote token create` builds the invite on the client: the address is the
client's own transport (`ssh://host:port`, `ws://`, `wss://`), and the host key
is the one this connection verified against `known_hosts`. The agent returns
only the token.

### A reply to the key it revokes

`key rm -f` of the connected key revokes the connection carrying the request.
`serveEnrol` holds the connection through the sweep and closes it a second after
the reply, so the reply arrives.

### Audit

Every change logs `component=audit op by account token key dir from`. Never a
secret.

### An old agent

It runs `workspace-enrol` in a shell, which answers 127; the client prints
`this workspace's agent predates account management` / `fix: ask its operator
to upgrade it`.

## What it costs

- **An admin is close to root on the host.** An unbound token creates an
  account, which is a privileged dind (ADR 0019). Name admins as you would give
  a shell on the host. A non-admin's token only adds a key to their own account,
  which they could already use.
- **Names in `WORKSPACE_ADMINS` are rights before they are accounts.** An admin
  name nobody holds is reserved: an unbound token never takes it. A bound token
  from the operator or an admin is the only way in.
- **Removal is revocation, not deletion.** Disk is freed only by `--purge`,
  which cannot be undone, or by an operator's `remote-dockerd daemons reset
  --purge`. Either way only a bound token brings the name back.
- **The containers check asks the daemon**, and a daemon that cannot answer
  counts as running. `-f` is then the only way through.
- **A hand edit in the enrolled directory is still not an admin operation**: it
  bypasses the lock (ADR 0052) and nothing here audits it.

## Verification

- Unit: `TestAuthorize` (the whole policy as a table), and one test per
  operation over a real SSH connection in `agent/internal/sshd/manage_test.go`,
  including the reply to a revoking `key rm` and the token of a removed account
  being revoked; the client's commands against a fake workspace, including the
  127 answer, in `client/cmd/remote-docker/manage_test.go`.
- End to end: `test/per-user-dind.sh` section 16 (bob refused other tokens,
  bob's own token enrols a machine, `user rm` refused without `-f` then
  removing `rd-dind-bob` and keeping `-lib`, bob back with the same uid and his
  images, alice not removing alice, removing carol naming `WORKSPACE_ADMINS`,
  the last admin's key protected); `test/two-clients.sh` section 3b (the second
  machine enrols by a token the first made).
- Purge, unit: `TestUserRemovePurgeTakesTheStorageAndThePortsAndKeepsTheUID`
  (sshd), `TestAPurgeRemovesOnlyTheLabelledVolume` (daemons), and in
  `core-agent/accounts` `TestPurgeRemovesTheHomeAndTheUserAndKeepsTheUID`, the
  `removeHome` refusals, `TestRemoval` and `TestForgetDropsOneAccountsMachines`.
  End to end: `per-user-dind.sh` section 17 (`user rm bob --purge -f` removes
  `-lib`, the home, the unix user and the `clientports` lines, the uidmap keeps
  bob, and a bound token brings bob back with the same uid and no images).
