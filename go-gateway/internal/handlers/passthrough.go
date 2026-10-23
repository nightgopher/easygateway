package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/weijian/go-llm-gateway/internal/provider"
)

// PassthroughHandler 处理非会话协议的透传端点（embeddings、audio transcriptions）。
// 与 new-api 的渠道透传一致：按 model 路由到 provider，请求体原样转发、
// 响应原样回传，网关不做任何格式转换。模型须已在 providers.yaml 注册。
type PassthroughHandler struct {
	router *provider.Router
	logger *log.Logger
	client *http.Client
}

const (
	embeddingsPath     = "/v1/embeddings"
	transcriptionsPath = "/v1/audio/transcriptions"
	// 电信 TeleSpeech ASR 转写长音频可能超过常规超时；embeddings 毫秒级。
	passthroughTimeout = 5 * time.Minute
	// 请求体上限：SiliconFlow 侧音频限制 50MB，留出 multipart 编码余量。
	maxPassthroughBody = 64 << 20
)

// NewPassthroughHandler 创建透传处理器。provider 的 URL 配置为上游源站
// （如 https://api.siliconflow.cn），这里在其后拼接端点路径。
func NewPassthroughHandler(router *provider.Router, logger *log.Logger) *PassthroughHandler {
	return &PassthroughHandler{
		router: router,
		logger: logger,
		client: &http.Client{Timeout: passthroughTimeout},
	}
}

// HandleEmbeddings 转发 POST /v1/embeddings（JSON）。
func (h *PassthroughHandler) HandleEmbeddings(c *gin.Context) {
	h.forward(c, embeddingsPath)
}

// HandleTranscriptions 转发 POST /v1/audio/transcriptions（multipart）。
func (h *PassthroughHandler) HandleTranscriptions(c *gin.Context) {
	h.forward(c, transcriptionsPath)
}

func (h *PassthroughHandler) forward(c *gin.Context, path string) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxPassthroughBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "read request body: " + err.Error(), "type": "invalid_request_error"}})
		return
	}
	if len(body) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "request body is required", "type": "invalid_request_error"}})
		return
	}

	// 注意用原始 header：gin 的 ContentType() 会剥掉 multipart boundary 参数
	model := extractPassthroughModel(c.Request.Header.Get("Content-Type"), body)
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "model is required", "type": "invalid_request_error"}})
		return
	}

	providers, err := h.router.Route(model)
	if err != nil {
		var unknown *provider.UnknownModelError
		if errors.As(err, &unknown) {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": err.Error(), "type": "server_error"}})
		return
	}
	p := providers[0]

	target := provider.DeriveUpstreamOrigin(p.URL()) + path
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "build upstream request: " + err.Error(), "type": "server_error"}})
		return
	}
	if ct := c.GetHeader("Content-Type"); ct != "" {
		// multipart 必须原样保留 boundary；JSON 保持客户端原始类型
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey())

	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.Printf("passthrough %s model=%s upstream error: %s", path, model, provider.RedactedErrorText(err))
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "upstream request failed: " + err.Error(), "type": "server_error"}})
		return
	}
	defer resp.Body.Close()

	h.logger.Printf("passthrough %s model=%s provider=%s status=%d", path, model, p.Name(), resp.StatusCode)
	c.Status(resp.StatusCode)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Writer.Flush()
	_, _ = io.Copy(c.Writer, resp.Body)
}

// extractPassthroughModel 从 JSON 或 multipart 请求体中提取 model 字段。
// 只做读取，不改写请求体：multipart 的 boundary 与文件内容必须原样转发。
func extractPassthroughModel(contentType string, body []byte) string {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = contentType
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary, ok := params["boundary"]
		if !ok {
			return ""
		}
		form, err := multipart.NewReader(bytes.NewReader(body), boundary).ReadForm(maxPassthroughBody)
		if err != nil {
			return ""
		}
		if v := form.Value["model"]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	// JSON：最小解析，仅取 model 字段
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Model
}
