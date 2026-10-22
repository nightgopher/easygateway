package provider

import (
	"strings"
	"testing"
)

func validCheckConfig() *ProvidersConfig {
	return &ProvidersConfig{Providers: []ProviderConfig{{Name: "test", Type: "openai", URL: "https://example.invalid/v1/chat/completions", APIKeyEnv: "CONFIG_CHECK_TEST_KEY", Models: []ModelConfig{{ID: "test-model", Capabilities: []string{"text", "streaming"}}}}}, Chains: map[string][]string{"coding": {"test-model"}}}
}

func TestConfigCheckReportsStructuralErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProvidersConfig)
		want   string
	}{
		{"duplicate model", func(c *ProvidersConfig) {
			c.Providers = append(c.Providers, ProviderConfig{Name: "other", Type: "openai", URL: "https://example.invalid", Models: []ModelConfig{{ID: "test-model"}}})
		}, "globally unique"},
		{"duplicate alias", func(c *ProvidersConfig) { c.Providers[0].Models[0].Aliases = []string{"test-model"} }, "globally unique"},
		{"unknown capability", func(c *ProvidersConfig) { c.Providers[0].Models[0].Capabilities = []string{"vission"} }, "unknown capability"},
		{"unknown modality", func(c *ProvidersConfig) { c.Providers[0].Models[0].InputModalities = []string{"images"} }, "unknown modality"},
		{"missing reference", func(c *ProvidersConfig) { c.Chains["coding"] = []string{"absent"} }, "unknown provider instance"},
		{"URL", func(c *ProvidersConfig) { c.Providers[0].URL = "file:///tmp/test" }, "HTTP(S)"},
		{"URL credentials", func(c *ProvidersConfig) { c.Providers[0].URL = "https://user:secret@example.invalid" }, "HTTP(S)"},
		{"unknown channel", func(c *ProvidersConfig) { c.Providers[0].Type = "channel"; c.Providers[0].Channel = "not-a-channel" }, "unknown account channel"},
		{"provider duplicate", func(c *ProvidersConfig) { c.Providers = append(c.Providers, c.Providers[0]) }, "provider name"},
		{"negative timeout", func(c *ProvidersConfig) { c.Providers[0].RequestTimeoutSeconds = -1 }, "negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validCheckConfig()
			tt.mutate(cfg)
			report := ValidateProvidersConfig(cfg, ValidationOptions{})
			if report.Valid() || !strings.Contains(report.Err().Error(), tt.want) {
				t.Fatalf("report=%+v, want %q", report, tt.want)
			}
		})
	}
}

func TestConfigCheckCredentialsAndExternalProviders(t *testing.T) {
	t.Setenv("CONFIG_CHECK_TEST_KEY", "")
	cfg := validCheckConfig()
	if ValidateProvidersConfig(cfg, ValidationOptions{CheckEnvironment: true}).Valid() {
		t.Fatal("missing required key accepted")
	}
	cfg.Providers[0].AllowNoKey = true
	report := ValidateProvidersConfig(cfg, ValidationOptions{CheckEnvironment: true})
	if !report.Valid() || len(report.Diagnostics) != 1 || report.Diagnostics[0].Level != "warning" {
		t.Fatalf("anonymous key report=%+v", report)
	}
	cfg.Providers[0].AllowNoKey = false
	t.Setenv("CONFIG_CHECK_TEST_KEY", "must-never-appear")
	cfg.Chains["coding"] = append(cfg.Chains["coding"], "native-model")
	report = ValidateProvidersConfig(cfg, ValidationOptions{CheckEnvironment: true, ExternalProviders: []string{"native-model"}})
	if !report.Valid() {
		t.Fatal(report.Err())
	}
	for _, d := range report.Diagnostics {
		if strings.Contains(d.Message, "must-never-appear") {
			t.Fatal("secret leaked")
		}
	}
}

func TestConfigLoadRejectsTyposAndMultipleDocuments(t *testing.T) {
	for _, body := range []string{"providers: []\nchaims: {}", "providers: []\n---\nproviders: []"} {
		if _, err := LoadProvidersConfig(writeTempProvidersConfig(t, body)); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}
