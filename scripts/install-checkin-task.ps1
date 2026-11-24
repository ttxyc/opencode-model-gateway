param(
    [string]$TaskName = "WorkBuddy Daily Checkin",
    [string]$SourceScript = "",
    [string]$TargetScript = "C:\ProgramData\opencode-model-gateway\scripts\workbuddy-checkin.ps1",
    [string]$LogDir = "C:\ProgramData\WorkBuddyCheckin",
    [switch]$RunNow
)

$ErrorActionPreference = "Stop"

function Assert-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    $p = New-Object Security.Principal.WindowsPrincipal($id)
    if (-not $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Write-Host "Please run this script as Administrator." -ForegroundColor Red
        exit 1
    }
}

Assert-Admin

if (-not $SourceScript) {
    $SourceScript = Join-Path $PSScriptRoot "workbuddy-checkin.ps1"
}
if (-not (Test-Path $SourceScript)) {
    throw "Source script not found: $SourceScript"
}

$targetDir = Split-Path -Parent $TargetScript
New-Item -ItemType Directory -Force -Path $targetDir | Out-Null
Copy-Item -Path $SourceScript -Destination $TargetScript -Force
Write-Host "Installed check-in script: $TargetScript"

New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
try {
    $acl = Get-Acl $LogDir
    $rule = New-Object System.Security.AccessControl.FileSystemAccessRule(
        "BUILTIN\Users",
        "Read",
        "ContainerInherit,ObjectInherit",
        "None",
        "Allow"
    )
    $acl.SetAccessRule($rule)
    Set-Acl -Path $LogDir -AclObject $acl
} catch {
    Write-Warning "Could not grant Users read access to ${LogDir}: $_"
}

try {
    $health = Invoke-RestMethod -Uri "http://127.0.0.1:8788/healthz" -TimeoutSec 5
    Write-Host "Gateway version: $($health.version)"
    if ([string]$health.version -ne "1.2.7" -and [string]$health.version -ne "1.2.7-local") {
        Write-Warning "Gateway is not 1.2.7; the WorkBuddy check-in endpoint may be missing."
    }
} catch {
    Write-Warning "Gateway health check failed: $_"
}

$action = New-ScheduledTaskAction -Execute "powershell.exe" `
    -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$TargetScript`""

$startupTrigger = New-ScheduledTaskTrigger -AtStartup
$dailyTrigger = New-ScheduledTaskTrigger -Daily -At "10:00"

$settings = New-ScheduledTaskSettingsSet `
    -StartWhenAvailable `
    -MultipleInstances IgnoreNew `
    -RestartCount 3 `
    -RestartInterval (New-TimeSpan -Minutes 5) `
    -ExecutionTimeLimit (New-TimeSpan -Minutes 30) `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -RunOnlyIfNetworkAvailable

$principal = New-ScheduledTaskPrincipal -UserId "SYSTEM" -LogonType ServiceAccount -RunLevel Highest

Register-ScheduledTask `
    -TaskName $TaskName `
    -Action $action `
    -Trigger @($startupTrigger, $dailyTrigger) `
    -Settings $settings `
    -Principal $principal `
    -Force | Out-Null

Write-Host "Registered scheduled task: $TaskName"
Get-ScheduledTask -TaskName $TaskName |
    Select-Object TaskName, State |
    Format-Table -AutoSize

if ($RunNow) {
    Write-Host "Starting task now..."
    Start-ScheduledTask -TaskName $TaskName
    Start-Sleep -Seconds 15
    Get-ScheduledTaskInfo -TaskName $TaskName |
        Select-Object TaskName, LastRunTime, LastTaskResult, NextRunTime |
        Format-List
}