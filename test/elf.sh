#!/usr/bin/env bash
# What a built binary has to be, read off the file.
#
#   test/elf.sh android dist/remote-docker-android_android_arm64/remote-docker
#   test/elf.sh linux   dist/remote-docker_linux_arm64/remote-docker
#
# ADR 0023 says the check is three facts: ELF type, PT_TLS alignment and
# PT_INTERP. It was run by hand once, and the claim it disproved had been
# committed on the strength of grepping the binary for an interpreter string,
# which found nothing because it was looking for the wrong thing.
#
# Nothing executes either binary in CI, so this is the only assertion available
# for them, and the two targets need opposite answers:
#
#   android  dynamic, linked against bionic, loaded by /system/bin/linker64
#   linux    static, no interpreter, no libc, which is ADR 0004 on musl
#
# Run against the goreleaser output, or against an archive somebody downloaded.
set -euo pipefail

GOOS=${1:-}
BIN=${2:-}

if [ -z "$GOOS" ] || [ -z "$BIN" ]; then
    echo "usage: elf.sh <goos> <binary>" >&2
    exit 2
fi
if [ ! -f "$BIN" ]; then
    echo "elf.sh: no such file: $BIN" >&2
    exit 2
fi

# For the counters and the summary. Nothing else in it is called.
# shellcheck source=test/lib.sh
. "$(dirname "$0")/lib.sh"

# Captured once and matched here, never `cmd | grep -q` (why: lib.sh).
headers=$(readelf -lWh "$BIN")
dynamic=$(readelf -dW "$BIN" 2>/dev/null || true)

echo "== $GOOS: $BIN =="

# ELF type is asserted per target below, not here: Android requires ET_DYN,
# while a static Go binary for Linux is ET_EXEC and expected to be.
echo "  ....  $(echo "$headers" | grep -E '^  Type:' | tr -s ' ')"

# PT_TLS is the segment bionic rejects when it is underaligned, which is what a
# linux/arm64 PIE gets wrong. Printed rather than asserted: GOOS=android emits
# none at all, so there is no alignment to compare, and the value is what a
# toolchain change would move.
if [[ "$headers" =~ TLS ]]; then
    echo "  ....  PT_TLS: $(echo "$headers" | grep -E '^  TLS' | tr -s ' ')"
else
    echo "  ....  no PT_TLS segment"
fi

case "$GOOS" in
android)
    # Android has required position-independent executables since Android 5,
    # and Go's default elsewhere is ET_EXEC, which is the "unexpected e_type: 2"
    # a linux binary is refused with on a phone.
    if [[ "$headers" =~ Type:[[:space:]]+DYN ]]; then
        ok "ELF type is DYN"
    else
        bad "ELF type is not DYN: $(echo "$headers" | grep -E '^  Type:' | tr -s ' ')"
    fi

    # The loader the device actually has. A binary naming any other one is
    # unloadable there, and says so as "no such file or directory" about a file
    # that is present.
    if [[ "$headers" =~ /system/bin/linker64 ]]; then
        ok "interpreter is /system/bin/linker64"
    else
        bad "interpreter is not the device's: $(echo "$headers" | grep -i interpreter || echo none)"
    fi

    # The whole reason this target is built with cgo. Without libc there is no
    # getaddrinfo, so Go uses its own resolver, which on Android has no
    # configuration to read and resolves nothing.
    if [[ "$dynamic" =~ NEEDED.*libc\.so ]]; then
        ok "links libc.so, so DNS goes through bionic"
    else
        bad "does not link libc.so: built without cgo, and no hostname will resolve"
    fi
    ;;
linux)
    # ADR 0004: one binary, nothing installed, musl included. A PT_INTERP here
    # means a glibc dependency, which is the regression that shipped once
    # already as -buildmode=pie.
    if [[ "$headers" =~ Requesting[[:space:]]program[[:space:]]interpreter ]]; then
        bad "has an interpreter: $(echo "$headers" | grep -i interpreter | tr -s ' ')"
    else
        ok "no interpreter, so it runs where there is no glibc"
    fi

    if [[ "$dynamic" =~ NEEDED ]]; then
        bad "links a shared library: $(echo "$dynamic" | grep NEEDED | tr -s ' ')"
    else
        ok "links nothing, so it is static"
    fi
    ;;
*)
    echo "elf.sh: nothing to assert for $GOOS" >&2
    exit 2
    ;;
esac

summary
