# 0030 — A core module for the tunnel

The client and the agent are the two ends of one SSH connection carrying Docker
API streams, an NFS export, port forwards and change notifications. What that
connection IS now lives in `core/tunnel`, in the module both binaries already
depend on, so the two ends cannot disagree about it.

## What forced it

Twice, one mechanism was written down twice and the copies drifted, and both
times the failure was silent:

- **Half-closing a stream.** Each binary had its own bidirectional copy, and
  they had OPPOSITE fallbacks for a connection that cannot half-close: one did
  nothing, the other closed the whole connection, which this project's own
  invariant forbids. It presented as `docker run` exiting 0 having printed
  nothing. That created the shared module (ADR 0021) and `internal/iox`.
- **The uid→port formula.** It lived in two shell scripts, drifted, and
  presented as a network fault. It now lives once, in `core/workspace`.

Extracting the names before moving any code found three more of the same shape,
which is the evidence that this was not a tidiness exercise:

- `docker system dial-stdio` was a constant in the client's proxy AND a constant
  in the agent's session handler.
- `workspace-info` was a constant in the agent and a bare literal at the
  client's call site.
- `workspace-notify` was in `core/workspace`, which is the contract for the
  FORMAT of a change frame. The frame's shape belongs there; the name of the
  command carrying it belongs with the other commands.

None of those is a compile error when it goes wrong. A client asking for a name
the agent's switch does not recognise falls through to the shell, runs a command
that does not exist and exits 127 — the same failure the notify version check
already has to tell apart from a working channel.

## The decision

`core/tunnel` holds what the two ends must agree on, and `core/tunnel/client` and
`core/tunnel/server` hold the two implementations of it.

**The root package imports neither SSH library.** That is the load-bearing part.
A single package importing both would link a server into the client binary,
which never runs one; Go links what is imported, so the split is what keeps the
client free of gliderlabs and the agent free of the client's dialling code. The
constraint also keeps the root honest: anything that cannot be expressed without
an SSH library is an implementation, not an agreement.

**Auth is policy and stays out.** The core takes an `ssh.Signer` and an
`ssh.HostKeyCallback` from the client, and a key authorizer from the agent. It
does not decide who may log in. This is not only a design preference: the
client's `KeyPair` and `KnownHosts` live under `client/internal/`, which the
shared module cannot import at all, so the interface boundary is the only shape
that compiles. `client/internal/sshx` keeps `keys.go` and `hostkey.go` and
becomes the adapter that builds those two values.

## What stays where it is, and why that is not arbitrary

The test is whether both ends must AGREE, not whether the code is
infrastructure:

- The client serves NFS and the agent only mounts it. There is no NFS server on
  the agent side, so `nfsserve` moving into the core would put `go-nfs` and
  `go-billy` in the agent's module graph — an agent deliberately built from 7
  dependencies and 24 `go.sum` lines — in exchange for no agreement.
- Ports are the same shape: the client makes local forwards, the agent makes
  remote ones. What is shared is the uid→port formula, and that was already in
  `core/workspace` for exactly this reason.
- **Watching is two ends of one promise, not two copies of one implementation.**
  The agreement — the change frame's format and the mode names — is already
  shared (ADR 0016). `fswatch` is three platform backends the agent will never
  run; `notify` performs syscalls on the user's files, and its knowledge is
  measured and one-ended (`utimensat` with `atime=UTIME_OMIT` gives `IN_MODIFY`,
  with both times set it gives `IN_ATTRIB`, which is why the feature works at
  all). Neither half belongs in a module the other binary imports.

## What it costs

The shared module gains `x/crypto/ssh` and `gliderlabs/ssh`, in sub-packages, so
neither binary links the one it does not use. The agent's own dependency count is
unchanged, because it already had both.

It also concentrates risk: this is the transport, so a mistake here is a mistake
in every suite at once. That is why the move is sequenced one step at a time,
each green on its own, and why unit tests are treated as necessary and not
sufficient — `test/integration.sh`, `test/per-user-dind.sh`,
`test/two-clients.sh`, `test/vm.sh` and the WSL machine job are the real check.

## The rule, for whatever comes next

Something belongs in `core/tunnel` if both binaries must AGREE about it. It
belongs in `core/workspace` if both must agree about a FORMAT. It belongs in
neither if only one side ever reads it, however much like infrastructure it
looks. ADR 0021 stated the first tier of this rule; this is the second, and the
question it answers is the same one: which failures can happen silently.
