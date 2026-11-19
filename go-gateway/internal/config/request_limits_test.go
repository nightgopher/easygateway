package config

import "testing"

// The two entry-point knobs default to "safe": no proxy is trusted and the body
// ceiling falls back to the handler default rather than to unlimited.
func TestEntryBoundaryDefaults(t *testing.T) {
	t.Setenv("EASYGATEWAY_MASTER_KEY", "test-key")
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("MAX_REQUEST_BODY_MB", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Fatalf("TrustedProxies = %v, want empty by default", cfg.TrustedProxies)
	}
	if cfg.MaxRequestBodyMB != 0 {
		t.Fatalf("MaxRequestBodyMB = %d, want 0 so the handler default applies", cfg.MaxRequestBodyMB)
	}
}

func TestEntryBoundaryOverrides(t *testing.T) {
	t.Setenv("EASYGATEWAY_MASTER_KEY", "test-key")
	t.Setenv("TRUSTED_PROXIES", "127.0.0.1, 10.0.0.0/8 ")
	t.Setenv("MAX_REQUEST_BODY_MB", "16")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if len(cfg.TrustedProxies) != 2 || cfg.TrustedProxies[0] != "127.0.0.1" || cfg.TrustedProxies[1] != "10.0.0.0/8" {
		t.Fatalf("TrustedProxies = %#v, want the trimmed list", cfg.TrustedProxies)
	}
	if cfg.MaxRequestBodyMB != 16 {
		t.Fatalf("MaxRequestBodyMB = %d, want 16", cfg.MaxRequestBodyMB)
	}
}

func TestInvalidMaxRequestBodyFallsBackToDefault(t *testing.T) {
	t.Setenv("EASYGATEWAY_MASTER_KEY", "test-key")
	t.Setenv("MAX_REQUEST_BODY_MB", "not-a-number")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.MaxRequestBodyMB != 0 {
		t.Fatalf("MaxRequestBodyMB = %d, want 0 (handler default) for an invalid value", cfg.MaxRequestBodyMB)
	}
}
