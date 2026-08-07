#Requires -Version 5.1
<#
.SYNOPSIS
    Run Docker on a remote host as if it were local, with the current
    directory mounted into it over a reverse SSH tunnel.

.DESCRIPTION
    Nothing needs to be installed on Windows. ssh.exe and ssh-keygen.exe ship
    with Windows 10 1809+; rclone is a single portable .exe this script
    downloads on first use.

    What happens on `dockerbox shell`:

      1. an ed25519 keypair is created (once) under ~\.ssh
      2. rclone serves the current directory as NFSv3 on 127.0.0.1:<random>
      3. ssh -R forwards the workspace container's 127.0.0.1:<yourport>
         back to that rclone listener
      4. the container mounts it at ~/workspace with the kernel NFS client
      5. you get a shell there, talking to the container's own dockerd

    Because dockerd and the mount share one mount namespace, bind mounts in
    your compose files resolve normally -- no path translation, no volume
    plugin, no sync daemon.

.EXAMPLE
    dockerbox shell
    dockerbox docker compose up -d
    dockerbox shell -Forward 8080:127.0.0.1:8080
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('shell', 'run', 'docker', 'mount', 'umount', 'status', 'enroll', 'key', 'help')]
    [string]$Command = 'shell',

    [Parameter(Position = 1, ValueFromRemainingArguments = $true)]
    [string[]]$Arguments = @(),

    [string]$Server,
    [int]$SshPort,
    [string]$User,
    [string]$Path,
    [string[]]$Forward = @(),
    [string]$Cipher = 'aes128-gcm@openssh.com'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:StateDir    = Join-Path $env:LOCALAPPDATA 'dockerbox'
$script:SessionFile = Join-Path $script:StateDir 'session.json'
$script:RcloneExe   = Join-Path $script:StateDir 'rclone.exe'
$script:KeyPath     = Join-Path $HOME '.ssh\dockerbox_ed25519'
$script:ConfigFile  = Join-Path $HOME '.dockerbox.json'

function Write-Info { param([string]$m) Write-Host "  $m" -ForegroundColor DarkGray }
function Write-Step { param([string]$m) Write-Host "==> $m" -ForegroundColor Cyan }
function Die       { param([string]$m) Write-Host "!!  $m" -ForegroundColor Red; exit 1 }

# ---------------------------------------------------------------- config ---

function Resolve-Settings {
    $defaultUser = 'user'
    if ($env:USERNAME) { $defaultUser = $env:USERNAME.ToLower() }
    $cfg = @{ Server = $null; SshPort = 2222; User = $defaultUser }

    if (Test-Path $script:ConfigFile) {
        $json = Get-Content $script:ConfigFile -Raw | ConvertFrom-Json
        foreach ($k in 'Server', 'SshPort', 'User') {
            if ($json.PSObject.Properties.Name -contains $k -and $json.$k) { $cfg[$k] = $json.$k }
        }
    }
    if ($env:DOCKERBOX_HOST)     { $cfg.Server  = $env:DOCKERBOX_HOST }
    if ($env:DOCKERBOX_SSH_PORT) { $cfg.SshPort = [int]$env:DOCKERBOX_SSH_PORT }
    if ($env:DOCKERBOX_USER)     { $cfg.User    = $env:DOCKERBOX_USER }

    if ($Server)  { $cfg.Server  = $Server }
    if ($SshPort) { $cfg.SshPort = $SshPort }
    if ($User)    { $cfg.User    = $User }

    return $cfg
}

# Only the commands that actually connect need a host. `enroll` must work
# before anything is configured -- that is how you get a key issued.
function Assert-Server {
    param($Cfg)
    if (-not $Cfg.Server) {
        Die "No workspace host configured. Set `$env:DOCKERBOX_HOST, pass -Server, or write $($script:ConfigFile):`n    { `"Server`": `"dockerbox.lan`", `"SshPort`": 2222, `"User`": `"$($Cfg.User)`" }"
    }
}

# ------------------------------------------------------------ ssh + keys ---

function Initialize-Key {
    if (Test-Path $script:KeyPath) { return }

    Write-Step "Generating a keypair for this machine"
    $sshDir = Split-Path $script:KeyPath -Parent
    if (-not (Test-Path $sshDir)) { New-Item -ItemType Directory -Path $sshDir -Force | Out-Null }

    # -N '""' is the PowerShell incantation for an empty passphrase: PS strips
    # the single quotes and ssh-keygen receives a literal "" it reads as empty.
    # Plain '' arrives as no argument at all and ssh-keygen prompts.
    & ssh-keygen -t ed25519 -N '""' -C "dockerbox-$env:COMPUTERNAME-$env:USERNAME" -f $script:KeyPath
    if ($LASTEXITCODE -ne 0) { Die "ssh-keygen failed" }

    Show-Enrollment
    exit 0
}

function Show-Enrollment {
    $cfg = Resolve-Settings
    $pub = Get-Content "$($script:KeyPath).pub" -Raw
    Write-Host ""
    Write-Host "Give this to whoever runs the workspace container." -ForegroundColor Yellow
    Write-Host "It must be saved as: " -NoNewline; Write-Host "authorized_keys.d/$($cfg.User).pub" -ForegroundColor White
    Write-Host "(the filename becomes your unix account name)" -ForegroundColor DarkGray
    Write-Host ""
    Write-Host $pub.Trim()
    Write-Host ""
    try {
        Set-Clipboard -Value $pub.Trim()
        Write-Info "copied to clipboard"
    } catch { }
}

function Get-SshArgs {
    param($Cfg)
    $a = @(
        '-o', 'StrictHostKeyChecking=accept-new'
        '-o', 'ServerAliveInterval=15'
        '-o', 'ServerAliveCountMax=3'
        '-o', 'Compression=no'
        '-o', 'LogLevel=ERROR'
        '-i', $script:KeyPath
        '-p', "$($Cfg.SshPort)"
    )
    if ($Cipher) { $a += @('-c', $Cipher) }
    return $a
}

# Quote a single argument for a POSIX shell.
function ConvertTo-ShellArg {
    param([string]$Value)
    return "'" + ($Value -replace "'", "'\''") + "'"
}

# Run a shell snippet on the workspace. The snippet is base64-encoded so that
# neither PowerShell's parser nor ssh's argument joining can mangle it.
function Invoke-Remote {
    param($Cfg, [string]$Script, [switch]$Tty, [switch]$Quiet)

    $b64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($Script))
    $a = Get-SshArgs $Cfg
    if ($Tty) { $a += '-tt' }
    $a += "$($Cfg.User)@$($Cfg.Server)"
    $a += "echo $b64 | base64 -d | /bin/sh -s"

    if ($Quiet) {
        $out = & ssh @a 2>$null
    } else {
        $out = & ssh @a
    }
    return $out
}

function Get-RemoteInfo {
    param($Cfg)
    $lines = Invoke-Remote $Cfg 'workspace-info' -Quiet
    if ($LASTEXITCODE -ne 0 -or -not $lines) {
        Die "Cannot reach the workspace as '$($Cfg.User)@$($Cfg.Server):$($Cfg.SshPort)'.`n    If this is a new machine, run: dockerbox enroll"
    }
    $info = @{}
    foreach ($line in $lines) {
        if ($line -match '^([A-Z_]+)=(.*)$') { $info[$Matches[1]] = $Matches[2] }
    }
    return $info
}

# --------------------------------------------------------------- rclone ----

function Initialize-Rclone {
    if (Test-Path $script:RcloneExe) { return $script:RcloneExe }

    Write-Step "Downloading rclone (portable, no install)"
    if (-not (Test-Path $script:StateDir)) { New-Item -ItemType Directory -Path $script:StateDir -Force | Out-Null }

    $arch = if ([Environment]::Is64BitOperatingSystem) { 'amd64' } else { '386' }
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { $arch = 'arm64' }
    $url = "https://downloads.rclone.org/rclone-current-windows-$arch.zip"
    $zip = Join-Path $script:StateDir 'rclone.zip'
    $tmp = Join-Path $script:StateDir 'rclone-unzip'

    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing

    if (Test-Path $tmp) { Remove-Item $tmp -Recurse -Force }
    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    $found = Get-ChildItem -Path $tmp -Filter 'rclone.exe' -Recurse | Select-Object -First 1
    if (-not $found) { Die "rclone.exe not found inside $url" }

    Move-Item $found.FullName $script:RcloneExe -Force
    Remove-Item $tmp -Recurse -Force
    Remove-Item $zip -Force

    Write-Info "rclone installed at $($script:RcloneExe)"
    return $script:RcloneExe
}

function Get-FreePort {
    $l = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $l.Start()
    $p = $l.LocalEndpoint.Port
    $l.Stop()
    return $p
}

# -------------------------------------------------------------- session ----

function Get-LiveSession {
    param($Cfg)
    if (-not (Test-Path $script:SessionFile)) { return $null }

    # A session file written by an older version will be missing properties,
    # which throws under Set-StrictMode. Treat any malformed file as "no
    # session" rather than crashing the client.
    try {
        $s = Get-Content $script:SessionFile -Raw | ConvertFrom-Json
        $needed = 'Server', 'User', 'Path', 'RclonePid', 'TunnelPid', 'Mountpoint'
        foreach ($n in $needed) {
            if ($s.PSObject.Properties.Name -notcontains $n) { return $null }
        }
        foreach ($p in $s.RclonePid, $s.TunnelPid) {
            if (-not (Get-Process -Id $p -ErrorAction SilentlyContinue)) { return $null }
        }
        if ($s.Server -ne $Cfg.Server -or $s.User -ne $Cfg.User) { return $null }
        return $s
    } catch {
        return $null
    }
}

function Start-Session {
    param($Cfg, [string]$LocalPath)

    $existing = Get-LiveSession $Cfg
    if ($existing) {
        if ($existing.Path -eq $LocalPath) {
            Write-Info "reusing session (pid $($existing.TunnelPid)) for $LocalPath"
            return $existing
        }
        Write-Info "a session is open for a different directory; replacing it"
        Stop-Session $Cfg
    }

    $rclone = Initialize-Rclone
    $info   = Get-RemoteInfo $Cfg
    $remotePort = [int]$info.WORKSPACE_NFS_PORT
    $localPort  = Get-FreePort

    Write-Step "Serving $LocalPath over NFS on 127.0.0.1:$localPort"
    # --uid/--gid/--umask are not supported by rclone on Windows, so files
    # would always appear as uid 1000. Wide permission bits keep them usable
    # by whatever uid the container account happens to have.
    $rcloneArgs = @(
        'serve', 'nfs', $LocalPath
        '--addr', "127.0.0.1:$localPort"
        '--file-perms', '0666'
        '--dir-perms', '0777'
        '--nfs-cache-handle-limit', '1000000'
        '--log-file', (Join-Path $script:StateDir 'rclone.log')
        '--log-level', 'NOTICE'
    )
    $rcloneProc = Start-Process -FilePath $rclone -ArgumentList $rcloneArgs -PassThru -WindowStyle Hidden

    Write-Step "Opening tunnel to $($Cfg.User)@$($Cfg.Server):$($Cfg.SshPort) (remote port $remotePort)"
    $tunnelArgs = Get-SshArgs $Cfg
    $tunnelArgs += @('-N', '-o', 'ExitOnForwardFailure=yes')
    $tunnelArgs += @('-R', "127.0.0.1:${remotePort}:127.0.0.1:${localPort}")
    foreach ($f in $Forward) { $tunnelArgs += @('-L', $f) }
    $tunnelArgs += "$($Cfg.User)@$($Cfg.Server)"
    $tunnelProc = Start-Process -FilePath 'ssh' -ArgumentList $tunnelArgs -PassThru -WindowStyle Hidden

    # Wait for the forwarded port to answer inside the container before
    # attempting the mount -- ssh binds the listener asynchronously.
    $ready = $false
    for ($i = 0; $i -lt 20; $i++) {
        Start-Sleep -Milliseconds 500
        if ($tunnelProc.HasExited) { Die "ssh tunnel exited immediately (exit $($tunnelProc.ExitCode)) -- is port $remotePort already bound on the workspace?" }
        Invoke-Remote $Cfg "nc -z 127.0.0.1 $remotePort" -Quiet | Out-Null
        if ($LASTEXITCODE -eq 0) { $ready = $true; break }
    }
    if (-not $ready) { Die "the reverse tunnel never came up on the workspace side" }

    # --force because this is a brand new rclone process: its NFS file handles
    # are freshly generated, so any pre-existing mount is now stale.
    Write-Step "Mounting on the workspace"
    Invoke-Remote $Cfg 'sudo workspace-mount --force' | ForEach-Object { Write-Info $_ }
    if ($LASTEXITCODE -ne 0) { Die "remote mount failed" }

    $session = [pscustomobject]@{
        Server     = $Cfg.Server
        User       = $Cfg.User
        Path       = $LocalPath
        LocalPort  = $localPort
        RemotePort = $remotePort
        RclonePid  = $rcloneProc.Id
        TunnelPid  = $tunnelProc.Id
        Mountpoint = $info.WORKSPACE_MOUNTPOINT
    }
    if (-not (Test-Path $script:StateDir)) { New-Item -ItemType Directory -Path $script:StateDir -Force | Out-Null }
    $session | ConvertTo-Json | Set-Content $script:SessionFile
    return $session
}

function Stop-Session {
    param($Cfg)
    if (-not (Test-Path $script:SessionFile)) { return }
    try { $s = Get-Content $script:SessionFile -Raw | ConvertFrom-Json } catch { Remove-Item $script:SessionFile -Force; return }

    Write-Step "Closing session"
    Invoke-Remote $Cfg 'sudo workspace-umount' -Quiet | Out-Null
    foreach ($p in $s.TunnelPid, $s.RclonePid) {
        Stop-Process -Id $p -Force -ErrorAction SilentlyContinue
    }
    Remove-Item $script:SessionFile -Force -ErrorAction SilentlyContinue
}

# ----------------------------------------------------------------- main ----

if ($Command -eq 'help') {
    Get-Help $PSCommandPath -Detailed
    exit 0
}

$cfg = Resolve-Settings

switch ($Command) {
    { $_ -in 'key', 'enroll' } {
        Initialize-Key
        Show-Enrollment
    }
    'status' {
        Assert-Server $cfg
        Initialize-Key
        $info = Get-RemoteInfo $cfg
        $info.GetEnumerator() | Sort-Object Name | ForEach-Object { "{0,-24} {1}" -f $_.Key, $_.Value }
        $live = Get-LiveSession $cfg
        if ($live) { "{0,-24} {1}" -f 'LOCAL_SESSION', "$($live.Path) (tunnel pid $($live.TunnelPid))" }
        else       { "{0,-24} {1}" -f 'LOCAL_SESSION', 'none' }
    }
    'umount' {
        Assert-Server $cfg
        Stop-Session $cfg
    }
    'mount' {
        Assert-Server $cfg
        Initialize-Key
        $target = if ($Path) { (Resolve-Path $Path).Path } else { (Get-Location).Path }
        $s = Start-Session $cfg $target
        Write-Host ""
        Write-Host "$($s.Path)  ->  $($s.User)@$($s.Server):$($s.Mountpoint)" -ForegroundColor Green
        Write-Info "session stays open in the background; close it with: dockerbox umount"
    }
    'shell' {
        Assert-Server $cfg
        Initialize-Key
        $target = if ($Path) { (Resolve-Path $Path).Path } else { (Get-Location).Path }
        $s = Start-Session $cfg $target
        Write-Host ""
        Write-Host "$($s.Path)  ->  $($s.Mountpoint)" -ForegroundColor Green
        Write-Host ""
        $a = Get-SshArgs $cfg
        $a += @('-t', "$($cfg.User)@$($cfg.Server)", 'cd ~/workspace && exec bash -l')
        & ssh @a
        Stop-Session $cfg
    }
    { $_ -in 'run', 'docker' } {
        Assert-Server $cfg
        Initialize-Key
        $target = if ($Path) { (Resolve-Path $Path).Path } else { (Get-Location).Path }
        $s = Start-Session $cfg $target

        $parts = @()
        if ($Command -eq 'docker') { $parts += 'docker' }
        foreach ($x in $Arguments) { $parts += (ConvertTo-ShellArg $x) }
        if ($parts.Count -eq 0) { Die "nothing to run" }

        Invoke-Remote $cfg ("cd ~/workspace && exec " + ($parts -join ' ')) -Tty
        exit $LASTEXITCODE
    }
}
