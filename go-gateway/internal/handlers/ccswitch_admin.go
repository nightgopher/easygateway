package handlers

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/nightgopher/easygateway/internal/ccswitchconfig"
	"github.com/nightgopher/easygateway/internal/provider"
)

// CCSwitchHandler 把 cc-switch 里已配置的供应商迁移进网关：扫描预览
// （GET）与导入（POST）。导入会写入网关 cwd 的 providers.yaml 与 .env
// 并把新 provider 注册进运行中的 Router，即时生效、无需重启；cc-switch
// 侧数据只读不动，密钥不回显到响应。
type CCSwitchHandler struct {
	home          string // cc-switch 主目录默认值，请求可用 home 参数覆盖
	providersYAML string
	envFile       string
	gatewayPort   int
	router        *provider.Router
	logger        *log.Logger
}

// NewCCSwitchHandler 创建 cc-switch 迁移 handler。providersYAML/envFile 是
// 网关进程装载时使用的同一路径（cwd 下的 providers.yaml 与 .env）。
func NewCCSwitchHandler(home string, gatewayPort int, router *provider.Router, logger *log.Logger) *CCSwitchHandler {
	return &CCSwitchHandler{
		home:          home,
		providersYAML: "providers.yaml",
		envFile:       ".env",
		gatewayPort:   gatewayPort,
		router:        router,
		logger:        logger,
	}
}

func (h *CCSwitchHandler) effectiveHome(requested string) string {
	if requested != "" {
		return requested
	}
	return h.home
}

// HandleScan GET /admin/ccswitch?home=PATH 预览可迁移条目，不写入任何文件。
func (h *CCSwitchHandler) HandleScan(c *gin.Context) {
	home := h.effectiveHome(strings.TrimSpace(c.Query("home")))
	scan, err := ccswitchconfig.Scan(home)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	context, err := ccswitchconfig.LoadContext(h.providersYAML, h.envFile, h.gatewayPort)
	if err != nil {
		h.logger.Printf("load gateway context for cc-switch scan: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	candidates := ccswitchconfig.Convert(scan.Entries, context)

	importable := 0
	for _, candidate := range candidates {
		if candidate.Status == "importable" {
			importable++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"home":       home,
		"source":     scan.Source,
		"candidates": candidates,
		"importable": importable,
		"skipped":    len(candidates) - importable,
	})
}

// HandleImport POST /admin/ccswitch/import {"home":"...","include":["名称"]}
// 导入可迁移条目：写入前轮转备份，写入后注册进运行中的 Router。
func (h *CCSwitchHandler) HandleImport(c *gin.Context) {
	var request struct {
		Home    string   `json:"home"`
		Include []string `json:"include"`
	}
	if err := c.ShouldBindJSON(&request); err != nil && err.Error() != "EOF" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体必须是 JSON 对象"})
		return
	}

	include := map[string]bool{}
	for _, name := range request.Include {
		if name = strings.TrimSpace(name); name != "" {
			include[name] = true
		}
	}

	result, err := ccswitchconfig.Import(ccswitchconfig.ImportOptions{
		Home:          h.effectiveHome(strings.TrimSpace(request.Home)),
		ProvidersYAML: h.providersYAML,
		EnvFile:       h.envFile,
		GatewayPort:   h.gatewayPort,
		Include:       include,
	})
	if err != nil {
		h.logger.Printf("import from cc-switch failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	result.RegisterLive(h.router, h.logger)
	if result.Wrote {
		h.logger.Printf("imported %d providers from cc-switch (%s): %v", len(result.Imported), result.Source, result.Imported)
	}
	c.JSON(http.StatusOK, result)
}

// remoteRequest 是桌面端上传迁移的请求体：装有 cc-switch 的机器上读出的
// providers 表行（新版）或 config.json 原文（旧版），二选一。API Key 随
// 管理信道上传，仅用于写入网关 .env，响应不回显。
type remoteRequest struct {
	LegacyJSON string                       `json:"legacy_json"`
	Rows       []ccswitchconfig.ProviderRow `json:"rows"`
	Include    []string                     `json:"include"`
}

// parseRemote 校验上传体并解析为条目，返回条目与来源标注。
func (h *CCSwitchHandler) parseRemote(request *remoteRequest) ([]ccswitchconfig.Entry, string, error) {
	switch {
	case request.LegacyJSON != "":
		entries, err := ccswitchconfig.ParseLegacyJSON([]byte(request.LegacyJSON))
		if err != nil {
			return nil, "", err
		}
		return entries, "cc-switch config.json（桌面端上传）", nil
	case len(request.Rows) > 0:
		return ccswitchconfig.ParseProviderRows(request.Rows), "cc-switch cc-switch.db（桌面端上传）", nil
	default:
		return nil, "", fmt.Errorf("请求体需要 legacy_json 或 rows 字段")
	}
}

// HandleRemoteScan POST /admin/ccswitch/remote-scan 预览桌面端上传的
// cc-switch 配置，不写入任何文件。响应与 GET /admin/ccswitch 同构。
func (h *CCSwitchHandler) HandleRemoteScan(c *gin.Context) {
	var request remoteRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体必须是 JSON 对象"})
		return
	}
	entries, source, err := h.parseRemote(&request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	context, err := ccswitchconfig.LoadContext(h.providersYAML, h.envFile, h.gatewayPort)
	if err != nil {
		h.logger.Printf("load gateway context for remote cc-switch scan: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	candidates := ccswitchconfig.Convert(entries, context)

	importable := 0
	for _, candidate := range candidates {
		if candidate.Status == "importable" {
			importable++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"home":       "",
		"source":     source,
		"candidates": candidates,
		"importable": importable,
		"skipped":    len(candidates) - importable,
		"uploaded":   true,
	})
}

// HandleRemoteImport POST /admin/ccswitch/remote-import 导入桌面端上传的
// cc-switch 配置：与本机导入共用一条写入路径（去重、备份、即时注册）。
func (h *CCSwitchHandler) HandleRemoteImport(c *gin.Context) {
	var request remoteRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体必须是 JSON 对象"})
		return
	}
	entries, source, err := h.parseRemote(&request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	include := map[string]bool{}
	for _, name := range request.Include {
		if name = strings.TrimSpace(name); name != "" {
			include[name] = true
		}
	}

	result, err := ccswitchconfig.ImportEntries(ccswitchconfig.ImportOptions{
		ProvidersYAML: h.providersYAML,
		EnvFile:       h.envFile,
		GatewayPort:   h.gatewayPort,
		Include:       include,
	}, source, entries)
	if err != nil {
		h.logger.Printf("import from uploaded cc-switch data failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	result.RegisterLive(h.router, h.logger)
	if result.Wrote {
		h.logger.Printf("imported %d providers from uploaded cc-switch data: %v", len(result.Imported), result.Imported)
	}
	c.JSON(http.StatusOK, result)
}
