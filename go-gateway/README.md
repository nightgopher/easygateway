# go-gateway

EasyGateway 的网关运行时：轻量级个人 AI 基础设施网关，用 Go 编写。它统一 OpenAI Chat/Responses 与 Anthropic Messages 入口，按模型能力选择 Provider，并提供 fallback、熔断、指标和管理 API。

Provider、模型和路由以本目录的 [`providers.yaml`](providers.yaml) 为准；客户端和文档不维护另一份静态模型清单。运行中的实际目录请以 `GET /v1/models` 为准。

## 快速启动

### 前提条件

- Go 1.21+（编译时需要，运行时不需要）
- 至少一个提供商的 API key

### 1. 配置 .env

```bash
cp .env.example .env
# 编辑 .env，填入你的 API keys
```

`.env` 最小配置：

```env
LITELLM_MASTER_KEY=sk-local-gateway-xxx   # 必填，网关认证 token
GLM_API_KEY=your_glm_key                  # 至少填一个 provider
PORT=4001
```

### 2. 编译并运行

```bash
go build -o gateway .
./gateway version
./gateway
```

或使用 Makefile：

```bash
make run
```

网关仅提供 HTTP API，不携带或提供浏览器页面。使用 `GET /health` 检查状态，通过 [原生 Go 桌面端](../desktop/README.md) 管理连接、模型、路由、日志和助理。

## System One：结构化判断

Jev 使用独立的 JSON 协议：`POST /v1/systemone`（兼容别名 `/systemone`）。网关按模型路由、绑定上游模型名、记录轻量指标，并保留原生答案和扩展字段。它不把判断结果包装成聊天文本；聊天入口会返回 400 并提示正确端点。

```bash
curl http://127.0.0.1:4001/v1/systemone \
  -H "Authorization: Bearer $LITELLM_MASTER_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"jev-1.13-free","state":"支付连续失败，客户无法下单。","questions":{"urgent":{"type":"noul","instructions":"是否需要紧急处理？"}}}'
```

`questions` 可同时包含 `noul`、`choice`、`score`。`choice.criteria` 为选项对象，`score.criteria` 为从低到高的等级数组；结构化 state、instructions、criteria 和扩展字段保留给上游。请求最大 4 MiB，响应最大 8 MiB，支持取消；这是一次 JSON 请求，不支持 SSE 文本流。上游 401/403/429 和原生错误类型会保留，`Retry-After` 和上游请求 ID 通过响应头提供。

配置使用 `type: systemone`、完整原生端点 URL、`capabilities: [systemone]`。OpenCode 端点为 `https://opencode.ai/inference/systemone/v1/systemone`；有工作区 Key 时在受保护 `.env` 设置 `OPENCODE_API_KEY`，重启加载。付费档缺 Key 时保留目录但不发请求；免费档 `allow_no_key: true` 允许匿名额度，额度失败不会触发付费回退。它与 OpenCode 渠道设备码登录相互独立。

默认模型 doctor 仅报告配置状态，不调用 System One；管理员明确选择某个 Provider 探活时才发送原生最小判断请求。成功响应、协议测试成功和真实上游额度错误应分别记录，模型出现在目录中不代表推理已通过。

依据：[OpenCode Jev](https://opencode.ai/v2/docs/console/models/#jev)、[TypeSafe 协议](https://docs.typesafe.ai/introduction/quickstart)。

## 账号渠道登录

在原生桌面端的「账号渠道」中发起登录，由浏览器完成授权。账号令牌保存在网关主机的受保护凭据目录，桌面端不接收令牌明文。渠道是否需要额外客户端配置、是否支持匿名模型以及本机回调要求以 `GET /admin/channels` 返回值为准；缺少配置时按桌面端说明补齐并重启网关。

账号授权成功不代表上游模型有额度或可用，仍需实际调用验证。成功授权会重置该渠道对应 Provider 的熔断与历史探测状态，避免之前的缺少凭据错误阻止新账号调用。

TRAE 渠道在 macOS 上可自动识别 `/Applications` 或 `~/Applications` 下已安装的 TRAE CN 客户端，读取安装包 `product.json` 中的客户端标识与版本，无需手动填写额外标识。非标准路径可用 `TRAE_CHANNEL_CLIENT_PRODUCT` 指定该文件；环境变量与 `trae-client.json` 仍优先。此识别不导入 IDE 的账号令牌，仍由用户在浏览器中为网关完成授权。国际版配置不会用于国内渠道。

这些接口使用管理凭据鉴权：

| 请求 | 用途 |
|---|---|
| `GET /admin/channels` | 渠道、账号概要及登录前提；不返回令牌 |
| `POST /admin/channels/:channel/login` | 返回登录地址、授权码、有效期和 `login_id` |
| `GET /admin/channels/:channel/login/:id` | 查询 `pending` / `complete` / `failed` / `cancelled` 状态 |
| `DELETE /admin/channels/:channel/login/:id` | 取消此次授权，释放回调服务；不移除账号 |
| `DELETE /admin/channels/:channel/accounts/:index` | 移除指定账号凭据 |

## 配置 Pi

安装版网关可一键写入 Pi 的自定义模型配置：

```bash
llm-gateway setup pi
```

命令只更新 `~/.pi/agent/models.json` 中的 `llm-gateway` Provider，保留已有配置。默认端点会读取 `~/.llm-gateway/.env` 的 `PORT`，认证密钥不会写入 Pi 配置文件；Pi 使用 `llm-gateway auth print-master-key` 在请求时读取网关主密钥。

```bash
llm-gateway setup pi --dry-run
llm-gateway setup pi --endpoint https://gateway.example.com/v1
```

完成后在 Pi 的 `/model` 中选择 `llm-gateway/coding`。

### 3. 配置 Claude Code

#### Anthropic 兼容客户端

编辑 `~/.claude/settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:4001/v1",
    "ANTHROPIC_AUTH_TOKEN": "sk-local-gateway-xxx"
  }
}
```

#### OpenAI 兼容客户端

将 base URL 指向：

- `http://localhost:4001/v1`

聊天接口完整地址为：

- `http://localhost:4001/v1/chat/completions`

## 从 cc-switch 迁移

之前用 [cc-switch](https://github.com/farion1231/cc-switch) 管理 Claude Code / Codex 供应商的，可以直接把已有配置迁进网关：

```bash
llm-gateway migrate cc-switch --dry-run  # 只预览，不写任何文件
llm-gateway migrate cc-switch            # 执行迁移
llm-gateway migrate cc-switch --include "MyGLM,API Relay"   # 只迁指定条目
llm-gateway migrate cc-switch --home /path/to/.cc-switch    # 指定 cc-switch 目录
```

行为与边界：

- 兼容新版 cc-switch（v4+ 的 SQLite `~/.cc-switch/cc-switch.db`）与旧版 `config.json`；对 cc-switch 的数据只读，不做任何修改；
- 可迁移：claude 条目（Anthropic 协议与 OpenAI chat 协议，按 meta 的 apiFormat 区分）、codex 的 API-Key 中转（wire_api=chat）、openclaw 条目（自带模型清单）。转换为 providers.yaml 的 provider 与模型，API Key 以 `CCS_<名称>_API_KEY` 变量写入 `.env`，不在终端与 API 响应中回显；
- 跳过并说明原因：指向网关自身的条目、codex OAuth 订阅（网关 chatgpt 渠道直接读 `~/.codex/auth.json`，无需迁移）、Responses API 直连端点（暂不支持）、Gemini 协议条目（暂无原生 provider）、没有模型名的条目；
- 写入前备份原文件（`providers.yaml.pre-import.bak` / `.env.pre-import.bak`），已导入过的端点不会重复导入，providers.yaml 既有注释与顺序保持原样；
- 迁移的模型立即可路由（每个模型自带一条同名链），但不自动加入 `chains` 里的降级链，需要的话编辑 providers.yaml 后重启。

同样能力也有 Admin API：`GET /admin/ccswitch` 预览、`POST /admin/ccswitch/import` 执行（请求体 `{"include":["MyGLM"]}` 可只导指定条目），导入后即时生效、无需重启。

网关在远程主机（如迷你主机）而 cc-switch 装在本机时，桌面端走上传迁移：本机读出 cc-switch 配置后上传，解析与转换仍由网关完成。对应端点 `POST /admin/ccswitch/remote-scan`（预览）与 `POST /admin/ccswitch/remote-import`（执行），请求体为 `{"rows":[...]}`（新版 cc-switch.db 的 providers 行）或 `{"legacy_json":"..."}`（旧版 config.json 原文），可附 `include`。API Key 随管理信道（Bearer 主密钥）上传、仅写入网关 .env，响应与日志不回显；带 `loopback: true` 标记的条目是回环地址，仅在网关主机可达时有效。

---

## API 端点

| 端点 | 方法 | 认证 | 说明 |
|------|------|------|------|
| `/health` | GET | 无需 | 健康检查 |
| `/readyz` | GET | 无需 | 就绪检查；未加载任何 Provider 时返回 503 |
| `/v1/models` | GET | Bearer | 模型目录及健康注解；`?available_only=true` 过滤当前不可用项 |
| `/v1/chat/completions` | POST | Bearer | OpenAI 兼容接口，支持流式 |
| `/v1/messages` | POST | Bearer | Anthropic 兼容接口，支持流式 |
| `/v1/responses` | POST | Bearer | OpenAI Responses API，Codex CLI 专用，支持流式 |
| `/chat/completions` | POST | Bearer | `/v1/chat/completions` 的短路径兼容别名 |
| `/messages` | POST | Bearer | `/v1/messages` 的短路径兼容别名 |
| `/responses` | POST | Bearer | `/v1/responses` 的短路径兼容别名 |
| `/v1/embeddings` | POST | Bearer | OpenAI 兼容嵌入接口，按 model 透传（如 `BAAI/bge-m3`） |
| `/v1/audio/transcriptions` | POST | Bearer | OpenAI 兼容语音转文本（multipart），按 model 透传（如 `TeleAI/TeleSpeech-ASR1.0`） |
| `/embeddings`、`/audio/transcriptions` | POST | Bearer | 对应 `/v1/*` 的短路径兼容别名 |

透传端点不做格式转换：请求/响应原样转发给模型所属 Provider；模型须在 `providers.yaml` 注册，Provider 的 `url` 配上游源站，网关在其后拼接端点路径。

管理面板（使用 `ADMIN_TOKEN`，未配置时回退到 `LITELLM_MASTER_KEY`）：

| 端点 | 方法 | 说明 |
|------|------|------|
| `/admin/providers` | GET | 查看 Provider 运行状态、熔断状态和用量 |
| `/admin/providers/:name` | PATCH | 设置 `{"enabled":true/false}`，运行时启停 Provider |
| `/admin/providers/:name/reset` | POST | 手动重置熔断器 |
| `/admin/providers/:name/health-check` | POST | 执行一次 Provider 健康探测 |
| `/admin/routes` | GET | 查看模型的故障转移顺序 |
| `/admin/routes/:model` | PUT | 用 `{"providers":[...]}` 调整同一链路的优先级 |
| `/admin/models/:model` | PUT | 调整模型 `capabilities` 和 `input_modalities` |
| `/admin/models/doctor` | POST | 批量检查模型状态；`{"probe_anonymous":true}` 显式探活匿名聊天模型 |
| `/admin/config/status` | GET | 配置代次、最近生效时间与重载失败原因 |
| `/admin/config/check` | POST | 离线预检当前 YAML，不应用变更 |
| `/admin/config/reload` | POST | 校验、构建并原子重载当前 YAML |
| `/admin/config/cors` | GET / PUT | 读取 / 保存 `{"allowed_origins":[...]}` 并热加载 |
| `/admin/pi` | GET | 查看 Pi 模型清单同步状态（期望清单、当前清单、差异） |
| `/admin/pi/sync` | POST | 把精选模型清单同步到 `~/.pi/agent/models.json`（写入前自动备份），等价于 `llm-gateway setup pi`，也可在控制面板「设置 → Pi 集成」一键操作 |
| `/admin/archives` | GET | 分页查询对话归档（`limit`/`offset`） |
| `/admin/archives/export` | GET | 增量导出归档为 JSONL（`since`/`limit`，响应头返回下一游标） |
| `/admin/archives` | DELETE | 按时间清理归档（`before_days` 或 `before`） |
| `/admin/archives/:id` | DELETE | 删除单条归档 |
| `/admin/fs/dirs` | GET | 只读目录浏览：`?path=` 绝对路径（缺省为主目录），返回该目录下的子目录名列表，供桌面端选择网关主机路径；不返回文件内容 |
| `/admin/skills` | GET | 技能面板状态：技能目录、启用清单、各目标目录链接是否同步（未设置 `SKILLS_REPO_PATH` 时返回 `configured:false`） |
| `/admin/skills/:id` | GET | 单个技能详情（registry 条目 + SKILL.md 原文） |
| `/admin/skills/config` | PUT | 写入启用清单 `{"targets":[...],"enabled":[...]}`（写入前自动备份） |
| `/admin/skills/sync` | POST | 按清单落盘：为启用技能建符号链接、清理指向技能仓库的旧链接 |
| `/admin/skills/market/browse` | GET | 技能市场目录：`q`/`category`/`sort`/`page`/`section`（默认启用，`SKILLS_MARKET_ENABLED=false` 时返回 `enabled:false`） |
| `/admin/skills/market/sections` | GET | 技能市场主题分组 |
| `/admin/skills/market/installed` | GET | 已安装的市场技能（含 EasyAgent 安装的，按安装清单识别） |
| `/admin/skills/market/install` | POST | 安装市场技能 `{"id":<技能 ID>}`（同步等待完成）：下载、SHA-256 校验、解包到安装目录 |
| `/admin/skills/market/install-stream` | POST | 同上，以 SSE 逐事件推送阶段与字节进度 |
| `/admin/skills/market/install-jobs` | POST | 提交后台安装任务 `{"id":<技能 ID>}`，立刻返回任务号；安装归网关所有，客户端断开不影响。同一时刻仅允许一个安装任务（并发提交返回 409） |
| `/admin/skills/market/install-jobs/:id` | GET | 查询安装任务进度（phase / bytes / display_name；任务随网关重启清空） |
| `/admin/skills/market/install-jobs` | GET | 最近安装任务列表（保留最近 20 条） |
| `/admin/skills/market/uninstall` | POST | 卸载市场技能 `{"name":"<目录名>"}`（只删带安装清单的目录） |

### 技能面板（custom-skills 技能市场）

桌面端的 Skills 页在设置 `SKILLS_REPO_PATH` 指向 custom-skills 仓库根目录后可用。仓库内的 `registry/skills.json` 是技能目录（`generate:registry` 生成），`skills.enabled.json` 是启用清单（声明哪些技能装到哪些目录）。同步用**符号链接**把仓库的 `skills/<id>` 铺进目标目录：仓库更新即时生效，重装系统后一条同步命令即可恢复；清理时只移除指向技能仓库的链接，目标目录里的其他文件一律不动。

目标目录属于**运行网关的主机**，使用绝对路径或 `~/`（网关进程用户的 home）；不支持相对目标路径。技能 ID 必须是 registry 内不含路径分隔符的目录名。保存与同步均校验清单，保存采用临时文件替换，并串行处理管理端的保存/同步请求。`targets` 和 `enabled` 都必须显式传入数组，全部停用用 `enabled: []`。

同步结果中的 `linked/current/removed/skipped/errors` 始终为数组；有同名文件冲突时跳过并保持 `in_sync: false`，其他错误显示在面板。同步只检查当前清单中的目标目录：如需移除旧目标内的技能，先停用并同步旧目标，再修改目标列表。

页面支持 **全局技能 / 项目技能** 两种范围：
- 全局范围继续使用技能仓库的 `skills.enabled.json` 与配置的目标目录。
- 项目范围输入现有项目的绝对路径，每个项目独立保存 `.agents/skills.enabled.json`，仅同步到该项目的 `.agents/skills`；不会改变全局启用清单。清单内保存相对目标，便于项目移动。
- 「本地已有」只读展示未由技能库管理的技能；项目范围检查 `.agents/skills`、`.claude/skills` 和 `.codex/skills`。同名自定义文件和外部链接不会被覆盖；全局与项目技能的实际加载及优先级由客户端决定。
- `/admin/skills`、`/admin/skills/config`、`/admin/skills/sync` 可携带 `?project=<URL 编码的项目绝对路径>`，省略时保持全局行为。项目目录需存在；项目 `.agents`、安装目录和清单不接受符号链接，以免跨范围写入。
- 选择技能会先形成草稿。「保存并同步」一次完成清单保存与符号链接落盘；同步是幂等的目录对齐，也可单独用于修复断链。同步结果区分新增、清理、同名跳过及错误，空结果返回数组。

#### 技能市场

技能市场把远端商城（默认 `https://skills.deepvlab.ai`）接到技能页：浏览、搜索、一键安装与卸载。安装包经 SHA-256 校验后解包到 `SKILLS_MARKET_SKILLS_DIR`（默认 `~/.agents/skills`），目录内写入安装清单，卸载只删除带清单的目录——手写技能与符号链接不受影响。

- 环境变量：`SKILLS_MARKET_ENABLED=false` 关闭面板；`SKILLS_MARKET_BASE_URL` 指向其他商城地址；`SKILLS_MARKET_SKILLS_DIR` 自定义安装目录。
- 安装目录与 EasyAgent 同源，安装清单格式一致：任何一侧安装的技能在另一侧都显示为已安装；商城不可达时浏览返回带内 `error`，技能库（custom-skills）来源不受影响。
- 商城为外部服务，其可用性与内容归属不受网关控制；技能的实际加载与优先级仍由各客户端决定。

`/admin/stats`、`/admin/dashboard`、`/admin/models` 和 `/admin/logs` 会返回聚合指标或请求日志。缓存统计包含 `cache_read_input_tokens`、`cache_creation_input_tokens`、`cache_input_tokens`、`cache_usage_requests` 和 `cache_hit_rate`；命中率按缓存读取 token ÷ 已报告的总输入 token 加权计算。上游没有返回缓存 usage 的请求不计入命中率样本，数据不足时 `cache_hit_rate` 为 `null`。

`/admin/stats` 与 `/admin/dashboard` 只覆盖「今日」；历史趋势用 `GET /admin/stats/history`：

- `?days=30`（默认 7，上限 400）或 `?from=YYYY-MM-DD&to=YYYY-MM-DD`（闭区间，跨度上限 400 天）；
- 返回 `days`（按天序列，缺失日期补零，可直接画图）、`models`（按请求量排序，`provider` 为该模型用量最大的渠道）与 `providers` 排行；
- 数据来自 SQLite `daily_stats` 按天聚合表，跨重启可查；该表独立于请求日志保留（默认 365 天，`STATS_RETENTION_DAYS` 可调），请求日志仍按 30 天清理。

### 对话归档与增量导出

默认情况下网关只记录轻量指标（状态码、token、延迟），不存任何请求/响应正文。开启归档后会额外保存脱敏后的完整对话，用于知识库增量同步。

**环境变量：**

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ARCHIVE_ENABLED` | `false` | 总开关，关闭时归档器为纯 no-op，零开销 |
| `ARCHIVE_MAX_BODY_KB` | `16384` | 单条 request/response body 的安全上限（KB）；超限记录 `truncated=true`，不会写入破损 JSON |
| `ARCHIVE_RETENTION_DAYS` | `90` | 归档保留天数，超期由后台任务自动清理 |

**脱敏规则：** 归档写入前会递归清除 `Authorization`、`Cookie`、`x-api-key`、`api_key`、`token`、`password`、`secret` 等敏感字段的值（替换为 `[REDACTED]`）；图片/音频/文件等多媒体内容只保留 `{type, size, sha256}` 摘要，不存 Base64 原文。流式响应在透传 SSE 的同时 tee 到内部 buffer，流结束后聚合归档；中断时记录原因。

**增量导出示例：**

```bash
# 首次导出
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:4001/admin/archives/export?limit=100" \
  -o batch_1.jsonl

# 响应头 X-Archive-Next-Cursor 给出下一页游标（timestamp,id 格式）
# 带游标续跑，实现断点续传和去重
NEXT=$(curl -sI -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:4001/admin/archives/export?limit=100" \
  | grep -i X-Archive-Next-Cursor | awk '{print $2}' | tr -d '\r')

curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:4001/admin/archives/export?limit=100&since=$NEXT" \
  -o batch_2.jsonl
```

每行 JSON 包含 `schema_version`、`request_id`、`timestamp`、`protocol`、`source`、`conversation_id`、`session_id`、`model`、`provider`、`is_stream`、`status`、`status_code`、`input_tokens`、`output_tokens`、`request_bytes`、`response_bytes`、`truncated`、`request_body`、`response_body`、`error_reason` 等字段。`schema_version` 当前为 `2`。客户端可以通过 `X-AI-Source`、`X-Conversation-ID`、`X-Session-ID` 写入可关联的来源与会话标识；密钥类内容仍会被脱敏。

Provider 熔断默认在连续 3 次可重试上游失败后打开，30 秒后允许一次半开探测；429 则立即冷却，优先使用有效的 `Retry-After`（秒数或 HTTP 日期），否则从 30 秒指数退避到最多 300 秒（`RATE_LIMIT_INITIAL_SECONDS` / `RATE_LIMIT_MAX_SECONDS`）。冷却期间自动跳过该 Provider，恢复时只允许一个半开请求；可通过 `CIRCUIT_FAILURE_THRESHOLD`、`CIRCUIT_RECOVERY_SECONDS` 和 `CIRCUIT_SUCCESS_THRESHOLD` 调整。管理接口只返回脱敏的运行状态，API key 始终来自环境变量。

### 等待限制与测速

三种 API 共用路由等待策略。首个实际文本、推理内容或工具调用到来前，元数据及心跳会缓冲，超时可以切换下一家；一旦对客户端输出，就不会拼接另一家模型的回答。

流式成功需要上游的协议终态：转换路径检查 `finish_reason` / `finishReason`、`[DONE]` 或对应 Provider 的结束事件，直通路径检查 `message_stop` / Responses 终态。缺少终态的 EOF 和上游错误帧均按中断处理，不补造成功结束；已输出的请求不再 fallback。即便响应头已经是 HTTP 200，最终错误仍计入失败，并写入归档终态。

| 环境变量 | 默认秒数 | 用途 |
|---|---:|---|
| `ROUTING_FIRST_OUTPUT_SECONDS` | 15 | 普通流等待首个实际输出 |
| `ROUTING_REASONING_FIRST_OUTPUT_SECONDS` | 120 | 推理模型或启用推理的请求等待首个实际输出 |
| `ROUTING_STREAM_IDLE_SECONDS` | 30 | 已开始的流连续无实际进展；心跳不延长等待 |
| `ROUTING_ATTEMPT_SECONDS` | 30 | 非流式单次完整响应上限 |
| `ROUTING_FALLBACK_WAIT_SECONDS` | 180 | 同一请求所有尝试共用的等待预算 |

流开始输出后只检查 idle 超时，不用总等待预算截断正在生成的长回答。客户端取消立即取消上游，不增加 Provider 熔断失败计数；请求日志仍按中断标记错误，即使响应头已是 HTTP 200。普通超时按现有连续失败阈值熔断，不会因一次偶发慢响应永久删除模型。`provider_attempts` 中的 `first_output_ms` 记录实际首输出，排除响应头和心跳。

收到退出信号后，网关停止接收新请求，最多等待 15 秒让在途 HTTP 请求完成，再用共同的 15 秒清理窗口等待异步指标、归档写入，最后关闭数据库。超时会强制收尾并记录未完成的步骤，不保证超时后的全部写入成功。重启后的今日 Token 从完整日汇总恢复，不受最近 2,000 条日志窗口限制，缓存 Token 口径与模型统计一致。

始终思考的模型（如 GLM 系列）在长思考时可能超过思考档默认值。可在 `providers.yaml` 的 Provider 条目按需覆盖首字期限：`first_output_timeout_seconds` 覆盖普通档、`reasoning_first_output_timeout_seconds` 覆盖思考档（0 表示沿用全局）。覆盖生效时该请求的 fallback 等待预算会抬升到至少覆盖值，不会被默认预算钳回；等待预算仍约束未覆盖的其他档。

默认按模型实例冷却。确实共用同一限流额度的 Provider，可在 `providers.yaml` 的 Provider 条目设置 `rate_limit_scope: channel`，使该条目所有模型共用 429 冷却；只在 429 时共享，普通失败仍按模型计数。`model` 或留空使用独立冷却。手动 reset 该组任意模型也会重置组的冷却。

在含 `.env` 的网关目录运行（或设置 `LITELLM_MASTER_KEY`）：

```bash
./gateway benchmark --models coding --samples 3 --timeout 60s
./gateway benchmark --models model-a,model-b --samples 2 --timeout 45s
```

`--endpoint` 默认 `http://127.0.0.1:4001`。命令串行测指定模型，每个模型最多 10 次，遇到 HTTP 429 停止该模型剩余样本。输出 JSON 包含成功率、实际首输出、完成耗时、输出 token 数及从首输出到完成的平均 tokens/s（上游有 usage 时）。不保存提示词、回答和密钥，也不扫描整个模型目录。测 alias 时会走真实 fallback 链；比较模型要使用具体模型 ID。测速只反映该时刻、小提示词下的速度和可用性，不能代替代码能力、工具调用及长上下文评测；默认 `coding` 保留精简备用链，模型目录仍可用于手动选择。

### 助理接口与验证

`ASSISTANT_ENABLED=true` 且配置 `ASSISTANT_MODEL` 后启用。沿用管理接口的 Bearer 认证，LLM 默认回环经过网关；SDK 固定版本见 [go.mod](go.mod)。

| 方法 | 端点 | 契约 |
|---|---|---|
| GET | `/admin/assistant/models` | 返回 `default` 和可选 `models` |
| POST | `/admin/assistant/chat` | body 为 `{"message":"...","model":"可选模型 ID"}`；省略 model 使用默认值 |
| GET / PUT | `/admin/assistant/prompt` | 查询 / 更新人设；空串恢复默认，更新会重建内存会话 |
| GET / POST | `/admin/assistant/feedback` | 查询 / 提交回复反馈 |

聊天 SSE 使用 `data: {"type":"...",...}`，没有具名 `event:`。`text_delta` 的 `content` 为增量；`tool_start` / `tool_end` 携带 `tool`；成功终态为 `done`（content 为最终文字），失败为 `error`（content 为错误摘要）。部分文字之后的错误仍是失败，HTTP 200 不代表成功。消费者应保留已展示文字；服务端会读完 SDK channel 再释放本轮，允许下一次请求。

与 EasyAgent 的会话任务不同，此助理请求由 HTTP 上下文持有：关闭连接即取消本轮，断开后不保证收到终态帧。桌面「取消」会显示已停止，保留部分文字并恢复发送。按次选模不修改 `ASSISTANT_MODEL`；每个模型有独立内存历史，网关重启清空。

部署后先检查 `/health` 与 `/readyz`，再用 `/admin/assistant/models` 中已有授权、额度可用的模型验证：

1. 普通聊天持续收到增量并以 `done` 完成；
2. 长回复途中断开连接，随后可以再次聊天；
3. 请求助理调用只读 `gateway_status`，观察 `tool_start` / `tool_end` 后得到健康摘要。

429 或订阅额度耗尽属于上游限制。保留原错误和重置时间，按需改选其他已授权模型；不要把熔断重置当成额度恢复。助理关闭时聊天、模型和人设端点返回 503；反馈接口独立依赖存储。记忆治理与工具边界见 [MEMORY_DESIGN.md](../docs/MEMORY_DESIGN.md)。

### Provider 探测

`POST /admin/providers/:name/health-check` 会**真实发一次最小请求到上游**，用来验证「鉴权 + 模型可用」，而不是只看本地配置。结论分四类：

| 结论 | 含义 | 触发条件 |
|------|------|---------|
| `online` | 上游可用 | 上游返回 2xx |
| `degraded` | 上游可达但当前受限，通常可自愈 | 429、5xx、网络错误 |
| `offline` | 需要改配置才能恢复 | 401/402/403/404（凭据失效、无模型权限、欠费、模型不存在） |
| `unknown` | 无法得出结论 | provider 未实现探测能力 |

响应里带 `probe_status`、`detail`、`status_code`、`latency_ms`，并同时返回该 provider 的完整运行状态。只有 `online`/`degraded`/`offline` 会写入熔断器；`unknown` 不写结果，避免一次「测不了」被当成成功而清掉真实的连续失败计数。

熔断打开（包括 429 冷却）时运行状态显示 `offline`，并带 `next_retry_at`；探测本身仍报告 `degraded`。其余情况下，管理面板的「在线」状态以**最近一次探测结论**为准，优先于「历史上请求成功过」——否则一个已被上游限流的 provider 会因为早先成功过而长期显示在线。从未探测过的 provider 显示「未知」，面板会注明状态来自请求历史。

`Provider.IsHealthy` 只反映本地配置（凭据是否存在），不再作为上游可用性的判断依据。

### 模型能力与多模态

`GET /v1/models` 除了标准模型字段，还会返回 `capabilities`、`input_modalities`、`protocol` 和可选的 token 上限。网关会在转发前按这些能力筛选路由：如果请求包含图片而目标模型没有 `vision`，返回明确的 `400`，不会把图片静默降成文本或错误 fallback 到文本模型。

当前配置中：

- 智谱只提供 `glm-5.3` 与 `glm-5.3-flash` 两个模型，其余型号（含视觉专用的 `glm-5v-turbo`）已从网关删除；
- `glm-5.3` 是纯文本模型，不支持图片（上游会以 `1210 messages.content.type 参数非法` 拒绝）；
- `glm-5.3-flash` 兼有图片能力，`coding` 链上的图片请求会跳过 `glm-5.3` 落到它；
- Antigravity（Google 账号 OAuth）经本机 CLIProxyAPI 反代为 OpenAI 兼容上游，
  `gemini-3.8-flash-high`（当前 Gemini 系列主力）实测支持图片输入；OAuth 凭据与 token 续期都在 CLIProxyAPI 侧维护；
- 图片请求使用 OpenAI `image_url` content block，网关会保留原始块和 `extra_body`/`thinking` 等扩展字段。

配置新模型时建议显式声明能力：

```yaml
models:
  - id: glm-5.3-flash
    capabilities: [text, vision, tool_calling, streaming, reasoning]
    input_modalities: [text, image]
```

### 日志查看

网关默认将结构化摘要写到 stdout。每个请求都会带 `X-Request-ID`，指标日志中还会记录最终 Provider 和 fallback 尝试；请求正文不会默认写入日志。

```bash
# 运行时保存日志（按需替换为 systemd/Docker 日志收集）
./gateway 2>&1 | tee -a gateway.log

# 按 request ID 检索
grep 'request_id=client-trace-42' gateway.log
```

完整请求/响应归档默认关闭；开启 `ARCHIVE_ENABLED=true` 后会脱敏写入独立的 `conversation_archives` 表，可通过 `/admin/archives` 查询和 `/admin/archives/export` 增量导出，详见下方「对话归档与增量导出」一节。

### 健康检查

```bash
curl http://localhost:4001/health
# {"status":"ok"}

curl http://localhost:4001/readyz
# {"status":"ready"}
```

### OpenAI 兼容：chat completions（非流式）

对于支持推理的模型，`max_tokens` 也计入推理 token。上游返回推理内容时，非流式响应会通过 `message.reasoning_content` 返回；若 `content` 为空且 `finish_reason` 为 `length`，可能表示 token 预算已用于推理。

```bash
curl -X POST http://localhost:4001/v1/chat/completions \
  -H "Authorization: Bearer sk-local-gateway-xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "coding",
    "messages": [
      {"role": "system", "content": "You are helpful"},
      {"role": "user", "content": "你好"}
    ],
    "max_tokens": 100
  }'
```

### OpenAI 兼容：chat completions（流式）

上游提供推理内容时，流式响应会通过 `choices[].delta.reasoning_content` 返回增量。

```bash
curl -N -X POST http://localhost:4001/v1/chat/completions \
  -H "Authorization: Bearer sk-local-gateway-xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "coding",
    "stream": true,
    "messages": [
      {"role": "user", "content": "你好"}
    ]
  }'
```

### Anthropic 兼容：messages（非流式）

```bash
curl -X POST http://localhost:4001/v1/messages \
  -H "Authorization: Bearer sk-local-gateway-xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "coding",
    "max_tokens": 100,
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

### Anthropic 兼容：messages（流式）

```bash
curl -N -X POST http://localhost:4001/v1/messages \
  -H "Authorization: Bearer sk-local-gateway-xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "coding",
    "max_tokens": 100,
    "stream": true,
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

---

## 可用模型

Provider、模型能力和路由链见 [providers.yaml](providers.yaml)，运行时可用目录以带认证的 `GET /v1/models` 为准。未配置凭据或未启用的 Provider 可能被跳过，账号渠道还可动态增加模型。目录中存在某个 ID 不能证明当前订阅额度充足，仍需实际调用验证。

具体模型通常沿用上游 ID；跨供应商重名、账号渠道及兼容入口可能带前缀或映射。客户端使用 `/v1/models` 返回的完整 ID，不自行拼接 Provider 名。`coding` 等路由链是聚合入口，与直接调用具体模型区分。

`coding` 是配置中的聚合降级链；客户端也可以直接调用目录中的具体模型 ID。链的顺序由配置决定，按请求能力过滤后，只对可重试错误切换下一档，且已向客户端输出后不再 fallback：

- 上游限流（429）、5xx、网络错误 → 换下一档重试
- 账号无该模型权限（403）、凭据失效（401）、欠费（402）、模型不存在（404）→ 换下一档重试
- 请求本身不合法（400/422）→ **不**降级，直接返回（换个 provider 也是同样的错误）
- 请求带图片时，不具备视觉能力的档位会被自动跳过

启用 `DEEPV_ENABLED=true`（EasyCode/DeepVCode 聚合服务）后，网关读取本地 JWT 登录态注册相应模型；绑定与兼容 ID 以 Provider 实现和运行时目录为准。

DeepV 工具调用适配会合并 JSON 字符串参数分片及同一调用 ID 的重复帧，完整参数收齐后再交给客户端执行；历史调用即使没有参数也保留 `args: {}`。不完整或非对象参数会明确报错，避免静默丢失后继续执行工具。

DeepV 上游按单请求 token 总量（输入 + `max_output_tokens`）不超过 200000 校验，两个模型的目录条目声明 `max_input_tokens: 160000`、`max_output_tokens: 32000`。超过该限制的请求会被上游以配额错误拒绝，网关识别后转换为 400 并附处置说明，避免客户端把参数问题当成欠费（402 Payment Required）。

配置了 ChatGPT Codex OAuth 凭证或 GitHub Copilot 后，额外模型会动态加入目录；ChatGPT 的代理是可选的。不要在客户端硬编码版本，直接读取 `/v1/models`。

OpenAI 兼容 Provider 的请求超时默认是 120 秒。需要承载长推理请求时，可在
`providers.yaml` 的 Provider 节点设置 `request_timeout_seconds`；当前 GLM 与
Antigravity Provider 为知识飞轮的长请求设置了 900 秒。这个值只控制 Provider HTTP 客户端的墙钟超时，
调用方的取消信号仍然优先生效。

---

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `LITELLM_MASTER_KEY` | 是 | — | 网关认证 token |
| `GLM_API_KEY` | 否 | — | 智谱 API key |
| `CLIPROXY_API_KEY` | 否 | — | 本机 CLIProxyAPI（Antigravity 反代）的静态 api-key |
| `COPILOT_TOKEN` | 否 | — | GitHub Copilot token（短期有效，约 30 分钟） |
| `COPILOT_GITHUB_TOKEN` | 否 | — | GitHub OAuth token（用于自动刷新 Copilot token） |
| `DEEPV_ENABLED` | 否 | `false` | 启用 DeepV Server（EasyCode/DeepVCode，deepseek / glm-5.3-flash 系列） |
| `DEEPV_WORK_DIR` | 否 | 启动目录 | DeepV 请求附带的 Git 信息头来源目录 |
| `HTTP_PROXY` | 否 | — | ChatGPT Codex 的可选 HTTP 代理地址（如 `http://127.0.0.1:7890`） |
| `CHATGPT_AUTH_FILE` | 否 | 自动查找 | ChatGPT/Pi OAuth `auth.json` 路径 |
| `PORT` | 否 | 4001 | 监听端口 |
| `LOG_LEVEL` | 否 | info | 日志级别 |
| `ADMIN_TOKEN` | 否 | 使用 `LITELLM_MASTER_KEY` | 管理接口独立认证 token |
| `CIRCUIT_FAILURE_THRESHOLD` | 否 | 3 | 连续可重试失败后打开熔断 |
| `CIRCUIT_RECOVERY_SECONDS` | 否 | 30 | 打开后等待半开探测的秒数 |
| `CIRCUIT_SUCCESS_THRESHOLD` | 否 | 1 | 半开状态连续成功后关闭熔断 |
| `ARCHIVE_ENABLED` | 否 | `false` | 对话归档总开关，关闭时零开销 |
| `ARCHIVE_MAX_BODY_KB` | 否 | 16384 | 单条 body 安全上限（KB），超限标记 `truncated=true` |
| `ARCHIVE_RETENTION_DAYS` | 否 | 90 | 归档保留天数 |

未配置 key 的 provider 会被自动跳过，不影响其他 provider 正常工作。

---

## 架构

```text
OpenAI / Anthropic / Codex CLI 客户端
            │
            ▼
┌─────────────────────────────┐
│        Go Gateway           │
│                             │
│  Auth Middleware            │
│  Logging Middleware         │
│                             │
│  Handlers                   │
│  ├── /v1/chat/completions   │
│  ├── /v1/messages           │
│  └── /v1/responses          │
│                             │
│  Router                     │
│  ┌──────────────────────┐   │
│  │ model → provider 映射│   │
│  │ fallback 链管理       │   │
│  └──────────────────────┘   │
│                             │
│  Providers                  │
│  ├── OpenAIProvider         │──▶ providers.yaml 中的 OpenAI 兼容 Provider
│  ├── AnthropicProvider      │──▶ Anthropic 兼容 Provider
│  ├── CopilotProvider        │──▶ GitHub Copilot（可选）
│  └── ChatGPTProvider        │──▶ ChatGPT Codex (OAuth token; proxy optional)
└─────────────────────────────┘
```

**当前设计**：

- 对外同时提供 OpenAI 与 Anthropic 两套接口
- OpenAI 主链优先直连支持 OpenAI 的上游
- Anthropic 兼容链保留给 `/v1/messages`
- `OpenAIProvider` 会把上游 OpenAI SSE 转为内部可复用流，再由 handler 输出对应协议

---

## 开发

### 运行测试

```bash
make test          # 单元测试
go test -v ./...   # 详细输出
```

### 常用命令

```bash
make build         # 编译
make run           # 编译并运行
make test          # 测试
make fmt           # 格式化代码
make docker-build  # 构建 Docker 镜像
make docker-run    # Docker Compose 启动
```

### 新增 Provider

1. 在 `internal/provider/` 新建实现（参考 `anthropic.go` 或 `openai.go`）
2. 实现 `Provider` 接口；需要流式时同时实现 `StreamProvider`
3. 在 `providers.yaml` 声明 Provider、模型能力、输入模态和 chain
   DeepV 使用专用协议，模型及独立路由在 `main.go` 的 `setupDeepVProviders` 中注册；ChatGPT 订阅模型还需同步 `internal/provider/chatgpt.go` 的备用目录。新增后需重新构建并重启对应运行实例才能生效。
4. 若需要新的密钥环境变量，同步更新 `.env.example` 和配置加载逻辑
5. 为路由、能力筛选和 fallback 增加测试，并运行 `go vet ./...`、`go test ./...`

---

## 服务器部署

### 完整地址

- 对外地址: `http://localhost:4001`
- OpenAI 接口: `http://localhost:4001/v1/chat/completions`
- Anthropic 接口: `http://localhost:4001/v1/messages`

### 方式一：迷你主机自动同步（推荐）

.github/workflows/ci.yml 在每个 PR 上运行 Go 检查、迷你主机更新器回归、Windows 安装器回归和 Linux x86_64 构建；合并到 main 后，只有这些检查全通过，GitHub Actions 才会发布迷你主机使用的构建产物。PR 构建不会发布给更新器。

迷你主机上的用户级 systemd timer 每 10 分钟检查一次 GitHub。主机只会部署与当前 main 完全一致、CI 成功且摘要校验通过的构建产物；安装后会检查网关健康状态，并触发现有 systemd 服务重启。如果更新后健康检查失败，会恢复上一份二进制。构建失败、检查进行中或 main 已继续前进时，现有网关不变。

该流程只需要迷你主机访问 GitHub 的出站 HTTPS，不配置 GitHub Secrets、部署私钥、入站端口或仓库 Runner。首次安装前，在 GitHub 创建只对本仓库开放、仅有 Actions Read 权限的 fine-grained token，并在主机终端隐藏输入：

```bash
bash -c 'install -d -m 700 ~/.config/litellm-gateway; read -r -s -p "GitHub Actions read token: " EA_GITHUB_TOKEN; printf "\\n"; printf "%s" "$EA_GITHUB_TOKEN" > ~/.config/litellm-gateway/github-token; unset EA_GITHUB_TOKEN; chmod 600 ~/.config/litellm-gateway/github-token'
```

令牌只用于读取 GitHub Actions API 元数据和成功 CI 生成的构建产物，保存在主机的独立权限文件中，不会进入服务环境变量、进程参数或日志。主机保留自己的 .env 和 provider 凭据。随后在主机上运行一次安装命令（此项目默认使用 Linux 用户 q 和 llm-gateway.service）：

    set -o pipefail
    curl --fail --silent --show-error https://api.github.com/repos/nightgopher/easygateway/contents/scripts/install-mini-gateway-updater.sh | python3 -c 'import base64,json,sys; print(base64.b64decode("".join(json.load(sys.stdin)["content"].split())).decode(), end="")' | bash

如果从 Mac 执行，可通过 SSH 完成这次凭据录入和安装；后续检查、下载和重启均由主机本地 timer 完成，无需逐次 SSH。安装脚本不使用 sudo；配置文件位于 ~/.config/litellm-gateway/update.env。

查看同步状态和最近日志：

    systemctl --user status gateway-auto-update.timer
    journalctl --user -u gateway-auto-update.service -n 30 --no-pager

只有网关进程健康时才会更新；失败的版本会记在 ~/.local/state/litellm-gateway-update/failed-revision，排除问题后删除该文件即可重试。

### 方式二：手动部署

#### 1. 构建镜像

```bash
cd go-gateway
docker build -t go-llm-gateway:latest .
```

#### 2. 传到服务器

```bash
# SSH 到服务器
ssh user@your-server-ip

# 创建 .env
mkdir -p /opt/go-gateway
cat > /opt/go-gateway/.env << 'EOF'
LITELLM_MASTER_KEY=sk-local-gateway-xxx
GLM_API_KEY=
ALI_API_KEY=
PORT=4001
LOG_LEVEL=info
ARCHIVE_ENABLED=true
ARCHIVE_MAX_BODY_KB=16384
ARCHIVE_RETENTION_DAYS=90
EOF

# 启动
docker run -d \
  --name go-gateway \
  --restart unless-stopped \
  -p 4001:4001 \
  --env-file /opt/go-gateway/.env \
  go-llm-gateway:latest
```

### 服务器防火墙

确保服务器开放 4001 端口：

```bash
# ufw
ufw allow 4001/tcp

# iptables
iptables -A INPUT -p tcp --dport 4001 -j ACCEPT
```

### 验证

```bash
# 健康检查
curl http://localhost:4001/health
# {"status":"ok"}

# OpenAI 风格
curl -X POST http://localhost:4001/v1/chat/completions \
  -H "Authorization: Bearer sk-local-gateway-xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"coding","messages":[{"role":"user","content":"hi"}]}'

# Anthropic 风格
curl -X POST http://localhost:4001/v1/messages \
  -H "Authorization: Bearer sk-local-gateway-xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"coding","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}'
```

### 配置 Claude Code（远程）

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:4001/v1",
    "ANTHROPIC_AUTH_TOKEN": "sk-local-gateway-xxx"
  }
}
```

### 查看日志

```bash
docker logs -f go-gateway
```

### 停止服务

```bash
docker stop go-gateway
docker rm go-gateway
```

---

## Docker 部署

```bash
# 构建镜像
docker build -t go-llm-gateway .

# 运行（传入 .env 文件）
docker run -d \
  --name go-gateway \
  -p 4001:4001 \
  --env-file .env \
  go-llm-gateway

# 或用 Docker Compose
docker-compose up -d
```

---

## 资源对比

| | Go 网关 | LiteLLM |
|--|--|--|
| 内存 | ~18 MB | ~570 MB |
| 启动时间 | <1 秒 | ~15 秒 |
| 二进制大小 | ~15 MB | — |
| Docker 镜像 | ~50 MB | ~711 MB |

客户端集成面板支持分别勾选 EasyAgent、ZCode 和 Harness 要同步的聊天模型。默认勾选客户端当前已有的模型，新增模型不会自动加入；首次配置默认全选。同步只替换该客户端的网关模型清单，并在写入前备份。取消勾选的模型会从清单移除；至少选择一个模型才能同步。

降级链遇到 Gemini 的 `400 User location is not supported for the API use` 时，会继续尝试下一供应商；其他 HTTP 400 参数错误与 HTTP 422 仍终止降级。

DeepV 的 `claude-haiku-5-5` 不接受 `temperature`；网关转换该模型请求时会省略此字段，其他模型保持原有转发行为。

### 助理模型目录与 OpenCode 限制

`GET /admin/assistant/models` 每次读取当前路由目录，渠道登录、退出或 Provider 启停后无需重启网关。`models` 保持模型 ID 数组，同时新增 `model_options`（`id`、`provider`、`channel`）供客户端搜索模型或渠道名；多渠道 fallback 不强行标成单个渠道。默认模型仍由 `ASSISTANT_MODEL` 决定，但未登录或已停用时不再列为可选模型。

OpenCode 免费档仅允许官方 OpenCode 客户端使用，上游登录不能解除此限制，见 [维护者说明](https://github.com/anomalyco/opencode/issues/49588)。配置中的免费模型保留目录兼容性，网关不将其推荐给助理；渠道页面明确提示此限制。付费模型需在 `providers.yaml` 中添加并获得对应授权。转换器只保留调用方真实工具，不再补不能执行的 bash/read 占位工具。助理继续通过网关自身的 OpenAI 端点调用，保留渠道鉴权、路由、指标与 fallback。

### 配置预检与热加载

在网关工作目录执行 `easygateway config check`，离线检查 `providers.yaml`，无需 master key、登录账号或访问上游。也可指定 `--file /path/providers.yaml`；它读取同目录 `.env`（已有进程环境变量优先）。检查包含全局唯一模型 ID/别名、Provider 实例、能力与输入模态枚举、协议、URL、渠道名、token/超时范围及 fallback 引用；未知 YAML 字段和多文档 YAML 会被拒绝。

必需 `api_key_env` 未设置时默认检查失败；`allow_no_key: true` 的匿名 OpenAI Provider 只给警告。部分渠道尚未配置密钥的安装可执行 `easygateway config check --allow-missing-keys`，把缺失密钥全部作为警告。输出包含 Provider / 模型 / chain 数量；OAuth、DeepV 等运行时授权仍需单独验证。

改配置后可向运行进程发送 `SIGHUP`，或带管理 token 调用 `POST /admin/config/reload`（Windows 也支持）。`POST /admin/config/check` 只预检，`GET /admin/config/status` 返回配置代次、上次成功时间、诊断和最近失败原因。现有 `GET /admin/config` 保持原契约。

重载先校验并独立构建，再原子替换 Provider、模型与路由表；已取得 Provider 的在途请求继续完成。格式错误、无效引用或新建/原先已装载的 Provider 初始化失败时保留旧配置。启动时因未配置凭据而跳过的可选 Provider 可以继续保持跳过，并在报告中给出警告。账号池与已有订阅客户端复用，管理员的禁用状态和模型能力覆盖保留，已删除的模型与路由移出目录。路由手动排序以 YAML 为准重新应用，熔断及历史探测状态重新开始。

这里只重载 `providers.yaml`；端口、master/admin token 及其他 `.env` 启动配置仍需重启。配置中的 DeepV/Copilot 引用是代码注册的集成实例，预检不证明相应集成已启用或有上游权限。

### 模型健康目录与 doctor

`/v1/models` 和短路径 `/models` 保留完整目录，每项新增 `gateway_status`：

- `available`：当前是否可尝试。未登录、官方客户端限制、管理员禁用、临时熔断/限流或未过期的失败探活会使其为 false；
- `status`：`available` 表示近期探活成功，`unavailable` 表示确定不可用，`degraded` 表示临时受限，`unknown` 表示尚未验证或探活已过期；
- `reason`、`checked_at`：本次本地状态判定及时间；有探活时另有 `probe_status`、`last_probe_at` 和 `probe_expires_at`。

`GET /v1/models?available_only=true` 只返回 `available=true` 的项。凭据已配置但未探活的模型仍可尝试，状态明确为 `unknown`，不声称上游鉴权或推理已通过；fallback 链中任一候选可尝试即可保留该链。公开目录不回传上游原始错误正文、密钥或请求内容。Admin 模型与渠道列表也提供健康信息，桌面端展示原因和探活时间。

`easygateway doctor` 调用当前网关的管理接口，只检查本地可用性，不发推理请求。设置 `ADMIN_TOKEN` 或 `LITELLM_MASTER_KEY`；可用 `--endpoint http://127.0.0.1:4001` 指定网关，`--timeout 75s` 指定客户端期限。

`easygateway doctor --probe-anonymous` 或桌面「模型与路由 → 探活匿名模型」会对 `allow_no_key: true` 且实际未配置 API key 的聊天模型发 `max_tokens=1` 探活，最多 4 个并发、每次 20 秒、服务端整批 60 秒。嵌入/转写、付费 key、账号订阅渠道均不参与；它们的上游鉴权与推理是否有效仍需明确触发已有的单 Provider 手动探测（会产生真实调用）。匿名模型过小预算造成的普通参数错误会显示降级；明确的 `ModelProtocolUnsupported` 拒绝会显示不可用。

`MODEL_HEALTH_TTL_SECONDS` 默认 900；过期探活不再作为有效结论，临时熔断仍按其恢复时间判定。`MODEL_HEALTH_INTERVAL_SECONDS=0` 默认关闭后台 doctor；设置正数秒数启用定时检查，只有同时设置 `MODEL_HEALTH_PROBE_ANONYMOUS=true` 才会自动调用匿名模型。结果留在当前进程内，重启或配置重载后重新验证；不会把旧故障永久写进目录。

### 可选 CORS 白名单

默认 `cors.allowed_origins: []` 保持 `Access-Control-Allow-Origin: *`，适用于本机个人网关与自己托管的静态页面。需要限制浏览器来源时，在 `providers.yaml` 配置精确 origin：

```yaml
cors:
  allowed_origins:
    - https://nightgopher.github.io
    - http://localhost:5173
```

随后热加载，或直接在桌面「设置 → 网关配置 → 浏览器跨域来源」逐行填写并保存。管理接口 `PUT /admin/config/cors` 接收 `{"allowed_origins":["https://nightgopher.github.io"]}`，验证完整配置并原子保存到 YAML 后热加载；验证或写入失败保留原文件与策略。空数组恢复默认 `*`，不接受通配符、路径、凭据、query、fragment 或 `null` 来源。

白名单匹配时回显 Origin 并设置 `Vary: Origin`；不匹配的实际请求与 OPTIONS 返回 403 且没有允许跨域响应头。CORS 不使用 cookie 凭据；预检不需要 token，实际 API 请求仍须鉴权。原生客户端与没有 Origin 的请求仍按原有认证规则处理，因此白名单只能约束浏览器，不能替代 API key 管理。
