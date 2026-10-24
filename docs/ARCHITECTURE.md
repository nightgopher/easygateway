# 纯 Go 架构总览

> 状态：当前架构（2026-10）。运行代码全部为 Go，仓库不含 TypeScript、JavaScript、HTML/CSS、Node.js 构建链或 WebView 包装。
> 需求与范围见 [PRD.md](../PRD.md)，API 细节见 [go-gateway/README.md](../go-gateway/README.md)，构建与连接见 [desktop/README.md](../desktop/README.md)。

## 1. 系统组成

```text
Claude Code / Codex CLI / Pi / 飞书等客户端
   │  OpenAI Chat Completions · Responses · Anthropic Messages（:4001）
   ▼
┌─────────────────────────────┐        Admin API（Bearer）            ┌──────────────────────────┐
│ go-gateway（Go 运行时）      │ ◀─────────────────────────────────── │ desktop（MyGo 原生客户端）│
│ 路由 · fallback · 熔断       │                                       │ 原生控件 UI，无 WebView    │
│ 指标 · 归档 · 记忆 · 助理     │                                       └──────────────────────────┘
└──────────┬──────────────────┘
           ▼
   GLM / Antigravity / DeepV / Copilot / ChatGPT OAuth 等上游 Provider
```

两个可执行单元：

- **go-gateway**：无状态要求低、单二进制 HTTP 服务，是数据面（三种协议转发）和管理面（Admin API）的唯一提供者；
- **desktop**：MyGo 原生桌面客户端，只通过 Admin API 管理网关，不启动、不替换、不内嵌网关服务。

Admin API 是两者之间唯一的契约边界：网关不感知客户端的存在，客户端也不读网关的本地文件或数据库。

## 2. go-gateway 内部结构

| 包 | 职责 |
|---|---|
| `config` | 加载 `providers.yaml` 中的 Provider、模型、能力和 fallback 链；运行时还可注册账号渠道模型 |
| `provider` | 上游实现；OpenAI 兼容参考 `openai.go`，Anthropic 兼容参考 `anthropic.go` |
| `handlers` | 三种协议入口、模型目录、路由与 Admin API 的 HTTP 处理器 |
| `auth` | Bearer 认证；管理接口接受 Admin Token 或 Master Key，普通模型接口不接受独立 Admin Token |
| `middleware` | request ID、指标采集、请求日志 |
| `requestmeta` | 请求关联元数据契约（request ID ↔ 最终 Provider 与每次尝试） |
| `metrics` | 轻量指标聚合（缓存统计、命中率、Provider 健康） |
| `storage` | SQLite 持久化（指标、归档、记忆、设置同文件分表） |
| `archive` | 对话归档：脱敏、终态记录、游标增量导出（`ARCHIVE_ENABLED` 独立开关） |
| `memory` | Agent 长期记忆：作用域路由、candidate/active 治理（见 [MEMORY_DESIGN.md](MEMORY_DESIGN.md)） |
| `assistant` | 常驻助理：EasyAgent SDK，LLM 调用回环走网关自身 |
| `skills` | custom-skills 技能市场的清单管理与符号链接同步 |
| `piconfig` / `zcodeconfig` / `dshconfig` | 把网关模型清单同步进 Pi / ZCode / deepseek-harness 的客户端配置 |

数据红线（AGENTS.md）：Provider API Key 不进响应、日志、Admin API；Authorization、Token、密码永不落库；归档默认脱敏且有保留期。

### 流式与关闭边界

三种协议共用首输出、空闲与 fallback 等待策略。只有收到协议终态才确认流式成功；缺少终态的 EOF、显式错误均保留中断状态，已经输出后不再拼接其他 Provider。HTTP 200 不能覆盖真实错误，指标和归档按最终结果记录。客户端取消停止上游，不计入 Provider 熔断失败，但请求日志仍记录中断。

收到退出信号后先排空 HTTP 请求（最多 15 秒），再使用共同的 15 秒窗口收尾指标、归档与 SQLite；超时记录未完成步骤。助理消费 EasyAgent SDK 事件直到 channel 关闭，保留部分回复与原错误，再释放本轮以便重试。依赖版本以 [go.mod](../go-gateway/go.mod) 为准。

固定对照 ZCode `ba61ca16`，参考其业务错误优先、输出后禁止重放、退出等待清理和共同 deadline 原则；缺终态 EOF 检测、Go HTTP 排空与 SQLite 写入由本项目实现。源码对照与差异见 [PR #93](https://github.com/nightgopher/litellm-gateway/pull/93)；SDK 升级及助理适配见 [PR #94](https://github.com/nightgopher/litellm-gateway/pull/94)。

## 3. desktop 内部结构

| 文件/包 | 职责 |
|---|---|
| `main.go` | 窗口、托盘常驻、单实例锁、快捷键、窗口状态持久化 |
| `internal/connection` | 连接配置存储（`connections.json`，0600）与 Admin API 客户端（TLS 正常验证、不跟随重定向） |
| `internal/console` | 全部界面：连接管理、概览、模型与路由、Provider、日志、技能、记忆、助理、设置 |

界面约定：所有耗时操作走 `App.work/launch`（后台 goroutine + UI 线程回写 + 序列号防串台）；导航与切换主机会取消在途请求并清空旧主机数据；列表按可见行虚拟化。

## 4. TS → 纯 Go 迁移记录

浏览器 Dashboard（React/Vite + Node 构建链 + WebView 桌面壳）已于 2026-10-09 分三步移除，全部合入 `main`：

1. `5a44afe` — 新增 MyGo 原生桌面客户端（与 WebView 壳并存）；
2. `c613d60` — 桌面端从 WebView 切换为原生 Go UI；
3. `1b024f8` — 删除浏览器 Dashboard、前端工具链与嵌入静态资源。

防回退措施：

- `scripts/deploy-local-gateway.sh` 部署预检断言旧浏览器路由（`/`、`/dashboard`、`/assets/index.js` 等）返回 404、未认证返回 401；
- CI（`ci.yml`、`desktop.yml`）只安装 Go 工具链，不再有前端构建与 Dashboard 嵌入校验；
- AGENTS.md 明确不再新增 TypeScript/JavaScript/HTML/CSS/Node.js 构建链或 WebView 包装。

## 5. 部署形态

- 本机 macOS：`scripts/deploy-local-gateway.sh` 维护 LaunchAgent（隔离端口预检 → 原子切换 → 失败回滚）；
- 迷你主机 Linux：systemd 服务 + timer 每 10 分钟拉取 CI 发布的校验产物，健康检查失败自动回退；
- 服务器 / 其他设备：`scripts/install.sh|bat|ps1` 一键安装，版本标签触发 Release；
- 推送 main 只触发 CI，不直接连接或重启主机；已安装更新器的迷你主机由自己的 timer 自动更新。本机脚本与首次安装仍需显式运行。

## 6. 命名与版本

- **产品名**：EasyGateway（2026-10-10 起），前身 litellm-gateway / LLM Gateway；
- **版本线**：自 `v0.1.0` 重新开始，旧 `v1.x` 标签与 Release 已移除；
- **兼容标识**：安装脚本默认仍使用 `gateway` / `llm-gateway`、`~/.llm-gateway`；`LLM_GATEWAY_HOME` 可覆盖安装根目录。仓库迷你主机安装器默认服务名为 `llm-gateway.service`，已迁移主机可使用不同单元与目录，须核实实际服务和更新器配置。macOS 脚本维护 `local.go-gateway`；`LITELLM_*` 环境变量和客户端 Provider 配置保留兼容，不依据产品改名擅自重命名运行入口；
- **未迁移项**：GitHub 仓库名仍为 `litellm-gateway`，安装脚本 URL 与 Go module 路径随之保留。
