# Daily WorkBuddy check-in helper.
# Runs as SYSTEM from Task Scheduler; calls the local gateway admin API.
# The gateway owns the WorkBuddy tokens, so this script never reads them.

$ErrorActionPreference = "Continue"

$GatewayBase   = "http://127.0.0.1:8788"
$AdminTokenFile = "C:\ProgramData\opencode-model-gateway\config\admin.token"
$LogDir        = "C:\ProgramData\WorkBuddyCheckin"
$LogFile       = Join-Path $LogDir "checkin.log"
$MaxLogBytes   = 5MB

function Write-Log {
    param([string]$Level, [string]$Message)
    try {
        if (-not (Test-Path $LogDir)) {
            New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
        }
        if ((Test-Path $LogFile) -and ((Get-Item $LogFile).Length -gt $MaxLogBytes)) {
            Move-Item -Path $LogFile -Destination "$LogFile.1" -Force
        }
        $line = "{0} [{1}] {2}" -f (Get-Date -Format "yyyy-MM-dd HH:mm:ss"), $Level, $Message
        Add-Content -Path $LogFile -Value $line -Encoding UTF8
    } catch {
        # Never let logging break the task.
    }
}

function Wait-Gateway {
    param([int]$TimeoutSec = 300)
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -Uri "$GatewayBase/healthz" -UseBasicParsing -TimeoutSec 3
            if ($r.StatusCode -eq 200) { return $true }
        } catch { }
        Start-Sleep -Seconds 5
    }
    return $false
}

Write-Log "INFO" "checkin task start"

if (-not (Wait-Gateway)) {
    Write-Log "ERROR" "gateway health check timed out"
    exit 1
}
if (-not (Test-Path $AdminTokenFile)) {
    Write-Log "ERROR" "admin token not found: $AdminTokenFile"
    exit 1
}

$token = (Get-Content $AdminTokenFile -Raw).Trim()
$body = @{ region = "all"; dryRun = $false } | ConvertTo-Json -Compress
$attempts = 3

for ($i = 1; $i -le $attempts; $i++) {
    try {
        $resp = Invoke-RestMethod -Uri "$GatewayBase/admin/api/workbuddy/checkin" `
            -Method Post `
            -Headers @{ "X-Admin-Token" = $token } `
            -ContentType "application/json" `
            -Body $body `
            -TimeoutSec 120
        $result = $resp.result
        if ($null -eq $result) { throw "response missing result" }

        foreach ($item in @($result.results)) {
            $msg = "account={0} region={1} state={2} active={3} todayCheckedIn={4} credit={5} streakDays={6} message={7}" -f `
                $item.label, $item.region, $item.state, $item.active, $item.todayCheckedIn, `
                $item.credit, $item.streakDays, $item.message
            if ($item.error) { $msg += " error=" + $item.error }
            Write-Log "INFO" $msg
        }

        Write-Log "INFO" ("summary success={0} already={1} eligible={2} skipped={3} failed={4} totalCredit={5}" -f `
            $result.success, $result.already, $result.eligible, $result.skipped, $result.failed, $result.totalCredit)

        if ($result.failed -gt 0 -or $result.ok -ne $true) {
            Write-Log "WARN" "some accounts failed"
            if ($i -lt $attempts) {
                Start-Sleep -Seconds 60
                continue
            }
            exit 1
        }

        Write-Log "INFO" "checkin task done"
        exit 0
    } catch {
        $err = $_.Exception.Message
        if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
            $err += " | " + $_.ErrorDetails.Message
        }
        Write-Log "WARN" ("attempt {0}/{1} failed: {2}" -f $i, $attempts, $err)
        if ($i -lt $attempts) {
            Start-Sleep -Seconds 60
        } else {
            Write-Log "ERROR" "checkin task failed"
            exit 1
        }
    }
}