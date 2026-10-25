package channel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nightgopher/easygateway/internal/provider"
)

// loomy 渠道测试：覆盖 HMAC-SHA1 签名原文、信封解析、微信扫码原语（uuid 提取、长轮询
// 状态映射）、凭据装配与探测、动态头/模型名改写、provider 装配面的鉴权头形态，以及完整
// 的回环扫码登录流程（已绑定 / 首次使用两条分支）。
//
// 所有用例都离线：微信侧与讯飞账号侧都指向 httptest 假上游。

// newLoomyForTest 造一个应用凭据齐全、端点指向假上游的 loomy 渠道。
func newLoomyForTest(t *testing.T, wechat, wechatLong, account string) *LoomyChannel {
	t.Helper()
	c := NewLoomyChannel(t.TempDir())
	c.accessKeyID, c.accessKeySecret = "test-key-id", "test-key-secret"
	c.appID, c.wechatAppID = "GM3LOOMY", "wx-test-app"
	c.wechatBase, c.wechatLong, c.accountBase = wechat, wechatLong, account
	c.apiBase = account
	c.loginTimeout = 5 * time.Second
	return c
}

// loomyAuthPageHTML 是微信授权页的两种 uuid 承载形态（图片路径 / 轮询链接）。
func loomyAuthPageHTML(mode, uuid string) string {
	if mode == "img" {
		return `<html><body><img class="qrcode" src="/connect/qrcode/` + uuid + `"></body></html>`
	}
	return `<html><body><script>var u="https://long.open.weixin.qq.com/connect/l/qrconnect?uuid=` + uuid + `&_=1";</script></body></html>`
}

// loomyProvider 复刻 channels.go 里 loomy 的装配：OpenAI 协议 + Bearer 鉴权 +
// 客户端固定头 + 每请求动态 `token` 头 + 模型名剥前缀。刻意与生产重复，channels.go
// 改错时这里会失败。
func loomyProvider(c *LoomyChannel, accounts *Accounts) *provider.OpenAIProvider {
	return provider.NewOpenAIProvider(&provider.Config{
		Name:           "loomy/Kimi-k2.6",
		URL:            c.ChatURL(),
		Auth:           accounts,
		ExtraHeaders:   c.ClientHeaders(),
		DynamicHeaders: c.DynamicHeaders(),
		Transform:      LoomyTransformRequest,
	})
}

// ── 签名 ──────────────────────────────────────────────────────────────────

// TestLoomySigningString 锁住签名原文的 9 段结构：方法大写、路径逐段转义、查询串与
// 末尾两个占位固定为空。任一段错位都会让真实账号接口返回签名错误。
func TestLoomySigningString(t *testing.T) {
	got := loomySigningString("post", "/login/phone/sendMsgCode", `{"a":1}`, "application/json", "Thu, 01 Jan 2026 00:00:00 GMT", "nonce-1")
	want := strings.Join([]string{
		"POST",
		"/login/phone/sendMsgCode",
		"",
		loomyContentMD5(`{"a":1}`),
		"application/json",
		"Thu, 01 Jan 2026 00:00:00 GMT",
		"nonce-1",
		"",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("签名原文不正确:\nwant %q\ngot  %q", want, got)
	}
	if strings.Split(loomySigningString("POST", "/x", "", "application/json", "d", "n"), "\n")[3] != "" {
		t.Fatalf("空 body 的 Content-MD5 段应为空（官方实现对空 body 不写该头）")
	}
}

// TestLoomySignature 用独立算出的向量验证 base64(HMAC-SHA1(secret, stringToSign))，
// 避免实现自证。
func TestLoomySignature(t *testing.T) {
	got := loomySignature("test-key-secret", "hello\nworld")
	// base64(hmac_sha1(b"test-key-secret", b"hello\nworld"))，由 Python hmac/hashlib 算出。
	const want = "TxpxPnqgs/iSX9TrpkW+nKeaTGk="
	if got != want {
		t.Fatalf("HMAC-SHA1 签名不正确: want %q got %q", want, got)
	}
}

// TestBuildEscapedPath 覆盖路径规范化与 RFC3986 转义：encodeURIComponent 不转义、
// 但官方 escapeRfc3986 要求转义的 ! ' ( ) * 四个字符必须转义。
func TestBuildEscapedPath(t *testing.T) {
	cases := map[string]string{
		"/login/phone/sendMsgCode": "/login/phone/sendMsgCode",
		"login/a":                  "/login/a",
		"/a/":                      "/a",
		"/":                        "/",
		"/a b/c+d":                 "/a%20b/c%2Bd",
		"/x/*!()'":                 "/x/%2A%21%28%29%27",
	}
	for in, want := range cases {
		if got := buildEscapedPath(in); got != want {
			t.Fatalf("buildEscapedPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── 信封 ──────────────────────────────────────────────────────────────────

// TestParseLoomyEnvelope 覆盖成功码、desc 优先于 message、只有 message、缺 code、nil。
func TestParseLoomyEnvelope(t *testing.T) {
	ok := parseLoomyEnvelope(map[string]any{"code": "000000", "data": map[string]any{"x": float64(1)}})
	if !ok.OK || ok.Data == nil {
		t.Fatalf("000000 应视为成功并带 data: %+v", ok)
	}

	failed := parseLoomyEnvelope(map[string]any{"code": "100002", "desc": "登录已失效", "message": "ignored"})
	if failed.OK || failed.Message != "登录已失效" {
		t.Fatalf("非成功码应报错且 desc 优先，得到 %+v", failed)
	}

	only := parseLoomyEnvelope(map[string]any{"code": "100003", "message": "业务拒绝"})
	if only.Message != "业务拒绝" {
		t.Fatalf("应回退到 message，得到 %+v", only)
	}

	missing := parseLoomyEnvelope(map[string]any{"data": map[string]any{}})
	if missing.OK || missing.Message == "" {
		t.Fatalf("缺 code 应报错并带文案，得到 %+v", missing)
	}
	if parseLoomyEnvelope(nil).OK {
		t.Fatalf("nil 响应不应视为成功")
	}
}

// ── 微信扫码原语 ──────────────────────────────────────────────────────────

// TestExtractLoomyWechatUUID 覆盖两种页面形态与形状校验（非法候选必须丢弃）。
func TestExtractLoomyWechatUUID(t *testing.T) {
	const uuid = "AbC123_-+/="
	for _, mode := range []string{"img", "poll"} {
		if got := extractLoomyWechatUUID(loomyAuthPageHTML(mode, uuid)); got != uuid {
			t.Fatalf("%s 形态未抽出 uuid，得到 %q", mode, got)
		}
	}
	// 过短（<6）的候选不满足 UUID_PATTERN，必须被拒。
	if got := extractLoomyWechatUUID(`<img src="/connect/qrcode/abc">`); got != "" {
		t.Fatalf("过短候选应被拒，得到 %q", got)
	}
	if got := extractLoomyWechatUUID(""); got != "" {
		t.Fatalf("空页面应返回空串，得到 %q", got)
	}
}

// TestFetchWechatUUIDAndQRImage 验证授权页抓取（带浏览器头）与二维码 data URL 生成。
func TestFetchWechatUUIDAndQRImage(t *testing.T) {
	var gotUA, gotReferer string
	fakePNG := append([]byte{0x89, 'P', 'N', 'G'}, make([]byte, 300)...)

	wechat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotReferer = r.Header.Get("Referer")
		switch {
		case r.URL.Path == "/connect/qrconnect":
			_, _ = w.Write([]byte(loomyAuthPageHTML("img", "qr-uuid-1")))
		case strings.HasPrefix(r.URL.Path, "/connect/qrcode/"):
			_, _ = w.Write(fakePNG)
		default:
			http.NotFound(w, r)
		}
	}))
	defer wechat.Close()

	c := newLoomyForTest(t, wechat.URL, "", "")
	uuid, err := c.fetchWechatUUID(context.Background(), "state-1")
	if err != nil {
		t.Fatalf("fetchWechatUUID: %v", err)
	}
	if uuid != "qr-uuid-1" {
		t.Fatalf("uuid 应为 qr-uuid-1，得到 %q", uuid)
	}
	if gotUA != loomyWeChatUA || gotReferer == "" {
		t.Fatalf("微信侧必须带浏览器 UA/Referer，得到 UA=%q Referer=%q", gotUA, gotReferer)
	}

	dataURL, err := c.fetchWechatQRImage(context.Background(), uuid)
	if err != nil {
		t.Fatalf("fetchWechatQRImage: %v", err)
	}
	if !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Fatalf("二维码应为 PNG data URL，得到 %q", dataURL[:min(32, len(dataURL))])
	}
}

// TestFetchWechatQRImageRejectsErrorPage 验证非图片响应（错误页）被拒，不会把 HTML
// 当二维码渲染进页面。
func TestFetchWechatQRImageRejectsErrorPage(t *testing.T) {
	wechat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer wechat.Close()

	c := newLoomyForTest(t, wechat.URL, "", "")
	if _, err := c.fetchWechatQRImage(context.Background(), "uuid-1"); err == nil {
		t.Fatalf("非图片响应应报错")
	}
}

// TestPollWechatOnceStatusMapping 锁定长轮询的状态映射；404（已扫码待确认）与 405
// 不带 code 都不得当成确认——404 被当成功会拿到空 code，是最容易写错的一处。
func TestPollWechatOnceStatusMapping(t *testing.T) {
	cases := []struct {
		name    string
		frame   string
		status  string
		code    string
		errcode string
	}{
		{"等待扫码", "wx_errcode=408", loomyPollWaiting, "", "408"},
		{"已扫码待确认", "wx_errcode=404", loomyPollScanned, "", "404"},
		{"已确认带 code", "wx_errcode=405;wx_code='THE-CODE'", loomyPollConfirmed, "THE-CODE", "405"},
		{"405 不带 code 只能算已扫码", "wx_errcode=405", loomyPollScanned, "", "405"},
		{"用户取消", "wx_errcode=403", loomyPollCancelled, "", "403"},
		{"二维码失效", "wx_errcode=402", loomyPollExpired, "", "402"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wechatLong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.RawQuery, "uuid=") {
					t.Errorf("轮询请求缺少 uuid 参数: %s", r.URL.RawQuery)
				}
				if r.Header.Get("Referer") == "" {
					t.Errorf("轮询请求应带 Referer")
				}
				_, _ = w.Write([]byte(tc.frame))
			}))
			defer wechatLong.Close()

			c := newLoomyForTest(t, "", wechatLong.URL, "")
			got := c.pollWechatOnce(context.Background(), "uuid-1", "408")
			if got.Status != tc.status || got.Code != tc.code || got.Errcode != tc.errcode {
				t.Fatalf("状态映射不正确: got %+v, want status=%s code=%s errcode=%s",
					got, tc.status, tc.code, tc.errcode)
			}
		})
	}
}

// TestPollWechatOnceNetworkErrorKeepsPolling 验证网络异常映射为 error 状态而不是终止
// 流程（瞬时故障不该让用户重扫）。
func TestPollWechatOnceNetworkErrorKeepsPolling(t *testing.T) {
	c := newLoomyForTest(t, "", "http://127.0.0.1:1", "")
	got := c.pollWechatOnce(context.Background(), "uuid-1", "")
	if got.Status != loomyPollError {
		t.Fatalf("网络异常应映射为 error（调用方继续轮询），得到 %+v", got)
	}
}

// ── 凭据 ──────────────────────────────────────────────────────────────────

// TestBuildLoomyCredential 验证本地推算 expires_at、RefreshToken 占位值（让刷新走探测
// 分支而不是静默成功）以及昵称回退到手机号。
func TestBuildLoomyCredential(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	cred := buildLoomyCredential(loomyLoginResult{Session: "sess-1", UserID: "u-1", Phone: "13800000000", Nickname: "小明"}, now)

	if cred.AccessToken != "sess-1" || cred.AccountID != "u-1" {
		t.Fatalf("会话/账号字段不正确: %+v", cred)
	}
	if cred.Nickname != "小明" {
		t.Fatalf("昵称不正确: %q", cred.Nickname)
	}
	wantExpiry := now.Add(time.Duration(loomySessionTTL) * time.Second)
	if !cred.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expires_at 应为本地推算的 14 天后 %v，得到 %v", wantExpiry, cred.ExpiresAt)
	}
	if cred.RefreshToken != loomyProbeToken {
		t.Fatalf("RefreshToken 应为探测占位值 %q，得到 %q", loomyProbeToken, cred.RefreshToken)
	}
	if cred.Extra["userid"] != "u-1" || cred.Extra["phone"] != "13800000000" {
		t.Fatalf("Extra 应带 userid/phone，得到 %+v", cred.Extra)
	}

	fallback := buildLoomyCredential(loomyLoginResult{Session: "s", UserID: "u", Phone: "139"}, now)
	if fallback.Nickname != "139" {
		t.Fatalf("无昵称应回退到手机号，得到 %q", fallback.Nickname)
	}
}

// TestProbeCredential 覆盖探测的三种结局：有效、凭据失效（100002，终态）、其他业务错误。
// 探测用 GET /points/records，必须带 token 头而不是 Bearer。
func TestProbeCredential(t *testing.T) {
	var gotToken, gotAuth, gotPath string
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("token")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		writeJSON(w, map[string]any{"code": loomyOKCode, "data": map[string]any{}})
	}))
	defer account.Close()

	c := newLoomyForTest(t, "", "", account.URL)
	if err := c.probeCredential(context.Background(), "sess-1"); err != nil {
		t.Fatalf("有效凭据不应报错: %v", err)
	}
	if gotToken != "sess-1" {
		t.Fatalf("探测必须带 token 头，得到 %q", gotToken)
	}
	if gotAuth != "" {
		t.Fatalf("业务端点不带 Authorization，得到 %q", gotAuth)
	}
	if gotPath != "/points/records?pageNo=1&pageSize=1&recordType=all" {
		t.Fatalf("探测路径不正确: %s", gotPath)
	}

	// 100002 → 终态：必须被识别为需要重新登录（账号池据此换号）。
	expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": loomyAuthErrorCode, "desc": "登录已失效"})
	}))
	defer expired.Close()
	cExpired := newLoomyForTest(t, "", "", expired.URL)
	err := cExpired.probeCredential(context.Background(), "sess-1")
	if !errors.Is(err, ErrLoginCancelled) {
		t.Fatalf("100002 应映射为 ErrLoginCancelled，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "登录已失效") {
		t.Fatalf("错误文案应带上服务端 desc，得到 %v", err)
	}

	// 其他业务错误：报错但不是终态（不换号）。
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": "999999", "message": "上游抖动"})
	}))
	defer other.Close()
	cOther := newLoomyForTest(t, "", "", other.URL)
	if err := cOther.probeCredential(context.Background(), "sess-1"); err == nil || errors.Is(err, ErrLoginCancelled) {
		t.Fatalf("其他业务错误应报非终态错误，得到 %v", err)
	}
}

// TestRefreshCredentialIsProbeOnly 验证「刷新」确实是探测：令牌原样返回（loomy 没有
// 续期端点），空凭据报错。
func TestRefreshCredentialIsProbeOnly(t *testing.T) {
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": loomyOKCode})
	}))
	defer account.Close()

	c := newLoomyForTest(t, "", "", account.URL)
	in := Credential{AccessToken: "sess-1", RefreshToken: loomyProbeToken}
	out, err := c.refreshCredential(context.Background(), in)
	if err != nil {
		t.Fatalf("refreshCredential: %v", err)
	}
	if out.AccessToken != "sess-1" {
		t.Fatalf("loomy 无续期端点，令牌应原样返回，得到 %q", out.AccessToken)
	}
	if _, err := c.refreshCredential(context.Background(), Credential{}); err == nil {
		t.Fatalf("空凭据应报错")
	}
}

// ── provider 装配面 ───────────────────────────────────────────────────────

// TestLoomyProviderSendsBearerAndTokenHeader 是关键的集成面回归：loomy 的 chat 端点
// 要求同时带 `Authorization: Bearer <token>` 与自定义头 `token: <token>`（业务端点只
// 认后者，聊天端点只认前者，官方客户端两个都发）。缺任意一个上游都会拒绝。
// 同时验证 Accept 固定头与模型名剥前缀真的作用到了出站请求。
func TestLoomyProviderSendsBearerAndTokenHeader(t *testing.T) {
	var gotAuth, gotToken, gotAccept string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("token")
		gotAccept = r.Header.Get("Accept")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, map[string]any{
			"id": "c", "object": "chat.completion", "model": "Kimi-k2.6",
			"choices": []any{map[string]any{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"}, "finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer upstream.Close()

	store := NewStore(t.TempDir())
	writeCreds(t, store, loomyChannel, Credential{
		AccessToken:  "sess-1",
		RefreshToken: loomyProbeToken,
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	c := newLoomyForTest(t, "", "", upstream.URL)
	c.accounts = NewAccounts(loomyChannel, store, newOAuthClient(), c.refreshCredential)

	p := loomyProvider(c, c.accounts)
	req := mustRequest(t, `{"model":"loomy/Kimi-k2.6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatalf("ForwardRequest failed: %v", err)
	}
	if gotAuth != "Bearer sess-1" {
		t.Fatalf("chat 端点必须带 Authorization: Bearer，得到 %q", gotAuth)
	}
	if gotToken != "sess-1" {
		t.Fatalf("chat 端点必须同时带自定义头 token，得到 %q", gotToken)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("Accept 应为 text/event-stream，得到 %q", gotAccept)
	}
	if gotBody["model"] != "Kimi-k2.6" {
		t.Fatalf("出站请求体的模型名应剥掉 loomy/ 前缀，得到 %v", gotBody["model"])
	}
}

// TestLoomyDynamicHeaders 验证动态头随当前账号令牌变化，未登录时返回 nil。
func TestLoomyDynamicHeaders(t *testing.T) {
	c := newLoomyForTest(t, "", "", "")
	headers := c.DynamicHeaders()
	if got := headers(); got != nil {
		t.Fatalf("未登录时动态头应为 nil，得到 %+v", got)
	}

	store := NewStore(t.TempDir())
	writeCreds(t, store, loomyChannel, Credential{
		AccessToken:  "sess-1",
		RefreshToken: loomyProbeToken,
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	c.accounts = NewAccounts(loomyChannel, store, newOAuthClient(), c.refreshCredential)
	if got := c.DynamicHeaders()()["token"]; got != "sess-1" {
		t.Fatalf("动态头 token 应为当前账号令牌，得到 %q", got)
	}
}

// TestLoomyTransformRequest 验证模型名剥前缀：只剥 loomy/，其它模型名与非法 JSON 原样
// 透传（交给上游报错）。
func TestLoomyTransformRequest(t *testing.T) {
	out, err := LoomyTransformRequest([]byte(`{"model":"loomy/Kimi-k2.6","messages":[]}`))
	if err != nil {
		t.Fatalf("LoomyTransformRequest: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if parsed["model"] != "Kimi-k2.6" {
		t.Fatalf("应剥掉 loomy/ 前缀，得到 %v", parsed["model"])
	}

	plain, err := LoomyTransformRequest([]byte(`{"model":"Kimi-k2.6"}`))
	if err != nil || !strings.Contains(string(plain), `"Kimi-k2.6"`) {
		t.Fatalf("无前缀应原样透传，得到 %s (%v)", plain, err)
	}

	raw := []byte(`not json`)
	got, err := LoomyTransformRequest(raw)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("非法 JSON 应原样返回，得到 %s (%v)", got, err)
	}
}

// TestLoomyChatURLAndClientHeaders 锁定 chat 端点与固定头：Accept 必须是
// text/event-stream（复刻官方客户端；上游只提供流式响应），Authorization 不在这里写。
func TestLoomyChatURLAndClientHeaders(t *testing.T) {
	c := newLoomyForTest(t, "", "", "https://loomyad.xunfei.cn/api/v1")
	if c.ChatURL() != "https://loomyad.xunfei.cn/api/v1/chat/completions" {
		t.Fatalf("chat 端点不正确: %s", c.ChatURL())
	}
	headers := c.ClientHeaders()
	if headers["Accept"] != "text/event-stream" {
		t.Fatalf("Accept 应为 text/event-stream，得到 %q", headers["Accept"])
	}
	if headers["Authorization"] != "" {
		t.Fatalf("Authorization 由 provider.applyAuth 写入，不应出现在固定头里")
	}
	if c.Namespace() != "loomy/" {
		t.Fatalf("命名空间应为 loomy/，得到 %q", c.Namespace())
	}
}

// TestLoomyNonStreamingUpstreamReturnsSSE 固定「非流式客户端请求打到只输出 SSE 的上游」
// 的真实行为：OpenAIProvider.ForwardRequest 不设 stream（见 openai.go），上游仍回 SSE 时
// JSON 解析失败。
//
// loomy 的官方客户端只发 stream:true（见 loomy.go 顶部注释），所以实践里客户端若走流式
// 就没问题；这条断言把「非流式调用 loomy 不可用」钉成显式事实，而不是留成隐性坑：一旦
// 上游将来按需返回 JSON，本测试会失败并提示更新此处，而不是静默变成「没测过」。
func TestLoomyNonStreamingUpstreamReturnsSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	store := NewStore(t.TempDir())
	writeCreds(t, store, loomyChannel, Credential{
		AccessToken:  "sess-1",
		RefreshToken: loomyProbeToken,
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	c := newLoomyForTest(t, "", "", upstream.URL)
	c.accounts = NewAccounts(loomyChannel, store, newOAuthClient(), c.refreshCredential)

	p := loomyProvider(c, c.accounts)
	req := mustRequest(t, `{"model":"loomy/Kimi-k2.6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err == nil {
		t.Fatalf("上游只回 SSE 时非流式解析应失败（若上游已支持非流式，请更新此断言与 loomy.go 注释）")
	}
}

// ── 登录流程 ──────────────────────────────────────────────────────────────

// loomyFakeAccount 是假讯飞账号服务：按路径回放绑定流程，并记录签名头。
type loomyFakeAccount struct {
	t         *testing.T
	bind      float64
	server    *httptest.Server
	seenPaths []string
	seenAuth  []string
	seenNonce []string
}

func newLoomyFakeAccount(t *testing.T, bind float64) *loomyFakeAccount {
	t.Helper()
	f := &loomyFakeAccount{t: t, bind: bind}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seenPaths = append(f.seenPaths, r.URL.Path)
		f.seenAuth = append(f.seenAuth, r.Header.Get("Authorization"))
		f.seenNonce = append(f.seenNonce, r.Header.Get("Nonce"))
		switch r.URL.Path {
		case "/login/thirdAccount/bind/auth":
			writeJSON(w, map[string]any{"code": loomyOKCode, "data": map[string]any{
				"bind": f.bind, "rcode": "rcode-1", "nickname": "小明", "isnew": f.bind != 1,
			}})
		case "/login/thirdAccount/bind/skip":
			writeJSON(w, map[string]any{"code": loomyOKCode, "data": map[string]any{
				"session": "sess-skip", "userid": "u-1",
			}})
		case "/login/thirdAccount/bind/sendMsg":
			writeJSON(w, map[string]any{"code": loomyOKCode, "data": map[string]any{"msgid": float64(12345)}})
		case "/login/thirdAccount/bind/checkCode":
			writeJSON(w, map[string]any{"code": loomyOKCode, "data": map[string]any{
				"session": "sess-sms", "userid": "u-2", "phone": "13800000000",
			}})
		case "/points/first-login":
			writeJSON(w, map[string]any{"code": loomyOKCode})
		default:
			http.NotFound(w, r)
		}
	}))
	return f
}

// newLoomyWechatStub 造微信侧假上游：授权页返回指定 uuid、二维码返回假 PNG，长轮询按
// frames 逐帧回放（越界后重复最后一帧）。
func newLoomyWechatStub(t *testing.T, uuid string, frames []string) (*httptest.Server, *httptest.Server) {
	t.Helper()
	fakePNG := append([]byte{0x89, 'P', 'N', 'G'}, make([]byte, 300)...)
	var index int
	wechatLong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if index >= len(frames) {
			index = len(frames) - 1
		}
		_, _ = w.Write([]byte(frames[index]))
		index++
	}))
	wechat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/connect/qrcode/") {
			_, _ = w.Write(fakePNG)
			return
		}
		_, _ = w.Write([]byte(loomyAuthPageHTML("img", uuid)))
	}))
	return wechat, wechatLong
}

// TestLoomyLoginAlreadyBound 跑完整扫码登录：已绑定账号（bind==1）扫完直接出会话，页面
// 经 /wechat/qr → /wechat/poll 走通，凭据落盘，账号端点带 HMAC 签名。
func TestLoomyLoginAlreadyBound(t *testing.T) {
	account := newLoomyFakeAccount(t, 1)
	defer account.server.Close()
	wechat, wechatLong := newLoomyWechatStub(t, "qr-uuid-1", []string{"wx_errcode=408", "wx_errcode=405;wx_code='WX-CODE'"})
	defer wechat.Close()
	defer wechatLong.Close()

	c := newLoomyForTest(t, wechat.URL, wechatLong.URL, account.server.URL)
	session, err := c.BeginLogin(context.Background())
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	dc := session.DeviceCode()
	if !strings.HasPrefix(dc.VerificationURIComplete, "http://127.0.0.1:") || !strings.HasSuffix(dc.VerificationURIComplete, loomyWeChatQRPath) {
		t.Fatalf("login_url 应为本机扫码页，得到 %s", dc.VerificationURIComplete)
	}

	// 页面先拉一次二维码，确认内联了 data URL。
	qrResp, err := http.Get(dc.VerificationURIComplete)
	if err != nil {
		t.Fatalf("拉取扫码页失败: %v", err)
	}
	qrBody, _ := io.ReadAll(qrResp.Body)
	qrResp.Body.Close()
	if !strings.Contains(string(qrBody), "data:image/png;base64,") {
		t.Fatalf("扫码页未内联二维码: %s", string(qrBody)[:min(200, len(qrBody))])
	}

	type waitResult struct {
		cred *Credential
		err  error
	}
	done := make(chan waitResult, 1)
	go func() {
		cred, err := session.Wait(context.Background())
		done <- waitResult{cred, err}
	}()

	base := strings.TrimSuffix(dc.VerificationURIComplete, loomyWeChatQRPath)
	for _, want := range []string{loomyPollWaiting, "done"} {
		resp, err := http.Get(base + loomyWeChatPollPath)
		if err != nil {
			t.Fatalf("轮询失败: %v", err)
		}
		var payload map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if payload["status"] != want {
			t.Fatalf("轮询状态应为 %q，得到 %v", want, payload)
		}
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("Wait: %v", res.err)
	}
	if res.cred == nil || res.cred.AccessToken != "sess-skip" || res.cred.AccountID != "u-1" {
		t.Fatalf("凭据不正确: %+v", res.cred)
	}
	if res.cred.Nickname != "小明" {
		t.Fatalf("昵称应来自 bind/auth 响应，得到 %q", res.cred.Nickname)
	}
	if creds := c.Accounts().Credentials(); len(creds) != 1 {
		t.Fatalf("应有 1 条已保存凭据，得到 %d", len(creds))
	}
	if got := c.Accounts().BearerToken(); got != "sess-skip" {
		t.Fatalf("落盘后当前账号令牌应为 sess-skip，得到 %q", got)
	}

	// /login/** 端点必须带 HMAC 签名的 Authorization 与 Nonce；/points/first-login 走
	// apiBase + token 头，不在此列。
	var checked int
	for i, path := range account.seenPaths {
		if !strings.HasPrefix(path, "/login/") {
			continue
		}
		checked++
		if !strings.HasPrefix(account.seenAuth[i], "account test-key-id:") {
			t.Fatalf("账号端点 %s 的 Authorization 应为 account <keyId>:<sig>，得到 %q", path, account.seenAuth[i])
		}
		if account.seenNonce[i] == "" {
			t.Fatalf("账号端点 %s 缺少 Nonce 头", path)
		}
	}
	if checked == 0 {
		t.Fatalf("未触达任何 /login/** 账号端点")
	}
}

// TestLoomyLoginNeedsPhoneBinding 覆盖首次使用（bind==0）：轮询后返回 need_phone，
// 发短信 → 校验 → 结算。
func TestLoomyLoginNeedsPhoneBinding(t *testing.T) {
	account := newLoomyFakeAccount(t, 0)
	defer account.server.Close()
	wechat, wechatLong := newLoomyWechatStub(t, "qr-uuid-2", []string{"wx_errcode=405;wx_code='WX-CODE'"})
	defer wechat.Close()
	defer wechatLong.Close()

	c := newLoomyForTest(t, wechat.URL, wechatLong.URL, account.server.URL)
	session, err := c.BeginLogin(context.Background())
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	dc := session.DeviceCode()
	base := strings.TrimSuffix(dc.VerificationURIComplete, loomyWeChatQRPath)

	done := make(chan error, 1)
	go func() {
		_, err := session.Wait(context.Background())
		done <- err
	}()

	resp, err := http.Get(base + loomyWeChatPollPath)
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	var polled map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&polled)
	resp.Body.Close()
	if polled["status"] != "need_phone" {
		t.Fatalf("首次使用应返回 need_phone，得到 %v", polled)
	}

	post := func(body string) map[string]any {
		t.Helper()
		r, err := http.Post(base+loomyWeChatCompletePath, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("提交表单失败: %v", err)
		}
		defer r.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return out
	}

	if out := post(`{"action":"send_sms","phone":"138-0000-0000"}`); out["ok"] != true {
		t.Fatalf("发短信应成功，得到 %v", out)
	}
	if out := post(`{"action":"verify_sms","phone":"13800000000","code":"1234"}`); out["ok"] != true || out["done"] != true {
		t.Fatalf("校验短信应完成登录，得到 %v", out)
	}

	if err := <-done; err != nil {
		t.Fatalf("Wait: %v", err)
	}
	creds := c.Accounts().Credentials()
	if len(creds) != 1 {
		t.Fatalf("应落盘短信会话凭据，得到 %d 条", len(creds))
	}
	if got := c.Accounts().BearerToken(); got != "sess-sms" {
		t.Fatalf("落盘后当前账号令牌应为 sess-sms，得到 %q", got)
	}
}

// TestLoomyBeginLoginWithoutCredential 验证未配置应用凭据时直接返回可操作的报错，不发
// 任何网络请求。
func TestLoomyBeginLoginWithoutCredential(t *testing.T) {
	c := NewLoomyChannel(t.TempDir())
	c.accessKeyID, c.accessKeySecret = "", ""
	c.wechatBase = "http://127.0.0.1:1"

	_, err := c.BeginLogin(context.Background())
	if err == nil {
		t.Fatalf("未配置凭据时应直接报错")
	}
	for _, want := range []string{"LOOMY_ACCESS_KEY_ID", "LOOMY_ACCESS_KEY_SECRET", loomyClientCredentialFile} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("报错应给出指引 %q，得到 %v", want, err)
		}
	}
}

// TestLoomyResolveCredentials 验证凭据解析优先级：环境变量 > 本地文件 > 内置默认值。
func TestLoomyResolveCredentials(t *testing.T) {
	for _, key := range []string{"LOOMY_ACCESS_KEY_ID", "LOOMY_ACCESS_KEY_SECRET", "LOOMY_APP_ID", "LOOMY_WECHAT_APP_ID"} {
		t.Setenv(key, "")
	}

	// 无文件无环境变量：accessKey 为空，公开标识回退到内置默认值。
	bare := NewLoomyChannel(t.TempDir())
	if bare.accessKeyID != "" || bare.accessKeySecret != "" {
		t.Fatalf("无配置时 accessKey 应为空，得到 %q/%q", bare.accessKeyID, bare.accessKeySecret)
	}
	if bare.appID != loomyAppID || bare.wechatAppID != loomyWeChatAppID {
		t.Fatalf("公开标识应回退到默认值，得到 %q/%q", bare.appID, bare.wechatAppID)
	}

	// 本地文件：<home>/loomy-client.json，home 是凭据目录的上一级。
	home := t.TempDir()
	body := `{"access_key_id":"file-id","access_key_secret":"file-secret","app_id":"FILE-APP","wechat_app_id":"wx-file"}`
	if err := os.WriteFile(filepath.Join(home, loomyClientCredentialFile), []byte(body), 0o600); err != nil {
		t.Fatalf("写凭据文件失败: %v", err)
	}
	fromFile := NewLoomyChannel(filepath.Join(home, "channels"))
	if fromFile.accessKeyID != "file-id" || fromFile.accessKeySecret != "file-secret" {
		t.Fatalf("应从本地文件读取 accessKey，得到 %q/%q", fromFile.accessKeyID, fromFile.accessKeySecret)
	}
	if fromFile.appID != "FILE-APP" || fromFile.wechatAppID != "wx-file" {
		t.Fatalf("应从本地文件读取公开标识，得到 %q/%q", fromFile.appID, fromFile.wechatAppID)
	}

	// 环境变量优先于文件。
	t.Setenv("LOOMY_ACCESS_KEY_ID", "env-id")
	t.Setenv("LOOMY_ACCESS_KEY_SECRET", "env-secret")
	fromEnv := NewLoomyChannel(filepath.Join(home, "channels"))
	if fromEnv.accessKeyID != "env-id" || fromEnv.accessKeySecret != "env-secret" {
		t.Fatalf("环境变量应优先，得到 %q/%q", fromEnv.accessKeyID, fromEnv.accessKeySecret)
	}
}
