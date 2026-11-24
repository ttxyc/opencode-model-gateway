#!/usr/bin/env bash
# Register the WorkBuddy daily check-in scheduled task on Windows.
#   ./scripts/install-checkin-task.sh            只注册
#   ./scripts/install-checkin-task.sh --run-now  注册后立即跑一次
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WIN_USER="${WIN_USER:-$(cmd.exe /c "echo %USERNAME%" 2>/dev/null | tr -d '\r')}"
if [ -z "$WIN_USER" ]; then
  echo "无法确定 Windows 用户名，可手动指定: WIN_USER=xxx $0" >&2
  exit 1
fi

WIN_TMP="/mnt/c/Users/$WIN_USER/AppData/Local/Temp"
if [ ! -d "$WIN_TMP" ]; then
  echo "临时目录不存在: $WIN_TMP" >&2
  exit 1
fi

mkdir -p "$WIN_TMP"
cp "$HERE/workbuddy-checkin.ps1" "$WIN_TMP/workbuddy-checkin.ps1"
cp "$HERE/install-checkin-task.ps1" "$WIN_TMP/install-checkin-task.ps1"

EXTRA=""
if [ "${1:-}" = "--run-now" ]; then
  EXTRA=",'-RunNow'"
fi

echo ">>> 注册计划任务（需要管理员权限，可能弹 UAC）"
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command \
  "Start-Process powershell -Verb RunAs -Wait -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File','C:\\Users\\$WIN_USER\\AppData\\Local\\Temp\\install-checkin-task.ps1'$EXTRA"