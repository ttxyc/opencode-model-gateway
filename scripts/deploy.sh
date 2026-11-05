#!/usr/bin/env bash
# 在 WSL 里部署 opencode-model-gateway 到 Windows 服务。
#   ./scripts/deploy.sh              部署 dist/gateway.exe
#   ./scripts/deploy.sh --rollback   回滚到上一个版本
#   ./scripts/deploy.sh --dry-run    只打印将要做什么
#   ./scripts/deploy.sh --exe /path/to/gateway.exe
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
PS1="$HERE/deploy-1.2.2.ps1"
EXE="$ROOT/dist/gateway.exe"
MODE="deploy"

# 自动定位 Windows 临时目录，避免把用户名写死
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

while [ $# -gt 0 ]; do
  case "$1" in
    --rollback) MODE="rollback"; shift ;;
    --dry-run)  MODE="deploy"; DRY="--DryRun"; shift ;;
    --exe)      EXE="$2"; shift 2 ;;
    -h|--help)  sed -n '2,6p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

if [ "$MODE" = "deploy" ] && [ ! -f "$EXE" ]; then
  echo "找不到二进制: $EXE" >&2
  echo "先编译: GOOS=windows GOARCH=amd64 go build -trimpath -ldflags \"-s -w -X main.version=1.2.2\" -o dist/gateway.exe ./cmd/gateway" >&2
  exit 1
fi

if [ "$MODE" = "deploy" ] && [ ! -f "$PS1" ]; then
  echo "找不到脚本: $PS1" >&2; exit 1
fi

# PowerShell 无法直接从 \\wsl$ 读取 -File，需先复制到 Windows 侧。
# Windows PowerShell 5.1 读无 BOM 的 UTF-8 会按 ANSI 解析，中文注释导致脚本
# 在第一个中文字符串后截断；必须确保落盘文件带 UTF-8 BOM。
WIN_PS1="$WIN_TMP/deploy-gateway.ps1"
mkdir -p "$WIN_TMP"
if head -c 3 "$PS1" | od -An -tx1 | grep -q "ef bb bf"; then
  cp "$PS1" "$WIN_PS1"
else
  { printf '\xEF\xBB\xBF'; cat "$PS1"; } > "$WIN_PS1"
fi
WIN_PS1_PATH="C:\\Users\\$WIN_USER\\AppData\\Local\\Temp\\deploy-gateway.ps1"

if [ "$MODE" = "rollback" ]; then
  echo ">>> 回滚（需要管理员权限，可能弹 UAC）"
  powershell.exe -NoProfile -ExecutionPolicy Bypass -Command \
    "Start-Process powershell -Verb RunAs -Wait -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File','$WIN_PS1_PATH','-Rollback'"
  exit $?
fi

if [ -n "${DRY:-}" ]; then
  WIN_EXE="C:\\Users\\$WIN_USER\\AppData\\Local\\Temp\\gateway-dryrun.exe"
  cp "$EXE" "$WIN_TMP/gateway-dryrun.exe"
else
  WIN_EXE="C:\\Users\\$WIN_USER\\AppData\\Local\\Temp\\gateway-new.exe"
  cp "$EXE" "$WIN_TMP/gateway-new.exe"
fi

echo ">>> 部署 $EXE"
ARGS="'-NoProfile','-ExecutionPolicy','Bypass','-File','$WIN_PS1_PATH','-Exe','$WIN_EXE'"
if [ -n "${DRY:-}" ]; then
  ARGS="$ARGS,'-DryRun'"
fi
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command \
  "Start-Process powershell -Verb RunAs -Wait -ArgumentList $ARGS"
