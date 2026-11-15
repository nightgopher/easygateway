package provider

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestChatGPTProviderLoadsCodexAuthFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	accessToken := testChatGPTToken("acct-codex")
	writeChatGPTAuthFile(t, path, map[string]any{
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": "refresh-codex",
		},
	})

	provider := NewChatGPTProviderWithAuthPath("", path)
	if !provider.IsHealthy(nil) {
		t.Fatal("Codex OAuth credential should be healthy")
	}
	if provider.getAccountID() != "acct-codex" {
		t.Fatalf("account id = %q, want acct-codex", provider.getAccountID())
	}
}

func TestSetupProvidersFromConfigRegistersChatGPTModels(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "auth.json")
	writeChatGPTAuthFile(t, authPath, map[string]any{
		"openai-codex": map[string]any{
			"type":    "oauth",
			"access":  testChatGPTToken("acct-config"),
			"refresh": "refresh-config",
		},
	})
	configPath := filepath.Join(t.TempDir(), "providers.yaml")
	config := `providers:
  - name: chatgpt
    type: chatgpt
    url: https://chatgpt.com/backend-api
    models:
      - id: gpt-5.6-luna
        aliases: [legacy-luna]
        protocol: responses
        capabilities: [text, vision, tool_calling, streaming, reasoning]
        input_modalities: [text, image]
        max_input_tokens: 272000
        max_output_tokens: 128000
chains: {}
`
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHATGPT_AUTH_FILE", authPath)
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")

	router := NewRouter(log.New(io.Discard, "", 0))
	if _, err := SetupProvidersFromConfig(router, configPath, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	if got := router.ListProviders(); len(got) != 1 || got[0] != "chatgpt" {
		t.Fatalf("providers = %v, want [chatgpt]", got)
	}
	if _, err := router.Route("legacy-luna"); err != nil {
		t.Fatalf("Route(legacy-luna) error = %v", err)
	}
	// 上游模型 ID 是对外主名，别名 legacy-luna 作为兼容入口同时可用。
	if _, err := router.Route("gpt-5.6-luna"); err != nil {
		t.Fatalf("Route(gpt-5.6-luna) error = %v", err)
	}
	infos := router.ListModelInfos()
	seen := make(map[string]bool, len(infos))
	for _, info := range infos {
		if info.Protocol != "responses" || info.Provider != "chatgpt" {
			t.Fatalf("model info = %+v, want ChatGPT Responses model", info)
		}
		seen[info.ID] = true
	}
	for _, modelName := range []string{"gpt-5.6-luna", "legacy-luna"} {
		if !seen[modelName] {
			t.Fatalf("model infos = %+v, want %s registered", infos, modelName)
		}
	}
}

func TestNormalizeChatGPTInputConvertsStringToMessageList(t *testing.T) {
	req := map[string]json.RawMessage{"input": json.RawMessage(`"hello"`)}
	if err := normalizeChatGPTInput(req); err != nil {
		t.Fatalf("normalizeChatGPTInput() error = %v", err)
	}

	var items []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(req["input"], &items); err != nil {
		t.Fatalf("normalized input is invalid JSON: %v", err)
	}
	if len(items) != 1 || items[0].Type != "message" || items[0].Role != "user" {
		t.Fatalf("normalized input = %+v, want one user message", items)
	}
	if len(items[0].Content) != 1 || items[0].Content[0].Type != "input_text" || items[0].Content[0].Text != "hello" {
		t.Fatalf("normalized content = %+v, want input_text hello", items[0].Content)
	}
}

func TestNormalizeChatGPTRequestDropsUnsupportedOutputLimitFields(t *testing.T) {
	req := map[string]json.RawMessage{
		"input":                 json.RawMessage(`"hello"`),
		"max_output_tokens":     json.RawMessage(`128`),
		"max_completion_tokens": json.RawMessage(`128`),
		"max_tokens":            json.RawMessage(`128`),
	}
	if err := normalizeChatGPTRequest(req); err != nil {
		t.Fatalf("normalizeChatGPTRequest() error = %v", err)
	}
	for _, field := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if _, ok := req[field]; ok {
			t.Fatalf("request still contains unsupported field %q", field)
		}
	}
}

func testChatGPTToken(accountID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"` + accountID + `"}}`))
	return header + "." + payload + ".signature"
}

func writeChatGPTAuthFile(t *testing.T, path string, value map[string]any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
