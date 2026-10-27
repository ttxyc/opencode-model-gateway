#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p internal/installer/assets/scripts
cp config.example.json publickey.txt opencode-gateway.vbs internal/installer/assets/
cp scripts/install.ps1 scripts/install-wsl.ps1 internal/installer/assets/scripts/
