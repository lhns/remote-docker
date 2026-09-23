#!/usr/bin/env bash
# What a built binary has to be, read off the file.
#
#   test/elf.sh android dist/remote-docker-android_android_arm64/remote-docker
#   test/elf.sh linux   dist/remote-docker_linux_arm64/remote-docker
#
# Nothing executes either binary in CI, so this is all that is asserted about
# them (ADR 0023), and the two targets need opposite answers:
#
#   android  dynamic, linked against bionic, loaded by /system/bin/linker64
#   linux    static, no interpreter, no libc, which is ADR 0004 on musl
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

# For the counters and the summary only.
# shellcheck source=test/lib.sh
. "$(dirname "$0")/lib.sh"

headers=$(readelf -lWh "$BIN")
dynamic=$(readelf -dW "$BIN" 2>/dev/null || true)

echo "== $GOOS: $BIN =="

echo "  ....  $(echo "$headers" | grep -E '^  Type:' | tr -s ' ')"

# Bionic rejects an underaligned PT_TLS. Printed, not asserted: GOOS=android
# emits none.
if [[ "$headers" =~ TLS ]]; then
    echo "  ....  PT_TLS: $(echo "$headers" | grep -E '^  TLS' | tr -s ' ')"
else
    echo "  ....  no PT_TLS segment"
fi

case "$GOOS" in
android)
    # A phone refuses ET_EXEC with "unexpected e_type: 2".
    if [[ "$headers" =~ Type:[[:space:]]+DYN ]]; then
        ok "ELF type is DYN"
    else
        bad "ELF type is not DYN: $(echo "$headers" | grep -E '^  Type:' | tr -s ' ')"
    fi

    # Any other loader fails as "no such file or directory" about a file that
    # is present.
    if [[ "$headers" =~ /system/bin/linker64 ]]; then
        ok "interpreter is /system/bin/linker64"
    else
        bad "interpreter is not the device's: $(echo "$headers" | grep -i interpreter || echo none)"
    fi

    if [[ "$dynamic" =~ NEEDED.*libc\.so ]]; then
        ok "links libc.so, so DNS goes through bionic"
    else
        bad "does not link libc.so: built without cgo, and no hostname will resolve"
    fi
    ;;
linux)
    # A PT_INTERP means a glibc dependency, which shipped once already as
    # -buildmode=pie.
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
