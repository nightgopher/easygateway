# EasyGateway 原生桌面客户端

桌面界面使用 MyGo 的 `ui` 控件，以 Go 实现布局、表单、导航、虚拟列表和流式助理。窗口直接显示原生 UI，不启动 WebView，也不依赖 HTML、JavaScript、React 或 Node.js。客户端通过 Go 网络层直接调用网关 Admin API；仓库不再保留浏览器 Dashboard。客户端不安装、启动或替换网关服务。

## 开发与构建

需要 Go 1.27.1+。MyGo 版本以 `go.mod` 为准（当前 v0.6.0）。

```bash
go test -race ./...
go run github.com/egoist/mygo/cmd/mygo vet
go run github.com/egoist/mygo/cmd/mygo build
```

本机开发使用 `go run github.com/egoist/mygo/cmd/mygo dev`。产物位于 `build.noindex/<os>-<arch>/`，构建缓存和应用包不提交。桌面 CI 仅使用 Go，不安装 Node.js 或构建浏览器界面。

支持 `build -platform darwin/arm64,darwin/amd64,windows/amd64,linux/amd64`。macOS 打包需要 macOS；原生控件由平台图形与文本系统绘制，不要求 WebView2/WebKitGTK。实机验收平台为 macOS arm64，其他平台需另做实机验收。

## 功能与连接

「账号渠道」提供浏览器授权、授权码复制、自动确认、取消授权和账号移除。无需额外应用密钥的渠道优先显示；缺少客户端配置的渠道展示配置说明并禁用登录。需要本机回调的渠道只允许在本机网关连接中登录。切换主机、离开页面或退出应用会取消未完成的授权，取消不删除已保存账号。登录完成会清除该渠道因缺少凭据产生的熔断状态；「已登录」只表示账号授权完成，模型可用性仍需单独探测和实际调用验证。

账号行显示的是访问令牌到期时间，不是账号授权的截止时间。有续期凭据时显示「支持自动续期」；调用遇到鉴权失败后，网关会尝试续期并重试。上游撤销授权或续期凭据失效时仍需重新登录。

包含连接管理、运行概览、模型能力和路由顺序编辑、Provider 启停/探测/熔断重置、账号渠道登录与授权管理、请求日志、技能库与技能市场、记忆管理、流式助理、客户端模型同步和助理人设设置。支持原生搜索字段、列排序与日志详情可拖拽分栏；通过侧边栏角标与轻量 Toast 呈现状态与反馈。技能页与记忆页（project 范围）的路径选择：本机网关连接且为 cgo 构建时提供 macOS 原生目录面板（「本机选择」，sheet 挂主窗口，见 `internal/nativefolder`，需 `CGO_ENABLED=1` 构建，mygo build 默认关闭 cgo 时自动回退）；其余情况走「浏览」打开网关主机目录浏览器（`/admin/fs/dirs` 只读列目录），路径语义始终属于网关主机。记忆页 client 范围提供候选客户端下拉，global 范围无需范围键。技能页提供「技能库 / 技能市场」两种来源：技能库管理 custom-skills 清单，技能市场搜索远端商城并安装、卸载。助理页通过搜索弹窗按次切换聊天模型（`/admin/assistant/models`，输入即滤、当前选择带标记），助理用 `gateway_status` / `gateway_logs` 查询实时运行状态后回答运维问题。

首次启动填写连接名称、网关根地址与 `ADMIN_TOKEN` 或 `EASYGATEWAY_MASTER_KEY`。支持 `/admin` 后缀和反向代理前缀。先验证 `/admin/health`，再保存。可以添加、编辑、移除或切换多个连接；编辑时 Token 留空保留已有值。切换主机取消旧请求并清除旧主机页面数据；每个请求固定到发起时的连接。操作异步执行，可取消，不阻塞界面。

连接配置兼容原版桌面端，位于 MyGo 应用用户数据目录中的 `connections.json`。目录创建权限 `0700`，文件 `0600`，凭据仅由 Go 网络层使用；文件不是加密保险库。TLS 证书照常验证，不跟随带管理凭据的上游重定向。

技能的项目路径、安装目录和客户端同步路径属于**当前连接的网关主机**。全局和项目范围分别加载；未保存的技能选择必须保存或撤销后再切换范围。同步报告显示保留项和错误。助理支持增量回复、工具状态、停止与复制回复；连接中断会显示提示。个人管理端不展示助理评分或反馈收集入口。

助理回复使用 MyGo 0.5 原生 `ui/markdown`，呈现流式 Markdown、标题、强调、任务列表、表格及语法高亮代码块，支持跨段选择和复制。代码块可单独复制；HTML 作为文本显示，远程图片只显示说明，不加载。消息输入使用 `TextArea.Lines(2, 6)` 随内容增高，Enter 发送、Shift+Enter 在光标处换行并支持撤销；输入法确认候选字不会发送消息。流式回复仅在阅读末尾时跟随，向上滚动后可继续阅读。

助理成功显示「回复完成」；点击「取消」显示「已停止」并保留当前文字，随后可再次发送。上游错误会显示失败信息，已经显示的部分回复不等于成功。429 或额度耗尽时可从模型选择器切换其他已授权模型；按次选模不修改 `ASSISTANT_MODEL`。健康页可用不能代替真实模型调用验证，完整事件契约见 [助理 API](../go-gateway/README.md#助理接口与验证)。

关闭窗口后应用留在托盘；点击 Dock 或托盘恢复窗口。`⌘/Ctrl+,` 管理连接，`⌘/Ctrl+1` 显示窗口；应用菜单或托盘可完全退出。窗口尺寸与位置会保存。

「模型与路由」显示模型健康原因和探活有效期，可批量探活匿名模型；付费与账号订阅模型保留单 Provider 手动探测。账号渠道同时显示可尝试模型数量与近期探活通过数量。

「设置 → 网关配置」提供离线预检、热加载和 CORS 来源白名单编辑。每行一个 HTTP(S) origin，留空保持允许所有来源；保存会持久化到当前网关主机的 `providers.yaml` 并热加载。输入支持原生撤销，「撤销来源修改」恢复读取值；取消、切换主机或离开页面会丢弃旧请求的界面回填。网关未提供这些接口时，该卡片显示版本或连接提示。

## macOS 安装与发布

本机只维护 `/Applications/EasyGateway.app` 一份，通过替换这个固定路径更新。不要打开不同构建目录中的 `.app`，避免 Spotlight 和应用注册重复。构建目录使用 `.noindex` 后缀；单实例锁避免重复运行。

按 main 更新桌面时，先核对源码提交与本地改动，在 `desktop/` 执行上述测试、vet 和构建。正常退出旧应用后，验证候选签名（`codesign --verify --deep --strict <应用路径>`）及二进制来源（`go version -m <应用路径>/Contents/MacOS/EasyGateway`），再替换固定入口并保留可恢复旧包。重新启动后验证原连接、助理流式、取消及重试；配置与网关数据保留。`.noindex` 不代替 LaunchServices 注册清理，测试及回滚副本退出后须按已核实路径撤销注册，保留正式入口。

桌面版本来自 `mygo.json`；main 构建不自动增加版本或发布 tag，应通过 `vcs.revision` 核对最新代码。网关服务需另用仓库根目录的 `scripts/deploy-local-gateway.sh` 更新，两者不能互相代替。

推送版本标签后，Release 流程会验证标签与 `mygo.json` 版本一致，并上传 macOS Apple Silicon 桌面 DMG、网关二进制及 `SHA256SUMS`；任一构建失败时不会发布缺少客户端的 Release。桌面安装包当前仅提供 macOS arm64，其他平台仍需单独验收。

默认构建为 ad-hoc 签名。公开分发需在 `mygo.json` 配置 `macos.signingIdentity` 与 `macos.notarize.keychainProfile`，完成 Developer ID 签名、公证和票据附加；不在仓库保存证书或凭据。桌面客户端支持应用内自更新：`mygo.json` 的 `updates` 指向 GitHub Release（Ed25519 签名清单，密钥与网关 `gateway update` 共用），应用菜单「应用 → 检查更新…」手动检查，默认每日后台自动检查；Release 流水线用 `MYGO_UPDATER_PRIVATE_KEY` 产出签名更新包。签名私钥保存在本机 `~/Library/Application Support/mygo/update-keys/mygo-update.key` 与 GitHub Secrets——**丢失私钥意味着已安装的实例无法再更新到新版本**，请妥善备份。

复现问题时，可设置 `GATEWAY_DESKTOP_CONFIG_DIR` 到临时目录以隔离连接配置。应用图标保存在 `resources/icon.png`，由 MyGo 打包为各平台资源。
