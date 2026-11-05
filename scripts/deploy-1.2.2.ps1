param(
  [string]$DataHome = "$env:ProgramData\opencode-model-gateway",
  [string]$Exe = "",
  [switch]$Rollback,
  [switch]$DryRun
)
$ErrorActionPreference = "Stop"

$ServiceName = "opencode-model-gateway"
$AppDir = Join-Path $DataHome "app"
$Live = Join-Path $AppDir "gateway.exe"
$Backup = Join-Path $AppDir "gateway.exe.old"
$HealthURL = "http://127.0.0.1:8788/healthz"

try { [Console]::OutputEncoding = [Text.Encoding]::UTF8 } catch { }

function Assert-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  $p = New-Object Security.Principal.WindowsPrincipal($id)
  if (-not $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host "请以管理员身份运行此脚本。" -ForegroundColor Red
    exit 1
  }
}

function Get-ServiceState {
  $s = & sc.exe query $ServiceName 2>$null
  if ($LASTEXITCODE -ne 0) { return "MISSING" }
  if ($s -match "RUNNING") { return "RUNNING" }
  if ($s -match "STOPPED") { return "STOPPED" }
  return "UNKNOWN"
}

function Wait-Health {
  param([int]$TimeoutSec = 30)
  $deadline = (Get-Date).AddSeconds($TimeoutSec)
  while ((Get-Date) -lt $deadline) {
    try {
      $r = Invoke-WebRequest -Uri $HealthURL -UseBasicParsing -TimeoutSec 3
      if ($r.StatusCode -eq 200) {
        return ($r.Content | ConvertFrom-Json)
      }
    } catch { }
    Start-Sleep -Milliseconds 500
  }
  return $null
}

function Get-ExeVersion {
  param([string]$Path)
  if (-not (Test-Path $Path)) { return "<missing>" }
  try { return (& $Path -version 2>$null) } catch { return "<unknown>" }
}

# ---------------- 回滚 ----------------
if ($Rollback) {
  Assert-Admin
  Write-Host "=== 回滚到上一个版本 ===" -ForegroundColor Cyan

  if (-not (Test-Path $Backup)) {
    Write-Host "找不到备份 $Backup，无法回滚。" -ForegroundColor Red
    Write-Host "可用的历史备份：" -ForegroundColor Yellow
    Get-ChildItem $AppDir -Filter "gateway.exe*" | Select-Object Name, Length, LastWriteTime | Format-Table
    exit 1
  }

  $curVer = Get-ExeVersion $Live
  $oldVer = Get-ExeVersion $Backup
  Write-Host "当前版本: $curVer"
  Write-Host "回滚目标: $oldVer ($Backup)"

  if ($DryRun) {
    Write-Host "[DryRun] 将停止服务、用备份替换、再启动。" -ForegroundColor Yellow
    exit 0
  }

  & sc.exe stop $ServiceName | Out-Null
  $deadline = (Get-Date).AddSeconds(30)
  while ((Get-Date) -lt $deadline) {
    if ((Get-ServiceState) -eq "STOPPED") { break }
    Start-Sleep -Milliseconds 300
  }

  # 当前版本另存一份，便于回滚后再前进
  $keep = Join-Path $AppDir ("gateway.exe.rollback-" + (Get-Date -Format "yyyyMMdd-HHmmss"))
  try {
    Copy-Item $Live $keep -Force
    Write-Host "已保留当前版本: $keep"
  } catch { Write-Host "保留当前版本失败（忽略）: $_" -ForegroundColor Yellow }

  try {
    Copy-Item $Backup $Live -Force
  } catch {
    Write-Host "替换失败: $_" -ForegroundColor Red
    & sc.exe start $ServiceName | Out-Null
    exit 1
  }

  & sc.exe start $ServiceName | Out-Null
  $health = Wait-Health 30
  if (-not $health) {
    Write-Host "回滚后健康检查失败！请手动检查。" -ForegroundColor Red
    exit 1
  }
  Write-Host "回滚成功，当前运行版本: $($health.version)" -ForegroundColor Green
  exit 0
}

# ---------------- 部署 ----------------
Assert-Admin

if (-not $Exe) { $Exe = Join-Path (Split-Path -Parent $PSScriptRoot) "dist\gateway.exe" }
if (-not (Test-Path $Exe)) {
  Write-Host "找不到新版本二进制: $Exe" -ForegroundColor Red
  exit 1
}

$newVer = Get-ExeVersion $Exe
$curVer = Get-ExeVersion $Live

Write-Host "=== 部署 opencode-model-gateway ===" -ForegroundColor Cyan
Write-Host "数据目录: $DataHome"
Write-Host "当前版本: $curVer"
Write-Host "目标版本: $newVer ($Exe)"
Write-Host "服务状态: $(Get-ServiceState)"

if ($DryRun) {
  Write-Host "[DryRun] 将执行 --apply-update（内含停服/备份/替换/健康检查/失败自动回滚）。" -ForegroundColor Yellow
  exit 0
}

# 手工留一份版本化备份：--apply-update 只会覆盖 gateway.exe.old，
# 多次更新后原始回滚点会丢失。
try {
  $stamp = Join-Path $AppDir ("gateway.exe.bak-" + (Get-Date -Format "yyyyMMdd-HHmmss") + "-" + ($curVer -replace '[^\w\.-]','_'))
  Copy-Item $Live $stamp -Force
  Write-Host "已留版本化备份: $stamp" -ForegroundColor DarkGray
} catch {
  Write-Host "版本化备份失败（继续）: $_" -ForegroundColor Yellow
}

Write-Host "`n调用内置 --apply-update..." -ForegroundColor Cyan
& $Live --apply-update --home $DataHome --new $Exe
$rc = $LASTEXITCODE

$deadline = (Get-Date).AddSeconds(45)
$health = $null
while ((Get-Date) -lt $deadline) {
  $health = Wait-Health 3
  if ($health) { break }
}

Write-Host ""
if ($rc -ne 0) {
  Write-Host "apply-update 返回码 $rc（可能已自动回滚）。" -ForegroundColor Red
}
if (-not $health) {
  Write-Host "健康检查未通过。当前服务状态: $(Get-ServiceState)" -ForegroundColor Red
  Write-Host "可用以下命令回滚: powershell -ExecutionPolicy Bypass -File $PSCommandPath -Rollback" -ForegroundColor Yellow
  exit 1
}

Write-Host "部署完成" -ForegroundColor Green
Write-Host "  版本    : $($health.version)"
Write-Host "  pid     : $($health.pid)"
Write-Host "  uptime  : $($health.uptimeSec)s"
Write-Host "  服务    : $(Get-ServiceState)"
if ($health.version -ne $newVer) {
  Write-Host "警告: 运行版本 $($health.version) 与目标 $newVer 不一致" -ForegroundColor Yellow
}

Write-Host "`n=== 端到端自检（WorkBuddy SSE 清洗）===" -ForegroundColor Cyan
$token = ""
$tokFile = Join-Path $DataHome "config\client.token"
if (Test-Path $tokFile) { $token = (Get-Content $tokFile -Raw).Trim() }
if (-not $token) {
  Write-Host "未找到 client.token，跳过自检。" -ForegroundColor Yellow
  exit 0
}

try {
  $body = '{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"1+1=?"}]}'
  $resp = Invoke-WebRequest -Uri "http://127.0.0.1:8788/p/workbuddy/v1/chat/completions" -Method Post `
    -Headers @{ authorization = "Bearer $token"; "content-type" = "application/json" } `
    -Body $body -UseBasicParsing -TimeoutSec 120
  $text = $resp.Content
  $empty = @()
  foreach ($p in @('"tool_calls":[]', '"function_call":null', '"extra_fields":null', '"refusal":""')) {
    if ($text.Contains($p)) { $empty += $p }
  }
  if ($empty.Count -gt 0) {
    Write-Host "自检: 仍存在空字段 -> $($empty -join ', ')" -ForegroundColor Red
    Write-Host "说明清洗未生效，可回滚。" -ForegroundColor Yellow
  } else {
    Write-Host "自检: 空字段已清除，SSE 清洗生效。" -ForegroundColor Green
  }
  if ($text.Contains('data: [DONE]')) {
    Write-Host "自检: [DONE] 正常。" -ForegroundColor Green
  } else {
    Write-Host "自检: 未收到 [DONE]，流可能异常。" -ForegroundColor Yellow
  }
} catch {
  Write-Host "自检请求失败: $_" -ForegroundColor Yellow
}
