package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weijian/go-llm-gateway/internal/provider"
)

func TestShippedConfigPassesOfflineValidation(t *testing.T) {
	cfg, err := provider.LoadProvidersConfig("providers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	report := provider.ValidateProvidersConfig(cfg, provider.ValidationOptions{ExternalProviders: externalConfigProviders()})
	if err := report.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigCheckDoesNotRequireMasterKey(t *testing.T) {
	t.Setenv("LITELLM_MASTER_KEY", "")
	var out bytes.Buffer
	if err := checkConfigCommand([]string{"--file", filepath.Join("providers.yaml"), "--allow-missing-keys"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "providers /") || !strings.Contains(out.String(), "全部通过") {
		t.Fatal(out.String())
	}
}
