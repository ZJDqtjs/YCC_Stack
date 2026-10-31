# StarStack VM host setup / preflight (Windows).
#
# Checks QEMU, optionally installs it with winget, prints the WHPX/firewall
# prerequisites, then starts the provider agent with --vm. The web console's
# one-click launcher embeds the same checks; this script is for operators who
# already have starstack-worker.exe.
#
# Example:
#   .\vm-host-setup.ps1 -Server http://broker:8787 -Id dorm-pc -Launch A1B2C3D4 -VmMax 2
#   .\vm-host-setup.ps1 -Server http://broker:8787 -Id dorm-pc -Bridge br0
#
# Keep this file ASCII: Windows PowerShell 5.1 may read a BOM-less UTF-8 script
# with the system code page.

param(
  [Parameter(Mandatory = $true)][string]$Server,
  [string]$Id = $env:COMPUTERNAME,
  [string]$Launch = "",
  [string]$Token = "",
  [string]$Name = "",
  [double]$Price = 0.8,
  [int]$Slots = 2,
  [int]$VmMax = 2,
  [string]$Bridge = "",
  [string]$Uplink = "",
  [string]$Worker = "starstack-worker.exe",
  [string]$Work = "",
  [switch]$Yes,
  [switch]$CheckOnly
)

$ErrorActionPreference = "Stop"

function Have([string]$name) {
  return [bool](Get-Command $name -ErrorAction SilentlyContinue)
}

function Add-QemuPath {
  $dir = Join-Path $env:ProgramFiles "qemu"
  if (Test-Path (Join-Path $dir "qemu-system-x86_64.exe")) {
    if ($env:Path -notlike "*$dir*") { $env:Path = "$dir;$env:Path" }
  }
}

if ($Bridge -ne "" -or $Uplink -ne "") {
  throw "Bridge/uplink are Linux routed-mode options; on Windows leave them empty and use slirp."
}
if ($VmMax -lt 1 -or $VmMax -gt 8) {
  throw "-VmMax must be between 1 and 8."
}

Write-Host "StarStack VM host preflight"
Write-Host "Broker: $Server"
Write-Host "Node id: $Id"

Add-QemuPath
if (-not (Have "qemu-system-x86_64") -or -not (Have "qemu-img")) {
  Write-Host "[!] QEMU is missing (qemu-system-x86_64 / qemu-img)." -ForegroundColor Yellow
  Write-Host "    Recommended: winget install --id SoftwareFreedomConservancy.QEMU -e"
  Write-Host "    Or download: https://www.qemu.org/download/#windows"
  if (Have "winget") {
    $install = $Yes
    if (-not $install) {
      $answer = Read-Host "Install QEMU with winget now? [y/N]"
      $install = ($answer -match '^[Yy]')
    }
    if ($install) {
      winget install --id SoftwareFreedomConservancy.QEMU -e `
        --accept-package-agreements --accept-source-agreements
      Add-QemuPath
    }
  }
}
if (-not (Have "qemu-system-x86_64") -or -not (Have "qemu-img")) {
  throw @"
QEMU is still not available. Install it, add its folder to PATH, then re-run.
QEMU is required for cloud hosts; transcoding alone does not need it.
"@
}

try {
  $feature = Get-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -ErrorAction Stop
  if ($feature.State -ne "Enabled") {
    Write-Host "[!] Windows Hypervisor Platform is $($feature.State)." -ForegroundColor Yellow
    Write-Host "    Enable it from an elevated PowerShell, then reboot:"
    Write-Host "    Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All"
  }
} catch {
  Write-Host "[i] Could not check WHPX without elevation. If QEMU reports a WHPX error,"
  Write-Host "    enable Windows Hypervisor Platform in Optional Features and reboot."
}

Write-Host "[i] Allow inbound IPv6 TCP 5900-5999 (console) and 20000-29999 (SSH/ports)."
Write-Host "    Elevated PowerShell:"
Write-Host '    netsh advfirewall firewall add rule name="StarStack VM" dir=in action=allow protocol=TCP localport=5900-5999,20000-29999'

$workerPath = $Worker
$cachedWorker = Join-Path $env:LOCALAPPDATA "starstack-node\starstack-worker.exe"
if (-not (Test-Path $workerPath) -and (Test-Path $cachedWorker)) {
  $workerPath = $cachedWorker
}
if (Test-Path $workerPath) {
  $workerPath = (Resolve-Path $Worker).Path
} elseif (-not (Have $Worker)) {
  throw "Cannot find the agent: $Worker. Download the launcher from the web console or pass -Worker <path>."
}

if ($Work -eq "") {
  $Work = Join-Path $env:LOCALAPPDATA "starstack-node\cache"
}
New-Item -ItemType Directory -Force -Path $Work | Out-Null

if ($CheckOnly) {
  Write-Host "Preflight passed; agent not started (-CheckOnly)." -ForegroundColor Green
  exit 0
}

$priceText = $Price.ToString([System.Globalization.CultureInfo]::InvariantCulture)
$agentArgs = @(
  "--server", $Server, "--id", $Id,
  "--price", $priceText, "--slots", "$Slots",
  "--work", $Work, "--vm", "--vm-max", "$VmMax",
  "--qemu", "qemu-system-x86_64", "--qemu-img", "qemu-img"
)
if ($Name -ne "") { $agentArgs += @("--name", $Name) }
if ($Launch -ne "") { $agentArgs += @("--launch", $Launch) }
if ($Token -ne "") { $agentArgs += @("--token", $Token) }

Write-Host "Starting the cloud-host agent (max $VmMax VMs)..." -ForegroundColor Green
& $workerPath @agentArgs
