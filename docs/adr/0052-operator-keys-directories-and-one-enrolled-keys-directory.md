# 0052 — Operator keys directories, and one enrolled keys directory

- Status: Accepted. The writer's callers are redeeming an enrolment token (ADR
  0051) and account management (ADR 0053).
- Date: 2026-10-05, amended 2026-10-06 (provisioning out of the sync lock; the
  stale-lock break, a failed account creation, subscribers)

## What forced it

An account is a file in a keys directory, put there by hand (ADR 0010). For the
agent to enrol a key itself, it needs somewhere to write, and the directory the
operator fills is the wrong place:

| deployment | the operator's directory is |
|---|---|
| Kubernetes | a Secret mounted read-only (ADR 0035) |
| compose, swarm | a bind mounted `:ro` |
| VM | the operator's, edited by hand or by configuration management |

Writing into any of these either fails or fights the thing that owns it.

## Decision

- **Two kinds of directory**, both holding `<account>.pub`, one key per line:

  | env var | what | default |
  |---|---|---|
  | `WORKSPACE_KEYS_DIR` | the operator's; the agent only reads them. A comma-separated list | `<state>/authorized_keys.d` |
  | `WORKSPACE_ENROLLED_KEYS_DIR` | the ONE directory the agent writes | `<state>/enrolled_keys.d` |

- **Merged by account.** Every directory is read with the same rules, operator
  directories first. An account's files in all of them are one account:
  `Account.Keys` is the union, one key per fingerprint, and `Account.Sources`
  says which directory contributed what. `Authorized` is unchanged.
- **The two-read rule is per directory.** A present but unusable file
  contributes, on its first read, what that directory contributed last time,
  and nothing on its second. A missing file contributes nothing at once. An
  account is revoked when no directory contributes a key. A save caught
  mid-write in one directory therefore never costs the keys of another.
- **Startup refuses** an enrolled directory that is, contains or sits inside an
  operator directory, and an operator directory named twice, compared after
  `Abs`, `Clean` and `EvalSymlinks`. Overlap means the agent writes the
  operator's files or reads its own as the operator's.
- **A missing or read-only enrolled directory is a warning.** A probe at
  startup creates and removes a file; the operator's keys still serve.
- **Watched and polled, every directory.** Dotfiles are skipped when reading,
  since the writer's locks and temporary files are dotfiles. Events are not
  filtered: a Kubernetes Secret changes by swapping its `..data` link.
- **The writer** (`accounts/keyfile_write.go`): `AppendKey`, `CreateAccount`,
  `RemoveKey`, `RemoveAccountFile`.
  1. Lock: `os.Mkdir` of `.<account>.pub.lock`, atomic on NFS and CephFS. A
     holder is told apart by the lock's mtime. A waiter that has watched ONE
     holder for `lockWait` (10s) breaks the lock if its mtime is older than
     30s, and otherwise gives up; a queue that keeps moving is waited out
     however long. The break renames the lock aside under a second lock
     (`.lock.break`), and only if the lock is still that holder: two writers
     that both saw it stale would otherwise each move a lock the other had
     just taken.
  2. Read the file, keeping every line that is not the one changed.
  3. Append idempotently by fingerprint, or remove the matching lines. A
     comment is folded onto one line, since a newline in it would be a line of
     the caller's choosing.
  4. Write through a temporary, fsync and rename. A file left with no key is
     deleted, which revokes at once rather than after two reads.
  5. Unlock, then sync before returning, so the change is in force. The sync
     lock, which `Known` and every write take, is never held across a
     `useradd` (one took 170s, PR 268): an account new to the agent is created
     in the background, one at a time, and published when that finishes. A
     write that adds a key waits for its own account's creation up to 30s,
     then returns `ErrProvisioning` with the key written; one whose account
     could not be created returns `ErrNotProvisioned` with the key taken out
     again. A removal waits for nothing. `Store.Subscribe`'s callbacks run
     after the sync lock is released, so one may call back into the store.
  - `CreateAccount` refuses a name `Known` has (the uidmap, or a file in any
    directory) and links rather than renames, so of concurrent creations one
    wins even against a file written without the lock.
  - Removing a key, or an account's file, that an operator directory also
    enrols is refused with `OperatorKeyError` naming that file: removing the
    enrolled copy alone would leave the key working.

## What it costs

- **A hand edit in the enrolled directory bypasses the lock.** Hand edits
  belong in an operator directory.
- **A crashed writer's lock costs up to 10s**, once: the next writer watches
  it for `lockWait` before breaking it. The watch, not the mtime, is what
  keeps a live lock: hosts whose clocks differ by more than 30s on shared
  storage see every lock as old, and a live holder still finishes within the
  watch.
- **A deployment whose keys directory contains the state directory's
  `enrolled_keys.d`**, such as `WORKSPACE_KEYS_DIR=<state>`, no longer starts
  until `WORKSPACE_ENROLLED_KEYS_DIR` is moved.
- **An unreadable enrolled directory stops every sync**, as an unreadable
  operator directory always did, until it is fixed. Only a missing one is read
  as empty.
