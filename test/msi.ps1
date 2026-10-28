# The Windows installer, end to end: install, the PATH feature, the docker.exe
# feature, the refusal that guards it, uninstall, the per-user install by a user
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
# and without one present. Meant for a
# throwaway machine, which is what .github/workflows/msi.yml gives it.

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
function Invoke-Msi([string[]]$Arguments, [string]$Tag, [int]$Expect) {
  $log = Join-Path $LogDir "$Tag.log"
  $all = $Arguments + @('/qn', '/l*v', $log)
  Write-Host "  msiexec $($all -join ' ')"
  $p = Start-Process -FilePath msiexec.exe -ArgumentList $all -PassThru
  # Bounded, so a hang fails here with the log and what was still running,
  # rather than at the job's limit with nothing to read.
  if (-not $p.WaitForExit(10 * 60 * 1000)) {
    Write-Host "  msiexec still running after 10 minutes; still running:" -ForegroundColor Yellow
    Get-CimInstance Win32_Process -Filter "Name='msiexec.exe' OR Name='remote-docker.exe'" |
      ForEach-Object { Write-Host "    $($_.ProcessId) $($_.CommandLine)" }
    if (Test-Path $log) { Get-Content $log -Tail 200 }
    Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    Fail "msiexec did not finish within 10 minutes"
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

# ---------------------------------------------------------------------------
Section 'a machine with no docker.exe anywhere the installer looks'

# GitHub's Windows runners ship a docker.exe, which is exactly what the refusal
# is about, so the positive cases need it out of the way first. Moved and
# reported rather than deleted quietly. These four are what remote-docker.wxs
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
Section '7. the feature is refused when a docker.exe is already on PATH'

$stub = Join-Path $env:SystemRoot 'System32\docker.exe'
Copy-Item (Join-Path $env:SystemRoot 'System32\where.exe') $stub
Ok "put a docker.exe at $stub, which is on the system PATH"

# 1603 is what a type 19 error custom action gives a silent install.
Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,Path,DockerName') 'refusal' 1603
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

Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,Path,DockerName', 'ALLOWDOCKERSHADOW=1') 'override' 0
if (-not (Test-Path $Alias)) { Fail "the override was given and $Alias was not created" }
Ok 'the copy was made'
Invoke-Msi @('/x', $Msi) 'uninstall-override' 0
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '9b. docker.exe without the Path feature is installed beside another one'

# The stub from section 7 is still in System32. Nothing is shadowed when the
# install directory is not on PATH (ADR 0048).
$stubBefore = (Get-FileHash $stub).Hash
Invoke-Msi @('/i', $Msi, 'ADDLOCAL=Main,DockerName') 'no-path-docker' 0
if (-not (Test-Path $Alias)) { Fail "the feature was selected and $Alias was not created" }
if (-not (Test-Path $stub)) { Fail "$stub was removed" }
if ((Get-FileHash $stub).Hash -ne $stubBefore) { Fail "$stub was changed" }
Ok 'installed beside the other docker.exe, which is untouched'
Invoke-Msi @('/x', $Msi) 'uninstall-no-path-docker' 0
Assert-NotInstalled

Remove-Item $stub -Force

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
Section '9d. an upgrade from the latest released MSI keeps the PATH entry'

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

function Invoke-AsStd([string[]]$Arguments, [string]$Tag) {
  $p = Start-Process -FilePath $Exe -Credential $cred -LoadUserProfile -UseNewEnvironment -WorkingDirectory $env:SystemRoot `
    -ArgumentList $Arguments -WindowStyle Hidden -PassThru `
    -RedirectStandardOutput "$Pub\$Tag.out" -RedirectStandardError "$Pub\$Tag.err"
  $null = $p.WaitForExit(180000)
  $out = (Get-Content "$Pub\$Tag.out", "$Pub\$Tag.err" -Raw -ErrorAction SilentlyContinue) -join ''
  if (-not $p.HasExited -or $p.ExitCode -ne 0) { Fail "$StdName could not run '$($Arguments -join ' ')'; output: $out" }
}

# The MSIs where the standard user can read them, and msiexec as that user: a
# token without the Administrators group, never elevated.
$StdMsi = Join-Path $Pub 'remote-docker.msi'
$StdStandin1 = Join-Path $Pub 'standin_0.0.1.msi'
$StdStandin2 = Join-Path $Pub 'standin_0.0.2.msi'
Copy-Item $Msi $StdMsi -Force
Copy-Item $StandinMsi $StdStandin1 -Force
Copy-Item $StandinNextMsi $StdStandin2 -Force

function Invoke-AsStdMsi([string[]]$Arguments, [string]$Tag, [int]$Expect) {
  $log = Join-Path $Pub "$Tag.log"
  $all = $Arguments + @('/qn', '/l*v', $log)
  Write-Host "  msiexec as ${StdName}: $($all -join ' ')"
  $p = Start-Process -FilePath msiexec.exe -Credential $cred -LoadUserProfile -UseNewEnvironment -WorkingDirectory $env:SystemRoot `
    -ArgumentList $all -WindowStyle Hidden -PassThru
  if (-not $p.WaitForExit(10 * 60 * 1000)) {
    Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    if (Test-Path $log) { Get-Content $log -Tail 200 }
    Fail "msiexec as $StdName did not finish within 10 minutes"
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

function Get-StdFacts([string]$Tag, [string]$Cmd = '') {
  $p = Start-Process -FilePath powershell.exe -Credential $cred -LoadUserProfile -UseNewEnvironment -WorkingDirectory $env:SystemRoot `
    -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $factsScript, ('"' + $Cmd + '"')) -WindowStyle Hidden -PassThru `
    -RedirectStandardOutput "$Pub\$Tag.out" -RedirectStandardError "$Pub\$Tag.err"
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

function Assert-StdNotInstalled([string]$What) {
  $f = Get-StdFacts 'facts-clean'
  $dir = Join-Path $f.localappdata 'Programs\remote-docker'
  if (Test-Path $dir) {
    $left = Get-ChildItem $dir -Recurse -File
    if ($left) { Fail "$What : $dir still holds: $(($left | ForEach-Object Name) -join ', ')" }
  }
  if ($f.userpath -like '*remote-docker*') { Fail "$What : $StdName's PATH still names remote-docker: $($f.userpath)" }
  if ($f.key -eq 'True') { Fail "$What : HKCU\Software\lhns\remote-docker still exists for $StdName" }
  if ($f.arp) { Fail "$What : $StdName's still has the product registered: $($f.arp)" }
  Ok "$What : nothing of it is left in $StdName's profile or registry"
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
Section '9f. a user with no administrator rights installs silently, with no arguments, for themselves'

Invoke-AsStdMsi @('/i', $StdMsi) 'std-default' 0
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
Section '9g. a per-user install with the docker.exe feature, and the refusal still fires'

Invoke-AsStdMsi @('/i', $StdMsi, 'ADDLOCAL=Main,Path,DockerName') 'std-feature' 0
$f = Get-StdFacts 'facts-feature' 'docker remote --help'
$stdDir = Join-Path $f.localappdata 'Programs\remote-docker'
if (-not (Test-Path (Join-Path $stdDir 'docker.exe'))) { Fail "the feature was selected and $stdDir\docker.exe was not created" }
if ($f.code -ne '0') { Fail "exit $($f.code) from 'docker remote --help' as $StdName; output: $($f.output)" }
Ok 'docker.exe beside remote-docker.exe, and a fresh shell reaches it as `docker`'
Invoke-AsStdMsi @('/x', $StdMsi) 'std-feature-uninstall' 0
Assert-StdNotInstalled 'after uninstalling the docker.exe feature'

# System32 is a directory the standard user cannot write, which is the point:
# the stub is the administrator's.
$stub = Join-Path $env:SystemRoot 'System32\docker.exe'
Copy-Item (Join-Path $env:SystemRoot 'System32\where.exe') $stub
Invoke-AsStdMsi @('/i', $StdMsi, 'ADDLOCAL=Main,Path,DockerName') 'std-refusal' 1603
$rl = Log-Of 'std-refusal'
Remove-Item $stub -Force
if ($rl -notmatch [regex]::Escape('System32\docker.exe')) { Fail 'the refusal did not name the docker.exe in System32' }
Ok 'refused, naming the docker.exe it found in System32'

# Docker Desktop's directory is under the real Program Files, not under the
# per-user "Programs" folder ProgramFiles64Folder means to this install.
$desktopBin = Join-Path $env:ProgramFiles 'Docker\Docker\resources\bin'
New-Item -ItemType Directory -Force -Path $desktopBin | Out-Null
Copy-Item (Join-Path $env:SystemRoot 'System32\where.exe') (Join-Path $desktopBin 'docker.exe')
Invoke-AsStdMsi @('/i', $StdMsi, 'ADDLOCAL=Main,Path,DockerName') 'std-refusal-desktop' 1603
$dl = Log-Of 'std-refusal-desktop'
Remove-Item (Join-Path $env:ProgramFiles 'Docker') -Recurse -Force
if ($dl -notmatch [regex]::Escape('Docker\Docker\resources\bin\docker.exe')) {
  Fail "the refusal did not find Docker Desktop's docker.exe in the real Program Files; tail: $($dl.Substring([Math]::Max(0, $dl.Length - 1500)))"
}
Ok "refused for Docker Desktop's docker.exe in $env:ProgramFiles"
Assert-StdNotInstalled 'after the refusals'

# ---------------------------------------------------------------------------
Section '9h. the Hyper-V workspace is refused in a per-user install, by name'

Invoke-AsStdMsi @('/i', $StdMsi, 'ADDLOCAL=Main,HyperV', 'HYPERVSERVICE=msiserver') 'std-hyperv' 1603
if ((Log-Of 'std-hyperv') -notmatch 'needs an installation for all users') { Fail 'the refusal message is not in the log' }
Ok 'refused with 1603, and the message says it needs all users'
Assert-StdNotInstalled 'after the Hyper-V refusal'

# ---------------------------------------------------------------------------
Section '9i. one install per user: a per-user install over a per-machine one is refused'

Invoke-Msi @('/i', $Msi) 'co-machine' 0
Invoke-AsStdMsi @('/i', $StdMsi) 'std-over-machine' 1603
$cl = Log-Of 'std-over-machine'
if ($cl -notmatch 'already installed for all users') { Fail "the refusal is not in the log; tail: $($cl.Substring([Math]::Max(0, $cl.Length - 1500)))" }
Ok 'refused, naming the all-users install'
# Claim: FindRelatedProducts does not see a product of the other context, which
# is why the registry markers exist.
$fr = [regex]::Match($cl, '(?m)^Property\(S\): WIX_UPGRADE_DETECTED = .*$').Value
Write-Host "  saw: WIX_UPGRADE_DETECTED [$fr]"
if ($fr) { Fail "FindRelatedProducts saw the per-machine product from a per-user install: [$fr]; the markers are not needed, so say so" }
Ok 'FindRelatedProducts did not see the product of the other context'
Assert-StdNotInstalled 'after the refused per-user install'
Invoke-Msi @('/x', $Msi) 'co-machine-uninstall' 0
Assert-NotInstalled

# ---------------------------------------------------------------------------
Section '9j. and a per-machine install over the installing user''s own per-user copy is refused'

# An administrator may install for themselves too, by asking for it.
$userDir = Join-Path $env:LOCALAPPDATA 'Programs\remote-docker'
Invoke-Msi @('/i', $Msi, 'MSIINSTALLPERUSER=1') 'co-user' 0
if (-not (Test-Path (Join-Path $userDir 'remote-docker.exe'))) { Fail "MSIINSTALLPERUSER=1 did not install into $userDir" }
if (Test-Path $Exe) { Fail "$Exe exists after a per-user install by an administrator" }
Ok "MSIINSTALLPERUSER=1 installs into $userDir, even for an administrator"
Invoke-Msi @('/i', $Msi) 'co-user-machine' 1603
$cl = Log-Of 'co-user-machine'
if ($cl -notmatch 'already installed for your account alone') { Fail "the refusal is not in the log; tail: $($cl.Substring([Math]::Max(0, $cl.Length - 1500)))" }
$fr = [regex]::Match($cl, '(?m)^Property\(S\): WIX_UPGRADE_DETECTED = .*$').Value
Write-Host "  saw: WIX_UPGRADE_DETECTED [$fr]"
if ($fr) { Fail "FindRelatedProducts saw the per-user product from a per-machine install: [$fr]" }
Ok 'refused, and FindRelatedProducts did not see it either'
if (Test-Path $Exe) { Fail "$Exe exists after the refused per-machine install" }
Invoke-Msi @('/x', $Msi) 'co-user-uninstall' 0
if (Test-Path (Join-Path $userDir 'remote-docker.exe')) { Fail "uninstalling left $userDir\remote-docker.exe" }
if ([Environment]::GetEnvironmentVariable('Path', 'User') -like '*remote-docker*') { Fail "the user PATH still names remote-docker: $([Environment]::GetEnvironmentVariable('Path', 'User'))" }
if (Test-Path 'HKCU:\SOFTWARE\lhns\remote-docker') { Fail 'HKCU\Software\lhns\remote-docker is left behind' }
Assert-NotInstalled
Ok 'the per-user copy is gone with its PATH entry and key'

# ---------------------------------------------------------------------------
Section '9k. a per-user install upgrades in place'

Invoke-AsStdMsi @('/i', $StdStandin1) 'std-up1' 0
if ((Get-StdFacts 'facts-up1').arp -ne '0.0.1') { Fail 'version 0.0.1 is not registered after installing it' }
Invoke-AsStdMsi @('/i', $StdStandin2) 'std-up2' 0
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

# $StdName, $cred and Invoke-AsStd are set up with sections 9e to 9k.

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

$live = Start-Process -FilePath powershell.exe -Credential $cred -LoadUserProfile -UseNewEnvironment -WorkingDirectory $env:SystemRoot `
  -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $liveScript, ('"' + $Exe + '"'), $Pub) -WindowStyle Hidden -PassThru `
  -RedirectStandardOutput "$Pub\live.out" -RedirectStandardError "$Pub\live.err"

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
