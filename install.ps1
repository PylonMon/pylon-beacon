# pylon-beacon installer (Windows) — https://pylonmon.com/docs#beacon
# Usage (PowerShell as Administrator):
#   irm https://pylonmon.com/beacon.ps1 | iex
# Non-interactive: set $env:PYLON_KEY first.

$ErrorActionPreference = "Stop"

if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
    ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  Write-Host "Please run from an elevated (Administrator) PowerShell." -ForegroundColor Yellow
  exit 1
}

$repo = "PylonMon/pylon-beacon"
$dir  = "$env:ProgramFiles\pylon-beacon"
$exe  = "$dir\pylon-beacon.exe"
$conf = "$dir\beacon.conf"

New-Item -ItemType Directory -Force $dir | Out-Null
Write-Host "-> downloading pylon-beacon (windows/amd64)..."
Invoke-WebRequest -UseBasicParsing `
  -Uri "https://github.com/$repo/releases/latest/download/pylon-beacon-windows-amd64.exe" `
  -OutFile $exe

if (-not (Test-Path $conf)) {
  $key = $env:PYLON_KEY
  if (-not $key) {
    $key = Read-Host "PylonMon API key (ingest-scoped; Settings -> Admin -> Status page & API)"
  }
  $pyurl = if ($env:PYLON_URL) { $env:PYLON_URL } else { "https://pylonmon.com" }
  $tmpl = if ($env:PYLON_TEMPLATE) { "template = $env:PYLON_TEMPLATE" } else { "# template = db-servers       # which beacon template configures this node (newest if unset)" }
  @"
# pylon-beacon — https://pylonmon.com/docs#beacon
key      = $key
url      = $pyurl
# node   = $env:COMPUTERNAME     # uncomment to override the monitor name
interval = 20
$tmpl

[custom]
# name = command   (first number in the output becomes the metric)
# battery_pct = powershell -c "(Get-CimInstance Win32_Battery).EstimatedChargeRemaining"
"@ | Set-Content -Encoding utf8 $conf
  Write-Host "-> wrote $conf"
} else {
  Write-Host "-> keeping existing $conf"
}

# Runs as a SYSTEM scheduled task at startup (survives reboots + logouts).
$action  = New-ScheduledTaskAction -Execute $exe
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
  -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero)
Register-ScheduledTask -TaskName "pylon-beacon" -Action $action -Trigger $trigger -Settings $settings `
  -User "SYSTEM" -RunLevel Highest -Force | Out-Null
Start-ScheduledTask -TaskName "pylon-beacon"

Write-Host ""
Write-Host "OK: pylon-beacon is running. Your node appears in PylonMon within a minute." -ForegroundColor Green
Write-Host "  status:  Get-ScheduledTask pylon-beacon | Get-ScheduledTaskInfo"
Write-Host "  config:  $conf"
Write-Host "  restart: Stop-ScheduledTask pylon-beacon; Start-ScheduledTask pylon-beacon"
