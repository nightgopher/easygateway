package config

import (
	"os"
	"testing"
)

func TestLoad(t *testing.T) {
	os.Setenv("EASYGATEWAY_MASTER_KEY", "test-key")
	os.Setenv("PORT", "8080")
	defer os.Unsetenv("EASYGATEWAY_MASTER_KEY")
	defer os.Unsetenv("PORT")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Expected port 8080, got %d", cfg.Port)
	}
	if cfg.MasterKey != "test-key" {
		t.Errorf("Expected key 'test-key', got %s", cfg.MasterKey)
	}
}

func TestLoadLegacyMasterKeyAlias(t *testing.T) {
	t.Setenv("EASYGATEWAY_MASTER_KEY", "")
	t.Setenv("LITELLM_MASTER_KEY", "legacy-key")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.MasterKey != "legacy-key" {
		t.Errorf("expected the legacy alias to authenticate, got %q", cfg.MasterKey)
	}
	t.Setenv("EASYGATEWAY_MASTER_KEY", "new-key")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.MasterKey != "new-key" {
		t.Errorf("expected EASYGATEWAY_MASTER_KEY to win over the alias, got %q", cfg.MasterKey)
	}
}

func TestLoadMissingMasterKey(t *testing.T) {
	os.Unsetenv("EASYGATEWAY_MASTER_KEY")
	os.Unsetenv("LITELLM_MASTER_KEY")
	_, err := Load()
	if err == nil {
		t.Fatal("Expected error for missing LITELLM_MASTER_KEY")
	}
}

func TestLoadDefaultPort(t *testing.T) {
	os.Setenv("LITELLM_MASTER_KEY", "test-key")
	os.Unsetenv("PORT")
	defer os.Unsetenv("LITELLM_MASTER_KEY")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.Port != 4001 {
		t.Errorf("Expected default port 4001, got %d", cfg.Port)
	}
}
