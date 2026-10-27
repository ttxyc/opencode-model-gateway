param(
  [string]$DataHome = "$env:ProgramData\opencode-model-gateway",
  [string]$Distro = "Ubuntu",
  [switch]$SkipOpenCodeConfig
)
$ErrorActionPreference = "Stop"
function WSLPath([string]$p) { $p2 = $p -replace "\\", "/"; (& wsl.exe -d $Distro -- wslpath -a $p2).Trim() }
function RunRoot([string]$cmd) { & wsl.exe -d $Distro -u root -- sh -c $cmd }
$list = (& wsl.exe -l -q) -replace "`0",""
if (-not ($list -split "`r?`n" | Where-Object { $_.Trim() -eq $Distro })) { throw "WSL distro '$Distro' not found" }

New-Item -ItemType Directory -Force -Path "$DataHome\bin" | Out-Null
$hostSh = @'
#!/bin/sh
set -e
ip="$(ip route | awk '/^default/ {print $3; exit}')"
[ -n "$ip" ] || exit 0
tmp="/tmp/opencode-gateway-hosts.$$"
grep -v 'opencode-gateway.local' /etc/hosts > "$tmp" 2>/dev/null || true
printf '%s opencode-gateway.local\n' "$ip" >> "$tmp"
cat "$tmp" > /etc/hosts
rm -f "$tmp"
'@
$confSh = @'
#!/bin/sh
set -e
touch /etc/wsl.conf
if ! grep -q 'opencode-gateway-host' /etc/wsl.conf; then
  awk -v cmd='command = /usr/local/bin/opencode-gateway-host' '
    BEGIN { boot=0; done=0 }
    /^\[/ { if ($0=="[boot]") { boot=1; print; if (!done) { print cmd; done=1 }; next } else { boot=0 } }
    boot && /^[[:space:]]*command[[:space:]]*=/ { next }
    { print }
    END { if (!done) { print ""; print "[boot]"; print cmd } }
  ' /etc/wsl.conf > /tmp/opencode-wsl.conf.$$
  cat /tmp/opencode-wsl.conf.$$ > /etc/wsl.conf
  rm -f /tmp/opencode-wsl.conf.$$
fi
'@
$ocSh = @'
#!/bin/sh
set -e
f="$HOME/.config/opencode/opencode.jsonc"
[ -f "$f" ] || exit 0
cp "$f" "$f.bak-gateway-$(date +%Y%m%d%H%M%S)"
sed -i 's#http://127\.0\.0\.1:8788#http://opencode-gateway.local:8788#g' "$f"
'@
[IO.File]::WriteAllText("$DataHome\bin\opencode-gateway-host.sh", $hostSh.Replace("`r`n","`n"), (New-Object Text.UTF8Encoding($false)))
[IO.File]::WriteAllText("$DataHome\bin\opencode-gateway-conf.sh", $confSh.Replace("`r`n","`n"), (New-Object Text.UTF8Encoding($false)))
[IO.File]::WriteAllText("$DataHome\bin\opencode-gateway-opencode.sh", $ocSh.Replace("`r`n","`n"), (New-Object Text.UTF8Encoding($false)))

$hostWsl = WSLPath "$DataHome\bin\opencode-gateway-host.sh"
$confWsl = WSLPath "$DataHome\bin\opencode-gateway-conf.sh"
$ocWsl = WSLPath "$DataHome\bin\opencode-gateway-opencode.sh"
RunRoot "cp '$hostWsl' /usr/local/bin/opencode-gateway-host && chmod 755 /usr/local/bin/opencode-gateway-host"
RunRoot "cp '$confWsl' /tmp/opencode-gateway-conf.sh && sh /tmp/opencode-gateway-conf.sh"
RunRoot "/usr/local/bin/opencode-gateway-host"
if (-not $SkipOpenCodeConfig) {
  & wsl.exe -d $Distro -- sh -c "cp '$ocWsl' /tmp/opencode-gateway-opencode.sh && sh /tmp/opencode-gateway-opencode.sh" | Out-Null
}
Write-Host "WSL bridge configured: WSL -> http://opencode-gateway.local:8788"
