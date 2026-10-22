package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const CapabilitySystemOne = "systemone"

// SystemOne is a typed decision protocol, not a text generation protocol.
// Keep its request/answer objects intact rather than coercing them into chat.
type SystemOneResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

type SystemOneForwarder interface {
	ForwardSystemOne(context.Context, []byte) (*SystemOneResponse, error)
}

type SystemOneProvider struct {
	config     *Config
	allowNoKey bool
	client     *http.Client
}

func NewSystemOneProvider(cfg *Config, allowNoKey bool) *SystemOneProvider {
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &SystemOneProvider{config: cfg, allowNoKey: allowNoKey, client: &http.Client{
		Timeout: timeout,
		// A provider redirect must not forward workspace credentials elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (p *SystemOneProvider) Name() string                   { return p.config.Name }
func (p *SystemOneProvider) URL() string                    { return p.config.URL }
func (p *SystemOneProvider) APIKey() string                 { return p.config.APIKey }
func (p *SystemOneProvider) UseBearer() bool                { return true }
func (p *SystemOneProvider) Available() bool                { return p.allowNoKey || p.config.APIKey != "" }
func (p *SystemOneProvider) IsHealthy(context.Context) bool { return p.Available() }
func (p *SystemOneProvider) Capabilities() []string         { return []string{CapabilitySystemOne} }
func (p *SystemOneProvider) UnavailableReason() string {
	return "System One 模型需要 provider.api_key_env 指定的推理 API Key（OpenCode: OPENCODE_API_KEY）；设备码登录不能替代"
}
func (p *SystemOneProvider) ForwardRequest(_ context.Context, req *Request) (*Response, error) {
	return nil, &UnsupportedCapabilityError{Model: req.Model, Required: []string{CapabilityText}, Endpoint: "/v1/systemone"}
}

func (p *SystemOneProvider) ForwardSystemOne(ctx context.Context, body []byte) (*SystemOneResponse, error) {
	if !p.Available() {
		return nil, &ProviderError{Provider: p.Name(), StatusCode: http.StatusUnauthorized, Message: p.UnavailableReason()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build System One request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "EasyGateway-SystemOne")
	if p.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.config.APIKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Network errors can embed URLs. Retain the class, not provider details.
		return nil, &ProviderError{Provider: p.Name(), StatusCode: http.StatusBadGateway, Message: "System One upstream request failed"}
	}
	defer resp.Body.Close()
	const maxResponse = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return nil, &ProviderError{Provider: p.Name(), StatusCode: http.StatusBadGateway, Message: "System One upstream response could not be read within the size limit"}
	}
	if !json.Valid(data) || resp.StatusCode < 200 || resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &ProviderError{Provider: p.Name(), StatusCode: http.StatusBadGateway, Message: "System One upstream returned an invalid JSON response or redirect"}
	}
	if resp.StatusCode >= 400 && p.config.APIKey != "" {
		// Preserve native error types (including FreeUsageLimitError), while
		// never returning a credential echoed by an upstream error message.
		var value any
		_ = json.Unmarshal(data, &value)
		data, _ = json.Marshal(redactSystemOneCredential(value, p.config.APIKey))
	}
	return &SystemOneResponse{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: data}, nil
}

func redactSystemOneCredential(value any, key string) any {
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(v, key, "[REDACTED]")
	case map[string]any:
		for k, item := range v {
			v[k] = redactSystemOneCredential(item, key)
		}
	case []any:
		for i, item := range v {
			v[i] = redactSystemOneCredential(item, key)
		}
	}
	return value
}

func (w *BoundModelProviderWrapper) ForwardSystemOne(ctx context.Context, body []byte) (*SystemOneResponse, error) {
	p, ok := w.Provider.(SystemOneForwarder)
	if !ok || !hasCapabilityFlag(w.capabilities, CapabilitySystemOne) {
		return nil, &UnsupportedCapabilityError{Model: w.boundModel, Required: []string{CapabilitySystemOne}}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	fields["model"], _ = json.Marshal(w.boundModel)
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return p.ForwardSystemOne(ctx, body)
}

func (p *SystemOneProvider) ProbeModel(ctx context.Context, model string) ProbeResult {
	if !p.Available() {
		return ProbeResult{Status: ProbeOffline, Detail: p.UnavailableReason()}
	}
	body, _ := json.Marshal(map[string]any{"model": model, "state": "A test request.", "questions": map[string]any{"test": map[string]any{"type": "noul", "instructions": "Is this a test request?"}}})
	start := time.Now()
	result, err := p.ForwardSystemOne(ctx, body)
	if err != nil {
		return ProbeResult{Status: ProbeDegraded, Detail: "System One 探测失败", Latency: time.Since(start)}
	}
	return ProbeResult{Status: classifyProbeStatus(result.StatusCode), StatusCode: result.StatusCode, Detail: http.StatusText(result.StatusCode), Latency: time.Since(start)}
}
