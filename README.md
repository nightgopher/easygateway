# EasyGateway

个人 AI 基础设施网关，用 Go 编写（前身 litellm-gateway / LLM Gateway）：把所有 AI 应用接到一个统一入口，按模型能力调度多个提供商，并提供原生桌面管理端、账号渠道、技能市场、记忆与常驻助理。对外支持 OpenAI Chat Completions、Anthropic Messages、OpenAI Responses API 三套协议，自动 fallback 与熔断恢复。

## 核心功能

- **统一接入**：一个端点、一套密钥接入 Claude Code、Codex CLI、Pi 等所有 AI 应用；三协议兼容，支持流式、工具调用与多模态。
- **能力调度**：按 text/vision 等能力选模型，多级模型链自动 fallback，健康检查与熔断恢复，请求级追踪与聚合指标。
- **账号渠道**：桌面端登录上游账号（OAuth/授权码），跨 AI 客户端复用已有订阅额度，支持自动续期与熔断联动。
- **技能中心**：custom-skills 仓库清单同步（符号链接落盘），远端技能市场浏览、搜索、安装与卸载，全局与项目两级范围。
- **记忆与助理**：跨客户端个人记忆层（candidate/active 治理、密钥守门）与流式常驻助理。
- **归档与导出**：可控的完整对话归档（脱敏、限长、保留期清理），JSONL 增量导出供知识库消费。
- **原生桌面端**：MyGo Go 原生控件的管理端，管理本机与远程网关，无 WebView、无 Node.js。

## 一键安装

### Linux / macOS

```bash
curl -fsSL https://raw.githubusercontent.com/hwj123hwj/litellm-gateway/main/scripts/install.sh | bash
```

### Windows (CMD)

```cmd
curl.exe -fsSL -o "%TEMP%\llm-gateway-install.bat" https://raw.githubusercontent.com/hwj123hwj/litellm-gateway/main/scripts/install.bat && call "%TEMP%\llm-gateway-install.bat"
```

### Windows (PowerShell)

```powershell
$installer = Join-Path $env:TEMP 'llm-gateway-install.ps1'; Invoke-WebRequest -UseBasicParsing https://raw.githubusercontent.com/hwj123hwj/litellm-gateway/main/scripts/install.ps1 -OutFile $installer; powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File $installer
```

安装脚本会自动完成：下载二进制 → 配置 PATH → 引导填入 API Key → 完成。
Windows 请直接在 CMD 或 PowerShell 中运行对应命令，不需要 WSL 或 Bash。配置向导可逐个启用多个供应商，API Key 会掩码输入。重复运行会自动去除 PATH 中的重复安装项，并可在保留 Master Key 和自定义变量的前提下重新配置供应商。

### Debian / Ubuntu (.deb)

Debian、Ubuntu 及其衍生发行版可以直接安装 Release 附带的 .deb 包（提供 amd64 / arm64 两个架构）：

```bash
# 从最新 Release 下载并安装服务端（自动注册 systemd 服务并生成随机 Master Key）
curl -fsSL -o easygateway-server.deb https://github.com/hwj123hwj/litellm-gateway/releases/latest/download/easygateway-server_0.3.10-1_amd64.deb
sudo dpkg -i easygateway-server.deb || sudo apt-get install -f
```

> 注意：上面的文件名含版本号，请到 [Releases](https://github.com/hwj123hwj/litellm-gateway/releases/latest) 按架构选择对应文件。从 v0.3.7 起服务端包名为 `easygateway-server`，与桌面客户端的 `easygateway` 包相互独立，可同时安装；从 v0.3.6 及更早的服务端包（旧名 `easygateway`）升级时，dpkg 会自动迁移到新包名。

安装后的布局：

| 路径 | 说明 |
|------|------|
| `/usr/bin/gateway` | 网关二进制 |
| `/etc/easygateway/providers.yaml` | Provider 配置（升级时保留本地修改） |
| `/etc/easygateway/easygateway.env` | 环境变量（首次安装自动生成，含随机 Master Key，填入 API Key 后重启服务） |
| `/var/lib/easygateway` | 运行数据（SQLite、技能同步等） |
| `easygateway.service` | systemd 服务，监听 `:4001` |

```bash
sudo systemctl status easygateway      # 查看服务状态
sudo systemctl restart easygateway     # 修改配置后重启
sudo dpkg --purge easygateway-server   # 卸载并清除全部数据
```

### 桌面端 (Linux .deb)

桌面管理端从 v0.3.7 起提供 Linux 包（amd64 / arm64），安装后出现在应用菜单，命令行入口为 `easygateway-desktop`。桌面端只管理网关，不自带网关进程；运行需要 GTK3 与 WebKitGTK 4.1（包依赖已声明）：

```bash
curl -fsSL -o easygateway-desktop.deb https://github.com/hwj123hwj/litellm-gateway/releases/latest/download/easygateway_0.3.10_amd64.deb
sudo dpkg -i easygateway-desktop.deb || sudo apt-get install -f
```

偏好用户级安装（免 root，装到 `~/.easygateway`）可下载 `easygateway-desktop-install-linux-<架构>.sh` 运行；首次打开按界面提示填写网关地址与 Admin Token（本机网关默认 `http://127.0.0.1:4001`）。卸载：`sudo dpkg --purge easygateway`。

## 快速启动

### 本地开发

```bash
cd go-gateway

# 创建 .env
cp .env.example .env
# 编辑 .env，填入 API keys

# 编译并运行
go build -o gateway . && ./gateway
```

网关默认监听 `:4001`。

网关只提供 HTTP API，不再提供浏览器管理页面或静态资源。通过 `GET /health` 检查服务状态，使用原生桌面端管理本机或远程网关。

### 桌面管理端

`desktop/` 使用 MyGo 的 Go 原生控件实现桌面管理界面，提供模型、路由、Provider、账号渠道、请求日志、技能库与技能市场、记忆和助理管理，可保存并切换本机和远程网关连接。桌面端无需 React、WebView 或 Node.js 构建。它管理已有网关，不替换正在运行的服务。构建、连接与安装说明见 [desktop/README.md](desktop/README.md)。

### 更新本机 macOS 网关

维护本机 `local.go-gateway` LaunchAgent 时，在仓库根目录运行：

```bash
bash scripts/deploy-local-gateway.sh
```

脚本要求当前分支包含本地已获取的 `origin/main`；更新源码后运行 `git fetch origin main` 再部署。它会构建 Go 二进制，在隔离端口及临时数据库中检查缓存统计 API、记忆/助理接口，以及旧浏览器入口已移除；检查通过后才切换 LaunchAgent，并确认 4001 端口由候选二进制提供服务。失败时会恢复之前的 LaunchAgent 配置并重新启动旧版本。每个版本目录记录源码提交、变更摘要和二进制校验和。新增关键功能或接口时，应同步扩展候选检查项。

脚本只更新网关服务，不安装桌面端。它沿用 LaunchAgent 的工作目录及其中 `.env`、Provider 配置和数据；从当前工作副本构建，本地改动也可能进入二进制，部署前须核对。候选目录的 `deploy-info.txt` 记录提交、工作树状态和摘要，`go version -m <二进制>` 可核对来源提交。隔离预检不调用真实模型，上线后仍需验证流式、取消和助理工具；`/health` 与 `/readyz` 成功不代表上游账号额度充足。桌面更新见 [安装说明](desktop/README.md#macos-安装与发布)。

## 配置 Pi

安装并启动本地网关后，执行：

```bash
llm-gateway setup pi
```

该命令会合并更新 `~/.pi/agent/models.json`，注册 `llm-gateway` 自定义 Provider 和网关模型。它不会覆盖其他 Pi Provider，也不会复制网关主密钥；Pi 会在每次请求时通过 `llm-gateway auth print-master-key` 从网关自己的 `.env` 读取认证信息。

可先预览变更，或为远程网关指定地址：

```bash
llm-gateway setup pi --dry-run
llm-gateway setup pi --endpoint https://gateway.example.com/v1
```

然后在 Pi 中运行 `/model` 并选择 `llm-gateway/coding`。

### 添加新提供商

无需修改代码！只需编辑 `providers.yaml` 文件：

```yaml
providers:
  # 添加新的 OpenAI 兼容提供商
  - name: my-provider
    type: openai  # 或 anthropic
    url: https://api.example.com/v1/chat/completions
    api_key_env: MY_PROVIDER_API_KEY  # 环境变量名
    models:
      - id: my-model                # 模型名即上游模型 ID，不设别名
        capabilities: [text]        # 按需声明能力
```

然后在 `.env` 中添加 API key：

```
MY_PROVIDER_API_KEY=sk-xxx
```

重启 gateway 即可使用新模型。

### 服务器部署

本机部署由用户显式运行脚本；推送 `main` 或提交 PR 不会直接重启服务。Debian/Ubuntu 服务器推荐安装 Release 附带的 .deb 包（见上文），由 systemd 托管常驻运行。已安装迷你主机更新器的主机则由 timer 每 10 分钟检查 main 的成功 CI 产物，校验并通过健康检查后自动切换；未安装更新器的主机不会更新。版本标签触发 Release 构建发布，详见 [服务器部署](go-gateway/README.md#服务器部署)。

版本线自 `v0.1.0` 重新开始（2026-10-10，产品更名 EasyGateway）；旧 `v1.x` 标签与 Release 已移除，迷你主机更新器只部署与 main 一致的最新构建，不受影响。

## 对外接口

### OpenAI 兼容

- Base URL: `http://localhost:4001/v1`（本地）
- Chat Completions: `POST /v1/chat/completions`
- Responses API: `POST /v1/responses`（Codex CLI 专用）

### Anthropic 兼容

- Base URL: `http://localhost:4001/v1`（本地）
- Messages: `POST /v1/messages`

### System One 结构化判断（Jev）

`POST /v1/systemone` 接收 `model`、`state` 和 `questions`，原样返回结构化 `answers`、概率与 usage。问题可组合 `noul`（是／否概率）、`choice`（分类）和 `score`（评分）；Jev 不用于 Chat Completions、Messages 或 Responses。

`jev-1.13-free` 可使用上游匿名免费额度；`jev-1.13` 需要在网关 `.env` 中设置工作区 `OPENCODE_API_KEY` 并重启。OpenCode 渠道设备码 OAuth 不能替代此 Key。模型目录以 `protocol: systemone`、`capabilities: [systemone]` 标记；缺少付费 Key 时仍保留目录并说明不可用原因。免费额度耗尽返回真实 `429`，不自动切换到付费模型。

使用 [Jev Demo](https://hwj123hwj.github.io/jev-demo/) 或参阅 [请求示例](go-gateway/README.md#system-one结构化判断)。

## 支持的模型

### ChatGPT Codex（OAuth，可选）

使用 ChatGPT Plus/Pro 订阅的 OAuth token 直接调用，不需要额外 API key。

本机存在 Codex/Pi OAuth token 后，ChatGPT Codex 模型会动态加入 `/v1/models`；代理是可选的，模型版本以运行时目录为准，不在客户端固定清单。

可用凭证来源：先运行 `codex login`，或在 Pi 中执行 `/login openai-codex`。网关自动读取 `~/.codex/auth.json` 和 `~/.pi/agent/auth.json`，也可以用 `CHATGPT_AUTH_FILE` 指定路径。

这些订阅模型通过原生 `/v1/responses` 流式接口调用，适合 Codex/Pi；模型列表以 `/v1/models` 返回结果为准。

### GitHub Copilot

通过账号渠道完成授权后，使用网关返回的模型 ID；授权成功与模型调用成功需分别验证。可用模型以运行时目录为准。

### 外部提供商

Provider、能力与 `coding` 降级链的配置见 [providers.yaml](go-gateway/providers.yaml)；启用状态、凭据和动态账号模型共同决定 `GET /v1/models` 的运行时结果。客户端可以调用具体模型 ID，也可以调用配置中的路由链；图片等多模态请求会按能力过滤上游。模型命名、重试边界与 Provider 集成说明见 [运行时文档](go-gateway/README.md#可用模型)。

## 助理与流式排错

启用 `ASSISTANT_ENABLED` 并配置 `ASSISTANT_MODEL` 后，桌面助理可按次选择 `/admin/assistant/models` 中的聊天模型，使用 `gateway_status` / `gateway_logs` 查询运行状态。选择模型不改写服务默认模型；额度耗尽或 429 时可选择其他已授权模型，具体额度需按上游错误与重置时间判断。

流式连接返回 HTTP 200 后仍可能失败。缺少上游终态或收到错误帧时不补造成功结束；客户端取消会停止上游，并按中断记录请求。助理保留已经显示的文本，等待 SDK 本轮收尾后允许继续请求；桌面「取消」后可再次发送。API 和事件契约见 [助理接口](go-gateway/README.md#助理接口与验证) 与 [记忆和助理设计](docs/MEMORY_DESIGN.md)。

## 配置 Claude Code

编辑 `~/.claude/settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:4001/v1",
    "ANTHROPIC_AUTH_TOKEN": "你的 LITELLM_MASTER_KEY"
  }
}
```

## 示例请求

### OpenAI 风格

```bash
curl -X POST http://localhost:4001/v1/chat/completions \
  -H "Authorization: Bearer sk-xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"coding","messages":[{"role":"user","content":"hi"}]}'
```

### Anthropic 风格

```bash
curl -X POST http://localhost:4001/v1/messages \
  -H "Authorization: Bearer sk-xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"coding-anthropic","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}'
```

## 项目结构

```text
litellm-gateway/
├── go-gateway/              # Go 网关（主项目）
│   ├── main.go              # 启动入口
│   ├── internal/
│   │   ├── provider/        # 提供商、能力路由、熔断与探测
│   │   ├── channel/         # 账号渠道：登录、账号与续期
│   │   ├── handlers/        # Admin API 与三协议入口
│   │   ├── skills/          # 技能清单、同步与技能市场面板
│   │   ├── memory/          # 个人记忆层
│   │   ├── assistant/       # 常驻助理（EasyAgent SDK）
│   │   ├── archive/         # 对话归档与增量导出
│   │   ├── auth/            # Bearer token 认证
│   │   └── middleware/      # request ID、日志与指标中间件
│   ├── .env.example         # 环境变量模板
│   ├── Dockerfile
│   └── README.md            # 完整文档
│
├── desktop/                 # MyGo 原生 Go 桌面管理端
├── scripts/                 # 配套工具脚本
│   └── codex-model          # Codex CLI 模型切换
├── .github/workflows/       # GitHub Actions CI/CD
├── docs/                    # 补充设计文档
└── README.md                # 本文件
```

## 文档

| 文档 | 说明 |
|------|------|
| [PRD.md](PRD.md) | 产品需求、范围和核心验收 |
| [AGENTS.md](AGENTS.md) | 项目开发准则 |
| [go-gateway/README.md](go-gateway/README.md) | Go 网关完整文档（架构、模型列表、部署） |
| [desktop/README.md](desktop/README.md) | 原生桌面端构建、连接与安装 |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 纯 Go 架构总览与 TS → Go 迁移记录 |
| [docs/MEMORY_DESIGN.md](docs/MEMORY_DESIGN.md) | Agent 长期记忆层设计 |
| [docs/knowledge-flywheel.md](docs/knowledge-flywheel.md) | 知识飞轮与归档导出接入手册 |

## 环境变量

| 变量 | 必填 | 说明 |
|------|------|------|
| `LITELLM_MASTER_KEY` | 是 | 网关认证 token |
| `GLM_API_KEY` | 否 | 智谱 API key |
| `CLIPROXY_API_KEY` | 否 | 本机 CLIProxyAPI（Antigravity 反代）的静态 api-key |
| `COPILOT_TOKEN` | 否 | GitHub Copilot token（短期有效，约 30 分钟） |
| `COPILOT_GITHUB_TOKEN` | 否 | GitHub OAuth token（用于自动刷新 Copilot token） |
| `HTTP_PROXY` | 否 | ChatGPT Codex 的可选 HTTP 代理地址（如 `http://127.0.0.1:7890`） |
| `CHATGPT_AUTH_FILE` | 否 | ChatGPT/Pi OAuth `auth.json` 路径；默认自动查找两个标准路径 |
| `ADMIN_TOKEN` | 否 | 原生管理端/Admin API 独立 token；未设置时回退到主 token |
| `PORT` | 否 | 监听端口（默认 4001） |
| `SKILLS_REPO_PATH` | 否 | custom-skills 技能仓库路径；未设置时技能库面板显示未配置 |
| `SKILLS_MARKET_ENABLED` | 否 | 技能市场面板开关（默认开启） |
| `SKILLS_MARKET_BASE_URL` | 否 | 远端技能商城地址（默认官方站） |
| `SKILLS_MARKET_SKILLS_DIR` | 否 | 市场技能安装目录（默认 `~/.agents/skills`，与 EasyAgent 共用） |
| `ASSISTANT_ENABLED` | 否 | 常驻助理开关（默认 false） |
| `ASSISTANT_MODEL` | 否 | 助理默认模型 |
| `ASSISTANT_API_KEY` | 否 | 助理 API Key（默认复用 MASTER_KEY） |
| `ARCHIVE_ENABLED` | 否 | 对话归档总开关（默认 false）；启用后供知识库增量导出 |
| `ARCHIVE_MAX_BODY_KB` | 否 | 单条归档 body 安全上限（默认 16384 KB） |
| `ARCHIVE_RETENTION_DAYS` | 否 | 归档保留天数（默认 90） |
| `CIRCUIT_FAILURE_THRESHOLD` | 否 | 连续可重试失败后打开熔断（默认 3） |
| `CIRCUIT_RECOVERY_SECONDS` | 否 | 熔断恢复探测间隔（默认 30） |
| `CIRCUIT_SUCCESS_THRESHOLD` | 否 | 半开状态连续成功次数（默认 1） |

## 资源占用

- 内存：~18 MB
- 启动时间：<1 秒
- Docker 镜像：~50 MB

## 相关项目

- [EasyAgent](https://github.com/hwj123hwj/easyagent)（原 pi-go）— AI 编程搭档，写代码、搜知识、放音乐

## 展示页

访问 [hwj123hwj.github.io](https://hwj123hwj.github.io) 查看所有项目。

### 管理端支持范围

管理端只维护 `desktop/` 中的 MyGo 原生 Go 客户端。网关提供受认证保护的 Admin API；浏览器 Dashboard、静态原型及手机 App 已移除。
