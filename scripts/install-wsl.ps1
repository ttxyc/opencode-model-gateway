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
#!/bin/bash
# opencode-gateway-host: point opencode-gateway.local at the Windows host.
# Uses only /proc/net/route + bash builtins; no iproute2/awk/hostname/grep/cat required.
set -u

hex=""
while read -r iface dest gw rest; do
  if [ "$dest" = "00000000" ] && [ -n "$gw" ]; then
    hex="$gw"
    break
  fi
done < /proc/net/route

[ -n "$hex" ] || exit 0
[ "${#hex}" -eq 8 ] || exit 0

# /proc/net/route gateway is little-endian hex: 01601EAC -> 172.30.96.1
a=$((16#${hex:6:2}))
b=$((16#${hex:4:2}))
c=$((16#${hex:2:2}))
d=$((16#${hex:0:2}))
ip="$a.$b.$c.$d"

tmp="/tmp/opencode-gateway-hosts.$$"
if command -v grep >/dev/null 2>&1; then
  grep -v 'opencode-gateway.local' /etc/hosts > "$tmp" 2>/dev/null || true
else
  while IFS= read -r line; do
    case "$line" in
      *opencode-gateway.local*) ;;
      *) printf '%s\n' "$line" ;;
    esac
  done < /etc/hosts > "$tmp"
fi
printf '%s opencode-gateway.local\n' "$ip" >> "$tmp"
while IFS= read -r line; do
  printf '%s\n' "$line"
done < "$tmp" > /etc/hosts
rm -f "$tmp" 2>/dev/null || true
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
