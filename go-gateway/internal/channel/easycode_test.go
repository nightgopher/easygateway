package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEasyCodeLoginRefreshAndCatalog(t *testing.T) {
	var exchanges, refreshes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/config/client":
			fmt.Fprint(w, `{"success":true,"data":{"feishuTenants":[{"appId":"app-a","label":"A"},{"appId":"app-b","label":"B"}]}}`)
		case "/api/auth/feishu/exchange":
			exchanges.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["code"] != "code-fixture" || body["app_id"] != "app-b" || body["redirect_uri"] != "http://localhost:7863/callback" {
				t.Error("wrong authorization exchange")
			}
			fmt.Fprint(w, `{"success":true,"data":{"accessToken":"feishu-secret"}}`)
		case "/auth/jwt/feishu-login":
			fmt.Fprint(w, `{"accessToken":"old-secret","refreshToken":"refresh-secret","expiresIn":900,"user":{"userId":"user-1","name":"Alice"}}`)
		case "/auth/jwt/refresh":
			refreshes.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["refreshToken"] != "refresh-secret" {
				t.Error("wrong refresh token")
			}
			fmt.Fprint(w, `{"success":true,"data":{"accessToken":"new-secret","refreshToken":"rotated-secret","expiresIn":900}}`)
		case "/web-api/models":
			if r.Header.Get("Authorization") == "Bearer old-secret" {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer new-secret" {
				t.Error("catalog used wrong account token")
			}
			fmt.Fprint(w, `{"success":true,"data":[{"name":"vision","displayName":"Vision","available":true,"supportsImages":true,"maxToken":200000},{"name":"disabled","available":false},{"name":""},{"name":"text","supportsImages":false}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	ch := NewEasyCodeChannel(dir)
	ch.base, ch.client = upstream.URL, upstream.Client()
	for _, invalid := range []string{"http://example.com:7863/callback", "https://localhost:7863/callback", "http://localhost:1/callback", "http://localhost:7863/callback?x=1", "http://127.0.0.1:7863/callback"} {
		if _, err := ch.BeginClientLogin(context.Background(), invalid); err == nil {
			t.Fatalf("accepted unsafe callback %q", invalid)
		}
	}
	session, err := ch.BeginClientLogin(context.Background(), "http://localhost:7863/callback")
	if err != nil {
		t.Fatal(err)
	}
	s := session.(*easyCodeLogin)
	if len(s.oauth.Tenants) != 2 {
		t.Fatal("tenant list missing")
	}
	if err := s.Complete(context.Background(), "wrong.1", "code-fixture", ""); err == nil {
		t.Fatal("accepted invalid state")
	}
	result := make(chan error, 1)
	go func() { _, err := s.Wait(context.Background()); result <- err }()
	if err := s.Complete(context.Background(), s.oauth.State+".1", "code-fixture", ""); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if s.Cancel() {
		t.Fatal("committed login reported cancellation")
	}
	if err := s.Complete(context.Background(), s.oauth.State+".1", "code-fixture", ""); err == nil {
		t.Fatal("callback replay accepted")
	}
	if exchanges.Load() != 1 || ch.accounts.Len() != 1 {
		t.Fatal("login did not save exactly one account")
	}
	file, err := os.Stat(filepath.Join(dir, "easycode.json"))
	if err != nil || file.Mode().Perm() != 0600 {
		t.Fatal("credential file permissions", err)
	}
	public, _ := json.Marshal(ch.accounts.Credentials())
	if strings.Contains(string(public), "secret") {
		t.Fatal("credential exposed in account summary")
	}
	models, err := ch.Models(context.Background())
	if err != nil || len(models) != 2 || models[0].ID != "text" || models[0].SupportsImages || !models[1].SupportsImages || models[1].ContextWindow != 160000 {
		t.Fatalf("wrong catalog: %#v %v", models, err)
	}
	if refreshes.Load() != 1 || ch.accounts.BearerToken() != "new-secret" {
		t.Fatal("expired access token not refreshed")
	}
	stored, err := ch.accounts.Store().Load("easycode")
	if err != nil || len(stored) != 1 || stored[0].RefreshToken != "rotated-secret" {
		t.Fatal("rotated refresh token not persisted")
	}
}

func TestEasyCodeCancelledLoginDoesNotSave(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"feishu":{"appId":"app-a"}}}`)
	}))
	defer upstream.Close()
	ch := NewEasyCodeChannel(t.TempDir())
	ch.base, ch.client = upstream.URL, upstream.Client()
	session, err := ch.BeginClientLogin(context.Background(), "http://localhost:7864/callback")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.Wait(ctx); err == nil {
		t.Fatal("cancelled login succeeded")
	}
	callbackCtx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	s := session.(*easyCodeLogin)
	if err := s.Complete(callbackCtx, s.oauth.State+".0", "code", ""); err == nil {
		t.Fatal("cancelled session accepted callback")
	}
	if ch.accounts.Len() != 0 {
		t.Fatal("cancelled session saved credentials")
	}
}

func TestEasyCodeCancelDuringExchangeDoesNotCommit(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/config/client":
			fmt.Fprint(w, `{"success":true,"data":{"feishu":{"appId":"app-a"}}}`)
		case "/api/auth/feishu/exchange":
			fmt.Fprint(w, `{"success":true,"data":{"accessToken":"feishu"}}`)
		case "/auth/jwt/feishu-login":
			close(entered)
			<-release
			fmt.Fprint(w, `{"accessToken":"secret","user":{"userId":"alice"}}`)
		}
	}))
	defer server.Close()
	ch := NewEasyCodeChannel(t.TempDir())
	ch.base, ch.client = server.URL, server.Client()
	session, err := ch.BeginClientLogin(context.Background(), "http://localhost:7863/callback")
	if err != nil {
		t.Fatal(err)
	}
	s := session.(*easyCodeLogin)
	result := make(chan error, 1)
	go func() { _, err := s.Wait(context.Background()); result <- err }()
	callback := make(chan error, 1)
	go func() { callback <- s.Complete(context.Background(), s.oauth.State+".0", "code", "") }()
	<-entered
	if !s.Cancel() {
		t.Fatal("unfinished login cannot be cancelled")
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("cancelled login succeeded")
	}
	if err := <-callback; err != nil {
		t.Fatal("authorization was not accepted before cancellation")
	}
	if ch.accounts.Len() != 0 {
		t.Fatal("cancelled login persisted credentials")
	}
}
