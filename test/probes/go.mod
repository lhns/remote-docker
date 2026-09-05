// Instruments the integration suites build and run, and nothing else.
//
// Its own module so that its x/sys dependency is nobody else's (ADR 0021).
// Nothing here is imported by any Go code, linked into either binary, or
// shipped: image/Dockerfile and .goreleaser.yaml do not mention it. It is
// built by test/lib.sh's build_probe.
//
// Linux in substance: these read raw inotify and perform raw VFS syscalls.
// The _other.go stubs stay, because the development machine is Windows and
// `go build ./...` has to work there.
module github.com/lhns/remote-docker/test/probes

go 1.26.3

require golang.org/x/sys v0.47.0
