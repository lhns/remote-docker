# The Windows installer, end to end: install, what lands on PATH, the docker.exe
# feature, the refusal that guards it, uninstall, and the optional Hyper-V
# workspace feature (service, group, a machine, upgrade, removal, adding and
# removing it with Modify) against a stand-in for Hyper-V.
#
#   pwsh test/msi.ps1 -Msi dist/remote-docker_0.6.0_windows_amd64.msi `
#     -StandinMsi dist/standin_0.0.1.msi -StandinNextMsi dist/standin_0.0.2.msi
#
# Runs elevated and makes machine-wide changes: it installs to Program Files,
# edits the system PATH, and moves any docker.exe it finds out of the way so the
# feature can be tested both with and without one present. Meant for a
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
$HyperVOn = @('ADDLOCAL=Main,HyperV', 'HYPERVSERVICE=msiserver')
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

function Log-Of([string]$Tag) { return Get-Content (Join-Path $LogDir "$Tag.log") -Raw }

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

Invoke-Msi @('/i', $StandinMsi, 'ADDLOCAL=Main,HyperV', 'HYPERVSERVICE=nosuchservice') 'hyperv-absent' 1603
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

# The rollback's `remote rm` used to remove whatever had the name, so a failed
# install deleted the user's own workspace of that name.
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

$Pub = 'C:\Users\Public\rdtest'
New-Item -ItemType Directory -Force -Path $Pub | Out-Null
& icacls.exe $Pub /grant 'Everyone:(OI)(CI)M' | Out-Null
$StdName = 'rdstd'
$StdPass = 'Rd-Std-' + [guid]::NewGuid().ToString('N').Substring(0, 12) + '!A1'
if (Get-LocalUser -Name $StdName -ErrorAction SilentlyContinue) { Remove-LocalUser -Name $StdName }
$null = New-LocalUser -Name $StdName -Password (ConvertTo-SecureString $StdPass -AsPlainText -Force) -PasswordNeverExpires -AccountNeverExpires
Add-LocalGroupMember -Group 'Users' -Member $StdName
if ((Get-LocalGroupMember -Group 'Administrators' | Where-Object { $_.Name -like "*\$StdName" })) { Fail "$StdName is an administrator" }
Ok "created the standard user $StdName"
$cred = New-Object System.Management.Automation.PSCredential($StdName, (ConvertTo-SecureString $StdPass -AsPlainText -Force))

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
Remove-Item "$Pub\stop.*" -ErrorAction SilentlyContinue
$p = Start-Process -FilePath $Exe -Credential $cred -LoadUserProfile -UseNewEnvironment -WorkingDirectory $env:SystemRoot `
  -ArgumentList @('remote', 'machine', 'stop', 'other') -WindowStyle Hidden -PassThru `
  -RedirectStandardOutput "$Pub\stop.out" -RedirectStandardError "$Pub\stop.err"
$null = $p.WaitForExit(180000)
$out = (Get-Content "$Pub\stop.out", "$Pub\stop.err" -Raw -ErrorAction SilentlyContinue) -join ''
if (-not $p.HasExited -or $p.ExitCode -ne 0) { Fail "the standard user could not use a stopped service; output: $out" }
if ((Get-Service $Service).Status -ne 'Running') { Fail 'the command succeeded but the service is not running' }
Ok 'the client started the service (SERVICE_START for Authenticated Users) and the command went through'

# ---------------------------------------------------------------------------
Section '17. an upgrade that would drop the workspace is refused'

Invoke-Msi @('/i', $StandinNextMsi, 'ADDLOCAL=Main') 'hyperv-upgrade-drop' 1603
if ((Log-Of 'hyperv-upgrade-drop') -notmatch 'cannot be added or removed during an upgrade') { Fail 'the refusal message is not in the log' }
$ver = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*' |
  Where-Object { $_.DisplayName -eq 'remote-docker' }).DisplayVersion
if ($ver -ne '0.0.1') { Fail "after the refused upgrade the installed version is '$ver', not 0.0.1" }
if (-not (ServiceExists)) { Fail 'the service is gone after the refused upgrade' }
Ok 'refused by name, and 0.0.1 is still installed with its service'

# ---------------------------------------------------------------------------
Section '17b. an upgrade touches nothing, and stops a standard user''s background session'

# Another user's session holding remote-docker.exe open: Restart Manager has to
# stop it for the upgrade to replace the file (ADR 0048).
function Invoke-AsStd([string[]]$Arguments, [string]$Tag) {
  $p = Start-Process -FilePath $Exe -Credential $cred -LoadUserProfile -UseNewEnvironment -WorkingDirectory $env:SystemRoot `
    -ArgumentList $Arguments -WindowStyle Hidden -PassThru `
    -RedirectStandardOutput "$Pub\$Tag.out" -RedirectStandardError "$Pub\$Tag.err"
  $null = $p.WaitForExit(120000)
  $out = (Get-Content "$Pub\$Tag.out", "$Pub\$Tag.err" -Raw -ErrorAction SilentlyContinue) -join ''
  if (-not $p.HasExited -or $p.ExitCode -ne 0) { Fail "$StdName could not run '$($Arguments -join ' ')'; output: $out" }
}
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

$ver = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*' |
  Where-Object { $_.DisplayName -eq 'remote-docker' }).DisplayVersion
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
