# 项目准则

## 1. 项目定位

EasyGateway 是个人 AI 基础设施，不只是 API 转发器。它为 Claude Code、Codex CLI、Pi、飞书及其他 AI 应用提供：

- 统一 API 入口和认证；
- OpenAI Chat Completions、Responses、Anthropic Messages 兼容；
- 模型能力路由、多 Provider fallback、健康检查和熔断；
- Provider、模型、路由、日志和运行状态管理；
- 账号渠道：上游账号登录、订阅额度跨客户端复用与授权续期；
- 技能中心：custom-skills 清单同步与远端技能市场安装；
- 记忆层与常驻助理；
- Go 原生桌面管理端和跨平台部署；
- 可选的对话归档与知识库导出。

需求范围以 [PRD.md](PRD.md) 为准，API 和部署细节以 [go-gateway/README.md](go-gateway/README.md) 与 [docs/](docs/) 为准。

## 2. 仓库结构

| 目录 | 作用 |
|---|---|
| go-gateway/ | Go 网关运行时、Provider、路由、认证、指标和 Admin API |
| desktop/ | MyGo 原生 Go 桌面管理端 |
| scripts/ | 安装、Codex 配置和回归测试脚本 |
| docs/ | 详细设计、兼容性和部署文档 |
| PRD.md | 产品需求和范围边界 |

项目运行代码使用 Go。desktop/ 使用 MyGo 的 ui 原生控件，以 Go 实现桌面界面、连接配置与鉴权传输，调用网关 Admin API。浏览器管理界面及其静态资源已移除，不再新增 TypeScript、JavaScript、HTML/CSS、Node.js 构建链或 WebView 包装。桌面端不自行启动或替换网关服务。不再维护手机 App，也不新增静态 mockup 作为正式客户端。

## 3. 配置和事实来源

- Provider、模型能力、模型链以 go-gateway/providers.yaml 为主要配置来源；
- 运行时可用模型以 GET /v1/models 为准；
- API Key、OAuth Token 和 Admin Token 只能来自环境变量或受保护配置；
- 不要在客户端、README、PRD、日志或测试数据中硬编码 Provider 密钥；
- 新增 Provider 或环境变量时，同时更新 providers.yaml、go-gateway/.env.example 和相关文档；
- 文档不要手工维护一份容易过期的 Provider/模型清单，优先引用配置和模型目录。

## 4. 网关开发准则

### API 与路由

- 保持三种协议的公开契约稳定；
- 保留工具调用、推理、扩展字段、多模态内容和流式语义；
- 图片、视频和文件请求必须先按 capabilities / input_modalities 选择兼容模型；
- 不要把图片请求静默降级到纯文本模型；
- fallback 前区分可重试错误与参数、认证、能力不匹配等不可重试错误；
- 每个逻辑请求保留 request ID，并能关联最终 Provider 和 Provider 尝试。

### Provider

- 新增 OpenAI 兼容 Provider 参考 internal/provider/openai.go；
- 新增 Anthropic 兼容 Provider 参考 internal/provider/anthropic.go；
- Provider API Key 不进入响应、日志或 Admin API；
- 运行时控制优先通过 Router、Admin API 和配置完成，避免把 Provider 判断散落在客户端。

### 指标与归档

- request_logs 只保存轻量指标，不直接存放大段 request/response 正文；
- 完整对话归档必须是独立、可开启、可脱敏、可清理的能力；
- 流式请求需要记录终态或中断原因；
- Authorization、Cookie、Token、密码和 API Key 永不落库；
- 多媒体原文默认保存元数据或引用，避免无限增长的 Base64/二进制正文。

## 5. 修改后的验证

按改动范围执行最小且充分的检查，不要只依赖编译成功：

### Go

~~~bash
cd go-gateway
gofmt -w <changed-go-files>
go vet ./...
go test ./...
go build -o gateway .
~~~

生成的二进制、临时数据库和本地配置不要提交。

### 原生桌面端

~~~bash
cd desktop
go test -race ./...
go vet ./...
go run github.com/egoist/mygo/cmd/mygo vet
go run github.com/egoist/mygo/cmd/mygo build
~~~

界面改动须验证原生控件、键盘输入、取消与主机切换，不能只检查窗口是否打开。

### 脚本和安装器

修改 scripts/ 或安装流程后，执行对应的 Unix/Windows 回归测试；不要把平台下载缓存或构建产物提交到仓库。

## 6. 文档维护

- PRD.md 只描述产品需求、范围、核心验收和交付顺序；
- README.md 描述用户可见的安装、配置和常用使用方式；
- go-gateway/README.md 描述 API、Provider 配置和运行细节；
- docs/ 保存较长的技术方案、兼容性说明和排障记录；
- API、模型能力、Provider、部署流程发生变化时，更新对应文档；
- 文档中的模型和 Provider 不能与 providers.yaml 或 /v1/models 长期冲突。

## 7. Git、PR 与 CI

- 唯一长期分支是 main，不保留 develop、备用或按功能命名的常驻分支；
- 开工前检查现有 PR、分支和工作副本；同一任务优先复用对应的临时分支，不重复创建分支；确需隔离时从最新 main 创建短期 codex/ 分支；
- PR 合并后确认提交已进入 main、相关 CI 通过，再同步本地 main；远端启用“合并后自动删除分支”，清理对应的本地临时分支并 prune 失效的远端跟踪引用；
- 只删除已核实合并且没有新提交的分支。未合并提交、未提交修改和仍在使用的工作副本必须先保留并说明；禁止用强制删除、reset 或覆盖文件来凑出“只剩 main”；
- 任务结束说明合并的 PR、验证结果和仍需保留的活动分支。
- 提交前检查工作树，避免把 .env、密钥、数据库、二进制和用户本地文件提交；
- 通过 Pull Request 合并，不直接向 main 推送；
- 代码修改完成后检查相关 GitHub Actions 状态；
- CI 失败必须定位并修复，不能留下已知红灯；
- 纯文档修改至少执行 git diff --check，并确认链接、命令和当前代码状态一致。

- 收尾时检查 `git worktree list`、各副本的修改和相关进程；确认工作已进入 `main`、所需检查通过后，同步并切回主目录的 `main`，主动清理本次任务的闲置 worktree 和已合并分支，再 prune 失效引用。
- worktree 只有在没有独有工作、未处理修改、活动会话或进程时才可清理。未跟踪文件也要核实；需要整合的先整合、验证，需要保留的先做可恢复备份，不仅凭目录名或 PR 已合并判断无用。
- Codex 管理的 worktree 使用应用归档工具，普通 Git worktree 使用 `git worktree remove`；不直接删目录、不强制清空，保留主目录和仍在使用的副本。
- 回归主线不构成提交、推送或部署授权。已授权的部署须确认构建包含最新已核实主线及需要保留的本地改动，避免旧分支覆盖较新运行版本。
- 完成时简要说明当前分支、清理结果和保留原因；只清理本次相关仓库。

常用检查：

~~~bash
git status --short --branch
git diff --check
gh run list --limit 3
gh run view <run-id> --log-failed
~~~

## 8. 常见操作

~~~bash
# 本地启动网关
cd go-gateway
go build -o gateway .
./gateway

# 配置 Pi
./gateway setup pi --dry-run

# Claude Code / Codex CLI 接管与还原
gateway claude coding
gateway claude status
gateway claude default
gateway codex coding
gateway codex default

# Codex CLI 模型切换（旧脚本，与 gateway codex 等价的 shell 版本）
codex-model gateway <model>
codex-model native
codex-model status
~~~

Codex 可用模型以网关 GET /v1/models 返回结果为准，不在本文件维护固定模型列表。
