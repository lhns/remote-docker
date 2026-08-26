# 0040 — Git Bash mangles argv, and the client undoes it

- Status: Accepted; extends [ADR 0024](0024-the-docker-cli-is-the-root.md)
- Date: 2026-08-26
- Current answer: on Windows, a `-v` value carrying `;` and a Windows-shaped
  target is restored before the Docker CLI parses it. Only the **target**, and
  only that flag.

## What forced it

`-v` is destroyed before this program starts when run from Git Bash. Measured
2026-08-25 by printing the argv a native Windows child receives:

| typed | received |
|---|---|
| `-v /etc/hostname:/x` | `C:\Program Files\Git\etc\hostname;X:\` |
| `-v /c/Users/you/x:/app` | `C:\Users\you\x;C:\Program Files\Git\app` |
| `-v /c/Users/you/x:/app:ro` | `C:\Users\you\x;C:\Program Files\Git\app;ro` |
| `-v ./rel:/app` | `.\rel;C:\Program Files\Git\app` |
| `-v x_named:/app`, `-v 'C:\…\x:/app'` | untouched |
| `--mount type=bind,source=/c/…,target=/app` | both sides correct |
| `-e PATH=/usr/bin:/bin` | `…\usr\bin;…\usr\bin` |

`msys-2.0.dll` converts in the PARENT while building the command line for a
native child: POSIX paths onto the MSYS root, a single-letter `/x` onto the drive
`X:\`, `:` lists into `;` lists. `/tmp` goes to `%TEMP%`, which is not under the
root at all.

The failure it produced here was a session spent believing the client could not
mount anything: the error named a path nobody typed.

## The decision

**Restore the target, keep the source.** MSYS maps `/c/Users/you/x` →
`C:\Users\you\x` correctly, using a mount table this program does not have. It
cannot map the container side, because nothing tells it `-v` has two halves with
different destinations. So the repair reverses field 1 only.

**Two conditions trigger it**, never one: the value contains `;`, AND the target
field is a Windows-shaped path. `;` alone proves nothing — NTFS permits it in a
file name — and a real bind specification cannot have a drive-rooted target,
because that field is a path in a Linux container.

Reversals, all measured:

| received | restored |
|---|---|
| `<root>\app` | `/app` |
| `X:\` | `/x` |
| `%TEMP%\cache` | `/tmp/cache` |
| anything else drive-rooted | left alone, and reported |

**Warn only when the reversal is ambiguous.** Git Bash maps `/bin` and
`/usr/bin` onto one directory, so a restored `/usr/bin` could have been either.
Measured: `/lib` and `/usr/lib` do NOT collide, so they are exact and silent. A
target that cannot be inverted at all is left as it is and reported, naming
`MSYS_NO_PATHCONV=1` — a wrong guess would be worse than an error the user can
act on.

## What was ruled out, and why

Each of these looks like the obvious answer.

- **Reading the original out of the parent.** bash holds the pre-conversion
  strings while building the command line, and offers no way to ask for them: no
  API, no IPC. The one thing Windows reports about a parent is its own command
  line (`bash.exe --login -i`), not what it built for a child. Anything further
  means `ReadProcessMemory` against an unstable heap, racing the parent freeing
  it, with debug rights.
- **Claiming to be an MSYS program so conversion is skipped.** `spawn.cc`
  branches on `iscygexec()`, set by scanning the child's PE import table for the
  runtime DLL. A normal import makes the loader refuse to start this binary
  wherever `msys-2.0.dll` is absent; a delay-load import survives that but lives
  in a different PE directory than the one scanned; either needs cgo, and Windows
  builds are `CGO_ENABLED=0` (ADR 0004). And succeeding would be a LOSS:
  conversion off means receiving `/c/Users/…` and reimplementing MSYS's mount
  table to map it.
- **Asking upstream.** Settled in 2015: `MSYS_NO_PATHCONV` was merged into the
  msys2-runtime for exactly this (git-for-windows/msys2-runtime#11). Measured
  2026-08-25: it ignores its value (`0` and empty both disable) and is
  all-or-nothing; `MSYS2_ARG_CONV_EXCL` matches each ARGUMENT's own prefix, so
  `-v` does not protect the value after it, and only `*` covers everything.
  **Neither knows about programs**, which is why nothing on our side of the
  process boundary can opt out.

## Consequences

- **`/bin` and `/usr/bin` cannot be told apart**, so a target restored to
  `/usr/bin` is a guess, warned about rather than engineered around. Pathological
  as a mount target.
- **Only `-v` is repaired.** `-w /src` and `-e PATH=/usr/bin:/bin` are mangled
  too and are left alone: the first has one field and no signature to trigger on,
  the second is lossy in a way no reversal can fix (both halves arrive as the
  same Windows path). `MSYS_NO_PATHCONV=1` or a leading `//` remains the answer
  for those, and the README says so.
- **Failing to repair warns; failing to notice cannot.** A target recognised as
  mangled but not invertible is left alone with one line on stderr, and docker
  then gives its own error -- we explain rather than pre-empt a daemon that may
  accept something we do not understand. A shape carrying no signature is silent,
  because it has to be: `-v /a:/b` arrives as `a:/b`, which is what mounting a
  named volume `a` at `/b` looks like, so warning would fire on correct commands.
  The cost, stated plainly: that spelling silently becomes a named volume.
- **`--mount` was never affected** and is the recommendation for anyone who
  wants no ambiguity at all.
- **A future Git Bash changing its mapping** would make the reversal wrong rather
  than absent. The trigger is narrow enough that it fails visibly, and the
  ambiguous case already prints what it read.
- **No CI job covers it.** The Windows job runs unit tests, and no runner drives
  this binary from Git Bash. The measured table is the fixture, and the manual
  check is in the README.
