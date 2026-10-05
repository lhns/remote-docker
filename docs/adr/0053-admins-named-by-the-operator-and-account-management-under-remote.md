# 0053 — Admins named by the operator, and account management under remote

- Status: Accepted. `user rm --purge` is to come; until then removal never
  deletes data.
- Date: 2026-10-05

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
| `token.create` | `remote token create [--account X \| --unbound] [--expires] [--note]` | bound to self only | anybody, or unbound |
| `token.ls` / `token.rm` | `remote token ls`, `remote token rm <id>` | tokens bound to self | all |
| `user.ls` | `remote user ls` | no | yes |
| `user.rm` | `remote user rm <account> [-f]` | no | see below |
| `key.ls` / `key.add` / `key.rm` | `remote key ls\|add\|rm [--account X]` | own keys | anybody's |

A refusal is one line and a fix: `only an admin can create a token for another
account (you are bob)` / `fix: ask an admin, or the operator: \`remote-dockerd
token create --account carol\``.

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
2. `RemoveAccountFile`, which syncs, and the sweep closes the account's
   connections (ADR 0028);
3. revoke every token bound to the account, which would otherwise bring it back;
4. `Daemons.Reset(purge=false)`: the daemon container goes.

Kept: `rd-dind-<account>-lib`, the home directory, the unix user, the uid and
the port records. A new bound token for the name brings the account back as it
was; an unbound one never takes the name (the uidmap keeps it).

- Removing an admin still named in `WORKSPACE_ADMINS` succeeds and says so,
  `fix: remove bob from WORKSPACE_ADMINS`: the name keeps the rights for
  whoever is enrolled under it next.
- On the shared daemon (ADR 0012) `Running` is 0 and `Reset` does nothing, and
  the reply says the containers were not touched.
- `daemons.Targets` gained `Running` and `Reset` for this; `Shared` implements
  them, so no use site asks which mode it is in (ADR 0019).

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
- **Removal is revocation, not deletion.** Disk is not freed until `--purge`
  exists, and an operator can still `remote-dockerd daemons reset --purge`.
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
