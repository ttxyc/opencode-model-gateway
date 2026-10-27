param(
  [string]$DataHome = "$env:ProgramData\opencode-model-gateway",
  [string]$QuotaLinePath = "",
  [string]$ClientToken = "",
  [string]$ClientTokenFile = "",
  [string]$AdminToken = "",
  [string]$ManifestUrl = "https://github.com/ttxyc/opencode-model-gateway/releases/latest/download/latest.json",
  [string]$UpdateProxy = "",
  [switch]$NoUpdateProxy,
  [switch]$SkipWSL,
  [switch]$SkipFirewall
)
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
if (-not $UpdateProxy -and -not $NoUpdateProxy) {
  try {
    $ie = Get-ItemProperty "HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings" -ErrorAction SilentlyContinue
    if ($ie.ProxyEnable -eq 1 -and $ie.ProxyServer) {
      $UpdateProxy = [string]$ie.ProxyServer
      if ($UpdateProxy -match "=") {
        $m = [regex]::Match($UpdateProxy, "(?:https|http)=([^;]+)")
        if ($m.Success) { $UpdateProxy = $m.Groups[1].Value }
      }
      if ($UpdateProxy -and $UpdateProxy -notmatch "^https?://") { $UpdateProxy = "http://" + $UpdateProxy }
    }
  } catch {}
}
function Fail($msg) { Write-Host "ERROR: $msg" -ForegroundColor Red; exit 1 }
function New-RandomToken { param($prefix) $b = New-Object byte[] 24; [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b); return $prefix + [BitConverter]::ToString($b).Replace("-","").ToLower() }

$id = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($id)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { Fail "Run install.ps1 as Administrator (one time only)." }
if (-not [Environment]::Is64BitOperatingSystem) { Fail "Only x64 Windows is supported." }

Write-Host "Installing to $DataHome"
if (Get-Service -Name "opencode-model-gateway" -ErrorAction SilentlyContinue) {
  Write-Host "Stopping existing service..."
  & "$DataHome\app\gateway.exe" --uninstall 2>$null | Out-Null
  Start-Sleep -Seconds 1
}
$dirs = @("$DataHome\app","$DataHome\config","$DataHome\keys","$DataHome\logs","$DataHome\state","$DataHome\updates","$DataHome\bin")
foreach ($d in $dirs) { New-Item -ItemType Directory -Force -Path $d | Out-Null }

Copy-Item "$Root\gateway.exe" "$DataHome\app\gateway.exe" -Force
Copy-Item "$Root\config.example.json" "$DataHome\config\gateway.example.json" -Force
$vbs = (Get-Content "$Root\opencode-gateway.vbs" -Raw).Replace("@@HOME@@", $DataHome)
[IO.File]::WriteAllText("$DataHome\opencode-gateway.vbs", $vbs, (New-Object Text.UTF8Encoding($false)))

if (-not $ClientToken -and $ClientTokenFile) {
  if (-not (Test-Path $ClientTokenFile)) { Fail "ClientTokenFile not found: $ClientTokenFile" }
  $ClientToken = (Get-Content $ClientTokenFile -Raw).Trim()
}
if (-not $ClientToken) { $ClientToken = New-RandomToken "gw-" }
if (-not $AdminToken) { $AdminToken = New-RandomToken "adm-" }

$pub = ""
if (Test-Path "$Root\publickey.txt") { $pub = (Get-Content "$Root\publickey.txt" -Raw).Trim() }
$cfg = Get-Content "$Root\config.example.json" -Raw -Encoding UTF8 | ConvertFrom-Json
$cfg.token = $ClientToken
$cfg.adminToken = $AdminToken
if ($ManifestUrl) { $cfg.update.manifestUrl = $ManifestUrl }
$cfg.update.enabled = [bool]$pub
$cfg.update.publicKey = $pub
$json = $cfg | ConvertTo-Json -Depth 20
[IO.File]::WriteAllText("$DataHome\config\gateway.json", $json, (New-Object Text.UTF8Encoding($false)))
[IO.File]::WriteAllText("$DataHome\config\client.token", $ClientToken + "`n", (New-Object Text.UTF8Encoding($false)))
[IO.File]::WriteAllText("$DataHome\config\admin.token", $AdminToken + "`n", (New-Object Text.UTF8Encoding($false)))

if ($QuotaLinePath) {
  if (-not (Test-Path $QuotaLinePath)) { Fail "QuotaLinePath not found: $QuotaLinePath" }
  Copy-Item $QuotaLinePath "$DataHome\keys\quota-line.json" -Force
} else {
  $providers = @{}
  Write-Host "Enter keys; leave blank to skip. Multiple keys separated by comma."
  foreach ($p in @("modelscope","cline","atria","amd","deepseek")) {
    $line = Read-Host "$p key(s)"
    if ($line) {
      $keys = @($line.Split(",") | ForEach-Object { $_.Trim() } | Where-Object { $_ })
      if ($keys.Count -gt 0) { $providers[$p] = @{ keys = $keys; label = $p } }
    }
  }
  $keysJson = $providers | ConvertTo-Json -Depth 6
  [IO.File]::WriteAllText("$DataHome\keys\quota-line.json", $keysJson, (New-Object Text.UTF8Encoding($false)))
}

# ACL: service files read-only to users; keys/state/config secrets only for SYSTEM/admins.
$userName = "$env:USERDOMAIN\$env:USERNAME"
& icacls $DataHome /inheritance:r /grant:r "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F" "*S-1-5-32-545:(OI)(CI)RX" | Out-Null
foreach ($d in @("$DataHome\keys","$DataHome\state","$DataHome\logs","$DataHome\updates")) {
  & icacls $d /inheritance:r /grant:r "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F" | Out-Null
}
function Grant-UserFull($path) {
  $acl = Get-Acl $path
  $acl.SetAccessRuleProtection($true, $true)
  $rule = New-Object System.Security.AccessControl.FileSystemAccessRule($userName, "FullControl", "Allow")
  $acl.SetAccessRule($rule)
  Set-Acl $path $acl
}
Grant-UserFull "$DataHome\config\gateway.json"
Grant-UserFull "$DataHome\config\admin.token"
Grant-UserFull "$DataHome\config\gateway.example.json"
& icacls "$DataHome\config\client.token" /inheritance:r /grant:r "*S-1-5-18:R" "*S-1-5-32-544:F" "*S-1-5-32-545:R" | Out-Null
& icacls "$DataHome\app" /inheritance:r /grant:r "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F" "*S-1-5-32-545:(OI)(CI)RX" | Out-Null
& icacls "$DataHome\opencode-gateway.vbs" /grant:r "*S-1-5-32-545:R" | Out-Null

& "$DataHome\app\gateway.exe" --install --home $DataHome
if ($LASTEXITCODE -ne 0) { Fail "service registration failed" }
Start-Service -Name "opencode-model-gateway" -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2
try {
  $h = Invoke-RestMethod -Uri "http://127.0.0.1:8788/healthz" -TimeoutSec 10
  if (-not $h.ok) { Fail "health check failed" }
} catch { Fail "health check failed: $($_.Exception.Message)" }

if ($UpdateProxy) {
  $svcKey = "HKLM:\SYSTEM\CurrentControlSet\Services\opencode-model-gateway"
  $noProxy = "127.0.0.1,localhost,modelscope.cn,api-inference.modelscope.cn,api.cline.bot,api.atria-asi.ai,developer.amd.com.cn,api.deepseek.com"
  New-ItemProperty -Path $svcKey -Name Environment -PropertyType MultiString -Value @("HTTPS_PROXY=$UpdateProxy","HTTP_PROXY=$UpdateProxy","NO_PROXY=$noProxy") -Force | Out-Null
  Restart-Service -Name "opencode-model-gateway" -Force -ErrorAction SilentlyContinue
  Start-Sleep -Seconds 3
} elseif ($NoUpdateProxy) {
  Remove-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\opencode-model-gateway" -Name Environment -Force -ErrorAction SilentlyContinue
  Restart-Service -Name "opencode-model-gateway" -Force -ErrorAction SilentlyContinue
  Start-Sleep -Seconds 3
}

if (-not $SkipFirewall) {
  try {
    Remove-NetFirewallRule -DisplayName "opencode Model Gateway" -ErrorAction SilentlyContinue | Out-Null
    New-NetFirewallRule -DisplayName "opencode Model Gateway" -Direction Inbound -Program "$DataHome\app\gateway.exe" -Protocol TCP -LocalPort 8788 -Action Allow -Profile Any -RemoteAddress 172.16.0.0/12 -ErrorAction Stop | Out-Null
  } catch { Write-Host "WARN: firewall rule not created: $($_.Exception.Message)" -ForegroundColor Yellow }
}

$shell = New-Object -ComObject WScript.Shell
$shortcutPaths = @()
$public = [Environment]::GetFolderPath("CommonDesktopDirectory")
if (-not $public) { $public = "C:\Users\Public\Desktop" }
$shortcutPaths += (Join-Path $public "opencode Gateway.lnk")
$desktop = [Environment]::GetFolderPath("Desktop")
if ($desktop) { $shortcutPaths += (Join-Path $desktop "opencode Gateway.lnk") }
foreach ($sp in $shortcutPaths) {
  $sc = $shell.CreateShortcut($sp)
  $sc.TargetPath = "$env:SystemRoot\System32\wscript.exe"
  $sc.Arguments = '"' + "$DataHome\opencode-gateway.vbs" + '"'
  $sc.WorkingDirectory = $DataHome
  $sc.IconLocation = "$DataHome\app\gateway.exe,0"
  $sc.Description = "opencode Model Gateway"
  $sc.Save()
}

if (-not $SkipWSL) {
  try { & "$PSScriptRoot\install-wsl.ps1" -DataHome $DataHome } catch { Write-Host "WARN: WSL setup failed: $($_.Exception.Message)" -ForegroundColor Yellow }
}

Write-Host ""
Write-Host "Installed. Client token: $DataHome\config\client.token"
Write-Host "Admin token:  $DataHome\config\admin.token"
Write-Host "Admin page:   http://127.0.0.1:8788/admin"
Write-Host "Service:      opencode-model-gateway (services.msc)"
