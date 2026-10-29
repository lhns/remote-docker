# The Windows installer, end to end: install, the PATH feature, the docker.exe
# feature (on by default, and what happens when another docker.exe is on PATH),
# uninstall, the per-user install by a user
# with no administrator rights, and the optional Hyper-V workspace feature
# (service, group, a machine, upgrade, removal, adding and removing it with
# Modify) against a stand-in for Hyper-V.
#
#   pwsh test/msi.ps1 -Msi dist/remote-docker_0.6.0_windows_amd64.msi `
#     -StandinMsi dist/standin_0.0.1.msi -StandinNextMsi dist/standin_0.0.2.msi
#
# Runs elevated and makes machine-wide changes: it installs to Program Files,
# edits the system PATH, creates a standard local user to install as, and moves
# any docker.exe it finds out of the way so the feature can be tested both with
# and without one present. Every PATH it edits is put back after the case. Meant for a throwaway machine, which is what
# .github/workflows/msi.yml gives it.

[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$Msi,
  # The same package built with the rdstandin tag (a stand-in for Hyper-V), at
  # two versions, for the Hyper-V workspace sections and the upgrade.
  [Parameter(Mandatory = $true)][string]$StandinMsi,
  [Parameter(Mandatory = $true)][string]$StandinNextMsi
)

$ErrorActionPreference = 'Stop'
$Msi = (Resolve-Path $Msi).Path
$StandinMsi = (Resolve-Path $StandinMsi).Path
$StandinNextMsi = (Resolve-Path $StandinNextMsi).Path
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
function Invoke-Msi([string[]]$Arguments, [string]$Tag, [int]$Expect, [string]$Ui = '/qn', [int]$Minutes = 10) {
  $log = Join-Path $LogDir "$Tag.log"
  $all = $Arguments + @($Ui, '/l*v', $log)
  Write-Host "  msiexec $($all -join ' ')"
  $p = Start-Process -FilePath msiexec.exe -ArgumentList $all -PassThru
  # Bounded, so a hang fails here with the log and what was still running,
  # rather than at the job's limit with nothing to read.
  if (-not $p.WaitForExit($Minutes * 60 * 1000)) {
    Write-Host "  msiexec still running after $Minutes minutes; still running:" -ForegroundColor Yellow
    Get-CimInstance Win32_Process -Filter "Name='msiexec.exe' OR Name='remote-docker.exe'" |
      ForEach-Object { Write-Host "    $($_.ProcessId) $($_.CommandLine)" }
    if (Test-Path $log) { Get-Content $log -Tail 200 }
    Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    Fail "msiexec did not finish within $Minutes minutes"
  }
  if ($p.ExitCode -ne $Expect) {
    Write-Host "  exit $($p.ExitCode), expected $Expect; log follows:" -ForegroundColor Yellow
    if (Test-Path $log) { Get-Content $log }
    Fail "msiexec exited $($p.ExitCode), expected $Expect"
  }
  Ok "msiexec exited $Expect"
}

function Log-Of([string]$Tag) { return Get-Content (Join-Path $LogDir "$Tag.log") -Raw }

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

# What the install logged about the docker.exe that `docker` already runs, or
# about leaving ours out: the value of the DOCKERNOTE property, or empty.
function Docker-Note([string]$Tag) {
  $log = Log-Of $Tag
  return [regex]::Match($log, "(?m)^.*PROPERTY CHANGE: Adding DOCKERNOTE property\. Its value is '(.*)'.*$").Groups[1].Value
}

# A PATH value as the registry holds it, and put back. Written as REG_EXPAND_SZ
# like the real ones, so the `#%` prefix a raw search puts on it is in play.
$SystemPathKey = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment'
$script:pathBackup = @{}
function Set-RegistryPath([string]$Key, [string]$Dir, [switch]$First) {
  $item = Get-Item $Key
  $old = [string]$item.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
  if (-not $script:pathBackup.ContainsKey($Key)) {
    $present = $null -ne $item.GetValue('Path', $null, 'DoNotExpandEnvironmentNames')
    $script:pathBackup[$Key] = @{ Value = $old; Present = $present; Kind = $(if ($present) { $item.GetValueKind('Path') } else { 'ExpandString' }) }
  }
  $new = if ($First) { "$Dir;$old" } else { "$old;$Dir".TrimStart(';') }
  Set-ItemProperty $Key -Name Path -Value $new -Type ExpandString
}
function Restore-RegistryPath([string]$Key) {
  $b = $script:pathBackup[$Key]
  if (-not $b) { return }
  if ($b.Present) { Set-ItemProperty $Key -Name Path -Value $b.Value -Type $b.Kind } else { Remove-ItemProperty $Key -Name Path -ErrorAction SilentlyContinue }
  $script:pathBackup.Remove($Key)
}

# ---------------------------------------------------------------------------
Section 'a machine with no docker.exe anywhere the installer looks'

# GitHub's Windows runners ship a docker.exe, which would be the docker.exe that
# `docker` already runs, so the cases below need it out of the way first. Moved
# and reported rather than deleted quietly. These are what remote-docker.wxs
# searches.
$candidates = @(
  (Join-Path $env:SystemRoot 'System32\docker.exe'),
  (Join-Path $env:SystemRoot 'SysWOW64\docker.exe'),
  (Join-Path $env:ProgramFiles 'Docker\docker.exe'),
  (Join-Path $env:ProgramFiles 'Docker\Dockeresourcesin\docker.exe'),
  (Join-Path $env:ProgramFiles 'WinGet\Links\docker.exe'),
  (Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Links\docker.exe')
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
Section '1. a default install puts remote-docker.exe and docker.exe in Program Files'

Invoke-Msi @('/i', $Msi) 'install-default' 0

if (-not (Test-Path $Exe)) { Fail "missing $Exe" }
Ok "$Exe is $((Get-Item $Exe).Length) bytes"

if (-not (Test-Path $Alias)) { Fail "$Alias is missing; docker.exe is on by default" }
Ok "$Alias is $((Get-Item $Alias).Length) bytes, with nothing selected"
if (Docker-Note 'install-default') { Fail "a default install with no other docker.exe logged: $(Docker-Note 'install-default')" }
Ok 'and nothing was logged about another docker.exe'

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

Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,Path,DockerName') 'install-feature' 0
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
Section '7. a docker.exe in System32 comes first, and ours is installed anyway'

$stub = Join-Path $env:SystemRoot 'System32\docker.exe'
Copy-Item (Join-Path $env:SystemRoot 'System32\where.exe') $stub
Ok "put a docker.exe at $stub, which is on the system PATH and ahead of the install directory"

Invoke-Msi @('/i', $Msi) 'docker-first' 0
if (-not (Test-Path $Alias)) { Fail "$Alias is missing; a docker.exe that comes first shadows nothing" }
$note = Docker-Note 'docker-first'
Write-Host "  saw: [$note]"
# Pins the SystemFolder/System64Folder search: System32 is the second of the two.
if ($note -notmatch 'keeps running' -or $note -notmatch [regex]::Escape('System32\docker.exe')) {
  Fail "the log does not say that docker keeps running $stub; note: [$note]"
}
Ok 'installed, and the log names the docker.exe that `docker` keeps running'
Invoke-Msi @('/x', $Msi) 'uninstall-docker-first' 0
Assert-NotInstalled

Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,Path,DockerName') 'docker-first-explicit' 0
if (-not (Test-Path $Alias)) { Fail "$Alias is missing after an explicit ADDLOCAL beside a docker.exe in System32" }
Ok 'an explicit ADDLOCAL is not refused either'
Invoke-Msi @('/x', $Msi) 'uninstall-docker-first-explicit' 0
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '8. a leftover Docker Desktop docker.exe whose folder is not on PATH counts for nothing'

$desktopBin = Join-Path $env:ProgramFiles 'Docker\Docker\resources\bin'
New-Item -ItemType Directory -Force -Path $desktopBin | Out-Null
Copy-Item (Join-Path $env:SystemRoot 'System32\where.exe') (Join-Path $desktopBin 'docker.exe')
Remove-Item $stub -Force
$sysPath = (Get-Item $SystemPathKey).GetValue('Path', '', 'DoNotExpandEnvironmentNames')
if ($sysPath -like '*Docker\Docker\resources\bin*') { Fail "the runner's system PATH already names Docker Desktop: $sysPath" }
Ok "put a docker.exe at $desktopBin, which is not on the system PATH"

Invoke-Msi @('/i', $Msi) 'desktop-off-path' 0
if (-not (Test-Path $Alias)) { Fail "$Alias is missing; a docker.exe off PATH must not count" }
if (Docker-Note 'desktop-off-path') { Fail "the log mentions it: $(Docker-Note 'desktop-off-path')" }
Ok 'installed, and nothing was logged about that file'
Invoke-Msi @('/x', $Msi) 'uninstall-desktop-off-path' 0
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '9. the same folder first on the system PATH (REG_EXPAND_SZ) is seen, and noted'

Set-RegistryPath $SystemPathKey $desktopBin -First
Invoke-Msi @('/i', $Msi) 'desktop-on-path' 0
if (-not (Test-Path $Alias)) { Fail "$Alias is missing; a docker.exe that comes first shadows nothing" }
$note = Docker-Note 'desktop-on-path'
Write-Host "  saw: [$note]"
if ($note -notmatch 'keeps running' -or $note -notmatch [regex]::Escape('Docker\Docker\resources\bin\docker.exe')) {
  Fail "the log does not say that docker keeps running Docker Desktop's file; note: [$note]"
}
Ok 'installed, and the log names the docker.exe in Docker Desktop''s folder, found first on the PATH'
Invoke-Msi @('/x', $Msi) 'uninstall-desktop-on-path' 0
Restore-RegistryPath $SystemPathKey
Remove-Item (Join-Path $env:ProgramFiles 'Docker') -Recurse -Force
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '9a. an all-users install would shadow a docker.exe on the user PATH: a silent install leaves it out'

# winget's command link, in the installing user's own PATH. A silent install
# cannot be asked, so by default it leaves docker.exe out and says why; an
# explicit ADDLOCAL is refused unless ALLOWDOCKERSHADOW=1.
$wingetLinks = Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Links'
$userKey = 'HKCU:\Environment'
New-Item -ItemType Directory -Force -Path $wingetLinks | Out-Null
$wingetStub = Join-Path $wingetLinks 'docker.exe'
Copy-Item (Join-Path $env:SystemRoot 'System32\where.exe') $wingetStub
Set-RegistryPath $userKey '%LOCALAPPDATA%\Microsoft\WinGet\Links'
Ok "put a docker.exe at $wingetStub, on this user's PATH"

Invoke-Msi @('/i', $Msi) 'user-path-default' 0
if (-not (Test-Path $Exe)) { Fail "missing $Exe" }
if (Test-Path $Alias) { Fail "$Alias exists; a silent default install must leave it out when it would shadow $wingetStub" }
$note = Docker-Note 'user-path-default'
Write-Host "  saw: [$note]"
if ($note -notmatch 'left out' -or $note -notmatch [regex]::Escape('WinGet\Links\docker.exe')) {
  Fail "the log does not say why docker.exe was left out; note: [$note]"
}
Ok 'remote-docker.exe installed, docker.exe left out, and the log names the file it would have shadowed'
Invoke-Msi @('/x', $Msi) 'uninstall-user-path-default' 0
Assert-NotInstalled

# 1603 is what a type 19 error custom action gives a silent install.
Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,Path,DockerName') 'refusal' 1603
Assert-NotInstalled
$refusalLog = Log-Of 'refusal'
$tail = $refusalLog.Substring([Math]::Max(0, $refusalLog.Length - 2000))
if ($refusalLog -notmatch 'in front of it') { Fail "the refusal message is not in the log; tail: $tail" }
if ($refusalLog -notmatch [regex]::Escape('WinGet\Links\docker.exe')) { Fail "the refusal did not name the docker.exe it found; tail: $tail" }
Ok 'an explicit ADDLOCAL is refused, naming the docker.exe it found; nothing was installed'

Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,Path,DockerName', 'ALLOWDOCKERSHADOW=1') 'override' 0
if (-not (Test-Path $Alias)) { Fail "the override was given and $Alias was not created" }
Ok 'ALLOWDOCKERSHADOW=1 overrides it: the copy was made'
Invoke-Msi @('/x', $Msi) 'uninstall-override' 0
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '9b. docker.exe without the Path feature is installed beside another one'

# Nothing is shadowed when the install directory is not on PATH (ADR 0048).
$stubBefore = (Get-FileHash $wingetStub).Hash
Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,DockerName') 'no-path-docker' 0
if (-not (Test-Path $Alias)) { Fail "the feature was selected and $Alias was not created" }
if (-not (Test-Path $wingetStub)) { Fail "$wingetStub was removed" }
if ((Get-FileHash $wingetStub).Hash -ne $stubBefore) { Fail "$wingetStub was changed" }
Ok 'installed beside the other docker.exe, which is untouched'
Invoke-Msi @('/x', $Msi) 'uninstall-no-path-docker' 0
Assert-NotInstalled

# A per-user install puts ours after the user PATH's, so the same file shadows nothing.
$userDir = Join-Path $env:LOCALAPPDATA 'Programs\remote-docker'
Invoke-Msi @('/i', $Msi, 'MSIINSTALLPERUSER=1') 'user-path-per-user' 0
if (-not (Test-Path (Join-Path $userDir 'docker.exe'))) { Fail "no $userDir\docker.exe in a per-user install beside a docker.exe on the same PATH" }
$note = Docker-Note 'user-path-per-user'
Write-Host "  saw: [$note]"
if ($note -notmatch 'keeps running' -or $note -notmatch [regex]::Escape('WinGet\Links\docker.exe')) {
  Fail "the log does not say that docker keeps running $wingetStub; note: [$note]"
}
Ok 'a per-user install is not asked: docker.exe goes in behind it, and the log says so'
Invoke-Msi @('/x', $Msi) 'uninstall-user-path-per-user' 0
Assert-NotInstalled

Restore-RegistryPath $userKey
Remove-Item $wingetStub -Force

# ---------------------------------------------------------------------------
Section '9c. the PATH entry is optional, and Modify adds and removes it'

Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main') 'path-off' 0
if (-not (Test-Path $Exe)) { Fail "missing $Exe" }
if ((MachinePath) -like "*$InstallDir*") { Fail "system PATH names $InstallDir without the Path feature: $(MachinePath)" }
$r = Invoke-FromFreshShell 'remote-docker remote --help'
if ($r.Code -eq 0) { Fail "a fresh shell ran remote-docker with no PATH entry: $($r.Output)" }
Ok 'installed, not on PATH, and a fresh shell does not find it'

Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Path') 'path-add' 0
if ((MachinePath) -notlike "*$InstallDir*") { Fail "Modify added the feature and PATH does not name $InstallDir : $(MachinePath)" }
$r = Invoke-FromFreshShell 'remote-docker remote --help'
if ($r.Code -ne 0) { Fail "exit $($r.Code) from a fresh shell after Modify; output: $($r.Output)" }
Ok 'Modify added the entry and a fresh shell runs it'

Invoke-Msi @('/i', $Msi, 'REMOVE=Path') 'path-remove' 0
if ((MachinePath) -like "*$InstallDir*") { Fail "removing the feature left the entry: $(MachinePath)" }
if (-not (Test-Path $Exe)) { Fail "removing the feature took $Exe with it" }
Ok 'Modify removed the entry and kept the exe'

Invoke-Msi @('/x', $Msi) 'uninstall-path-off' 0
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '9d. an upgrade from the latest released MSI keeps the PATH entry and does not add docker.exe'

# The only real check that the old product's PATH component, which lived in
# Main, migrates into the Path feature. Needs the network and gh; a failure to
# download is a failure, never a skip. $Msi is built at a version above any
# release (see msi.yml), or this would be a downgrade.
$relDir = Join-Path $Temp 'released-msi'
if (Test-Path $relDir) { Remove-Item -Recurse -Force $relDir }
New-Item -ItemType Directory -Force -Path $relDir | Out-Null
$gh = & gh release download --repo lhns/remote-docker --pattern '*windows_amd64.msi' --dir $relDir 2>&1 | Out-String
if ($LASTEXITCODE -ne 0) { Fail "could not download the latest released MSI: $gh" }
$released = @(Get-ChildItem $relDir -Filter '*.msi')
if ($released.Count -ne 1) { Fail "expected one released x64 MSI in $relDir, found $($released.Count): $gh" }
Ok "downloaded $($released[0].Name)"

Invoke-Msi @('/i', $released[0].FullName) 'released-install' 0
if ((MachinePath) -notlike "*$InstallDir*") { Fail "the released MSI left PATH without $InstallDir : $(MachinePath)" }
Invoke-Msi @('/i', $Msi) 'released-upgrade' 0
if ((MachinePath) -notlike "*$InstallDir*") { Fail "the upgrade dropped the PATH entry: $(MachinePath)" }
# docker.exe is on by default now, but an upgrade keeps what was installed.
if (Test-Path $Alias) { Fail "$Alias exists after an upgrade from a release that was installed without it" }
Ok 'the upgrade did not add docker.exe to an install that never had it'
Ok 'the system PATH still names the install directory'
if ($null -eq (Get-ItemProperty 'HKLM:\SOFTWARE\lhns\remote-docker' -Name Path -ErrorAction SilentlyContinue)) {
  Fail 'the Path registry value is missing after the upgrade'
}
Ok 'the Path registry value exists'
$r = Invoke-FromFreshShell 'remote-docker remote --help'
if ($r.Code -ne 0) { Fail "exit $($r.Code) from a fresh shell after the upgrade; output: $($r.Output)" }
Ok 'a fresh shell runs it'
Invoke-Msi @('/x', $Msi) 'released-uninstall' 0
Assert-NotInstalled

# ===========================================================================
# One package, two contexts (ADR 0048, 2026-10-28). The runner is an
# administrator, so "a user with no administrator rights" is a standard local
# user, the one sections 15 to 18 also use. Each claim about Windows Installer
# that the design rests on is printed from the log it came out of, so a wrong
# one shows what Windows Installer did instead.

$Pub = 'C:\Users\Public\rdtest'
New-Item -ItemType Directory -Force -Path $Pub | Out-Null
& icacls.exe $Pub /grant 'Everyone:(OI)(CI)M' | Out-Null
$StdName = 'rdstd'
$StdPass = 'Rd-Std-' + [guid]::NewGuid().ToString('N').Substring(0, 12) + '!A1'
if (Get-LocalUser -Name $StdName -ErrorAction SilentlyContinue) { Remove-LocalUser -Name $StdName }
$null = New-LocalUser -Name $StdName -Password (ConvertTo-SecureString $StdPass -AsPlainText -Force) -PasswordNeverExpires -AccountNeverExpires
Add-LocalGroupMember -Group 'Users' -Member $StdName
if ((Get-LocalGroupMember -Group 'Administrators' | Where-Object { $_.Name -like "*\$StdName" })) { Fail "$StdName is an administrator" }
Write-Host "created the standard user $StdName"
$cred = New-Object System.Management.Automation.PSCredential($StdName, (ConvertTo-SecureString $StdPass -AsPlainText -Force))

# One logon of the standard user, hidden, optionally with its output in $Pub\<tag>.out and .err.
function Start-AsStd([string]$File, [string[]]$Arguments, [string]$Tag = '') {
  $a = @{
    FilePath = $File; ArgumentList = $Arguments; Credential = $cred; LoadUserProfile = $true
    UseNewEnvironment = $true; WorkingDirectory = $env:SystemRoot; WindowStyle = 'Hidden'; PassThru = $true
  }
  if ($Tag) { $a.RedirectStandardOutput = "$Pub\$Tag.out"; $a.RedirectStandardError = "$Pub\$Tag.err" }
  return Start-Process @a
}

function Invoke-AsStd([string[]]$Arguments, [string]$Tag) {
  $p = Start-AsStd $Exe $Arguments $Tag
  $null = $p.WaitForExit(180000)
  $out = (Get-Content "$Pub\$Tag.out", "$Pub\$Tag.err" -Raw -ErrorAction SilentlyContinue) -join ''
  if (-not $p.HasExited -or $p.ExitCode -ne 0) { Fail "$StdName could not run '$($Arguments -join ' ')'; output: $out" }
}

# Windows Server defaults the machine policy DisableMsi to 1, "Windows Installer:
# Turn off Windows Installer for non-managed apps only", under which Windows
# Installer rejects any install by a user without administrator rights, in
# either context (1625, "Non-assigned apps are disabled for non-admin users").
# windows-latest is a Server image, so it is set to 0 for these sections and put
# back after them. Windows 11 defaults to 0.
$PolicyKey = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\Installer'
$policyBefore = (Get-ItemProperty $PolicyKey -Name DisableMsi -ErrorAction SilentlyContinue).DisableMsi
Write-Host "  DisableMsi policy before: [$policyBefore]"
if ($policyBefore -ne 0) {
  New-Item -Path $PolicyKey -Force | Out-Null
  Set-ItemProperty $PolicyKey -Name DisableMsi -Value 0 -Type DWord
  Write-Host '  set DisableMsi to 0 for sections 9e to 9l'
}

# The MSIs where the standard user can read them, and msiexec as that user: a
# token without the Administrators group, never elevated.
$StdMsi = Join-Path $Pub 'remote-docker.msi'
$StdStandin1 = Join-Path $Pub 'standin_0.0.1.msi'
$StdStandin2 = Join-Path $Pub 'standin_0.0.2.msi'
Copy-Item $Msi $StdMsi -Force
Copy-Item $StandinMsi $StdStandin1 -Force
Copy-Item $StandinNextMsi $StdStandin2 -Force

function Invoke-AsStdMsi([string[]]$Arguments, [string]$Tag, [int]$Expect, [string]$Ui = '/qn', [int]$Minutes = 10) {
  $log = Join-Path $Pub "$Tag.log"
  $all = $Arguments + @($Ui, '/l*v', $log)
  Write-Host "  msiexec as ${StdName}: $($all -join ' ')"
  $p = Start-AsStd msiexec.exe $all
  if (-not $p.WaitForExit($Minutes * 60 * 1000)) {
    Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    if (Test-Path $log) {
      Write-Host '  scope decisions in the log:'
      Select-String -Path $log -Pattern 'MSIINSTALLPERUSER|ALLUSERS|AdminUser|INSTALLSCOPE|Doing action: (FindRelated|CostInit|ExecuteAction|INSTALL)|Error 19|UILevel|Skipping action: Set' | ForEach-Object { Write-Host "    $($_.Line.Trim())" }
      Get-Content $log -Tail 200
    }
    Fail "msiexec as $StdName did not finish within $Minutes minutes"
  }
  if (Test-Path $log) { Copy-Item $log (Join-Path $LogDir "$Tag.log") -Force }
  if ($p.ExitCode -ne $Expect) {
    Write-Host "  exit $($p.ExitCode), expected $Expect; log follows:" -ForegroundColor Yellow
    if (Test-Path $log) { Get-Content $log }
    Fail "msiexec as $StdName exited $($p.ExitCode), expected $Expect"
  }
  Ok "msiexec as $StdName exited $Expect"
}

# What the standard user sees of its own profile and registry, which this
# elevated process cannot read (its hive is loaded only while it runs).
$factsScript = Join-Path $Pub 'facts.ps1'
Set-Content -Path $factsScript -Encoding ascii -Value @'
param($Cmd)
$u = [Environment]::GetEnvironmentVariable('Path', 'User')
$m = [Environment]::GetEnvironmentVariable('Path', 'Machine')
"localappdata=$env:LOCALAPPDATA"
"userpath=$u"
"machinepath=$m"
"key=" + (Test-Path 'HKCU:\Software\lhns\remote-docker')
# A per-user product has no Uninstall key; Windows Installer lists it by upgrade code.
$wi = New-Object -ComObject WindowsInstaller.Installer
$versions = foreach ($pc in $wi.RelatedProducts('{6FDE5FD9-D1A3-4094-B8EC-2193CA79C91F}')) { $wi.ProductInfo($pc, 'VersionString') }
"arp=" + ($versions -join ',')
if ($Cmd) {
  $env:Path = "$m;$u"
  $o = (& cmd.exe /c $Cmd 2>&1 | Out-String)
  "code=$LASTEXITCODE"
  "output=" + ($o -replace "\s+", ' ')
}
'@

# The standard user's own PATH value, added to and put back by a logon of that
# user, since its registry hive is loaded only while it runs.
$userPathScript = Join-Path $Pub 'userpath.ps1'
Set-Content -Path $userPathScript -Encoding ascii -Value @'
param($Action, $Dir)
$bak = 'C:\Users\Public\rdtest\userpath.json'
$key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
$old = $key.GetValue('Path', $null, 'DoNotExpandEnvironmentNames')
if ($Action -eq 'Add') {
  $kind = if ($null -ne $old) { [string]$key.GetValueKind('Path') } else { 'ExpandString' }
  @{ Present = ($null -ne $old); Value = [string]$old; Kind = $kind } | ConvertTo-Json | Set-Content $bak
  $key.SetValue('Path', ((([string]$old) + ';' + $Dir).TrimStart(';')), 'ExpandString')
} else {
  $b = Get-Content $bak -Raw | ConvertFrom-Json
  if ($b.Present) { $key.SetValue('Path', [string]$b.Value, [string]$b.Kind) } else { $key.DeleteValue('Path', $false) }
}
$key.Close()
'@

function Set-StdUserPath([string]$Action, [string]$Dir = 'none') {
  $p = Start-AsStd powershell.exe @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $userPathScript, $Action, $Dir) 'userpath'
  if (-not $p.WaitForExit(180000) -or $p.ExitCode -ne 0) {
    Fail "userpath $Action as $StdName failed: $((Get-Content "$Pub\userpath.out", "$Pub\userpath.err" -ErrorAction SilentlyContinue) -join ' | ')"
  }
}

function Get-StdFacts([string]$Tag, [string]$Cmd = '') {
  $p = Start-AsStd powershell.exe @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $factsScript, ('"' + $Cmd + '"')) $Tag
  if (-not $p.WaitForExit(180000)) { Fail "facts as $StdName did not finish" }
  $facts = @{}
  foreach ($line in (Get-Content "$Pub\$Tag.out" -ErrorAction SilentlyContinue)) {
    $i = $line.IndexOf('=')
    if ($i -gt 0) { $facts[$line.Substring(0, $i)] = $line.Substring($i + 1) }
  }
  if (-not $facts.ContainsKey('localappdata')) {
    Fail "no facts from ${StdName}: $((Get-Content "$Pub\$Tag.out", "$Pub\$Tag.err" -ErrorAction SilentlyContinue) -join ' | ')"
  }
  return $facts
}

# -MachineProductVisible: an all-users product is registered for every user, so only the user's own files and keys are checked.
function Assert-StdNotInstalled([string]$What, [switch]$MachineProductVisible) {
  $f = Get-StdFacts 'facts-clean'
  $dir = Join-Path $f.localappdata 'Programs\remote-docker'
  if (Test-Path $dir) {
    $left = Get-ChildItem $dir -Recurse -File
    if ($left) { Fail "$What : $dir still holds: $(($left | ForEach-Object Name) -join ', ')" }
  }
  if ($f.userpath -like '*remote-docker*') { Fail "$What : $StdName's PATH still names remote-docker: $($f.userpath)" }
  if ($f.key -eq 'True') { Fail "$What : HKCU\Software\lhns\remote-docker still exists for $StdName" }
  if ($f.arp -and -not $MachineProductVisible) { Fail "$What : $StdName's still has the product registered: $($f.arp)" }
  Ok "$What : nothing of it is left in $StdName's profile or registry"
}

# The all-users install: its files, system PATH entry and HKLM marker.
function Assert-MachineInstalled([string]$What) {
  if (-not (Test-Path $Exe) -or (MachinePath) -notlike "*$InstallDir*" -or
      -not (Get-ItemProperty 'HKLM:\SOFTWARE\lhns\remote-docker' -Name Installed -ErrorAction SilentlyContinue)) {
    Fail "$What : the all-users install is damaged"
  }
  Ok "$What : the all-users install, its system PATH entry and HKLM marker are intact"
}

# What FindRelatedProducts reported in a log: nothing, if it saw no product of the other context.
function Related-Detected([string]$Log) {
  return @([regex]::Matches($Log, '(?m)^Property\(S\): WIX_(UP|DOWN)GRADE_DETECTED = .*$') | ForEach-Object { $_.Value })
}

# ---------------------------------------------------------------------------
Section '9e. an administrator gets the per-machine default, and the log says what Windows Installer decided'

# Claim: AdminUser is set for an elevated administrator, so the per-user default
# (NOT AdminUser) does not fire, and ALLUSERS resolves to 1.
$log = Log-Of 'install-default'
$au = [regex]::Match($log, '(?m)^Property\(S\): AdminUser = .*$').Value
$al = [regex]::Match($log, '(?m)^Property\(S\): ALLUSERS = .*$').Value
$pf = [regex]::Match($log, '(?m)^Property\(S\): INSTALLFOLDER = .*$').Value
Write-Host "  saw: [$au] [$al] [$pf]"
if ($au -notmatch '= 1') { Fail "AdminUser is not set for the elevated runner: [$au]" }
if ($al -notmatch '= 1') { Fail "ALLUSERS is not 1 for the elevated runner's default install: [$al]" }
if ($log -match '(?m)^Property\(S\): MSIINSTALLPERUSER = 1') { Fail 'the elevated default install was made per user' }
Ok 'AdminUser = 1, ALLUSERS = 1 and MSIINSTALLPERUSER unset: per machine, as before'

# ---------------------------------------------------------------------------
Section '9f. a user with no administrator rights installs for themselves'

# A silent install resolves its context before any sequence runs, so with no
# argument a standard user gets a per-machine install that Windows Installer
# refuses (1603, error 1925). A silent per-user install has to ask for it.
Invoke-AsStdMsi @('/i', $StdMsi) 'std-noargs' 1603
if ((Log-Of 'std-noargs') -notmatch 'Error 1925') { Fail 'the refusal of a silent install with no arguments is not error 1925' }
Ok 'refused with error 1925, so a silent per-user install must pass MSIINSTALLPERUSER=1'

Invoke-AsStdMsi @('/i', $StdMsi, 'MSIINSTALLPERUSER=1') 'std-default' 0
$f = Get-StdFacts 'facts-default' 'remote-docker remote --help'
$stdDir = Join-Path $f.localappdata 'Programs\remote-docker'
Write-Host "  saw: localappdata=$($f.localappdata)"
if (-not (Test-Path (Join-Path $stdDir 'remote-docker.exe'))) { Fail "missing $stdDir\remote-docker.exe; the log is $LogDir\std-default.log" }
Ok "remote-docker.exe is in $stdDir"
if (Test-Path $Exe) { Fail "$Exe exists; nothing may reach Program Files from a standard user" }
if (Test-Path 'HKLM:\SOFTWARE\lhns\remote-docker') { Fail 'HKLM\Software\lhns\remote-docker exists after a per-user install' }
if ((MachinePath) -like '*remote-docker*') { Fail "the system PATH names remote-docker: $(MachinePath)" }
Ok 'nothing in Program Files, HKLM or the system PATH'

# Claim: the user PATH is appended, and a fresh shell finds the program.
$entries = @($f.userpath -split ';' | Where-Object { $_ -ne '' })
if ($entries.Count -eq 0 -or ($entries[-1] -ne ($stdDir + '\') -and $entries[-1] -ne $stdDir)) {
  Fail "the install directory is not the last entry of $StdName's PATH: $($f.userpath)"
}
Ok "appended to the user PATH: $($entries[-1])"
if ($f.code -ne '0') { Fail "exit $($f.code) from a fresh shell of $StdName; output: $($f.output)" }
Ok 'a fresh shell of that user runs `remote-docker remote --help`'
if ($f.arp -ne '99.0.0') { Fail "the product registered for that user is '$($f.arp)', not 99.0.0" }
Ok 'registered for that user as 99.0.0'

# Claims: ProgramFiles64Folder is %LocalAppData%\Programs, AdminUser is not set,
# and the elevation-not-required bit let it through.
$slog = Log-Of 'std-default'
$sau = [regex]::Match($slog, '(?m)^Property\(S\): AdminUser = .*$').Value
$spf = [regex]::Match($slog, '(?m)^Property\(S\): INSTALLFOLDER = .*$').Value
$sal = [regex]::Match($slog, '(?m)^Property\(S\): ALLUSERS = .*$').Value
$smp = [regex]::Match($slog, '(?m)^Property\(S\): MSIINSTALLPERUSER = .*$').Value
Write-Host "  saw: [$sau] [$sal] [$smp] [$spf]"
if ($sau) { Fail "AdminUser is set for the standard user: [$sau]" }
if ($sal -match '= 1') { Fail "ALLUSERS resolved to 1 for the standard user: [$sal]" }
Ok 'AdminUser unset, ALLUSERS not 1, and the install folder is under the profile'

Invoke-AsStdMsi @('/x', $StdMsi) 'std-uninstall' 0
Assert-StdNotInstalled 'after the per-user uninstall'

# ---------------------------------------------------------------------------
Section '9g. a per-user install has docker.exe by default, and another docker.exe in front of it is only noted'

Invoke-AsStdMsi @('/i', $StdMsi, 'MSIINSTALLPERUSER=1') 'std-feature' 0
$f = Get-StdFacts 'facts-feature' 'docker remote --help'
$stdDir = Join-Path $f.localappdata 'Programs\remote-docker'
if (-not (Test-Path (Join-Path $stdDir 'docker.exe'))) { Fail "$stdDir\docker.exe was not created; it is on by default" }
if ($f.code -ne '0') { Fail "exit $($f.code) from 'docker remote --help' as $StdName; output: $($f.output)" }
if (Docker-Note 'std-feature') { Fail "a per-user install with no other docker.exe logged: $(Docker-Note 'std-feature')" }
Ok 'docker.exe beside remote-docker.exe, and a fresh shell reaches it as `docker`'
Invoke-AsStdMsi @('/x', $StdMsi) 'std-feature-uninstall' 0
Assert-StdNotInstalled 'after uninstalling the docker.exe feature'

# System32 is a directory the standard user cannot write, which is the point:
# the stub is the administrator's. It comes first on PATH, so nothing is shadowed.
$stub = Join-Path $env:SystemRoot 'System32\docker.exe'
Copy-Item (Join-Path $env:SystemRoot 'System32\where.exe') $stub
Invoke-AsStdMsi @('/i', $StdMsi, 'MSIINSTALLPERUSER=1') 'std-first' 0
$note = Docker-Note 'std-first'
Remove-Item $stub -Force
Write-Host "  saw: [$note]"
if (-not (Test-Path (Join-Path $stdDir 'docker.exe'))) { Fail "$stdDir\docker.exe was not created beside a docker.exe in System32" }
if ($note -notmatch 'keeps running' -or $note -notmatch [regex]::Escape('System32\docker.exe')) {
  Fail "the log does not say that docker keeps running the docker.exe in System32; note: [$note]"
}
Ok 'installed, and the log names the docker.exe in System32'
Invoke-AsStdMsi @('/x', $StdMsi) 'std-first-uninstall' 0

# Docker Desktop's directory is under the real Program Files, not under the
# per-user "Programs" folder ProgramFiles64Folder means to this install. Not on
# PATH, so it counts for nothing.
$desktopBin = Join-Path $env:ProgramFiles 'Docker\Docker\resources\bin'
New-Item -ItemType Directory -Force -Path $desktopBin | Out-Null
Copy-Item (Join-Path $env:SystemRoot 'System32\where.exe') (Join-Path $desktopBin 'docker.exe')
Invoke-AsStdMsi @('/i', $StdMsi, 'MSIINSTALLPERUSER=1') 'std-desktop' 0
Remove-Item (Join-Path $env:ProgramFiles 'Docker') -Recurse -Force
if (-not (Test-Path (Join-Path $stdDir 'docker.exe'))) { Fail "$stdDir\docker.exe was not created beside a leftover Docker Desktop file" }
if (Docker-Note 'std-desktop') { Fail "the log mentions the leftover file: $(Docker-Note 'std-desktop')" }
Ok "installed beside a leftover docker.exe in $env:ProgramFiles, which is not on PATH, and nothing was logged"
Invoke-AsStdMsi @('/x', $StdMsi) 'std-desktop-uninstall' 0

# A docker.exe on that user's own PATH, in that user's own hive: this install goes after it.
$links = Join-Path $f.localappdata 'Microsoft\WinGet\Links'
New-Item -ItemType Directory -Force -Path $links | Out-Null
Copy-Item (Join-Path $env:SystemRoot 'System32\where.exe') (Join-Path $links 'docker.exe')
Set-StdUserPath 'Add' '%LOCALAPPDATA%\Microsoft\WinGet\Links'
Invoke-AsStdMsi @('/i', $StdMsi, 'MSIINSTALLPERUSER=1') 'std-user-path' 0
$note = Docker-Note 'std-user-path'
Write-Host "  saw: [$note]"
if (-not (Test-Path (Join-Path $stdDir 'docker.exe'))) { Fail "$stdDir\docker.exe was not created beside a docker.exe on the user's PATH" }
if ($note -notmatch 'keeps running' -or $note -notmatch [regex]::Escape('WinGet\Links\docker.exe')) {
  Fail "the log does not say that docker keeps running the docker.exe on $StdName's PATH; note: [$note]"
}
Ok "installed behind a docker.exe on $StdName's own PATH, which only that user's hive shows"
Invoke-AsStdMsi @('/x', $StdMsi) 'std-user-path-uninstall' 0
Set-StdUserPath 'Restore'
Remove-Item (Join-Path $f.localappdata 'Microsoft\WinGet') -Recurse -Force
Assert-StdNotInstalled 'after the per-user docker.exe cases'

# ---------------------------------------------------------------------------
Section '9h. the Hyper-V workspace is refused in a per-user install, by name'

Invoke-AsStdMsi @('/i', $StdMsi, 'MSIINSTALLPERUSER=1', 'ADDLOCAL=Main,HyperV', 'HYPERVSERVICE=msiserver') 'std-hyperv' 1603
if ((Log-Of 'std-hyperv') -notmatch 'needs an installation for all users') { Fail 'the refusal message is not in the log' }
Ok 'refused with 1603, and the message says it needs all users'
Assert-StdNotInstalled 'after the Hyper-V refusal'

# ---------------------------------------------------------------------------
Section '9i. one install per user: a per-user install over a per-machine one is refused'

# A different version, so a different ProductCode: the same package is refused
# by Windows Installer itself (1625, "maintenance mode ... installed
# per-machine"), which is not the case an upgrade is.
Invoke-Msi @('/i', $Msi) 'co-machine' 0
Invoke-AsStdMsi @('/i', $StdStandin1, 'MSIINSTALLPERUSER=1') 'std-over-machine' 1603
$cl = Log-Of 'std-over-machine'
if ($cl -notmatch 'already installed for all users') { Fail "the refusal is not in the log; tail: $($cl.Substring([Math]::Max(0, $cl.Length - 1500)))" }
Ok 'refused, naming the all-users install'
# Claim: FindRelatedProducts does not see a product of the other context, which
# is why the registry markers exist. The all-users product is the higher version,
# so seeing it would have set WIX_DOWNGRADE_DETECTED.
$fr = Related-Detected $cl
Write-Host "  saw: WIX_*GRADE_DETECTED [$($fr -join '; ')]"
if ($fr) { Fail "FindRelatedProducts saw the per-machine product from a per-user install: [$($fr -join '; ')]; the markers are not needed, so say so" }
Ok 'FindRelatedProducts did not see the product of the other context'
Assert-StdNotInstalled 'after the refused per-user install' -MachineProductVisible
Invoke-Msi @('/x', $Msi) 'co-machine-uninstall' 0
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '9j. an all-users install over the installing user''s own copy warns, and the copy can still be removed'

# The same package across contexts is refused by Windows Installer itself, so
# the user's copy is the 0.0.1 stand-in and the all-users one is 99.0.0.
# An administrator may install for themselves too, by asking for it.
$userDir = Join-Path $env:LOCALAPPDATA 'Programs\remote-docker'
Invoke-Msi @('/i', $StandinMsi, 'MSIINSTALLPERUSER=1') 'co-user' 0
if (-not (Test-Path (Join-Path $userDir 'remote-docker.exe'))) { Fail "MSIINSTALLPERUSER=1 did not install into $userDir" }
if (Test-Path $Exe) { Fail "$Exe exists after a per-user install by an administrator" }
Ok "MSIINSTALLPERUSER=1 installs into $userDir, even for an administrator"
Invoke-Msi @('/i', $Msi) 'co-user-machine' 0
$cl = Log-Of 'co-user-machine'
$warn = [regex]::Match($cl, '(?m)^.*PROPERTY CHANGE: Adding USEROVERMACHINEWARNING.*$').Value
Write-Host "  saw: [$warn]"
if ($warn -notmatch 'also installed for your account alone') { Fail "the warning is not in the log; tail: $($cl.Substring([Math]::Max(0, $cl.Length - 1500)))" }
if ($warn -notmatch 'msiexec /x') { Fail "the warning does not say how to remove the copy: [$warn]" }
Ok 'the all-users install succeeded and the log carries the warning'
$fr = Related-Detected $cl
Write-Host "  saw: WIX_*GRADE_DETECTED [$($fr -join '; ')]"
if ($fr) { Fail "FindRelatedProducts saw the per-user product from a per-machine install: [$($fr -join '; ')]" }
if (-not (Test-Path $Exe)) { Fail "$Exe is missing after the all-users install" }
if (-not (Test-Path (Join-Path $userDir 'remote-docker.exe'))) { Fail 'the all-users install took the per-user copy away' }
Ok 'both are installed, and FindRelatedProducts saw neither as the other'

# Which product does /x of the stand-in's package pick, with both present?
Invoke-Msi @('/x', $StandinMsi) 'co-user-remove' 0
if (Test-Path (Join-Path $userDir 'remote-docker.exe')) { Fail "uninstalling the per-user copy left $userDir\remote-docker.exe" }
if ([Environment]::GetEnvironmentVariable('Path', 'User') -like '*remote-docker*') { Fail "the user PATH still names remote-docker: $([Environment]::GetEnvironmentVariable('Path', 'User'))" }
if (Test-Path 'HKCU:\SOFTWARE\lhns\remote-docker') {
  Fail "HKCU\Software\lhns\remote-docker is left behind with: $((Get-ItemProperty 'HKCU:\SOFTWARE\lhns\remote-docker' | Format-List | Out-String))"
}
Assert-MachineInstalled 'msiexec /x of the older package removed the per-user copy only'
Invoke-Msi @('/x', $Msi) 'co-user-machine-uninstall' 0
Assert-NotInstalled

# The same, by a user with no administrator rights removing their own copy.
Invoke-AsStdMsi @('/i', $StdStandin1, 'MSIINSTALLPERUSER=1') 'std-co-user' 0
Invoke-Msi @('/i', $Msi) 'std-co-machine' 0
$f = Get-StdFacts 'facts-co-before'
if ($f.arp -notmatch '0\.0\.1') { Fail "the standard user's own copy is not registered: [$($f.arp)]" }
Invoke-AsStdMsi @('/x', $StdStandin1) 'std-co-remove' 0
Assert-StdNotInstalled 'after removing their own copy' -MachineProductVisible
Assert-MachineInstalled 'after the standard user removed their copy'
$f = Get-StdFacts 'facts-co-after' 'remote-docker remote --help'
if ($f.arp -ne '99.0.0') { Fail "after removing their copy the registered products are [$($f.arp)], expected only 99.0.0" }
if ($f.code -ne '0') { Fail "exit $($f.code) from a fresh shell of $StdName after the removal; output: $($f.output)" }
Ok 'a fresh shell of that user still runs remote-docker'
Invoke-Msi @('/x', $Msi) 'std-co-machine-uninstall' 0
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '9k. a per-user install upgrades in place'

Invoke-AsStdMsi @('/i', $StdStandin1, 'MSIINSTALLPERUSER=1') 'std-up1' 0
if ((Get-StdFacts 'facts-up1').arp -ne '0.0.1') { Fail 'version 0.0.1 is not registered after installing it' }
Invoke-AsStdMsi @('/i', $StdStandin2, 'MSIINSTALLPERUSER=1') 'std-up2' 0
$f = Get-StdFacts 'facts-up2'
if ($f.arp -ne '0.0.2') { Fail "after the upgrade the registered version is '$($f.arp)', not 0.0.2" }
$entries = @($f.userpath -split ';' | Where-Object { $_ -like '*remote-docker*' })
if ($entries.Count -ne 1) { Fail "expected one remote-docker entry on the user PATH after the upgrade: $($f.userpath)" }
$ul = [regex]::Match((Log-Of 'std-up2'), '(?m)^Property\(S\): WIX_UPGRADE_DETECTED = .*$').Value
Write-Host "  saw: [$ul]"
if (-not $ul) { Fail 'the upgrade did not detect the earlier per-user product' }
Ok 'FindRelatedProducts found the per-user product in the same context; one PATH entry, version 0.0.2'
Invoke-AsStdMsi @('/x', $StdStandin2) 'std-up-uninstall' 0
Assert-StdNotInstalled 'after the upgraded copy is uninstalled'

# ---------------------------------------------------------------------------
Section '9l. reduced UI runs the InstallUISequence, so the install-scope default is exercised without a click'

# /qn skips the InstallUISequence, where the scope default lives. /qr runs its
# actions and shows no authored dialog, so nobody has to click ScopeDlg. What
# stays unrun is the dialog itself (ADR 0048, 2026-10-28).
function Show-ScopeDefault([string]$Log, [bool]$Expected) {
  $lines = @([regex]::Matches($Log, '(?m)^.*((Action start|Skipping action).*MSIINSTALLPERUSER|PROPERTY CHANGE: Adding (MSIINSTALLPERUSER|INSTALLSCOPE|ALLUSERS|INSTALLFOLDER) ).*$') | ForEach-Object { $_.Value.Trim() })
  foreach ($l in $lines) { Write-Host "  saw: $l" }
  $ran = $Log -match '(?m)Action start .*MSIINSTALLPERUSER'
  $set = $Log -match "(?m)PROPERTY CHANGE: Adding MSIINSTALLPERUSER property\. Its value is '1'"
  if ($Expected -and -not ($ran -and $set)) { Fail 'the UI-sequence action did not run and set MSIINSTALLPERUSER=1, so /qr does not exercise the default' }
  if (-not $Expected -and $set) { Fail 'MSIINSTALLPERUSER was set to 1 for an elevated administrator' }
}

# The standard user, with no properties at all.
Invoke-AsStdMsi @('/i', $StdMsi) 'std-qr' 0 '/qr' 3
Show-ScopeDefault (Log-Of 'std-qr') $true
$f = Get-StdFacts 'facts-qr' 'remote-docker remote --help'
$stdDir = Join-Path $f.localappdata 'Programs\remote-docker'
Write-Host "  saw: localappdata=$($f.localappdata) userpath=$($f.userpath)"
if (-not (Test-Path (Join-Path $stdDir 'remote-docker.exe'))) { Fail "no $stdDir\remote-docker.exe after a /qr install by $StdName" }
if (Test-Path $Exe) { Fail "$Exe exists after a /qr install by $StdName" }
if (Test-Path 'HKLM:\SOFTWARE\lhns\remote-docker') { Fail 'HKLM\Software\lhns\remote-docker exists after a /qr install by a standard user' }
if ((MachinePath) -like '*remote-docker*') { Fail "the system PATH names remote-docker: $(MachinePath)" }
$entries = @($f.userpath -split ';' | Where-Object { $_ -ne '' })
if ($entries.Count -eq 0 -or ($entries[-1] -ne ($stdDir + '\') -and $entries[-1] -ne $stdDir)) {
  Fail "the install directory is not the last entry of $StdName's PATH: $($f.userpath)"
}
if ($f.code -ne '0') { Fail "exit $($f.code) from a fresh shell of $StdName; output: $($f.output)" }
if (-not (Test-Path (Join-Path $stdDir 'docker.exe'))) { Fail "no $stdDir\docker.exe after a /qr install by $StdName; it is on by default" }
Ok "a standard user's /qr install with no properties went to $stdDir and the user PATH, with docker.exe, and nothing machine-wide"
Invoke-AsStdMsi @('/x', $StdMsi) 'std-qr-uninstall' 0
Assert-StdNotInstalled 'after the /qr per-user uninstall'

# The elevated runner, with no properties either.
Invoke-Msi @('/i', $Msi) 'qr-machine' 0 '/qr' 3
Show-ScopeDefault (Log-Of 'qr-machine') $false
if (-not (Test-Path $Exe)) { Fail "no $Exe after an elevated /qr install" }
if (-not (Test-Path $Alias)) { Fail "no $Alias after an elevated /qr install; it is on by default" }
Assert-MachineInstalled 'after the elevated /qr install'
if (Test-Path (Join-Path $env:LOCALAPPDATA 'Programs\remote-docker\remote-docker.exe')) { Fail 'the elevated /qr install also wrote a per-user copy' }
Invoke-Msi @('/x', $Msi) 'qr-machine-uninstall' 0
Assert-NotInstalled

if ($policyBefore -ne 0) {
  if ($null -eq $policyBefore) { Remove-ItemProperty $PolicyKey -Name DisableMsi } else { Set-ItemProperty $PolicyKey -Name DisableMsi -Value $policyBefore }
  Write-Host "  put DisableMsi back to [$policyBefore]"
}

# ===========================================================================
# The optional Hyper-V workspace (ADR 0048 amendment, ADR 0055). The package is
# built with the rdstandin tag: the real service, pipe, group and installer
# around a stand-in for Hyper-V, which logs create/start/stop/destroy under
# %ProgramData%\remote-docker-standin (beside the service's directory, so the
# teardown that deletes that one leaves the log to read). HYPERVSERVICE names
# the Windows service whose presence means Hyper-V is enabled; msiserver stands
# in for vmms here.

$Service = 'remote-docker-machines'
$Group = 'remote-docker-users'
$ServiceDir = Join-Path $env:ProgramData 'remote-docker'
$StandinDir = Join-Path $env:ProgramData 'remote-docker-standin'
$OpsLog = Join-Path $StandinDir 'ops.log'
$MyConfig = Join-Path $env:USERPROFILE '.remote-docker.json'
$HyperVOn = @('ADDLOCAL=Main,Path,HyperV', 'HYPERVSERVICE=msiserver')
$RegKey = 'HKLM:\SOFTWARE\lhns\remote-docker'

function ServiceExists() {
  $null = & sc.exe query $Service 2>&1
  return ($LASTEXITCODE -eq 0)
}

function GroupText() {
  $text = (& net.exe localgroup $Group 2>&1 | Out-String)
  return @{ Exists = ($LASTEXITCODE -eq 0); Text = $text }
}

function Ops() {
  if (-not (Test-Path $OpsLog)) { return @() }
  return @(Get-Content $OpsLog | ForEach-Object { ($_ -split ' ')[1..2] -join ' ' })
}

function ConfigEntry([string]$Path, [string]$Name) {
  if (-not (Test-Path $Path)) { return $null }
  $json = Get-Content $Path -Raw | ConvertFrom-Json
  if (-not $json.workspaces) { return $null }
  return $json.workspaces.$Name
}

function ContextNames() {
  return (& $script:Tool context ls --format '{{.Name}}' 2>&1 | Out-String)
}

function Assert-NoHyperV([string]$What) {
  if (ServiceExists) { Fail "$What : the service $Service exists" }
  if ((GroupText).Exists) { Fail "$What : the group $Group exists" }
  if (Test-Path $ServiceDir) { Fail "$What : $ServiceDir exists" }
  if (Get-ItemProperty $RegKey -Name HyperVWorkspace -ErrorAction SilentlyContinue) {
    Fail "$What : the remembered HyperVWorkspace value is still in the registry"
  }
  if (ConfigEntry $MyConfig 'hyperv') { Fail "$What : $MyConfig still has a hyperv entry" }
  Ok "$What : no service, no group, no $ServiceDir, no registry flag, no config entry"
}

function InstalledVersion() {
  return (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*' |
    Where-Object { $_.DisplayName -eq 'remote-docker' }).DisplayVersion
}

# A tool that outlives the uninstall, for looking at the docker contexts after.
$Tool = Join-Path $Temp 'remote-docker-tool.exe'

if (Test-Path $StandinDir) { Remove-Item -Recurse -Force $StandinDir }

# ---------------------------------------------------------------------------
Section '10. what this runner has'

$vmms = Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\vmms' -ErrorAction SilentlyContinue
if ($vmms) { Ok 'this runner has the vmms service key (so Hyper-V is "enabled" here)' }
else { Ok 'this runner has no vmms service key' }

# ---------------------------------------------------------------------------
Section '11. ADDLOCAL=HyperV without Hyper-V is refused with the way to enable it'

Invoke-Msi @('/i', $StandinMsi, 'ADDLOCAL=Main,Path,HyperV', 'HYPERVSERVICE=nosuchservice') 'hyperv-absent' 1603
$log = Log-Of 'hyperv-absent'
if ($log -notmatch 'Hyper-V is not enabled on this computer') { Fail 'the refusal message is not in the log' }
if ($log -notmatch 'Enable-WindowsOptionalFeature') { Fail 'the refusal does not give the command that enables Hyper-V' }
Ok 'refused, and the message names the command'
Assert-NotInstalled
Assert-NoHyperV 'after the refusal'

# ---------------------------------------------------------------------------
Section '12. a refused workspace name fails the install, reaches the log, and rolls everything back'

Invoke-Msi (@('/i', $StandinMsi) + $HyperVOn + @('WORKSPACENAME=INVALID')) 'hyperv-badname' 1603
$log = Log-Of 'hyperv-badname'
if ($log -notmatch 'not a valid machine name') {
  Fail "the name check's message is not in the log; tail: $($log.Substring([Math]::Max(0, $log.Length - 3000)))"
}
Ok "the service's own name check is in the msiexec log"
Assert-NotInstalled
Assert-NoHyperV 'after the rollback'
if ((Ops) -contains 'create INVALID') { Fail 'a machine was created under the refused name' }

# ---------------------------------------------------------------------------
Section '12b. a workspace that already has the name is refused, and the rollback leaves it alone'

# The rollback's `remote rm` must not run for a name that was already taken, or
# a failed install deletes the user's own workspace of that name.
$configBefore = if (Test-Path $MyConfig) { Get-Content $MyConfig -Raw } else { $null }
Set-Content -Path $MyConfig -Encoding ascii -Value '{"workspaces":{"kept":{"host":"build.example","port":22,"user":"alice"}}}'
Invoke-Msi (@('/i', $StandinMsi) + $HyperVOn + @('WORKSPACENAME=kept')) 'hyperv-existing' 1603
$log = Log-Of 'hyperv-existing'
if ($log -notmatch 'a workspace named "kept" already exists') {
  Fail "the refusal is not in the log; tail: $($log.Substring([Math]::Max(0, $log.Length - 3000)))"
}
Ok 'refused, naming the workspace'
$kept = ConfigEntry $MyConfig 'kept'
if (-not $kept -or $kept.host -ne 'build.example') { Fail "the existing workspace is gone or changed: $(Get-Content $MyConfig -Raw -ErrorAction SilentlyContinue)" }
if (@(Ops | Where-Object { $_ -match ' kept$' }).Count -ne 0) { Fail "the stand-in saw operations on kept: $((Ops) -join '; ')" }
Ok 'the existing workspace is still there, unchanged, and no machine was made or destroyed for it'
Assert-NotInstalled
if ($null -eq $configBefore) { Remove-Item $MyConfig -Force } else { Set-Content -Path $MyConfig -Value $configBefore -NoNewline }

# ---------------------------------------------------------------------------
Section '13. the properties without the feature install nothing of the workspace'

Invoke-Msi @('/i', $StandinMsi, 'HYPERVSERVICE=msiserver', 'WORKSPACENAME=hyperv') 'standin-off' 0
Assert-NoHyperV 'feature not selected'
Invoke-Msi @('/x', $StandinMsi) 'standin-off-uninstall' 0
Assert-NotInstalled
Assert-NoHyperV 'feature not selected, after uninstall'

# Without Hyper-V the feature is level 0, which even ADDLOCAL=ALL leaves out.
Invoke-Msi @('/i', $StandinMsi, 'ADDLOCAL=ALL', 'HYPERVSERVICE=nosuchservice') 'standin-all-absent' 0
Assert-NoHyperV 'ADDLOCAL=ALL without Hyper-V'
Invoke-Msi @('/x', $StandinMsi) 'standin-all-absent-uninstall' 0
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '14. the feature installs the service, the group, the machine and the context'

Invoke-Msi (@('/i', $StandinMsi) + $HyperVOn) 'hyperv-install' 0

$qc = (& sc.exe qc $Service | Out-String)
if ($qc -notmatch 'DEMAND_START') { Fail "the service is not demand start: $qc" }
if ($qc -notmatch 'LocalSystem') { Fail "the service does not run as LocalSystem: $qc" }
if ($qc -notmatch 'remote machine service run') { Fail "the service does not run the service command: $qc" }
Ok 'the service is LocalSystem, demand start, running `remote machine service run`'

$sd = (& sc.exe sdshow $Service | Out-String).Trim()
if ($sd -notmatch '\(A;;[A-Z]*RP[A-Z]*;;;AU\)') { Fail "Authenticated Users cannot start the service: $sd" }
if ($sd -match '\(A;;[A-Z]*(WD|DC|SD|WO)[A-Z]*;;;AU\)') { Fail "Authenticated Users hold more than start and query: $sd" }
if ($sd -notmatch ';;;SY\)' -or $sd -notmatch ';;;BA\)') { Fail "SYSTEM or Administrators lost their rights: $sd" }
Ok "Authenticated Users may start it and nothing more: $sd"

$g = GroupText
if (-not $g.Exists) { Fail "the group $Group does not exist" }
if ($g.Text -notmatch [regex]::Escape($env:USERNAME)) { Fail "the installing user is not in ${Group}: $($g.Text)" }
Ok "$Group exists with $env:USERNAME in it"

$entry = ConfigEntry $MyConfig 'hyperv'
if (-not $entry) { Fail "no hyperv entry in $MyConfig; the create ran as another user? $(Get-Content $MyConfig -Raw -ErrorAction SilentlyContinue)" }
if ($entry.machine.broker -ne $true) { Fail "the entry is not brokered: $($entry | ConvertTo-Json -Depth 5)" }
if ($entry.machine.image -notmatch '^ghcr\.io/lhns/remote-docker-workspace:0\.0\.1$') { Fail "unexpected image: $($entry.machine.image)" }
Ok "$MyConfig has the hyperv entry with broker: true and the release's image"

Copy-Item $Exe $Tool -Force
$contexts = ContextNames
if ($contexts -notmatch '(?m)^hyperv\s*$') { Fail "no docker context named hyperv: $contexts" }
Ok 'the docker context exists'

$ops = Ops
if (@($ops | Where-Object { $_ -eq 'create hyperv' }).Count -ne 1) { Fail "expected one 'create hyperv'; the stand-in log has: $($ops -join '; ')" }
Ok 'the stand-in saw exactly one create'

$state = (Get-ItemProperty $RegKey)
if ($state.HyperVWorkspace -ne '1' -or $state.WorkspaceName -ne 'hyperv') { Fail "the remembered state is wrong: $($state | Out-String)" }
Ok 'the flag and the name are remembered for upgrade and uninstall'

# ---------------------------------------------------------------------------
Section '15. a standard user is refused until added to the group, with no new logon'

# $StdName, $cred and Invoke-AsStd are set up with sections 9e to 9l.

# One process that keeps its logon token across the group change: it asks, waits
# for the signal, and asks again. Two Start-Process calls would be two logons,
# and the second token would already carry the group.
$liveScript = Join-Path $Pub 'live.ps1'
Set-Content -Path $liveScript -Encoding ascii -Value @'
param($Exe, $Dir)
& $Exe remote machine create other --backend hyperv *> "$Dir\first.out"
"$LASTEXITCODE" | Set-Content "$Dir\first.code"
$end = (Get-Date).AddSeconds(240)
while (-not (Test-Path "$Dir\go") -and (Get-Date) -lt $end) { Start-Sleep -Milliseconds 500 }
& $Exe remote machine create other --backend hyperv *> "$Dir\second.out"
"$LASTEXITCODE" | Set-Content "$Dir\second.code"
'@
Remove-Item "$Pub\first.*", "$Pub\second.*", "$Pub\go" -ErrorAction SilentlyContinue

$live = Start-AsStd powershell.exe @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $liveScript, ('"' + $Exe + '"'), $Pub) 'live'

$deadline = (Get-Date).AddSeconds(120)
while (-not (Test-Path "$Pub\first.code") -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
if (-not (Test-Path "$Pub\first.code")) {
  $exited = $live.HasExited
  $code = if ($exited) { $live.ExitCode } else { 'still running' }
  $seen = (Get-Content "$Pub\live.out", "$Pub\live.err" -ErrorAction SilentlyContinue) -join "`n"
  Fail "the standard user process never reported (exit: $code); its output: $seen; pub dir: $((Get-ChildItem $Pub | ForEach-Object Name) -join ', ')"
}
$first = (Get-Content "$Pub\first.out" -Raw)
if ((Get-Content "$Pub\first.code").Trim() -eq '0') { Fail "a user outside the group was served: $first" }
if ($first -notmatch 'remote-docker-users') { Fail "the refusal does not name the group: $first" }
Ok "refused, and it names the group: $($first.Trim())"

& net.exe localgroup $Group $StdName /add | Out-Null
if ($LASTEXITCODE -ne 0) { Fail "adding $StdName to $Group failed" }
Set-Content "$Pub\go" 'go'
$deadline = (Get-Date).AddSeconds(240)
while (-not (Test-Path "$Pub\second.code") -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
if (-not (Test-Path "$Pub\second.code")) { Fail 'the second request never finished' }
$second = (Get-Content "$Pub\second.out" -Raw)
if ((Get-Content "$Pub\second.code").Trim() -ne '0') { Fail "the same process, now in the group, was refused: $second" }
Ok 'the same logon, added to the group a moment ago, is served'
if (@(Ops | Where-Object { $_ -eq 'create other' }).Count -ne 1) { Fail "expected one 'create other': $((Ops) -join '; ')" }

# ---------------------------------------------------------------------------
Section '16. a stopped service is started by a standard user'

Stop-Service $Service -Force
(Get-Service $Service).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(60))
Ok 'stopped the service'
Invoke-AsStd @('remote', 'machine', 'stop', 'other') 'stop'
if ((Get-Service $Service).Status -ne 'Running') { Fail 'the command succeeded but the service is not running' }
Ok 'the client started the service (SERVICE_START for Authenticated Users) and the command went through'

# ---------------------------------------------------------------------------
Section '17. an upgrade that would drop the workspace is refused'

Invoke-Msi @('/i', $StandinNextMsi, 'ADDLOCAL=Main,Path') 'hyperv-upgrade-drop' 1603
if ((Log-Of 'hyperv-upgrade-drop') -notmatch 'cannot be added or removed during an upgrade') { Fail 'the refusal message is not in the log' }
$ver = InstalledVersion
if ($ver -ne '0.0.1') { Fail "after the refused upgrade the installed version is '$ver', not 0.0.1" }
if (-not (ServiceExists)) { Fail 'the service is gone after the refused upgrade' }
Ok 'refused by name, and 0.0.1 is still installed with its service'

# ---------------------------------------------------------------------------
Section '17b. an upgrade touches nothing, and stops a standard user''s background session'

# Another user's session holding remote-docker.exe open: Restart Manager has to
# stop it for the upgrade to replace the file (ADR 0048).
function SessionProcess() {
  return @(Get-CimInstance Win32_Process -Filter "Name='remote-docker.exe'" |
    Where-Object { $_.CommandLine -match 'start --foreground --workspace other' })
}
Invoke-AsStd @('remote', 'start', 'other') 'sess-start'
$sessions = SessionProcess
if ($sessions.Count -ne 1) { Fail "expected one background session of $StdName, found $($sessions.Count)" }
Ok "$StdName has a background session running $Exe (pid $($sessions[0].ProcessId))"

$hashRecords = (Get-FileHash (Join-Path $ServiceDir 'machines.json')).Hash
$hashConfig = (Get-FileHash $MyConfig).Hash
$opsBefore = (Ops) -join "`n"
$membersBefore = (GroupText).Text

Invoke-Msi @('/i', $StandinNextMsi) 'hyperv-upgrade' 0

$ver = InstalledVersion
if ($ver -ne '0.0.2') { Fail "after the upgrade the installed version is '$ver', not 0.0.2" }
Ok 'version 0.0.2 is installed'
if (-not (ServiceExists)) { Fail 'the service is gone after the upgrade' }
$sd = (& sc.exe sdshow $Service | Out-String).Trim()
if ($sd -notmatch '\(A;;[A-Z]*RP[A-Z]*;;;AU\)') { Fail "the upgraded service lost the Authenticated Users grant: $sd" }
Ok 'the service is installed again, with the grant'
if ((Get-FileHash (Join-Path $ServiceDir 'machines.json')).Hash -ne $hashRecords) { Fail 'machines.json changed' }
if ((Get-FileHash $MyConfig).Hash -ne $hashConfig) { Fail "$MyConfig changed" }
if ((GroupText).Text -ne $membersBefore) { Fail "the group changed: $((GroupText).Text)" }
if (((Ops) -join "`n") -ne $opsBefore) { Fail "the stand-in saw operations during the upgrade: $((Ops) -join '; ')" }
Ok 'the record, the config entry, the group and the machine are untouched; no destroy, no second create'
if ((SessionProcess).Count -ne 0) { Fail "the background session of $StdName survived the upgrade, so the old file was not replaced in place" }
Ok "the upgrade stopped $StdName's session"
Invoke-AsStd @('remote', 'start', 'other') 'sess-restart'
if ((SessionProcess).Count -ne 1) { Fail "the session of $StdName did not start again after the upgrade" }
Invoke-AsStd @('remote', 'stop', 'other') 'sess-stop'
if ((SessionProcess).Count -ne 0) { Fail "remote stop left the session of $StdName running" }
Ok 'it starts again from the upgraded program, and stops'
Start-Service $Service
Ok 'the reinstalled service starts'

# ---------------------------------------------------------------------------
Section '18. uninstall deletes the workspace and everything setup made'

# Teardown deletes the group only when nobody it did not add is in it, so the
# member this test added by hand leaves first (as an administrator would).
& net.exe localgroup $Group $StdName /delete | Out-Null
Invoke-Msi @('/x', $StandinNextMsi) 'hyperv-uninstall' 0
Assert-NotInstalled
$ops = Ops
foreach ($want in 'destroy hyperv', 'destroy other') {
  if ($ops -notcontains $want) { Fail "no '$want' in the stand-in log: $($ops -join '; ')" }
}
Ok "both machines were destroyed (hyperv by 'remote rm', other by the teardown): $($ops -join '; ')"
Assert-NoHyperV 'after uninstall'
if (Get-ItemProperty "$RegKey\Service" -ErrorAction SilentlyContinue) { Fail 'the setup record is still in the registry' }
$contexts = ContextNames
if ($contexts -match '(?m)^hyperv\s*$') { Fail "the docker context hyperv is still there: $contexts" }
Ok 'the docker context is gone'

Remove-LocalUser -Name $StdName -ErrorAction SilentlyContinue
Remove-Item -Recurse -Force $Pub -ErrorAction SilentlyContinue

# ---------------------------------------------------------------------------
Section '19. an upgrade cannot add the workspace, and Modify adds and removes it'

Invoke-Msi @('/i', $StandinMsi) 'modify-base' 0
Invoke-Msi (@('/i', $StandinNextMsi) + $HyperVOn) 'modify-upgrade-add' 1603
if ((Log-Of 'modify-upgrade-add') -notmatch 'cannot be added or removed during an upgrade') { Fail 'the refusal message is not in the log' }
Assert-NoHyperV 'after the refused upgrade'

$creates = @(Ops | Where-Object { $_ -eq 'create hyperv' }).Count
Invoke-Msi @('/i', $StandinMsi, 'ADDLOCAL=HyperV', 'HYPERVSERVICE=msiserver') 'modify-add' 0
if (-not (ServiceExists)) { Fail "Modify added the feature and $Service does not exist" }
if (-not (GroupText).Exists) { Fail "Modify added the feature and $Group does not exist" }
if (-not (ConfigEntry $MyConfig 'hyperv')) { Fail "Modify added the feature and $MyConfig has no hyperv entry" }
if (@(Ops | Where-Object { $_ -eq 'create hyperv' }).Count -ne $creates + 1) { Fail "expected one more 'create hyperv': $((Ops) -join '; ')" }
Ok 'Modify adding the feature ran service create, setup and create'

$destroys = @(Ops | Where-Object { $_ -eq 'destroy hyperv' }).Count
Invoke-Msi @('/i', $StandinMsi, 'REMOVE=HyperV') 'modify-remove' 0
if (-not (Test-Path $Exe)) { Fail "removing the feature took $Exe with it" }
if (@(Ops | Where-Object { $_ -eq 'destroy hyperv' }).Count -ne $destroys + 1) { Fail "expected one more 'destroy hyperv': $((Ops) -join '; ')" }
Assert-NoHyperV 'after Modify removed the feature'
Invoke-Msi @('/x', $StandinMsi) 'modify-uninstall' 0
Assert-NotInstalled

Remove-Item $Tool -Force -ErrorAction SilentlyContinue

Write-Host ''
Write-Host 'all sections passed' -ForegroundColor Green
exit 0
