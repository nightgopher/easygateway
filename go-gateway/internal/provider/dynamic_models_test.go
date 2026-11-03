package provider

import (
	"os"
	"testing"
)

func TestDynamicCatalogSurvivesReloadAndRemovesOnlyItsModels(t *testing.T) {
	path := writeTempProvidersConfig(t, reloadTestYAML("https://example.invalid", "configured"))
	router, native := NewRouter(nil), NewRouter(nil)
	reloader := NewConfigReloader(router, path, nil, func(candidate *Router) error { candidate.IncludeMissingConfiguration(native); return nil })
	if _, err := reloader.Reload(); err != nil {
		t.Fatal(err)
	}
	entry := func(id string, caps []string) DynamicChannelModel {
		return DynamicChannelModel{Provider: NewDeepVProvider(&Config{Name: id, URL: "https://example.invalid", Auth: NewStaticAuth("test")}, "", "model").ConfigureAccountModel(caps), Info: ModelInfo{ID: id, Provider: id, Capabilities: caps}}
	}
	models := []DynamicChannelModel{entry("easycode/text", []string{CapabilityText}), entry("configured", []string{CapabilityText})}
	reloader.Synchronize(func() {
		native.ReplaceDynamicChannelModels("easycode", models)
		router.ReplaceDynamicChannelModels("easycode", models)
	})
	if router.ModelChannel("easycode/text") != "easycode" || router.ModelChannel("configured") == "easycode" {
		t.Fatal("catalog replaced a configured model")
	}
	withDynamicRoute := reloadTestYAML("https://example.invalid", "configured")
	withDynamicRoute += "  account-coding: [easycode/text, configured]\n"
	if err := os.WriteFile(path, []byte(withDynamicRoute), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloader.Reload(); err != nil {
		t.Fatal("dynamic model rejected in chain", err)
	}
	if routes, err := router.Route("account-coding"); err != nil || len(routes) != 2 {
		t.Fatal("dynamic model missing from fallback chain", err)
	}
	if _, err := router.Route("easycode/text"); err != nil {
		t.Fatal(err)
	}
	if _, err := reloader.Reload(); err != nil {
		t.Fatal(err)
	}
	if router.ModelChannel("easycode/text") != "easycode" {
		t.Fatal("reload lost account channel association")
	}
	models = []DynamicChannelModel{entry("easycode/text", []string{CapabilityText, CapabilityVision})}
	reloader.Synchronize(func() {
		native.ReplaceDynamicChannelModels("easycode", models)
		router.ReplaceDynamicChannelModels("easycode", models)
	})
	routes, err := router.Route("easycode/text")
	if err != nil || !hasCapability(routes[0].(CapabilityProvider).Capabilities(), CapabilityVision) {
		t.Fatal("catalog update did not update capabilities")
	}
	reloader.Synchronize(func() {
		native.ReplaceDynamicChannelModels("easycode", nil)
		router.ReplaceDynamicChannelModels("easycode", nil)
	})
	if _, err := router.Route("easycode/text"); err == nil {
		t.Fatal("removed account model remained routable")
	}
	if routes, err := router.Route("account-coding"); err != nil || len(routes) != 1 {
		t.Fatal("fallback chain did not survive account logout", err)
	}
	if _, err := router.Route("configured"); err != nil {
		t.Fatal("removed unrelated configured model")
	}
	if _, err := reloader.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Route("easycode/text"); err == nil {
		t.Fatal("reload restored removed account model")
	}
}
