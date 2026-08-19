# 0023. Running where the loader is not us

- Status: Accepted; extends [ADR 0004](0004-single-go-binary-client.md)
- Date: 2026-08-11, amended 2026-08-14
- Current answer: ask `self.go` which file this is (`selfPath`) and how to run
  it again (`selfCommand`), never `os.Executable` or `exec.Command`. Android is
  the one target built WITH cgo, for bionic's resolver.

## Context

Somebody ran the `linux/arm64` client on a Pixel 9 Pro. It refused to load, and
fixing that uncovered a chain of four faults, all of one cause, and one of them
a regression the phone had nothing to do with.

Android will not execute a file in an app data directory. Termux's answer is to
run programs through the system dynamic linker:

```
/system/bin/linker64 /absolute/path/to/program args...
```

The kernel and SELinux see only the linker being executed, which is permitted.
Everything else on the system copes because `libtermux-exec` is `LD_PRELOAD`ed
into libc and hides the arrangement. **This binary loads neither libc nor that
library**, so it sees the arrangement raw, in three places at once:

| what Go sees | how it presented |
|---|---|
| its own path inserted as `argv[1]` | `"/data/data/.../remote-docker" is not a remote-docker command` |
| `/proc/self/exe` is the linker | `start` spawned the linker: `error: expected absolute path: "start"` |
| the file cannot be exec'd at all | `fork/exec .../remote-docker: permission denied` |

Not one of those names the cause, and two of them name somebody else's program.

The asymmetry is worth stating precisely, because it is what made every
diagnostic we ran come back clean: **C programs never see the inserted
argument.** The dynamic linker hands `main()` an argv shifted past it. Go takes
argc and argv off the initial stack instead. `/proc/self/cmdline` shows the
extra path for *every* program there, `cat` included; `cat` simply never
receives it. Probing with the tools that are already on the device therefore
proves nothing, twice over — they are all C.

## Decision

**Android is its own build target, and the Linux build is not bent towards it.**
`GOOS=android` is ET_DYN, emits no `PT_TLS`, and names `/system/bin/linker64`
as its interpreter, which is the one the device has. ~~`arm64` only:
`android/amd64` requires cgo, so it would make the NDK a dependency of CI and
of every release, for emulators and Chromebooks.~~

*(2026-08-14: the second half no longer holds. Android is now built WITH cgo,
for a reason that has nothing to do with loading, so the NDK is a dependency
either way and `android/amd64` costs one more build. See the amendment below.)*

The first attempt was `-buildmode=pie` on the Linux build, and it was wrong for
a reason that has nothing to do with phones: it makes the binary **dynamic**,
adding `PT_INTERP /lib/ld-linux-aarch64.so.1` and so a glibc dependency,
everywhere. That is a regression against ADR 0004 on every musl system. It was
committed on a claim ("`CGO_ENABLED=0` keeps the linker internal, so this is a
static PIE") produced by grepping the binary for an interpreter string, which
found nothing because it was looking for the wrong thing. Reading the program
headers says otherwise. **The check is three facts read off the file: ELF type,
`PT_TLS` alignment, `PT_INTERP`.**

**One place answers "which file am I", and one place runs it again.**
`client/cmd/remote-docker/self.go`:

- `selfPath` — `TERMUX_EXEC__PROC_SELF_EXE` is what Termux sets to the real
  path, and is preferred **only** where `os.Executable` disagrees with it and
  the file is really there. An ordinary machine keeps the kernel's answer and a
  stale variable cannot redirect anything. Always absolute, because the
  variable holds the path as it was typed.
- `selfCommand` — re-execs the way this process was itself exec'd, through
  whatever loader is running it, which `os.Executable` names. **No linker path
  is written down anywhere.** Hardcoding `/system/bin/linker64` would be a
  guess about a platform nothing tests; where the two agree this is an ordinary
  exec of an ordinary file.
- `dropSelfArgument` — removes a first argument that is this executable,
  applied to `os.Args` in `main` before anything reads them.

None of it is conditioned on `GOOS`. Each test is narrow enough that it cannot
fire by accident — an argument has to *be* this executable and at position one
— and a rule that runs only on the platform nobody develops on is a rule that
rots unnoticed. Other exec wrappers do the same thing.

## Amendment, 2026-08-14: Android is built with cgo

The binary loaded and ran, and could not resolve a hostname:

```
lookup docker.lhns.de on [::1]:53: read udp [::1]:49190->[::1]:53:
read: connection refused
```

`CGO_ENABLED=0` gives Go its own resolver, which reads `/etc/resolv.conf`.
Android does not have one: DNS belongs to `netd` (`com.android.resolv` since
Android 10), which apps reach through `/dev/socket/dnsproxyd`, and bionic's
`getaddrinfo` is what talks to it. With no file to read, Go falls back to
`127.0.0.1:53` and `[::1]:53`, where nothing is listening. The address in the
error is not a misconfiguration; it is Go's default when it has no
configuration at all.

**So this target links against bionic.** `CGO_ENABLED=1` with the NDK's
`aarch64-linux-android24-clang`, taken from `ANDROID_NDK_HOME`, which ubuntu
runners already have, so nothing is downloaded. API 24 is Termux's own floor.

Two alternatives were rejected. `mtibben/androiddnsfix`, the only public
package for this, hardcodes `1.1.1.1` and `8.8.8.8` and reaches into `net`'s
internals with `go:linkname`, so it sends every lookup to a resolver the user
did not choose. Speaking netd's socket protocol from Go needs no NDK and does
use the device's resolver, but it is a private interface with no compatibility
promise, and nothing here could test a line of it.

What it costs, stated because the original decision was about avoiding exactly
this: the NDK is now required to build this target at all, the binary links
`libc.so` rather than nothing, and `android/amd64` stops being excluded, since
the reason it was excluded was that it would have required this.

The three facts this record names are now asserted rather than read by hand.
`test/elf.sh` runs on every push, and the Android side of it fails if the
binary stops linking `libc.so`, which is the way this fix would silently come
undone.

## Consequences

- **`os.Executable` appears once in the client**, inside `selfPath`. Six call
  sites go through it: the `start` respawn (ADR 0017), three in `shim`, `status`,
  and the argv strip. It is an invariant in `CLAUDE.md` because the obvious call
  is the wrong one and the failure surfaces two layers away.
- **`shim install` was broken here and nobody had reported it**, because nobody
  had got that far. It links to this binary, so it would have put a `docker` on
  PATH that was the Android dynamic linker. It works on the device now.
- **`os.Args[0]` survives the loader.** The retired shim (ADR 0024 replaced
  it) took the invoked name from
  `argv[0]` and never from `os.Executable`, for a reason that predated this
  platform, and it turned out to be the only identity that stays meaningful
  when the process really is the linker. *(2026-08-11: ADR 0024 deleted the
  feature that depended on it. The finding about the platform stands; the rule
  it was about no longer exists.)*
- **Loadable is not tested.** The client reaches a real workspace from a phone:
  `status`, `start`, `stop` and `docker run` all work. Nothing in CI runs on
  Android, no integration test does, and the release pipeline cross-compiles
  the target without executing it. Say "runs on Android" only about the things
  in the previous sentence. *(2026-08-14: what CI does assert now is the file
  itself, through `test/elf.sh`. That is not the same as running it.)*
- **The size cost is nothing and the surface cost is one target.** The Android
  artifact is built by the same goreleaser config and the same CI matrix leg as
  the rest.
- **A phone is a client, not a workspace.** Nothing here changes what the agent
  needs, and the agent does not call `os.Executable` at all.
