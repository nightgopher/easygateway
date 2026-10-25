# Agent 长期记忆层设计（v1）

> 状态：v1 已实现（存储 + 作用域路由 + 治理 API）；注入钩子与自动提取为后续阶段。
> 关联：PRD.md R7（归档与知识库导出）、AGENTS.md 数据红线。

## 1. 定位与分工

网关在记忆体系里承担**底座**角色，与既有分工对齐：

| 层 | 职责 |
|---|---|
| 网关（本仓库） | 记忆存储、作用域路由、命中观测、治理 API、（后续）请求注入 |
| agent-lessons | 从归档流编译候选知识（后续接入：编译产物以 candidate 落回本层） |
| 项目 `docs/` 与 agent-lessons 经验文档 | 长文档与代码事实的查阅，与命题记忆互补；具体存储以当前项目结构为准 |

设计原则：

- **命题化**：一条记忆 = 一条可检验的陈述（带来源与时间），不是聊天摘要；
- **作用域强制**：每条记忆必须属于 global / client / project 之一，杜绝跨域污染；
- **agent 起草、人拍板**：`/v1/memory` 写入一律进 `candidate`，`active` 必须经 admin 确认；
- **密钥红线**：写入前做模式守门，疑似密钥/凭据直接 422 拒绝（Token/密钥永不落库）。

## 2. 数据模型

表 `agent_memories`（SQLite，与 metrics/archives 同文件、物理隔离的表）：

| 字段 | 说明 |
|---|---|
| scope_type | `global` \| `client` \| `project` |
| scope_key | global 为空；client 为客户端名；project 为 `host:owner/repo` |
| statement | 命题正文（≤500 字符，写入前守门过滤） |
| status | `candidate` → `active` ⇄ `retired`（dead 用 delete） |
| source | `manual` \| `agent:<client>` \| `api` \|（规划）`archive:<request_id>`、`git:<sha>` |
| confidence / hit_count / last_hit_at | 检索排序与遗忘依据 |

`UNIQUE(scope_type, scope_key, statement)`：重复命题刷新既有行（吸收 agent 重试），不重复入库。

### 作用域路由

`LookupFor(client, project)` 合并三环：project ⊕ client ⊕ global，越窄越靠前（与 AGENTS.md 的目录作用域同构），命中自动累计 `hit_count` —— 这是后续"遗忘"（长期零命中降权/清理）的数据基础。

## 3. API（v1 已实现）

客户端（API Key 鉴权）：

| 端点 | 说明 |
|---|---|
| `GET /v1/memory?client=&project=&limit=` | 按作用域检索 active 记忆，响应含 `block`（渲染好的 `<agent-memory>` 注入块，wrapper 可直接用） |
| `POST /v1/memory` | 起草候选：`{statement, scope_type, scope_key, client}` → `candidate` |

管理端（Admin Token 鉴权）：

| 端点 | 说明 |
|---|---|
| `GET /admin/memories?status=&scope_type=&scope_key=` | 列表（分页） |
| `POST /admin/memories` | 人工直接创建（`active`） |
| `POST /admin/memories/:id/confirm` | candidate → active（人拍板） |
| `POST /admin/memories/:id/retire` | 过时/被反驳 → retired |
| `DELETE /admin/memories/:id` | 彻底删除 |

开关：`MEMORY_ENABLED=false`（默认）。停用时走 NoopStore——端点照常返回空结果，客户端与面板无需感知开关。

## 4. 客户端接入（通道 B，推荐）

个人 agent 场景每个客户端的改造成本要趋近于零，因此 v1 只要求一个最薄 wrapper：

1. 启动时按 `git remote get-url origin` 推导 `scope_key`（如 `github:nightgopher/easygateway`）；
2. 请求前 `GET /v1/memory?client=<名字>&project=<scope_key>`，把 `block` 拼进 system 末尾；
3. `POST /v1/memory` 上报候选记忆（可选）。

显式文件（各仓库 AGENTS.md）仍是第一优先记忆——本层只收它覆盖不了的跨客户端、跨仓库条目，两者不重复注入。

## 5. 路线图

- **P2 请求路径注入（通道 A 兜底）**：`MEMORY_INJECT=true` 时在 chat/messages/responses 处理器内，按请求头 `X-Memory-Client` / `X-Memory-Project` 注入短记忆块；与客户端自带记忆功能互斥检测，避免双份注入。
- **P3 自动提取管道**：后台任务消费 `conversation_archives`，用网关自有 Provider 调 LLM 抽取候选命题（带写入门槛：以后还用得到吗？与现有条目冲突吗？），以 `candidate` 落库走人工确认。归档是原料、候选是产物，仍然符合 R7 分工。
- **P4 平台事件锚点**：GitHub fine-grained PAT（只读）轮询个人仓库的 push/PR 事件（或 GitLab webhook，视仓库托管方）：MR 合入 → 时间窗口内同作用域候选记忆置信度上调并落 `git:<sha>` 锚点。公司仓库（git.seres.cn）作用域默认**排除提取**，避免工作上下文进入个人记忆库。
- **P5 遗忘机制**：基于 hit_count/last_hit_at 的降权与清理任务（如 90 天零命中 → 自动 retired）。

## 6. 明确不做

- 不做向量库检索起步：精确约定用精确查询；等自然语言知识规模上来再评估语义检索；
- 不做多租户：作用域键里没有租户维度，这是单用户基础设施；
- 不在网关做知识编译/清洗：那是 agent-lessons 的职责（R7 分工）。

## 7. 常驻助理（v1.1 已实现）

网关内嵌一个基于 EasyAgent SDK（`github.com/nightgopher/easyagent/sdk`）的常驻智能体，
端点 `POST /admin/assistant/chat`（Admin Token，SSE 流式）。

成功完成返回 `done`；上游缺少完成标记就断开，或发送显式业务错误时，
返回 `error`，已收到的 `text_delta` 保留供展示。助理等待 SDK 本轮收尾后
释放会话，随后可继续请求；客户端取消时保留取消错误并结束本轮。

SDK 版本以 [go.mod](../go-gateway/go.mod) 为准。`PromptStream` 的 `error` 后仍可能产生部分结果和清理事件，消费者须读到 channel 关闭再返回；不能把带部分消息的 `done` 当成成功覆盖原错误。设计参考 ZCode 的流式生命周期，网关适配与验证见 [PR #93](https://github.com/nightgopher/easygateway/pull/93) 和 [PR #94](https://github.com/nightgopher/easygateway/pull/94)。

`GET /admin/assistant/models` 返回默认值和可选模型，聊天 body 为 `{"message":"...","model":"可选模型 ID"}`；省略 model 使用 `ASSISTANT_MODEL`，按次选择不改写默认配置。每个模型各有独立的内存历史，助理 Chat 整体串行执行；更新人设重建全部模型实例。客户端断开取消本轮，已断开的连接不保证收到收尾帧。

**人设自定义与反馈（v1.2）**：
- `GET/PUT /admin/assistant/prompt` — 用户自定义 system prompt（存 `agent_settings` 表，
  保存后热更新无需重启；空串恢复内置默认）。内置默认已编码核心纪律：
  只记用户本人状态（设备/目标/现场经验），通用知识不入记忆；
- `POST/GET /admin/assistant/feedback` — 对回复打分（up/down + 备注，存 `assistant_feedback`），
  在设置页复盘规律后人工迭代人设。治理边界与记忆一致：agent 不改写自己的 prompt。

**架构要点**：

- LLM 调用**回环走网关自身**（`ASSISTANT_BASE_URL` 默认 `http://127.0.0.1:<port>`，
  Key 默认复用 MasterKey）——助理流量与普通客户端同路，享受 fallback 链与指标记录；
- 挂载网关域工具（EasyAgent `agent.Tool` 接口）：
  - `gateway_status`：只读查询 Provider 健康、熔断和今日统计；
  - `gateway_logs`：只读查询近期请求及失败日志；
  - `memory_lookup`：按作用域检索 active 记忆；
  - `memory_list_pending`：列出待确认候选；
  - `memory_propose`：提案候选记忆（**只能进 candidate，无权确认**）——
    助理可以打理记忆，但治理权在人；
- EasyAgent 引擎自带流式输出、上下文压缩、循环检测；会话历史暂驻内存
  （重启清零），持久化会话列入后续版本。

**配置**：`ASSISTANT_ENABLED` + `ASSISTANT_MODEL`（/v1/models 中的 ID），
`ASSISTANT_BASE_URL` / `ASSISTANT_API_KEY` 可选（默认自环 + MasterKey）。

**与管理端的关系**：desktop/ 原生桌面端的流式助理页直接消费该 SSE 端点，
助手即"住在网关里的记忆管家"；浏览器面板已随前端工具链一并移除。
