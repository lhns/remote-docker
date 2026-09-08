# The Windows installer, end to end: install, what lands on PATH, the docker.exe
# feature, the refusal that guards it, and uninstall.
#
#   pwsh test/msi.ps1 -Msi dist/remote-docker_0.6.0_windows_amd64.msi
#
# Runs elevated and makes machine-wide changes: it installs to Program Files,
# edits the system PATH, and moves any docker.exe it finds out of the way so the
# feature can be tested both with and without one present. Meant for a
# throwaway machine, which is what .github/workflows/msi.yml gives it.
#
# Two rules from the bash suites apply here as well: an assertion prints what it
# saw, and an msiexec failure with no log is a wasted round trip, so every
# unexpected exit code dumps the verbose log before failing.

[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$Msi
)

$ErrorActionPreference = 'Stop'
$Msi = (Resolve-Path $Msi).Path
$Temp = [System.IO.Path]::GetTempPath()
$InstallDir = Join-Path $env:ProgramFiles 'remote-docker'
$Exe = Join-Path $InstallDir 'remote-docker.exe'
$Alias = Join-Path $InstallDir 'docker.exe'
$LogDir = Join-Path $Temp 'msi-logs'
$Stash = Join-Path $Temp 'docker-exe-stash'
New-Item -ItemType Directory -Force -Path $LogDir, $Stash | Out-Null

$script:section = ''

function Section([string]$title) {
  $script:section = $title
  Write-Host ''
  Write-Host "== $title" -ForegroundColor Cyan
}

function Fail([string]$message) {
  Write-Host "FAIL [$script:section] $message" -ForegroundColor Red
  exit 1
}

function Ok([string]$message) {
  Write-Host "  ok: $message"
}

# The log is printed whenever the exit code is not the one asked for, because
# the reason an MSI failed is only ever in it.
function Invoke-Msi([string[]]$Arguments, [string]$Tag, [int]$Expect) {
  $log = Join-Path $LogDir "$Tag.log"
  $all = $Arguments + @('/qn', '/l*v', $log)
  Write-Host "  msiexec $($all -join ' ')"
  $p = Start-Process -FilePath msiexec.exe -ArgumentList $all -Wait -PassThru
  if ($p.ExitCode -ne $Expect) {
    Write-Host "  exit $($p.ExitCode), expected $Expect; log follows:" -ForegroundColor Yellow
    if (Test-Path $log) { Get-Content $log }
    Fail "msiexec exited $($p.ExitCode), expected $Expect"
  }
  Ok "msiexec exited $Expect"
}

function MachinePath() {
  # The registry, which is where the MSI wrote it and where a newly created
  # process gets its environment from. Reading $env:Path instead would report
  # the environment this shell was started with, which predates the install.
  return [Environment]::GetEnvironmentVariable('Path', 'Machine')
}

# The command as a fresh shell would resolve it: PATH taken from the machine
# and user registry values, and the program named without a directory.
function Invoke-FromFreshShell([string]$CommandLine) {
  $path = (MachinePath) + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = 'powershell.exe'
  $psi.Arguments = "-NoProfile -Command `"$CommandLine`""
  $psi.UseShellExecute = $false
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.EnvironmentVariables['Path'] = $path
  $p = [System.Diagnostics.Process]::Start($psi)
  $out = $p.StandardOutput.ReadToEnd() + $p.StandardError.ReadToEnd()
  $p.WaitForExit()
  return @{ Code = $p.ExitCode; Output = $out }
}

# An empty install directory left behind is not a failure worth reporting: MSI
# removes a folder it created only when nothing else claims it. A file in it is.
function Assert-NotInstalled() {
  if (Test-Path $InstallDir) {
    $left = Get-ChildItem $InstallDir -Recurse -File
    if ($left) { Fail "$InstallDir still holds: $(($left | ForEach-Object Name) -join ', ')" }
  }
  if ((MachinePath) -like '*remote-docker*') {
    Fail "the system PATH still names remote-docker: $(MachinePath)"
  }
}

# ---------------------------------------------------------------------------
Section 'a machine with no docker.exe anywhere the installer looks'

# GitHub's Windows runners ship a docker.exe, which is exactly what the refusal
# is about, so the positive cases need it out of the way first. Recorded and
# reported rather than deleted quietly. The four are the ones remote-docker.wxs
# searches.
$candidates = @(
  (Join-Path $env:SystemRoot 'System32\docker.exe'),
  (Join-Path $env:SystemRoot 'SysWOW64\docker.exe'),
  (Join-Path $env:ProgramFiles 'Docker\docker.exe'),
  (Join-Path $env:ProgramFiles 'Docker\Docker\resources\bin\docker.exe')
)
$moved = 0
foreach ($c in $candidates) {
  if (Test-Path $c) {
    Move-Item $c (Join-Path $Stash ([guid]::NewGuid().ToString() + '.exe'))
    $moved++
    Ok "moved aside: $c"
  }
}
if ($moved -eq 0) { Ok 'none were present' }
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '1. a default install puts remote-docker.exe in Program Files, and no docker.exe'

Invoke-Msi @('/i', $Msi) 'install-default' 0

if (-not (Test-Path $Exe)) { Fail "missing $Exe" }
Ok "$Exe is $((Get-Item $Exe).Length) bytes"

if (Test-Path $Alias) { Fail "$Alias exists, and the feature was not selected" }
Ok 'no docker.exe, which is the default'

# ---------------------------------------------------------------------------
Section '2. the install directory is on the system PATH, appended'

$path = MachinePath
if ($path -notlike "*$InstallDir*") { Fail "system PATH does not name $InstallDir : $path" }
$entries = @($path -split ';' | Where-Object { $_ -ne '' })
if ($entries[0] -like '*remote-docker*') {
  Fail "the install directory is FIRST on PATH; it must be appended, not prepended: $path"
}
$at = [array]::FindIndex($entries, [Predicate[string]] { param($e) $e -like '*remote-docker*' })
Ok "entry $($at + 1) of $($entries.Count): $($entries[$at])"

# ---------------------------------------------------------------------------
Section '3. a fresh shell runs `remote-docker remote --help` off that PATH'

$r = Invoke-FromFreshShell 'remote-docker remote --help'
if ($r.Code -ne 0) { Fail "exit $($r.Code) from a fresh shell; output: $($r.Output)" }
if ($r.Output -notmatch 'remote') { Fail "unexpected help output: $($r.Output)" }
Ok "exit 0, $(($r.Output -split "`n").Count) lines of help"

# ---------------------------------------------------------------------------
Section '4. uninstall removes the files and the PATH entry'

Invoke-Msi @('/x', $Msi) 'uninstall-default' 0
Assert-NotInstalled
Ok 'the file and the PATH entry are both gone'

# ---------------------------------------------------------------------------
Section '5. ADDLOCAL selects the docker.exe feature, and it works'

Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,DockerName') 'install-feature' 0
if (-not (Test-Path $Alias)) { Fail "the feature was selected and $Alias was not created" }
Ok "$Alias is $((Get-Item $Alias).Length) bytes"

$r = Invoke-FromFreshShell 'docker remote --help'
if ($r.Code -ne 0) { Fail "exit $($r.Code) from 'docker remote --help'; output: $($r.Output)" }
Ok 'a fresh shell reaches it as `docker`'

# ---------------------------------------------------------------------------
Section '6. uninstall removes the copy as well as the original'

Invoke-Msi @('/x', $Msi) 'uninstall-feature' 0
Assert-NotInstalled
Ok 'both names are gone'

# ---------------------------------------------------------------------------
Section '7. the feature is refused when a docker.exe is already on PATH'

$stub = Join-Path $env:SystemRoot 'System32\docker.exe'
Copy-Item (Join-Path $env:SystemRoot 'System32\where.exe') $stub
Ok "put a docker.exe at $stub, which is on the system PATH"

# 1603 is what a type 19 error custom action gives a silent install.
Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,DockerName') 'refusal' 1603
Assert-NotInstalled
Ok 'nothing was installed'

$refusalLog = Get-Content (Join-Path $LogDir 'refusal.log') -Raw
$tail = $refusalLog.Substring([Math]::Max(0, $refusalLog.Length - 2000))
if ($refusalLog -notmatch 'would shadow it') {
  Fail "the refusal message is not in the log; tail: $tail"
}
if ($refusalLog -notmatch [regex]::Escape('System32\docker.exe')) {
  Fail "the refusal did not name the docker.exe it found; tail: $tail"
}
Ok 'the message names the docker.exe it found'

# ---------------------------------------------------------------------------
Section '8. a default install is not refused by the same docker.exe'

Invoke-Msi @('/i', $Msi) 'install-alongside' 0
if (Test-Path $Alias) { Fail "$Alias exists; the feature was not asked for" }
Ok 'installed alongside it, without the feature'
Invoke-Msi @('/x', $Msi) 'uninstall-alongside' 0
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '9. ALLOWDOCKERSHADOW=1 overrides the refusal'

Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,DockerName', 'ALLOWDOCKERSHADOW=1') 'override' 0
if (-not (Test-Path $Alias)) { Fail "the override was given and $Alias was not created" }
Ok 'the copy was made'
Invoke-Msi @('/x', $Msi) 'uninstall-override' 0
Assert-NotInstalled

Remove-Item $stub -Force

Write-Host ''
Write-Host 'all sections passed' -ForegroundColor Green
exit 0
