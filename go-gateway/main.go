package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/archive"
	"github.com/nightgopher/easygateway/internal/assistant"
	"github.com/nightgopher/easygateway/internal/auth"
	"github.com/nightgopher/easygateway/internal/ccswitchconfig"
	"github.com/nightgopher/easygateway/internal/channel"
	"github.com/nightgopher/easygateway/internal/claudeconfig"
	"github.com/nightgopher/easygateway/internal/codexconfig"
	"github.com/nightgopher/easygateway/internal/config"
	"github.com/nightgopher/easygateway/internal/corspolicy"
	"github.com/nightgopher/easygateway/internal/gwkeys"
	"github.com/nightgopher/easygateway/internal/handlers"
	"github.com/nightgopher/easygateway/internal/memory"
	"github.com/nightgopher/easygateway/internal/metrics"
	"github.com/nightgopher/easygateway/internal/middleware"
	"github.com/nightgopher/easygateway/internal/piconfig"
	"github.com/nightgopher/easygateway/internal/provider"
	"github.com/nightgopher/easygateway/internal/providerpresets"
	"github.com/nightgopher/easygateway/internal/storage"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		if err := runCommand(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	logger := log.New(os.Stdout, "[gateway] ", log.LstdFlags)
	logger.Printf("Starting go-llm-gateway on port %d", cfg.Port)
	shutdownSignals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// 初始化指标收集器
	collector := metrics.NewCollector()

	// 初始化 SQLite 持久化存储
	dbPath := os.Getenv("PI_GO_DATA_DIR")
	if dbPath == "" {
		dbPath = "./data"
	}
	if err := os.MkdirAll(dbPath, 0700); err != nil {
		logger.Printf("Warning: failed to create data directory %s: %v, using memory only", dbPath, err)
	} else if err := os.Chmod(dbPath, 0700); err != nil {
		logger.Printf("Warning: failed to restrict data directory %s: %v", dbPath, err)
	}
	sqlitePath := dbPath + "/metrics.db"

	// 归档数据平面：当 ARCHIVE_ENABLED=true 时启用，否则使用 NoopStore。
	var archiver *archive.Archiver
	var archiveStore archive.Store = archive.NoopStore{}
	var memoryStore memory.Store = memory.NoopStore{}
	var sqliteStore *storage.SQLiteStore

	if store, err := storage.NewSQLiteStore(sqlitePath, logger); err != nil {
		logger.Printf("Warning: SQLite store init failed: %v, using memory only", err)
	} else {
		collector.SetStore(store)
		sqliteStore = store
		logger.Printf("SQLite metrics store: %s", sqlitePath)

		// 归档复用同一个 SQLite 文件（独立的 conversation_archives 表），
		// 避免引入新的外部依赖。
		if cfg.Archive.Enabled {
			archiveStore = store
		}

		// 记忆数据平面：MEMORY_ENABLED=true 时启用，否则 NoopStore。
		if cfg.Memory.Enabled {
			memoryStore = store
		}

		// 启动后台清理任务（每天清理 30 天前的指标数据 + 归档保留期外的对话）
		go startCleanupTask(shutdownSignals, store, cfg.Archive, cfg.StatsRetentionDays, logger)
	}

	// 构建归档器（即便 ARCHIVE_ENABLED=false 也是安全的 no-op）
	archiver = archive.NewArchiver(archiveStore, cfg.Archive, logger)

	// 初始化路由器
	router := provider.NewRouterWithCircuitConfig(logger, provider.CircuitBreakerConfig{
		FailureThreshold: cfg.CircuitFailureThreshold,
		RecoveryTimeout:  time.Duration(cfg.CircuitRecoverySeconds) * time.Second,
		SuccessThreshold: cfg.CircuitSuccessThreshold,
		RateLimitInitial: time.Duration(cfg.RateLimitInitialSeconds) * time.Second,
		RateLimitMax:     time.Duration(cfg.RateLimitMaxSeconds) * time.Second,
	})

	router.SetRoutingPolicy(provider.RoutingPolicy{FirstOutputTimeout: time.Duration(cfg.FirstOutputSeconds) * time.Second, ReasoningFirstOutputTimeout: time.Duration(cfg.ReasoningFirstOutputSeconds) * time.Second, StreamIdleTimeout: time.Duration(cfg.StreamIdleSeconds) * time.Second, AttemptTimeout: time.Duration(cfg.AttemptSeconds) * time.Second, FallbackWaitBudget: time.Duration(cfg.FallbackWaitSeconds) * time.Second})
	router.SetModelHealthTTL(time.Duration(cfg.ModelHealthTTLSeconds) * time.Second)

	// 账号渠道（第二类）：交互式登录取得凭据的渠道。必须先于 providers.yaml
	// 装载注册工厂，因为 type: channel 的条目在装载时就要构造 provider 实例。
	channelRegistry := channel.NewRegistry()
	setupChannelProviders(router, channelRegistry, logger)

	// Environment-backed integrations share their existing clients across reloads.
	nativeProviders := provider.NewRouter(logger)
	proxyURL := cfg.HTTPProxy
	if proxyURL == "" {
		proxyURL = os.Getenv("HTTPS_PROXY")
	}
	if proxyURL == "" {
		proxyURL = os.Getenv("https_proxy")
	}
	setupChatGPTProvider(nativeProviders, proxyURL, cfg.ChatGPTAuthFile, logger)
	if cfg.CopilotToken != "" {
		setupCopilotProviders(nativeProviders, cfg, logger)
	}
	if cfg.DeepVEnabled {
		setupDeepVProviders(nativeProviders, cfg, logger)
	}
	corsPolicy := corspolicy.New()
	configPath := "providers.yaml"
	reloader := provider.NewConfigReloader(router, configPath, externalConfigProviders(), func(candidate *provider.Router) error {
		candidate.IncludeMissingConfiguration(nativeProviders)
		return nil
	})
	reloader.SetCORSPolicy(corsPolicy)
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		setupDefaultProviders(router, cfg, logger)
		router.IncludeMissingConfiguration(nativeProviders)
	} else if _, err := reloader.Reload(); err != nil {
		logger.Fatalf("Failed to load providers.yaml: %v", err)
	}
	watchConfigReload(shutdownSignals, reloader, logger)
	startModelDoctor(shutdownSignals, router, time.Duration(cfg.ModelHealthIntervalSeconds)*time.Second, cfg.ModelHealthProbeAnonymous, logger)

	// 创建 Gin 引擎
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	// 按原始（未解码）路径路由：provider/模型名含「/」时，客户端编码成 %2F
	// 才能作为一个 :name 参数段匹配（默认解码后会被拆成多层导致 404）。
	// 参数值由 admin 组中间件统一解码。
	engine.UseRawPath = true
	engine.UnescapePathValues = false

	engine.Use(corsPolicy.Middleware())

	// gateway key 多密钥体系：发给客户端的受限身份（模型白名单 + token 限额）。
	// 文件不存在时为空集合，行为与从前一致；加载失败时禁用第二把锁并告警。
	gatewayKeys, gkErr := gwkeys.Load(filepath.Join(defaultGatewayHome(), "gateway-keys.json"))
	if gkErr != nil {
		logger.Printf("Warning: gateway keys disabled: %v", gkErr)
		gatewayKeys = nil
	}

	engine.Use(middleware.RequestID())
	engine.Use(middleware.Logging(logger, collector, gatewayKeys))
	engine.Use(auth.BearerAuthWithGatewayKeys(cfg.MasterKey, cfg.AdminToken, gatewayKeys, logger))

	// 管理端点使用独立的 admin auth
	adminAuth := auth.AdminAuth(cfg.MasterKey, cfg.AdminToken, logger)
	decodeAdminParams := func(c *gin.Context) {
		for i, p := range c.Params {
			if decoded, err := url.PathUnescape(p.Value); err == nil {
				c.Params[i].Value = decoded
			}
		}
		c.Next()
	}

	// 注册路由
	msgHandler := handlers.NewMessageHandler(router, logger)
	msgHandler.SetArchiver(archiver)
	chatHandler := handlers.NewChatCompletionsHandler(router, logger)
	chatHandler.SetArchiver(archiver)
	responsesHandler := handlers.NewResponsesHandler(router, logger)
	responsesHandler.SetArchiver(archiver)
	modelHandler := handlers.NewModelHandler(router, logger)
	passthroughHandler := handlers.NewPassthroughHandler(router, logger)
	healthHandler := handlers.NewHealthHandler(router, logger)
	adminHandler := handlers.NewAdminHandler(router, collector, logger, sqliteStore)
	configAdminHandler := handlers.NewConfigAdminHandler(reloader)
	piConfigHandler := handlers.NewPiConfigHandler(defaultGatewayHome(), defaultPiHome(), logger, router)
	clientConfigHandler := handlers.NewClientConfigHandler(defaultGatewayHome(), defaultZCodeHome(), defaultDshHome(), router, logger)
	ccswitchHandler := handlers.NewCCSwitchHandler(defaultCCSwitchHome(), cfg.Port, router, logger)
	archiveHandler := handlers.NewArchiveHandler(archiveStore, logger)
	memoryAdminHandler := handlers.NewMemoryAdminHandler(memoryStore, logger)
	memoryHandler := handlers.NewMemoryHandler(memoryStore, logger)
	skillsHandler := handlers.NewSkillsHandler(skillsRepoPath(), logger)
	skillsMarketHandler := handlers.NewSkillsMarketHandler(skillsMarketSkillsDir(), os.Getenv("SKILLS_MARKET_BASE_URL"), skillsMarketEnabled(), logger)
	skillsInstallJobsHandler := handlers.NewSkillsInstallJobsHandler(skillsMarketHandler.Client(), logger)
	fsHandler := handlers.NewFSHandler()
	channelAdminHandler := handlers.NewChannelAdminHandler(channelRegistry, logger)
	channelAdminHandler.SetRouter(router)

	// 常驻助理（EasyAgent SDK）：LLM 调用回环走网关自身，吃同一套路由与指标。
	var assistantHandler *handlers.AssistantHandler
	var assistantAdminHandler *handlers.AssistantAdminHandler
	if cfg.Assistant.Enabled {
		baseURL := cfg.Assistant.BaseURL
		if baseURL == "" {
			baseURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
		}
		apiKey := cfg.Assistant.APIKey
		if apiKey == "" {
			apiKey = cfg.MasterKey
		}
		resident, err := assistant.New(assistant.Config{
			Enabled: true, Model: cfg.Assistant.Model,
			ModelCatalog: func() []assistant.ModelOption { return assistantChatModelOptions(router) },
			BaseURL:      baseURL, APIKey: apiKey,
		}, memoryStore, logger)
		if err != nil {
			logger.Printf("Warning: assistant init failed: %v（继续运行，助理端点返回 503）", err)
		} else {
			// 用户自定义人设（agent_settings 表）优先于内置默认。
			if sqliteStore != nil {
				if custom, err := sqliteStore.GetSetting("assistant_system_prompt"); err == nil && custom != "" {
					resident.UpdateSystemPrompt(custom)
					logger.Printf("Assistant system prompt: user-customized (%d chars)", len(custom))
				}
			}
			assistantHandler = handlers.NewAssistantHandler(resident, logger)
			assistantAdminHandler = handlers.NewAssistantAdminHandler(resident, sqliteStore, logger)
			logger.Printf("Resident assistant enabled: model=%s via %s", cfg.Assistant.Model, baseURL)
		}
	}
	if assistantHandler == nil {
		assistantHandler = handlers.NewAssistantHandler(nil, logger)
	}
	if assistantAdminHandler == nil {
		// 助理未启用时 prompt 端点回 503，反馈端点仍可用（sqliteStore 为 nil 时也 503）。
		assistantAdminHandler = handlers.NewAssistantAdminHandler(nil, sqliteStore, logger)
	}

	engine.POST("/v1/messages", msgHandler.Handle)
	engine.POST("/v1/chat/completions", chatHandler.Handle)
	engine.POST("/v1/responses", responsesHandler.Handle)
	engine.POST("/v1/systemone", handlers.NewSystemOneHandler(router).Handle)
	engine.POST("/v1/embeddings", passthroughHandler.HandleEmbeddings)
	engine.POST("/v1/audio/transcriptions", passthroughHandler.HandleTranscriptions)
	engine.GET("/v1/models", modelHandler.Handle)
	engine.GET("/v1/memory", memoryHandler.HandleLookup)
	engine.POST("/v1/memory", memoryHandler.HandleDraft)
	engine.GET("/health", healthHandler.Handle)
	engine.GET("/readyz", healthHandler.HandleReady)
	// 兼容不带 /v1 前缀的客户端
	engine.POST("/messages", msgHandler.Handle)
	engine.POST("/chat/completions", chatHandler.Handle)
	engine.POST("/responses", responsesHandler.Handle)
	engine.POST("/systemone", handlers.NewSystemOneHandler(router).Handle)
	engine.POST("/embeddings", passthroughHandler.HandleEmbeddings)
	engine.POST("/audio/transcriptions", passthroughHandler.HandleTranscriptions)
	engine.GET("/models", modelHandler.Handle)

	// 管理面板 API
	admin := engine.Group("/admin")
	admin.Use(adminAuth, decodeAdminParams)
	{
		admin.GET("/dashboard", adminHandler.HandleDashboard)
		admin.GET("/providers", adminHandler.HandleProviders)
		admin.PATCH("/providers/:name", adminHandler.HandleUpdateProvider)
		admin.POST("/providers/:name/reset", adminHandler.HandleResetProvider)
		admin.POST("/providers/:name/health-check", adminHandler.HandleCheckProvider)
		admin.GET("/models", adminHandler.HandleModels)
		admin.POST("/models/doctor", adminHandler.HandleDoctor)
		admin.PUT("/models/:model", adminHandler.HandleUpdateModel)
		admin.GET("/routes", adminHandler.HandleRoutes)
		admin.PUT("/routes/:model", adminHandler.HandleUpdateRoute)
		admin.GET("/logs", adminHandler.HandleLogs)
		admin.GET("/health", adminHandler.HandleHealth)
		admin.GET("/config", adminHandler.HandleConfig)
		admin.GET("/config/status", configAdminHandler.HandleStatus)
		admin.GET("/config/cors", configAdminHandler.HandleCORS)
		admin.PUT("/config/cors", configAdminHandler.HandleUpdateCORS)
		admin.POST("/config/check", configAdminHandler.HandleCheck)
		admin.POST("/config/reload", configAdminHandler.HandleReload)
		admin.GET("/pi", piConfigHandler.HandleStatus)
		admin.POST("/pi/sync", piConfigHandler.HandleSync)
		admin.GET("/zcode", clientConfigHandler.HandleZCodeStatus)
		admin.POST("/zcode/sync", clientConfigHandler.HandleZCodeSync)
		admin.GET("/harness", clientConfigHandler.HandleHarnessStatus)
		admin.POST("/harness/sync", clientConfigHandler.HandleHarnessSync)
		admin.GET("/ccswitch", ccswitchHandler.HandleScan)
		admin.POST("/ccswitch/import", ccswitchHandler.HandleImport)
		admin.POST("/ccswitch/remote-scan", ccswitchHandler.HandleRemoteScan)
		admin.POST("/ccswitch/remote-import", ccswitchHandler.HandleRemoteImport)
		admin.GET("/stats", adminHandler.HandleStats)
		admin.GET("/stats/history", adminHandler.HandleStatsHistory)
		admin.GET("/usage", adminHandler.HandleUsage)
		modelsPruneHandler := handlers.NewModelsPruneHandler(router, reloader, configPath, logger)
		admin.POST("/models/prune", modelsPruneHandler.HandlePrune)
		gatewayKeyHandler := handlers.NewGatewayKeyHandler(gatewayKeys, logger)
		admin.GET("/gateway-keys", gatewayKeyHandler.HandleList)
		admin.POST("/gateway-keys", gatewayKeyHandler.HandleCreate)
		admin.POST("/gateway-keys/:id/rotate", gatewayKeyHandler.HandleRotate)
		admin.PATCH("/gateway-keys/:id", gatewayKeyHandler.HandleSetEnabled)
		admin.DELETE("/gateway-keys/:id", gatewayKeyHandler.HandleRemove)
		admin.GET("/archives", archiveHandler.HandleList)
		admin.GET("/archives/export", archiveHandler.HandleExport)
		admin.DELETE("/archives", archiveHandler.HandleDeleteBefore)
		admin.DELETE("/archives/:id", archiveHandler.HandleDeleteOne)
		admin.GET("/memories", memoryAdminHandler.HandleList)
		admin.POST("/memories", memoryAdminHandler.HandleCreate)
		admin.POST("/memories/:id/confirm", memoryAdminHandler.HandleConfirm)
		admin.POST("/memories/:id/retire", memoryAdminHandler.HandleRetire)
		admin.DELETE("/memories/:id", memoryAdminHandler.HandleDelete)
		admin.POST("/assistant/chat", assistantHandler.HandleChat)
		admin.GET("/assistant/models", assistantHandler.HandleModels)
		admin.GET("/assistant/prompt", assistantAdminHandler.HandleGetPrompt)
		admin.PUT("/assistant/prompt", assistantAdminHandler.HandlePutPrompt)
		admin.POST("/assistant/feedback", assistantAdminHandler.HandleAddFeedback)
		admin.GET("/assistant/feedback", assistantAdminHandler.HandleListFeedback)
		admin.GET("/skills", skillsHandler.HandleStatus)
		admin.GET("/skills/:id", skillsHandler.HandleDetail)
		admin.PUT("/skills/config", skillsHandler.HandleUpdateConfig)
		admin.POST("/skills/sync", skillsHandler.HandleSync)
		admin.GET("/skills/market/browse", skillsMarketHandler.HandleBrowse)
		admin.GET("/skills/market/sections", skillsMarketHandler.HandleSections)
		admin.GET("/skills/market/installed", skillsMarketHandler.HandleInstalled)
		admin.POST("/skills/market/install", skillsMarketHandler.HandleInstall)
		admin.POST("/skills/market/install-stream", skillsMarketHandler.HandleInstallStream)
		admin.POST("/skills/market/install-jobs", skillsInstallJobsHandler.HandleInstallJobStart)
		admin.GET("/skills/market/install-jobs", skillsInstallJobsHandler.HandleInstallJobList)
		admin.GET("/skills/market/install-jobs/:id", skillsInstallJobsHandler.HandleInstallJobGet)
		admin.POST("/skills/market/uninstall", skillsMarketHandler.HandleUninstall)
		admin.GET("/fs/dirs", fsHandler.HandleDirs)
		admin.GET("/channels", channelAdminHandler.HandleList)
		admin.GET("/channels/:channel", channelAdminHandler.HandleStatus)
		admin.POST("/channels/:channel/login", channelAdminHandler.HandleLoginStart)
		admin.GET("/channels/:channel/login/:id", channelAdminHandler.HandleLoginPoll)
		admin.DELETE("/channels/:channel/login/:id", channelAdminHandler.HandleLoginCancel)
		admin.DELETE("/channels/:channel", channelAdminHandler.HandleLogout)
		admin.DELETE("/channels/:channel/accounts/:index", channelAdminHandler.HandleRemoveAccount)
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	logger.Printf("Server listening on %s", addr)
	server := &http.Server{
		Addr:              addr,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	if err := serveGateway(shutdownSignals, server, server.ListenAndServe, 15*time.Second); err != nil {
		logger.Printf("Server stopped: %v", err)
	}
	// 所有正常请求已提交终态；异步指标和归档共用一个有界清理窗口，数据库最后关闭。
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelCleanup()
	var persistenceStore storeCloser
	if sqliteStore != nil {
		persistenceStore = sqliteStore
	}
	closeGatewayPersistence(cleanupCtx, collector, archiver, persistenceStore, logger)
}

func runCommand(args []string) error {
	switch args[0] {
	case "version":
		if len(args) != 1 {
			return fmt.Errorf("usage: llm-gateway version")
		}
		_, err := fmt.Fprintln(os.Stdout, version)
		return err
	case "config":
		return runConfigCommand(args[1:])
	case "doctor":
		return doctorCommand(args[1:])
	case "benchmark":
		return benchmarkModels(args[1:])
	case "setup":
		if len(args) < 2 || args[1] != "pi" {
			return fmt.Errorf("usage: llm-gateway setup pi [--endpoint URL] [--dry-run]")
		}
		return setupPi(args[2:])
	case "claude":
		return claudeCommand(args[1:])
	case "codex":
		return codexCommand(args[1:])
	case "provider":
		return providerCommand(args[1:])
	case "migrate":
		if len(args) < 2 || args[1] != "cc-switch" {
			return fmt.Errorf("usage: llm-gateway migrate cc-switch [--home PATH] [--include a,b] [--dry-run]")
		}
		return migrateCCSwitch(args[2:])
	case "auth":
		if len(args) != 2 || args[1] != "print-master-key" {
			return fmt.Errorf("usage: llm-gateway auth print-master-key")
		}
		key, err := piconfig.MasterKey(defaultGatewayHome())
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(os.Stdout, key)
		return err
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func setupPi(args []string) error {
	flags := flag.NewFlagSet("setup pi", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	endpoint := flags.String("endpoint", "", "Gateway base URL (defaults to the local gateway)")
	dryRun := flags.Bool("dry-run", false, "Print the resulting models.json without writing it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	merged, path, err := piconfig.Setup(piconfig.SetupOptions{
		GatewayHome: defaultGatewayHome(),
		PiHome:      defaultPiHome(),
		Endpoint:    *endpoint,
		DryRun:      *dryRun,
	})
	if err != nil {
		return err
	}
	if *dryRun {
		_, err = os.Stdout.Write(merged)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Would write: %s\n", path)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Pi configured: %s\nSelect llm-gateway/coding in Pi with /model.\n", path)
	return nil
}

// clientAction 解析 claude/codex 接管子命令的公共形态：
// 无参数 = status，default = 还原，其余视为要接入的模型名。
type clientAction struct {
	Model    string
	Status   bool
	Restore  bool
	Endpoint string
	DryRun   bool
}

func parseClientAction(name string, args []string) (clientAction, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	action := clientAction{}
	flags.StringVar(&action.Endpoint, "endpoint", "", "Gateway base URL (defaults to the local gateway)")
	flags.BoolVar(&action.DryRun, "dry-run", false, "Print the resulting config without writing it")
	if err := flags.Parse(args); err != nil {
		return action, err
	}
	rest := flags.Args()
	switch {
	case len(rest) == 0:
		action.Status = true
	case len(rest) == 1 && rest[0] == "status":
		action.Status = true
	case len(rest) == 1 && rest[0] == "default":
		action.Restore = true
	case len(rest) == 1:
		action.Model = rest[0]
	default:
		return action, fmt.Errorf("usage: llm-gateway %s [status|default|<model>] [--endpoint URL] [--dry-run]", name)
	}
	return action, nil
}

// gatewayEndpointAndKey 解析网关地址与认证 token（输出不回显 key）。
func gatewayEndpointAndKey(endpointFlag string) (endpoint, masterKey string, err error) {
	endpoint, err = piconfig.ResolveEndpoint(endpointFlag, defaultGatewayHome())
	if err != nil {
		return "", "", err
	}
	masterKey, err = piconfig.MasterKey(defaultGatewayHome())
	if err != nil {
		return "", "", err
	}
	return endpoint, masterKey, nil
}

func claudeCommand(args []string) error {
	action, err := parseClientAction("claude", args)
	if err != nil {
		return err
	}
	home := defaultClaudeHome()
	switch {
	case action.Status:
		status, err := claudeconfig.ReadStatus(home)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "settings: %s (exists: %v)\n", status.Path, status.FileExists)
		fmt.Fprintf(os.Stdout, "base_url: %s\n", status.BaseURL)
		fmt.Fprintf(os.Stdout, "auth_token_set: %v, api_key_set: %v\n", status.AuthTokenSet, status.APIKeySet)
		fmt.Fprintf(os.Stdout, "model: %s\n", status.Model)
		fmt.Fprintf(os.Stdout, "takeover_snapshot: %v\n", status.SnapshotExists)
		return nil
	case action.Restore:
		path, err := claudeconfig.Restore(home, action.DryRun)
		if err != nil {
			return err
		}
		if action.DryRun {
			fmt.Fprintf(os.Stdout, "Would restore: %s\n", path)
		} else {
			fmt.Fprintf(os.Stdout, "Claude Code restored: %s\n", path)
		}
		return nil
	}
	endpoint, masterKey, err := gatewayEndpointAndKey(action.Endpoint)
	if err != nil {
		return err
	}
	content, path, err := claudeconfig.Takeover(claudeconfig.Options{
		ClaudeHome: home,
		Endpoint:   endpoint,
		MasterKey:  masterKey,
		Model:      action.Model,
		DryRun:     action.DryRun,
	})
	if err != nil {
		return err
	}
	if action.DryRun {
		_, err = os.Stdout.Write(content)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Would write: %s\n", path)
		return nil
	}
	modelNote := ""
	if action.Model != "" {
		modelNote = fmt.Sprintf(" (model %s)", action.Model)
	}
	fmt.Fprintf(os.Stdout, "Claude Code taken over%s: %s\nUndo any time with: llm-gateway claude default\n", modelNote, path)
	return nil
}

func codexCommand(args []string) error {
	action, err := parseClientAction("codex", args)
	if err != nil {
		return err
	}
	home := defaultCodexHome()
	switch {
	case action.Status:
		status, err := codexconfig.ReadStatus(home)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "config: %s (exists: %v)\n", status.Path, status.FileExists)
		fmt.Fprintf(os.Stdout, "model_provider: %s, model: %s\n", status.ModelProvider, status.Model)
		fmt.Fprintf(os.Stdout, "gateway base_url: %s\n", status.BaseURL)
		fmt.Fprintf(os.Stdout, "takeover_snapshot: %v\n", status.SnapshotExists)
		return nil
	case action.Restore:
		path, err := codexconfig.Restore(home, action.DryRun)
		if err != nil {
			return err
		}
		if action.DryRun {
			fmt.Fprintf(os.Stdout, "Would restore: %s\n", path)
		} else {
			fmt.Fprintf(os.Stdout, "Codex CLI restored: %s\n", path)
		}
		return nil
	}
	endpoint, masterKey, err := gatewayEndpointAndKey(action.Endpoint)
	if err != nil {
		return err
	}
	content, path, err := codexconfig.Takeover(codexconfig.Options{
		CodexHome: home,
		Endpoint:  endpoint,
		MasterKey: masterKey,
		Model:     action.Model,
		DryRun:    action.DryRun,
	})
	if err != nil {
		return err
	}
	model := action.Model
	if model == "" {
		model = codexconfig.DefaultModel
	}
	if action.DryRun {
		_, err = os.Stdout.Write(content)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Would write: %s\n", path)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Codex CLI taken over (model %s): %s\nUndo any time with: llm-gateway codex default\n", model, path)
	return nil
}

func defaultClaudeHome() string {
	if home := os.Getenv("CLAUDE_HOME"); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ".claude"
	}
	return filepath.Join(userHome, ".claude")
}

func defaultCodexHome() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(userHome, ".codex")
}

func defaultGatewayHome() string {
	if home := os.Getenv("LLM_GATEWAY_HOME"); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ".llm-gateway"
	}
	return filepath.Join(userHome, ".llm-gateway")
}

func defaultZCodeHome() string {
	if home := os.Getenv("ZCODE_HOME"); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ".zcode"
	}
	return filepath.Join(userHome, ".zcode")
}

func defaultDshHome() string {
	if home := os.Getenv("DSH_HOME"); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ".dsh"
	}
	return filepath.Join(userHome, ".dsh")
}

// providerCommand 快速接入新供应商：
//
//	gateway provider presets                    列出内置 preset
//	gateway provider add <preset> <key> --models m1,m2
//
// 写入 providers.yaml（文本级插入，注释不动）与同目录 .env，随后用
// SIGHUP / POST /admin/config/reload 热重载即可生效，无需重启。
func providerCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: llm-gateway provider <presets|add> ...")
	}
	switch args[0] {
	case "presets":
		for _, preset := range providerpresets.All() {
			note := ""
			if preset.Note != "" {
				note = "  (" + preset.Note + ")"
			}
			fmt.Fprintf(os.Stdout, "%-14s %s  env: %s%s\n", preset.Name, preset.URL, preset.APIKeyEnv, note)
		}
		return nil
	case "add":
		return providerAddCommand(args[1:])
	default:
		return fmt.Errorf("unknown provider subcommand %q (presets | add)", args[0])
	}
}

func providerAddCommand(args []string) error {
	flags := flag.NewFlagSet("provider add", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	modelsRaw := flags.String("models", "", "Comma-separated model IDs to register (required)")
	configPath := flags.String("config", "", "providers.yaml path (defaults to $LLM_GATEWAY_HOME/providers.yaml, then ./providers.yaml)")
	dryRun := flags.Bool("dry-run", false, "Print the changes without writing them")
	if err := flags.Parse(args); err != nil {
		return err
	}
	rest := flags.Args()
	if len(rest) != 2 {
		return fmt.Errorf("usage: llm-gateway provider add <preset> <api-key> [--models m1,m2] [--config PATH] [--dry-run]")
	}
	preset, err := providerpresets.Lookup(rest[0])
	if err != nil {
		return err
	}
	models := splitAndTrim(*modelsRaw, ",")
	if len(models) == 0 {
		return fmt.Errorf("--models is required (comma-separated model IDs, e.g. --models %s-chat,%s-reasoner)", preset.Name, preset.Name)
	}

	path := resolveProvidersPath(*configPath)
	if path == "" {
		return fmt.Errorf("providers.yaml not found; pass --config PATH (the running gateway loads it from its working directory)")
	}
	config, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if providerpresets.HasProvider(string(config), preset.Name) {
		return fmt.Errorf("provider %q already exists in %s", preset.Name, path)
	}
	block := providerpresets.RenderYAML(providerpresets.Options{Preset: preset, APIKey: rest[1], Models: models})
	nextConfig := providerpresets.InsertIntoConfig(string(config), block)

	envPath := filepath.Join(filepath.Dir(path), ".env")
	env, err := os.ReadFile(envPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", envPath, err)
	}
	nextEnv := providerpresets.UpsertEnvFile(string(env), preset.APIKeyEnv, rest[1])

	if *dryRun {
		fmt.Fprintf(os.Stdout, "--- %s ---\n%s", path, nextConfig)
		fmt.Fprintf(os.Stdout, "--- %s ---\n%s", envPath, nextEnv)
		return nil
	}
	if err := os.WriteFile(path, []byte(nextConfig), 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.WriteFile(envPath, []byte(nextEnv), 0600); err != nil {
		return fmt.Errorf("write %s: %w", envPath, err)
	}
	fmt.Fprintf(os.Stdout, "Provider %s added: %s\n", preset.Name, path)
	fmt.Fprintf(os.Stdout, "API key saved to %s (%s)\n", envPath, preset.APIKeyEnv)
	fmt.Fprintf(os.Stdout, "Models: %s\n", strings.Join(models, ", "))
	fmt.Fprintf(os.Stdout, "Apply without restart: kill -HUP <gateway pid> or POST /admin/config/reload\n")
	return nil
}

// resolveProvidersPath 解析 providers.yaml 位置：显式 --config 优先，
// 其次网关主目录，最后当前目录（运行中的网关从其工作目录加载）。
func resolveProvidersPath(explicit string) string {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else {
		candidates = append(candidates, filepath.Join(defaultGatewayHome(), "providers.yaml"), "providers.yaml")
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func splitAndTrim(raw, sep string) []string {
	var out []string
	for _, part := range strings.Split(raw, sep) {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func defaultCCSwitchHome() string {
	if home := os.Getenv("CCSWITCH_HOME"); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ".cc-switch"
	}
	return filepath.Join(userHome, ".cc-switch")
}

// migrateCCSwitch 把 cc-switch 里已配置的供应商迁移进当前目录的
// providers.yaml 与 .env（与服务进程装载的是同一组文件）。
func migrateCCSwitch(args []string) error {
	flags := flag.NewFlagSet("migrate cc-switch", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	home := flags.String("home", "", "cc-switch 主目录（默认 ~/.cc-switch）")
	include := flags.String("include", "", "只导入这些 cc-switch 条目（逗号分隔名称，默认全部可导入项）")
	dryRun := flags.Bool("dry-run", false, "只预览，不写入 providers.yaml 与 .env")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	includeSet := map[string]bool{}
	for _, name := range strings.Split(*include, ",") {
		if name = strings.TrimSpace(name); name != "" {
			includeSet[name] = true
		}
	}

	result, err := ccswitchconfig.Import(ccswitchconfig.ImportOptions{
		Home:          *home,
		ProvidersYAML: "providers.yaml",
		EnvFile:       ".env",
		GatewayPort:   cwdGatewayPort(),
		Include:       includeSet,
		DryRun:        *dryRun,
	})
	if err != nil {
		return err
	}

	imported := map[string]bool{}
	for _, name := range result.Imported {
		imported[name] = true
	}
	keyNote := "（已写入 .env）"
	if *dryRun {
		keyNote = "（将写入 .env）"
	}
	for _, candidate := range result.Candidates {
		switch {
		case imported[candidate.Name]:
			fmt.Printf("✔ %s → %s %s\n   模型: %s\n   密钥: %s%s\n",
				candidate.CCName, candidate.Type, candidate.URL,
				strings.Join(candidate.Models, ", "), candidate.APIKeyEnv, keyNote)
		case candidate.Status == "importable":
			fmt.Printf("· %s 可导入（未被 --include 选中，未写入）\n", candidate.CCName)
		default:
			fmt.Printf("✘ %s：%s\n", candidate.CCName, candidate.Reason)
		}
	}
	if result.Wrote {
		fmt.Printf("\n已导入 %d 个供应商：%s\n密钥写入：%s\n", len(result.Imported), result.ProvidersYAML, result.EnvFile)
		if len(result.UpdatedKeys) > 0 {
			fmt.Printf("密钥已更新（.env 覆写并即时生效）：%s\n", strings.Join(result.UpdatedKeys, ", "))
		}
		fmt.Printf("（原文件备份为 .pre-import.bak，重启网关后仍从 providers.yaml 装载）\n")
		return nil
	}
	if len(result.Imported) > 0 {
		fmt.Printf("\ndry-run：%d 个条目将写入 %s 与 %s（未修改任何文件）\n",
			len(result.Imported), result.ProvidersYAML, result.EnvFile)
	} else {
		fmt.Println("\n没有可导入的条目。")
	}
	return nil
}

// cwdGatewayPort 返回网关自身端口：PORT 环境变量 → cwd .env → 4001。
// 迁移用它排除指向本网关的 cc-switch 条目。
func cwdGatewayPort() int {
	if port := os.Getenv("PORT"); port != "" {
		if n, err := strconv.Atoi(port); err == nil {
			return n
		}
	}
	raw, err := os.ReadFile(".env")
	if err != nil {
		return 4001
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "PORT="); ok {
			if n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(value), `"'`)); err == nil {
				return n
			}
		}
	}
	return 4001
}

// skillsRepoPath 返回技能仓库（custom-skills）根目录。未设置 SKILLS_REPO_PATH
// 时技能面板保持未配置状态（configured=false），不影响网关其他功能。
func skillsRepoPath() string {
	return os.Getenv("SKILLS_REPO_PATH")
}

// assistantChatModelIDs 从模型目录里选出助理聊天可用的模型：具备 text
// 能力、且不含单一用途能力（embedding 向量化、audio_transcription 语音
// 转写）。这些模型无法用于对话，不应出现在助理的可选清单里。
func assistantChatModelIDs(infos []provider.ModelInfo) []string {
	ids := []string{}
	for _, info := range infos {
		hasText, singlePurpose := false, false
		for _, capability := range info.Capabilities {
			switch capability {
			case "text":
				hasText = true
			case "embedding", "audio_transcription":
				singlePurpose = true
			}
		}
		if hasText && !singlePurpose {
			ids = append(ids, info.ID)
		}
	}
	return ids
}

// skillsMarketEnabled 默认启用技能市场面板；SKILLS_MARKET_ENABLED=false 关闭。
func skillsMarketEnabled() bool {
	return os.Getenv("SKILLS_MARKET_ENABLED") != "false"
}

// skillsMarketSkillsDir 返回市场技能安装目录，默认 ~/.agents/skills，与
// EasyAgent 同源：两边共用安装清单标记，互相识别对方安装的技能。
func skillsMarketSkillsDir() string {
	if dir := os.Getenv("SKILLS_MARKET_SKILLS_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents", "skills")
}

func defaultPiHome() string {
	if home := os.Getenv("PI_HOME"); home != "" {
		return home
	}
	if home := os.Getenv("EA_HOME"); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ".easyagent"
	}
	// pi-go 改名 easyagent 后，客户端数据目录默认从 ~/.pi 迁到 ~/.easyagent。
	// 同步模型清单要跟着新目录走；旧 ~/.pi 只在仍存有 models.json 时兜底（未迁移的环境）。
	easyagentHome := filepath.Join(userHome, ".easyagent")
	if _, err := os.Stat(filepath.Join(easyagentHome, "models.json")); err == nil {
		return easyagentHome
	}
	legacyPiHome := filepath.Join(userHome, ".pi")
	if _, err := os.Stat(filepath.Join(legacyPiHome, "agent", "models.json")); err == nil {
		return legacyPiHome
	}
	return easyagentHome
}

// setupDefaultProviders 设置默认提供商（当 providers.yaml 不存在时使用）。
// 与 providers.yaml 保持同一套命名规则：provider 实例名即上游模型 ID，不设别名，
// 对外只暴露 coding 一条持续降级的链。
func setupDefaultProviders(router *provider.Router, cfg *config.Config, logger *log.Logger) {
	var codingProviders []string

	if cfg.GLMAPIKey != "" {
		glmConfig := &provider.Config{
			URL:    "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
			APIKey: cfg.GLMAPIKey,
		}
		for _, modelID := range []string{"glm-5.3", "glm-5.3-flash"} {
			glmConfig.Name = modelID
			// 每个模型绑定自己的上游模型名，避免共用 provider 时发错模型。
			router.RegisterProvider(modelID, provider.NewBoundModelProviderWrapper(
				provider.NewOpenAIProvider(glmConfig), modelID))
			router.RegisterChain(modelID, []string{modelID})
			codingProviders = append(codingProviders, modelID)
		}
	}

	if len(codingProviders) == 0 {
		logger.Printf("Warning: no GLM API key configured; coding chain has no providers")
		return
	}
	router.RegisterChain("coding", codingProviders)
}

// setupDeepVProviders 设置 DeepV Server 提供商（EasyCode/DeepVCode 的聚合后端）。
// 上游模型使用绑定的模型 ID；模型与独立路由在此注册。
func setupDeepVProviders(router *provider.Router, cfg *config.Config, logger *log.Logger) {
	workDir := cfg.DeepVWorkDir
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	deepvURL := "https://api-code.deepvlab.ai/v1/chat/messages"

	// provider 实例名用上游模型 ID。注意智谱同样提供 glm-5.3-flash，两家实例名
	// 会撞车并互相覆盖，因此 DeepV 这几个统一加 deepv- 前缀区分。
	router.RegisterProvider("deepseek-flash", provider.NewDeepVProvider(&provider.Config{
		Name: "deepseek-flash",
		URL:  deepvURL,
	}, workDir, "deepseek-flash"))
	router.RegisterProvider("deepv-glm-5.3-flash", provider.NewDeepVProvider(&provider.Config{
		Name: "deepv-glm-5.3-flash",
		URL:  deepvURL,
	}, workDir, "glm-5.3-flash"))

	router.RegisterChain("deepseek-flash", []string{"deepseek-flash"})
	// DeepV 上游模型名是 deepseek-flash，历史客户端用的是 deepseek-v4.1-flash。
	// 主名对齐上游，同时保留旧名作为兼容入口。
	router.RegisterChain("deepseek-v4.1-flash", []string{"deepseek-flash"})
	// DeepV 的 glm-5.3-flash 独立成一个链名，避免与智谱同名实例混淆；
	// 客户端主要经 coding 链间接用到它。
	router.RegisterChain("deepv-glm-5.3-flash", []string{"deepv-glm-5.3-flash"})

	// DeepV 上游按 单请求总量（输入 + max_output_tokens）≤ 200000 校验。
	// 目录里声明合计留有余量的上限，客户端读到后不会再发出超出配额的
	// 输出预算（此前 ZCode 默认发 max_output_tokens=384000，新会话也会
	// 被上游以 402 拒绝）。
	// 两个对外模型名（上游名 + 兼容旧名）共用同一份元数据。
	for _, modelID := range []string{"deepseek-flash", "deepseek-v4.1-flash"} {
		router.RegisterModel(provider.ModelInfo{
			ID:              modelID,
			Provider:        "deepseek-flash",
			Capabilities:    []string{"text", "vision", "tool_calling", "streaming", "reasoning"},
			InputModalities: []string{"text", "image"},
			MaxInputTokens:  160000,
			MaxOutputTokens: 32000,
		})
	}
	// DeepV 的 glm-5.3-flash 用限定名登记，避免覆盖智谱同名模型的目录条目
	// （智谱那一档由 providers.yaml 负责声明）。
	router.RegisterModel(provider.ModelInfo{
		ID:              "deepv-glm-5.3-flash",
		Provider:        "deepv-glm-5.3-flash",
		Capabilities:    []string{"text", "vision", "tool_calling", "streaming", "reasoning"},
		InputModalities: []string{"text", "image"},
		MaxInputTokens:  160000,
		MaxOutputTokens: 32000,
	})

	// 独立模型入口沿用 DeepV 的请求总量预算，不改变 coding 降级顺序。
	for _, modelID := range []string{"claude-haiku-5-5", "mimo-v2.6-pro"} {
		router.RegisterProvider(modelID, provider.NewDeepVProvider(&provider.Config{
			Name: modelID,
			URL:  deepvURL,
		}, workDir, modelID))
		router.RegisterChain(modelID, []string{modelID})
		router.RegisterModel(provider.ModelInfo{
			ID:              modelID,
			Provider:        modelID,
			Capabilities:    []string{"text", "vision", "tool_calling", "streaming", "reasoning"},
			InputModalities: []string{"text", "image"},
			MaxInputTokens:  160000,
			MaxOutputTokens: 32000,
		})
	}

	logger.Printf("DeepV Server enabled, workdir=%s", workDir)
}

// setupCopilotProviders 设置 GitHub Copilot 提供商
func setupCopilotProviders(router *provider.Router, cfg *config.Config, logger *log.Logger) {
	// 可选：刷新 token
	if cfg.CopilotGithubToken != "" {
		newToken, expiresAt, err := provider.RefreshCopilotToken(cfg.CopilotGithubToken)
		if err != nil {
			logger.Printf("Warning: failed to refresh Copilot token: %v, using existing token", err)
		} else {
			cfg.CopilotToken = newToken
			logger.Printf("Copilot token refreshed, expires at %d", expiresAt)
		}
	}

	copilotConfig := &provider.Config{
		Name:   "copilot",
		URL:    "", // 会从 token 解析
		APIKey: cfg.CopilotToken,
	}

	router.RegisterProvider("copilot", provider.NewCopilotProvider(copilotConfig, cfg.CopilotGithubToken))
	router.RegisterChain("copilot", []string{"copilot"})

	logger.Printf("GitHub Copilot enabled")
}

// setupChatGPTProvider 设置 ChatGPT Codex 提供商（使用 ChatGPT Plus/Pro OAuth token）。
func setupChatGPTProvider(router *provider.Router, proxyURL, authPath string, logger *log.Logger) {
	chatgptProvider := provider.NewChatGPTProviderWithAuthPath(proxyURL, authPath)
	if !chatgptProvider.IsHealthy(context.Background()) {
		logger.Printf("ChatGPT Codex provider disabled: no usable OAuth credential (run codex login or Pi /login openai-codex)")
		return
	}
	router.RegisterProvider("chatgpt", chatgptProvider)

	for _, model := range provider.ChatGPTModelCatalog() {
		router.RegisterChain(model.ID, []string{"chatgpt"})
		router.RegisterModel(provider.ModelInfo{
			ID:              model.ID,
			Provider:        "chatgpt",
			Protocol:        "responses",
			Capabilities:    model.Capabilities,
			InputModalities: model.InputModalities,
			MaxInputTokens:  model.MaxInputTokens,
			MaxOutputTokens: model.MaxOutputTokens,
		})
	}

	connection := "direct"
	if proxyURL != "" {
		connection = "proxy"
	}
	logger.Printf("ChatGPT Codex provider enabled (auth: %s, connection: %s)", chatgptProvider.AuthPath(), connection)
}

func hasProvider(names []string, wanted string) bool {
	for _, name := range names {
		if name == wanted {
			return true
		}
	}
	return false
}

// startCleanupTask 启动后台清理任务
func startCleanupTask(ctx context.Context, store *storage.SQLiteStore, archiveCfg archive.Config, statsRetentionDays int, logger *log.Logger) {
	// 首次启动时清理
	if err := store.Cleanup(30, statsRetentionDays); err != nil {
		logger.Printf("Cleanup error: %v", err)
	}
	if archiveCfg.Enabled && archiveCfg.RetentionDays > 0 {
		if err := store.CleanupArchives(archiveCfg.RetentionDays); err != nil {
			logger.Printf("Archive cleanup error: %v", err)
		}
	}

	// 每天凌晨 3 点清理
	for {
		now := time.Now()
		tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 3, 0, 0, 0, now.Location())
		duration := tomorrow.Sub(now)
		logger.Printf("Next cleanup in %v", duration)
		timer := time.NewTimer(duration)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if err := store.Cleanup(30, statsRetentionDays); err != nil {
			logger.Printf("Cleanup error: %v", err)
		}
		if archiveCfg.Enabled && archiveCfg.RetentionDays > 0 {
			if err := store.CleanupArchives(archiveCfg.RetentionDays); err != nil {
				logger.Printf("Archive cleanup error: %v", err)
			}
		}
	}
}

func assistantChatModelOptions(router *provider.Router) []assistant.ModelOption {
	infos := router.ListModelInfos()
	byID := make(map[string]provider.ModelInfo, len(infos))
	for _, info := range infos {
		byID[info.ID] = info
	}
	options := []assistant.ModelOption{}
	for _, id := range assistantChatModelIDs(infos) {
		if router.ModelUsable(id) {
			options = append(options, assistant.ModelOption{ID: id, Provider: byID[id].Provider, Channel: router.ModelChannel(id)})
		}
	}
	return options
}
