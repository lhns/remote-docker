#!/usr/bin/env bash
# Build the Windows MSI. Runs on Linux: WiX v5 is a dotnet tool and needs no
# Windows anywhere, which is what lets the release job stay on ubuntu-latest.
#
#   installer/windows/build.sh --version 0.6.0 --arch x64 \
#       --binary dist/.../remote-docker.exe --out dist/remote-docker_0.6.0_windows_amd64.msi
#
# Prerequisites: dotnet, and `dotnet tool install --global wix --version 5.*`.
set -euo pipefail

version=""
arch=""
binary=""
out=""

while [ $# -gt 0 ]; do
  case "$1" in
    --version) version="$2"; shift 2 ;;
    --arch)    arch="$2";    shift 2 ;;
    --binary)  binary="$2";  shift 2 ;;
    --out)     out="$2";     shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

for name in version arch binary out; do
  if [ -z "${!name}" ]; then
    echo "missing --${name}" >&2
    exit 2
  fi
done

# An MSI ProductVersion is three numbers, and the installer compares only
# those: major and minor are one byte each, build is two. Anything else has to
# be refused here rather than truncated, because a version silently rounded to
# something else is an upgrade that never fires.
#
# This is why the MSI is built for tag releases only. A snapshot version is
# `sha-9370b24`, which has no mapping to three numbers at all.
if ! printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "not an MSI version: $version" >&2
  echo "  fix: build the installer only for a v<major>.<minor>.<patch> tag" >&2
  exit 1
fi
IFS=. read -r major minor build <<<"$version"
if [ "$major" -gt 255 ] || [ "$minor" -gt 255 ] || [ "$build" -gt 65535 ]; then
  echo "out of range for an MSI version (major/minor <= 255, build <= 65535): $version" >&2
  exit 1
fi

if [ ! -f "$binary" ]; then
  echo "no such binary: $binary" >&2
  exit 1
fi

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The licence the UI shows, derived from LICENSE rather than committed as a
# second copy that would go stale. RTF wants its own escapes and a \par per
# line; everything here is ASCII.
{
  printf '{\\rtf1\\ansi\\deff0{\\fonttbl{\\f0 Courier New;}}\\fs18\n'
  sed -e 's/\\/\\\\/g' -e 's/{/\\{/g' -e 's/}/\\}/g' -e 's/$/\\par/' "$root/LICENSE"
  printf '}\n'
} > "$work/license.rtf"

cp "$binary" "$work/remote-docker.exe"

mkdir -p "$(dirname "$out")"

wix build \
  -arch "$arch" \
  -ext WixToolset.UI.wixext \
  -bindpath "$work" \
  -d "BinPath=$work/remote-docker.exe" \
  -d "Version=$version" \
  -o "$out" \
  "$here/remote-docker.wxs"

ls -l "$out"
