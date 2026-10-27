param(
  [string]$DataHome = "$env:ProgramData\opencode-model-gateway",
  [string]$Exe = ""
)
$ErrorActionPreference = "Stop"
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($id)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { Write-Host "Run as Administrator." -ForegroundColor Red; exit 1 }
if (-not $Exe) { $Exe = Join-Path (Split-Path -Parent $PSScriptRoot) "gateway.exe" }
if (-not (Test-Path $Exe)) { Write-Host "Missing $Exe" -ForegroundColor Red; exit 1 }
& "$DataHome\app\gateway.exe" --apply-update --home $DataHome --new $Exe
Write-Host "Update requested. Check http://127.0.0.1:8788/healthz and the service status."
