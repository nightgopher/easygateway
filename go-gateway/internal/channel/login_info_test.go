package channel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLoginInfoReportsMissingConfigurationWithoutCredentials(t *testing.T) {
	for _, name := range []string{"GEMINI_CHANNEL_CLIENT_ID", "GEMINI_CHANNEL_CLIENT_SECRET", "CMDC_PAK_GOOGLE_CLIENT_ID", "CMDC_PAK_GOOGLE_CLIENT_SECRET", "TRAE_CHANNEL_CLIENT_ID", "TRAE_CHANNEL_APP_ID", "LOOMY_ACCESS_KEY_ID", "LOOMY_ACCESS_KEY_SECRET"} {
		t.Setenv(name, "")
	}
	dir := t.TempDir()
	t.Setenv("TRAE_CHANNEL_CLIENT_PRODUCT", dir+"/not-installed.json")
	for _, ch := range []Channel{NewGeminiChannel(dir), NewTraeChannel(dir), NewLoomyChannel(dir)} {
		info := LoginInfoFor(ch)
		if info.Available || !info.NeedsClientConfig || !info.LocalOnly || info.SetupHint == "" {
			t.Fatalf("%s %+v", ch.Name(), info)
		}
	}
	t.Setenv("GEMINI_CHANNEL_CLIENT_ID", "client-id-sentinel")
	t.Setenv("GEMINI_CHANNEL_CLIENT_SECRET", "client-secret-sentinel")
	info := LoginInfoFor(NewGeminiChannel(dir))
	raw, _ := json.Marshal(info)
	if !info.Available || strings.Contains(string(raw), "sentinel") {
		t.Fatal(string(raw))
	}
	for _, ch := range []Channel{NewMiniMaxChannel(dir), NewClineChannel(dir), NewLobsterChannel(dir), NewCodeBuddyChannel(dir), NewWorkBuddyChannel(dir), NewRaccoonChannel(dir), NewOpenCodeChannel(dir)} {
		info := LoginInfoFor(ch)
		if !info.Available || info.NeedsClientConfig {
			t.Fatalf("%s %+v", ch.Name(), info)
		}
	}
	if info := LoginInfoFor(NewOpenCodeChannel(dir)); info.Anonymous || !strings.Contains(info.SetupHint, "免费档") {
		t.Fatal("OpenCode restriction missing")
	}
}

func TestLoginInfoHonorsDisabledChannelLogin(t *testing.T) {
	dir := t.TempDir()
	orig := DisabledChannelLogin
	DisabledChannelLogin = map[string]bool{"workbuddy": true}
	defer func() { DisabledChannelLogin = orig }()

	info := LoginInfoFor(NewWorkBuddyChannel(dir))
	if info.Available || !strings.Contains(info.SetupHint, "EASYGATEWAY_DISABLE_CHANNEL_LOGIN") {
		t.Fatalf("%+v", info)
	}
	// 未列入禁用清单的渠道不受影响。
	if info := LoginInfoFor(NewCodeBuddyChannel(dir)); !info.Available {
		t.Fatalf("%+v", info)
	}
}
