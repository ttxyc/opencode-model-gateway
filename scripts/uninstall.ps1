param(
  [string]$DataHome = "$env:ProgramData\opencode-model-gateway",
  [switch]$Purge
)
$ErrorActionPreference = "Stop"
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($id)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { Write-Host "Run as Administrator." -ForegroundColor Red; exit 1 }
if (Test-Path "$DataHome\app\gateway.exe") { & "$DataHome\app\gateway.exe" --uninstall 2>$null | Out-Null }
try { Stop-Service -Name "opencode-model-gateway" -Force -ErrorAction SilentlyContinue } catch {}
try { Remove-NetFirewallRule -DisplayName "opencode Model Gateway" -ErrorAction SilentlyContinue | Out-Null } catch {}
$public = [Environment]::GetFolderPath("CommonDesktopDirectory")
if ($public) { Remove-Item (Join-Path $public "opencode Gateway.lnk") -Force -ErrorAction SilentlyContinue }
$desktop = [Environment]::GetFolderPath("Desktop")
if ($desktop) { Remove-Item (Join-Path $desktop "opencode Gateway.lnk") -Force -ErrorAction SilentlyContinue }
if ($Purge) { Remove-Item $DataHome -Recurse -Force -ErrorAction SilentlyContinue }
Write-Host "Uninstalled. Data kept at $DataHome (use -Purge to delete)."
