# gateway 1.2.2 部署与回滚

## 部署

```bash
cd /mnt/d/wsl-opencode/opencode-model-gateway
./scripts/deploy.sh --dry-run    # 先看要做什么，不改服务
./scripts/deploy.sh              # 实际部署（会弹 UAC）
```

脚本做的事：

1. 把 `dist/gateway.exe` 复制到 `C:\Users\12053\AppData\Local\Temp\gateway-new.exe`
2. 提权调用 `deploy-1.2.2.ps1`
3. 留一份版本化备份 `app\gateway.exe.bak-<时间戳>-<旧版本>`
4. 调用内置 `--apply-update`（停服 → 原名另存 `.old` → 替换 → 启动 → 健康检查 → 失败自动回滚）
5. 检查 `/healthz` 版本号
6. 端到端自检：请求一次 WorkBuddy，断言 `tool_calls:[]` 等空字段已消失、`[DONE]` 正常

## 三层回滚

### 第 1 层：`--apply-update` 自动回滚（免费获得的）

新版启动后健康检查连续 20 次（约 10s）失败，`platform.ApplyUpdate` 会自动：
停服 → 删除新 exe → 把 `.old` 改回来 → 启动 → 返回 `"new version failed health check; rolled back"`。
**这一层不需要你做任何事。**

### 第 2 层：脚本回滚（推荐）

```bash
cd /mnt/d/wsl-opencode/opencode-model-gateway
./scripts/deploy.sh --rollback
```

做的事：停服 → 把当前 exe 另存为 `gateway.exe.rollback-<时间戳>`（便于再次前进）→
用 `gateway.exe.old` 覆盖 → 启动 → 健康检查。

注意 `gateway.exe.old` 只保留**上一个**版本；连续升级两次后它就变成 1.2.2 了。

### 第 3 层：手工指定任意历史版本

脚本每次部署都会留 `gateway.exe.bak-<时间戳>-<版本>`，可以回到任意一个：

```bash
ls -l /mnt/c/ProgramData/opencode-model-gateway/app/
```

然后手工操作（管理员 PowerShell）：

```powershell
sc.exe stop opencode-model-gateway
Copy-Item "C:\ProgramData\opencode-model-gateway\app\gateway.exe.bak-20261105-235000-1.2.1" `
          "C:\ProgramData\opencode-model-gateway\app\gateway.exe" -Force
sc.exe start opencode-model-gateway
Invoke-WebRequest http://127.0.0.1:8788/healthz -UseBasicParsing
```

## 验证当前状态

```bash
curl -s http://opencode-gateway.local:8788/healthz
```

关注 `version` 字段和 `uptimeSec`（刚重启应是个小数字）。

## 已知坑

1. **`-File` 不能传 `\\wsl$` 路径**，必须先复制到 `C:\...`。`deploy.sh` 已处理。
2. **PowerShell 5.1 读无 BOM 的 UTF-8 会按 ANSI 解析**，中文注释会导致脚本被截断（本次实测：无 BOM 时只能解析出 1 个函数，加 BOM 后 4 个）。`deploy.sh` 复制时会自动补 BOM。
3. **`sc.exe` 而非 `Stop-Service`**：服务是 LocalSystem，用 `sc.exe` 少踩权限坑。
4. **UAC**：脚本必须提权，`deploy.sh` 用 `Start-Process -Verb RunAs` 弹窗，需手动确认。

## 如果 1.2.2 没解决问题

SSE 清洗只删空字段。若部署后 opencode 里 reasoning 块**仍然**刷屏（part 数仍为 N），
说明根因在 opencode 的 reasoning part id 合并逻辑，而不是空字段。
此时需要做"方案二"：在 `sseScrubber` 里把连续的 `reasoning_content` 合并成单帧。

解析骨架（`sseScrubber.Write`）已经写好且测试覆盖，加合并逻辑只需在
`scrubChunk` 之前插入一个按 `reasoning_content` / `content` / `tool_calls` 分组的缓冲层。
先回滚到 1.2.1，再改。
