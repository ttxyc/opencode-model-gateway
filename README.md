# opencode Model Gateway（Windows 原生服务）

Go 单文件实现的 OpenAI 兼容本地网关，Windows 10/11 x64，作为真正的 Windows 服务运行；支持 ModelScope / Cline / Atria / AMD / DeepSeek 多 key 路由、余额选择、失败切换、内置管理页面和签名自更新。

## 特性

- 原生 Windows 服务 `opencode-model-gateway`，services.msc 可设 **自动 / 手动 / 禁用**
- 单 exe 约 7~10MB，空闲 RSS 约 20~35MB；无 Node/NSSM/.NET/WSL 依赖（WSL 仅作为客户端）
- 开机自启、SCM failure 自动重启、进程内 `/healthz` watchdog
- `C:\ProgramData\opencode-model-gateway` 统一数据目录；key 与 token 分离 ACL
- `/admin` 内置网页：选择服务商、校验并添加/删除 key、余额/冷却/耗尽状态、更新
- 桌面快捷方式：`opencode Gateway.lnk`，双击打开管理页，服务停止时尝试启动
- 签名自更新：GitHub Release + Ed25519，升级无需管理员
- WSL 通过 `opencode-gateway.local` 访问（hosts 别名，无额外常驻进程）

## 构建

需要 Go 1.24+；**Windows 交叉编译必须显式设置 `GOOS=windows`**，否则生成的 .exe 实际是 ELF，Windows 报“不是有效的 Win32 应用”：

```bash
go test ./...
GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=1.1.1" \
  -o dist/gateway.exe ./cmd/gateway
```

本机没有 Go 时可用 Ubuntu 包解压（无需 root）：

```bash
apt-get download golang-1.26-go golang-1.26-src
dpkg-deb -x golang-1.26-go_*.deb /tmp/go
dpkg-deb -x golang-1.26-src_*.deb /tmp/go
export PATH=/tmp/go/usr/lib/go-1.26/bin:$PATH
```

## 发行包布局

```text
opencode-gateway-win-x64/
├─ gateway.exe
├─ config.example.json
├─ opencode-gateway.vbs
├─ publickey.txt
├─ README.md
└─ scripts/
   ├─ install.ps1
   ├─ install-wsl.ps1
   ├─ uninstall.ps1
   └─ upgrade.ps1
```

## 安装（双击安装，仅一次管理员）

1. 从 GitHub **Releases** 下载 `opencode-gateway-win-x64.zip` 并解压（不要用仓库 Code → Download ZIP；源码包没有 exe）
2. 双击 `gateway.exe`
3. 首次会弹一次 UAC；允许后自动安装服务
4. 安装完成会自动打开 `http://127.0.0.1:8788/admin`
5. 用 `C:\ProgramData\opencode-model-gateway\config\admin.token` 登录
6. 在页面「导入 key 配置」选择旧的 `quota-line.json`，或直接粘贴 JSON

服务已安装时，双击 `gateway.exe` 只打开管理页，不会重装。首次安装也可以继续用脚本：

```powershell
cd <解压目录>\scripts
.\install.ps1 -QuotaLinePath "C:\path\to\quota-line.json"
```

参数：

- `-QuotaLinePath`：从现有 `quota-line.json` 导入 key；不传则交互输入
- `-ClientTokenFile`：复用旧网关 token（迁移时避免所有工具改 token）
- `-UpdateProxy`：只给服务设置 GitHub 更新用的代理（如 `http://127.0.0.1:10808`）；不填会自动读取当前用户 Internet 代理
- `-NoUpdateProxy`：不设置更新代理（GitHub 可直连时用）
- `-NoKeyPrompt`：不交互输入 key（双击安装默认使用；首次无 key，安装后页面导入）
- `-SkipWSL`：不配置当前 Ubuntu 的 hosts 别名
- `-SkipFirewall`：不创建 WSL 网段防火墙规则

安装脚本会：

1. 创建 `C:\ProgramData\opencode-model-gateway`
2. 生成 client/admin token，写 ACL
3. 注册服务并启动，验证 `/healthz`
4. 添加 WSL 网段防火墙规则
5. 创建 Public Desktop + 当前用户桌面快捷方式
6. 配置当前 Ubuntu：`opencode-gateway.local` hosts 别名 + `[boot] command`
7. 把 WSL 里 `opencode.jsonc` 的 `127.0.0.1:8788` 改成 `opencode-gateway.local:8788`

token 位置：

- 客户端：`C:\ProgramData\opencode-model-gateway\config\client.token`
- 管理员：`C:\ProgramData\opencode-model-gateway\config\admin.token`

## 服务管理

```powershell
sc.exe query opencode-model-gateway
sc.exe start opencode-model-gateway
sc.exe stop opencode-model-gateway
sc.exe config opencode-model-gateway start= auto|demand|disabled
```

或直接 `services.msc`：

- **自动**：开机启动；工具调用不会触发启动
- **手动**：不自动启动；需手动启动（快捷方式/服务管理器）
- **禁用**：不能启动

## 管理页面

`http://127.0.0.1:8788/admin`

- 首次输入 `admin.token`
- 选择已有服务商（modelscope/cline/atria/amd/deepseek）
- 粘贴 key → 调平台余额/模型接口校验 → 保存 → 热加载
- **导入 key 配置**：选择 `quota-line.json` 或粘贴 JSON；默认合并追加、不覆盖已有 key，可勾选覆盖
- 删除 key、查看余额/冷却/耗尽
- 检查/安装更新

普通工具使用 `client.token` 调用 `/v1`，不能进入管理页。

## 客户端接入

统一入口：

```text
Base URL: http://127.0.0.1:8788/v1
API Key:  client.token
Model:    modelscope/deepseek-ai/DeepSeek-V4.1-Flash-1017
```

单平台入口（裸模型名）：

```text
http://127.0.0.1:8788/p/modelscope/v1
```

WSL 内：

```text
http://opencode-gateway.local:8788/...
```

## 自更新

- Release 资产：`gateway.exe`、`latest.json`、安装 zip
- 服务启动/每 24h/页面点击检查
- 校验 `latest.json` 的 Ed25519 签名与 exe SHA256
- 停服务 → 替换 exe → 启动 → `/healthz`；失败自动回滚旧 exe
- 签名私钥在 GitHub Actions Secret `GATEWAY_SIGNING_KEY`（值就是本机 `gateway-signing.key` 的内容；不要入库/贴聊天）；公钥在 `publickey.txt`
- 发布：在 GitHub 仓库 Settings → Secrets and variables → Actions 添加 `GATEWAY_SIGNING_KEY`，然后打 tag `gateway-v1.1.1`；CI 会构建、签名并发布 Release

## 迁移当前 WSL 网关

1. 用当前 `gateway.json` 的 token 作为 `-ClientTokenFile`
2. 用当前 `quota-line.json` 作为 `-QuotaLinePath`
3. 安装 Windows 服务
4. 确认 `http://127.0.0.1:8788/healthz`（Windows）正常
5. 停止 WSL 网关 + 移除 Startup VBS
6. 重启 opencode（WSL 会走 `opencode-gateway.local`）
7. DSH 不用改 token/端口，Windows 127.0.0.1 自动指向新服务

回滚：卸载服务，恢复 WSL 网关/VBS/配置。

## 安全

- 服务账户 LocalSystem；平台 key 仅 SYSTEM/Administrators 可读
- client token 所有用户可读（工具需要）；admin token 仅安装者/管理员
- 管理 API 只接受本机回环 + admin token
- 更新必须通过 Ed25519 签名校验
- 不修改其他服务、PATH、系统代理；只占用 8788
