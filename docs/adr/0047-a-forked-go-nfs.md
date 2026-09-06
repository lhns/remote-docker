# 0047 — A forked go-nfs, consumed through a `replace`

- Status: Accepted
- Date: 2026-09-06
- Current answer: `github.com/willscott/go-nfs` is replaced by
  `github.com/lhns/go-nfs`, branch `lhns-fixes`, forked from upstream `v0.0.4`,
  in both `core-client/go.mod` and `client/go.mod`. It carries five fixes and
  nothing else, and is dropped when upstream merges them.

## What forced it

`test/probes/fsprobe` runs the same filesystem operations against a share and
against a native bind mount and compares the answers. Five differences were the
server library's, not this project's: nothing in `core-client/nfsserve` or in
the export namespace could produce them.

| fix, in the fork | what a user sees without it |
|---|---|
| `nfs_onremove.go` (`dirNotEmpty`) | `rmdir` and `rename` over a non-empty directory answer EIO; a script testing for ENOTEMPTY takes the wrong branch |
| `nfs_onrename.go` | `rename` onto an existing empty directory is refused, where rename(2) replaces it atomically |
| `helpers/cachinghandler.go` (`Rename`) | the cached handle is invalidated rather than moved, so a file renamed while open goes `Stale file handle` — including the silly-rename an unlinked open file gets |
| `nfs_onlink.go` | LINK is parsed in SYMLINK's layout, so a hard link dies in the XDR parser with EINVAL before any filesystem call |
| `nfs_onremove.go` (`isInvalid`), used by create, mkdir, symlink and rename | a filesystem EINVAL (a name the host cannot spell) is reported as EACCES or EIO, sending the user after a permission or disk fault that is not there |

- All five are wire-level or handler-level: they cannot be worked around from
  the `billy.Filesystem` this project supplies.
- `rmdir` is `onRemove` in go-nfs (`nfs_onrmdir.go` delegates), so one mapping
  covers both.

## The decision

| | |
|---|---|
| fork | `github.com/lhns/go-nfs`, branch `lhns-fixes` |
| base | upstream `v0.0.4` |
| consumed as | `replace github.com/willscott/go-nfs => github.com/lhns/go-nfs@<pseudo-version>` |
| in | `core-client/go.mod` (direct) and `client/go.mod` (indirect, through core-client) |
| tests | each fix carries one in the fork (`nfs_fixes_test.go`, `nfs_einval_test.go`, `helpers/cachinghandler_rename_test.go`) |
| upstream | a pull request per fix, to follow, so the fork can be dropped rather than maintained |
| licence | Apache-2.0, unchanged from upstream |

Not forked: `github.com/willscott/go-nfs-client`, which supplies the XDR codec
and is upstream and untouched in both modules.

## What it costs

- **The pseudo-version is refreshed by hand when the branch moves.** In each of
  `core-client/` and `client/`:

  ```
  go mod edit -replace github.com/willscott/go-nfs=github.com/lhns/go-nfs@<sha>
  go mod tidy
  ```

- **Two modules to keep in step.** `client` reaches go-nfs only through
  `core-client`, and Go does not inherit a `replace` from a required module, so
  a replace in one module and not the other builds two different servers with
  nothing failing.
- **The notices file names the fork.** `THIRD-PARTY-NOTICES.md` is generated
  from what is linked (`scripts/third-party-notices.sh`), and a `replace`
  changes that, so the entry is the fork and its licence rather than upstream
  `v0.0.4`.
- **The fork must be re-based on any upstream release before it can be
  dropped**, and a rebase is where a fix silently stops applying. The tests in
  the fork are what catch that.
- **Exit condition:** upstream merges the five fixes and cuts a release. Then
  both `replace` lines go, `go.mod` requires that version, and this record is
  deleted.
