package main

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/weijian/go-llm-gateway/internal/config"
	"github.com/weijian/go-llm-gateway/internal/provider"
)

func TestDeepVNewModelRoutes(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	setupDeepVProviders(router, &config.Config{DeepVWorkDir: t.TempDir()}, logger)
	for _, id := range []string{"claude-haiku-5-5", "mimo-v2.6-pro"} {
		routes, err := router.Route(id)
		if err != nil || len(routes) != 1 {
			t.Fatalf("%s routes = %v, error = %v", id, routes, err)
		}
		bound, ok := routes[0].(interface{ BoundModel() string })
		if !ok || bound.BoundModel() != id {
			t.Fatalf("%s not bound to its upstream model", id)
		}
		found := false
		for _, info := range router.ListModelInfos() {
			if info.ID == id {
				found = true
				if info.Provider != id || info.MaxInputTokens+info.MaxOutputTokens > 200000 {
					t.Fatalf("%s invalid directory metadata: %+v", id, info)
				}
			}
		}
		if !found {
			t.Fatalf("%s missing from model directory", id)
		}
	}
}

func TestAssistantChatModelIDsExcludeSinglePurpose(t *testing.T) {
	infos := []provider.ModelInfo{
		{ID: "chat", Capabilities: []string{"text"}},
		{ID: "vision-chat", Capabilities: []string{"text", "vision"}},
		// 配置加载会为所有模型补 text 能力，嵌入与转写模型因此带 text。
		{ID: "BAAI/bge-m3", Capabilities: []string{"embedding", "text"}},
		{ID: "XingChenAGI/XingChenASR-V3.2", Capabilities: []string{"audio_transcription", "text"}},
		{ID: "no-text", Capabilities: []string{"tool_calling", "streaming"}},
		{ID: "jev-1.13-free", Protocol: "systemone", Capabilities: []string{"systemone"}},
	}
	got := assistantChatModelIDs(infos)
	want := []string{"chat", "vision-chat"}
	if len(got) != len(want) {
		t.Fatalf("assistant chat models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("assistant chat models = %v, want %v", got, want)
		}
	}
}

type catalogAuth struct{ token string }

func (a *catalogAuth) BearerToken() string          { return a.token }
func (a *catalogAuth) Refresh(context.Context) bool { return false }
func TestAssistantCatalogTracksChannelLoginAndDisable(t *testing.T) {
	auth := &catalogAuth{}
	r := provider.NewRouter(log.New(io.Discard, "", 0))
	r.RegisterProvider("bound", provider.NewOpenAIProvider(&provider.Config{Name: "bound", Auth: auth}))
	r.SetProviderChannel("bound", "cline")
	r.RegisterChain("mimo-model", []string{"bound"})
	r.RegisterModel(provider.ModelInfo{ID: "mimo-model", Provider: "bound", Capabilities: []string{"text"}})
	if len(assistantChatModelOptions(r)) != 0 {
		t.Fatal("logged out model advertised")
	}
	auth.token = "test-key"
	got := assistantChatModelOptions(r)
	if len(got) != 1 || got[0].Channel != "cline" {
		t.Fatal(got)
	}
	r.SetProviderEnabled("bound", false)
	if len(assistantChatModelOptions(r)) != 0 {
		t.Fatal("disabled model advertised")
	}
	r.SetProviderEnabled("bound", true)
	auth.token = ""
	if len(assistantChatModelOptions(r)) != 0 {
		t.Fatal("logout ignored")
	}
}
