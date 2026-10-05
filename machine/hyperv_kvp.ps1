# The host-to-guest KVP items of one Hyper-V machine, which is how its Ignition
# document reaches it (ADR 0026). Hyper-V has no cmdlet for them, so this is WMI.
#
#   -Add           reads a JSON array of {"Name": ..., "Data": ...} on stdin and
#                  adds each item. Stdin, never an argument: the data holds the
#                  machine's private host key.
#   -Remove <p>    removes every item whose name starts with <p>. Removing an
#                  item that is not there succeeds (measured 2026-10-05).
#
# Run by psKVP inside psScript, so any error ends the script and fails it. No
# error names an item's data.
param(
    $VM,  # as Get-VM returns it
    [switch] $Add,
    [string] $Remove
)

if (-not $VM) { throw 'the machine is not there' }

$ns = 'root\virtualization\v2'
$cs = Get-WmiObject -Namespace $ns -Class Msvm_ComputerSystem -Filter "Name='$($VM.Id)'"
$svc = Get-WmiObject -Namespace $ns -Class Msvm_VirtualSystemManagementService

# One item from the host's pool (Source 0, the one the guest reads), through
# AddKvpItems or RemoveKvpItems.
function Invoke-KvpItem([string] $Method, [string] $Name, [string] $Data) {
    $item = ([wmiclass]"${ns}:Msvm_KvpExchangeDataItem").CreateInstance()
    $item.Name = $Name
    $item.Data = $Data
    $item.Source = 0

    $result = $svc.$Method($cs, @($item.PSBase.GetText(1)))
    $code = $result.ReturnValue
    # 4096 means it finished as a job, and only the job says whether it worked.
    if ($code -eq 4096) {
        $job = [wmi]$result.Job
        while ($job.JobState -lt 7) {
            Start-Sleep -Milliseconds 200
            $job = [wmi]$result.Job
        }
        $code = $job.ErrorCode
    }
    if ($code -ne 0) { throw "Hyper-V refused $Method on the KVP item $Name with code $code" }
}

if ($Add) {
    $stdin = New-Object IO.StreamReader([Console]::OpenStandardInput(), (New-Object Text.UTF8Encoding $false))
    # foreach rather than a pipeline: Windows PowerShell's ConvertFrom-Json
    # emits an array as one object, and foreach walks it either way.
    foreach ($item in (ConvertFrom-Json $stdin.ReadToEnd())) {
        Invoke-KvpItem AddKvpItems $item.Name $item.Data
    }
}
elseif ($Remove) {
    # The machine's own settings rather than a checkpoint's. Asked through
    # them and not through Msvm_KvpExchangeComponent, which listed nothing for
    # a machine that was off (2026-10-05).
    $settings = $cs.GetRelated('Msvm_VirtualSystemSettingData') |
        Where-Object { $_.VirtualSystemType -eq 'Microsoft:Hyper-V:System:Realized' } |
        ForEach-Object { $_.GetRelated('Msvm_KvpExchangeComponentSettingData') }
    if (-not $settings) { throw "no KVP exchange settings for $($VM.Name)" }

    foreach ($xml in $settings.HostExchangeItems) {
        $name = (([xml]$xml).INSTANCE.PROPERTY | Where-Object { $_.Name -eq 'Name' }).VALUE
        if ($name.StartsWith($Remove)) { Invoke-KvpItem RemoveKvpItems $name '' }
    }
}
else {
    throw 'neither -Add nor -Remove'
}
