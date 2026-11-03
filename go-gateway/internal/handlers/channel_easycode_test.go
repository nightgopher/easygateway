package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/channel"
)

type browserTestChannel struct {
	accounts *channel.Accounts
	release  chan struct{}
	received chan string
}

func (*browserTestChannel) Name() string                  { return "easycode" }
func (c *browserTestChannel) Accounts() *channel.Accounts { return c.accounts }
func (*browserTestChannel) BeginLogin(context.Context) (channel.LoginSession, error) {
	panic("client callback required")
}
func (c *browserTestChannel) BeginClientLogin(_ context.Context, callback string) (channel.LoginSession, error) {
	return &browserTestSession{channel: c, callback: callback}, nil
}

type browserTestSession struct {
	channel  *browserTestChannel
	callback string
}

func (*browserTestSession) Channel() string { return "easycode" }
func (*browserTestSession) DeviceCode() channel.DeviceCode {
	return channel.DeviceCode{ExpiresIn: time.Minute, Interval: time.Second}
}
func (s *browserTestSession) ClientOAuth() channel.ClientOAuth {
	return channel.ClientOAuth{State: "test-state", Callback: s.callback}
}
func (s *browserTestSession) Complete(_ context.Context, state, code, oauthErr string) error {
	s.channel.received <- state + ":" + code + ":" + oauthErr
	return nil
}
func (s *browserTestSession) Wait(ctx context.Context) (*channel.Credential, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.channel.release:
		return &channel.Credential{AccessToken: "must-not-leak", Nickname: "Alice"}, nil
	}
}

func TestClientOAuthCallbackIsAcknowledgedWhileExchangeContinues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ch := &browserTestChannel{accounts: channel.NewAccounts("easycode", channel.NewStore(t.TempDir()), nil, nil), release: make(chan struct{}), received: make(chan string, 1)}
	registry := channel.NewRegistry()
	registry.Register(ch)
	h := NewChannelAdminHandler(registry, log.New(io.Discard, "", 0))
	r := gin.New()
	r.POST("/channels/:channel/login", h.HandleLoginStart)
	r.POST("/channels/:channel/login/:id/callback", h.HandleLoginCallback)
	r.GET("/channels/:channel/login/:id", h.HandleLoginPoll)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	start := request(http.MethodPost, "/channels/easycode/login", `{"callback":"http://localhost:7863/callback"}`)
	var payload struct {
		ID    string              `json:"login_id"`
		OAuth channel.ClientOAuth `json:"client_oauth"`
	}
	if start.Code != 200 || json.Unmarshal(start.Body.Bytes(), &payload) != nil || payload.ID == "" || payload.OAuth.Callback != "http://localhost:7863/callback" {
		t.Fatal("client login session missing", start.Body.String())
	}
	callback := request(http.MethodPost, "/channels/easycode/login/"+payload.ID+"/callback", `{"state":"state.0","code":"one-use"}`)
	if callback.Code != http.StatusAccepted || <-ch.received != "state.0:one-use:" {
		t.Fatal("callback was not acknowledged")
	}
	pending := request(http.MethodGet, "/channels/easycode/login/"+payload.ID, "")
	if pending.Code != http.StatusAccepted {
		t.Fatal("login finished before exchange")
	}
	h.mu.Lock()
	session := h.pendings[payload.ID]
	h.mu.Unlock()
	close(ch.release)
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("login did not complete")
	}
	finished := request(http.MethodGet, "/channels/easycode/login/"+payload.ID, "")
	if finished.Code != http.StatusOK || !strings.Contains(finished.Body.String(), `"status":"complete"`) || strings.Contains(finished.Body.String(), "must-not-leak") {
		t.Fatal("unsafe login result", finished.Body.String())
	}
}
