# 0007. A virtual NFS export namespace

- Status: Accepted
- Date: 2026-08-07

## Context

ADR 0006 requires each bind source to be independently mountable, and those
sources can be anywhere on the client: different drives on Windows, unrelated
subtrees on Linux, paths above the working directory.

The obvious implementations are both bad.

**One NFS server per shared directory** means one listener, one reverse-tunnel
port and one set of handle state per directory. The uid-derived port scheme
(ADR 0003) gives each account exactly one port, so this would need port
allocation and coordination — reintroducing precisely the problem that scheme
exists to avoid.

**Serve the filesystem root** and address everything by absolute path. Simple,
and unacceptable: it exposes the entire client machine to the workspace, where
a compromised or merely careless container could read anything the user can.

## Decision

Serve **one export with a synthetic root**. Its entries are registered on
demand:

```
/cwd            -> the directory remote-docker was invoked from
/m/<share id>   -> any other local directory named by a bind mount
```

`<share id>` is 16 hex characters of the SHA-256 of the directory's canonical
path. Canonicalisation folds case and separators on Windows and cleans the path
everywhere, so two spellings of the same directory produce one share.

## Consequences

- One server, one port, one tunnel, any number of unrelated local directories.
  The uid-derived port scheme is untouched.
- **Only directories explicitly named by a bind mount are reachable.** The
  export root lists nothing else, so the workspace's view of the client is
  exactly the set of paths the user asked for.

  Amended by ADR 0027: it is that, plus directories this machine previously
  offered to this workspace and which that workspace still holds a volume for.
  Registration is per process while a volume outlives one, so starting a
  container created in an earlier session found no share and failed to mount
  against a directory that was right there. What restores a share is a MOUNT
  that missed, never a session starting, and the workspace names an id rather
  than a path: the record is a capability list this machine checks again on
  every read, and `ShareID` cannot be inverted, so an id nobody wrote down
  resolves to nothing.
- Share ids are stable across runs, because they are derived from the path
  rather than allocated. A reconnecting client keeps its NFS handles and its
  remote volumes instead of orphaning one set per session.
- Case folding is Windows-only and deliberately so. Folding case on a
  case-sensitive filesystem would merge two genuinely different directories
  into one share.
- The digest is an identity, not a security boundary. The registry stores the
  path alongside the id and verifies it, rather than trusting a 64-bit hash not
  to collide.
- The mux filesystem is ours to write, and every NFS operation passes through
  it. Path traversal out of a registered subtree has to be refused explicitly
  and tested for; the export root's guarantee above depends entirely on it.
