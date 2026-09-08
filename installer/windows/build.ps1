# Build the Windows MSI.
#
#   installer/windows/build.ps1 -Version 0.6.0 -Arch x64 `
#     -Binary dist/windows_amd64/remote-docker.exe `
#     -Out dist/msi/remote-docker_0.6.0_windows_amd64.msi
#
# Needs WiX v5: `dotnet tool install --global wix` and
# `wix extension add -g WixToolset.UI.wixext`.
#
# ON WINDOWS, and it has to be. WiX is a dotnet tool and loads on Linux, but it
# says so itself and then means it: "The WiX Toolset only supports Windows...
# All behavior after this point is undefined", after which `wix build` refused
# an ordinary `Directory/@Name` as "not a relative path" (WIX0389). Measured on
# ubuntu-latest, 2026-09-08. See ADR 0048.

[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$Version,
  [Parameter(Mandatory = $true)][ValidateSet('x64', 'arm64')][string]$Arch,
  [Parameter(Mandatory = $true)][string]$Binary,
  [Parameter(Mandatory = $true)][string]$Out
)

$ErrorActionPreference = 'Stop'

# An MSI ProductVersion is three numbers, and the installer compares only
# those: major and minor are one byte each, build is two. Anything else is
# refused here rather than truncated, because a version silently rounded to
# something else is an upgrade that never fires.
#
# This is why the MSI is built for tag releases only. A snapshot version is
# `sha-9370b24`, which has no mapping to three numbers at all.
if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+$') {
  Write-Host "not an MSI version: $Version"
  Write-Host "  fix: build the installer only for a v<major>.<minor>.<patch> tag"
  exit 1
}
$parts = $Version -split '\.' | ForEach-Object { [int]$_ }
if ($parts[0] -gt 255 -or $parts[1] -gt 255 -or $parts[2] -gt 65535) {
  Write-Host "out of range for an MSI version (major/minor <= 255, build <= 65535): $Version"
  exit 1
}

if (-not (Test-Path -PathType Leaf $Binary)) {
  Write-Host "no such binary: $Binary"
  exit 1
}

$here = Split-Path -Parent $PSCommandPath
$root = Split-Path -Parent (Split-Path -Parent $here)
$work = Join-Path ([System.IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString())
New-Item -ItemType Directory -Force -Path $work | Out-Null

try {
  # The licence the UI shows, derived from LICENSE rather than committed as a
  # second copy that would go stale. RTF wants its own escapes and a \par per
  # line; everything in LICENSE is ASCII.
  $rtf = New-Object System.Text.StringBuilder
  [void]$rtf.Append("{\rtf1\ansi\deff0{\fonttbl{\f0 Courier New;}}\fs18`r`n")
  foreach ($line in Get-Content (Join-Path $root 'LICENSE')) {
    $escaped = $line -replace '\\', '\\\\' -replace '\{', '\{' -replace '\}', '\}'
    [void]$rtf.Append("$escaped\par`r`n")
  }
  [void]$rtf.Append("}`r`n")
  Set-Content -Path (Join-Path $work 'license.rtf') -Value $rtf.ToString() -Encoding ascii -NoNewline

  $payload = Join-Path $work 'remote-docker.exe'
  Copy-Item $Binary $payload

  $outDir = Split-Path -Parent $Out
  if ($outDir) { New-Item -ItemType Directory -Force -Path $outDir | Out-Null }

  wix build `
    -arch $Arch `
    -ext WixToolset.UI.wixext `
    -bindpath $work `
    -d "BinPath=$payload" `
    -d "Version=$Version" `
    -o $Out `
    (Join-Path $here 'remote-docker.wxs')
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}
finally {
  Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}

$built = Get-Item $Out
Write-Host "$($built.FullName)  $($built.Length) bytes"
