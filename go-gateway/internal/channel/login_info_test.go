package channel

import (
	"strings"
	"testing"
)

func TestLoginInfoHonorsDisabledChannelLogin(t *testing.T) {
	dir := t.TempDir()
	orig := DisabledChannelLogin
	DisabledChannelLogin = map[string]bool{"easycode": true}
	defer func() { DisabledChannelLogin = orig }()

	info := LoginInfoFor(NewEasyCodeChannel(dir))
	if info.Available || !strings.Contains(info.SetupHint, "EASYGATEWAY_DISABLE_CHANNEL_LOGIN") {
		t.Fatalf("%+v", info)
	}
	// 未列入禁用清单时不受影响。
	DisabledChannelLogin = map[string]bool{}
	if info := LoginInfoFor(NewEasyCodeChannel(dir)); !info.Available {
		t.Fatalf("%+v", info)
	}
}
