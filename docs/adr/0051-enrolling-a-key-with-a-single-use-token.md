# 0051 — Enrolling a key with a single-use token

- Status: Accepted. The operator mints tokens with `remote-dockerd token`; an
  admin, or an account for itself, with `remote token create` (ADR 0053).
- Date: 2026-10-05, amended 2026-10-06 (a redeem replies `pending`)

## What forced it

Enrolling a device took two people and a file (ADR 0010): the user prints a
public key, the operator saves it as `<account>.pub`, on Kubernetes through a
Secret and a `helm upgrade` (ADR 0035). The device also trusted the workspace's
host key on first use, with nothing to check it against.

## The decision

- **The operator mints a token, the device redeems it with its own key.** The
  agent writes the key into the enrolled directory (ADR 0052) through the locked
  writer. Nobody copies a key anywhere.
- **The protocol is `core/enrol`, whole:** the login prefix, the banner and its
  version, `workspace-redeem`, the request and reply frames, the error codes,
  and the token and invite codecs (ADR 0021's rule for a protocol package).

### The token and the invite

| | shape |
|---|---|
| token | `<id>.<secret>`: 8 base32 characters (40 bits, public), then 128 bits of `crypto/rand`, base64url |
| invite | `rdt1.` + base64url(JSON `{u, k, a, t}`): the address, the host key's SHA256 fingerprint, the bound account or none, the token. One shell-safe word |
| on disk | `WORKSPACE_TOKENS_DIR/<id>.json`, 0600: the sha256 of the secret, the account, created, expires, creator, note. Never the secret |

- **Single use, 24 hours by default, 7 days at most.**
- **No stdin form.** `--token -` is malformed: single use makes a token in
  shell history harmless.

### The redemption connection

1. The client dials as usual (ssh, ws, wss, through an ingress) with login
   `+token:<id>` and its own key.
2. **Host key:** the client requires `offered == the invite's fingerprint`, then
   asks `known_hosts`, which records the key or refuses one that changed. The
   token replaces trust on first use rather than adding to it.
3. **Before folding**, the agent sees the prefix and accepts any key for the id
   of a live, unexpired token. Read-only: nobody is authenticated yet. The
   connection gets a `redeemer`, never a `sessionAccount`, so every forward,
   `workspace-info`, the docker socket and a shell refuse it through
   `accountFor`.
4. **Banner:** `remote-docker-enrol 1`, sent to `+token:` logins only.
5. **`workspace-redeem`** is the only command, once per connection: JSON
   `{secret, account?, comment}` in, `{account, created}` or
   `{error: {code, msg, fix}}` out. The key enrolled is the one the connection
   authenticated with, never a field.

### Redemption order

1. the global failure limiter
2. the enrolled directory is writable
3. `Check`: the secret's hash, in constant time, and expiry
4. the name rules
5. `Consume`: rename to `.redeeming-<id>-<ns>-<rand>`, one winner on NFS and
   CephFS as well as locally
6. `CreateAccount` (unbound) or `AppendKey` (bound), each ending in `Sync`
7. the key now authenticates the account; otherwise undo the write and
   `Restore` the token. An account still being created after 30s is not
   undone: the reply says `pending` and the key works once it exists
8. `Done` deletes it. A claim `Revoke` took meanwhile (`user rm`, ADR 0053)
   fails here, and the write is undone
9. an audit line, `component=audit op=token.redeem token key account from`
10. the reply

Steps 1 to 4 spend nothing, so a refused name or a full disk leaves the token
redeemable. A failed `Check` spends one attempt from the limiter.

### Names

| token | `--user` | result |
|---|---|---|
| bound to alice | none or alice | alice: the key is added, the account created if it does not exist |
| bound to alice | bob | refused |
| unbound | bob | bob is created, unless the uidmap or any keys directory has bob, or bob is reserved or an admin's name (ADR 0053) |
| unbound | none | the client sends its local user name |

The uidmap never forgets a name, so an unbound token never reuses one.

### What the client says

| what happened | message |
|---|---|
| refused at login with the banner, or by the redeem | `the workspace refused the token: it is unknown, used or expired` / `fix: ask for a new one` |
| refused at login without the banner | `the workspace at <url> predates enrolment tokens` / `fix: ask its operator to upgrade it, or to enrol this key by file` |
| the host key is not the pinned one | names both fingerprints |
| the reply says `pending` | the client saves the workspace, prints `creating account <name> on the workspace...` and logs in as the account every 3s for up to 4 minutes (`session.WaitForAccount`); a refused login is retried, any other error ends it. On timeout: `the workspace has not finished creating account <name>` / `fix: ask its operator to check the agent log, then run docker remote status` |

Never which of unknown, used or expired: that would be an oracle.

`remote create --token` saves nothing, neither the config nor a docker context,
until the reply names an account, and then saves that account as the user.

## What it costs

- **An unauthenticated surface.** A token id lookup at login and a secret check
  on a connection that can open nothing else. Brute force meets one attempt per
  connection, `MaxAuthTries`, and a GLOBAL bucket (10 at once, then one per
  6s), global because behind an ingress every connection has one address. The
  bucket can be drained by anybody, which delays honest redemptions too.
- **A token that creates an account grants a privileged dind**, which is close
  to root on the host (ADR 0019). Only the operator and admins mint them (ADR
  0053); the threat model's Flow 1b holds the rest.
- **`remote-dockerd token create` runs inside the workspace**, as root, and
  generates the host key if the agent has never served. It needs
  `WORKSPACE_PUBLIC_URL` or `--url`, because what a device reaches behind a
  proxy is not what the agent listens on.
- **A crashed redemption may leave a claim**; `Sweep` deletes claims older than
  ten minutes and expired tokens, hourly. The token is then gone, not restored.
- **Windows renames one file to several names at once**, so the one-winner test
  runs on Linux only. The agent runs nowhere else.
