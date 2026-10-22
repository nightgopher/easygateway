package provider

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

func reloadTestYAML(url, model string) string {
	return fmt.Sprintf("providers:\n  - name: test\n    type: openai\n    url: %s\n    allow_no_key: true\n    models:\n      - id: %s\n        capabilities: [text, streaming]\nchains:\n  coding: [%s]\n", url, model, model)
}

func TestConfigReloadKeepsInflightRequestAndRejectsInvalidConfig(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"id":"test","choices":[{"message":{"role":"assistant","content":"completed"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	defer releaseRequest()
	path := writeTempProvidersConfig(t, reloadTestYAML(upstream.URL, "old"))
	router := NewRouter(log.New(io.Discard, "", 0))
	reloader := NewConfigReloader(router, path, nil, nil)
	if _, err := reloader.Reload(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := router.Forward(context.Background(), "old", newProbeRequest("old", "ping"))
		if err != nil || resp == nil {
			t.Errorf("in-flight request failed: %v", err)
		}
	}()
	<-entered
	if err := os.WriteFile(path, []byte(reloadTestYAML(upstream.URL, "new")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloader.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Route("old"); err == nil {
		t.Fatal("removed route remains")
	}
	if _, err := router.Route("new"); err != nil {
		t.Fatal(err)
	}
	releaseRequest()
	wg.Wait()
	if err := os.WriteFile(path, []byte("providers: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloader.Reload(); err == nil {
		t.Fatal("invalid reload succeeded")
	}
	if _, err := router.Route("new"); err != nil {
		t.Fatal("invalid reload damaged old route", err)
	}
	s := reloader.Status()
	if s.Generation != 2 || s.LastError == "" || s.LastAppliedAt.IsZero() {
		t.Fatalf("status=%+v", s)
	}
}

func TestConfigReloadPreservesControlsAndConcurrentReaders(t *testing.T) {
	path := writeTempProvidersConfig(t, reloadTestYAML("https://example.invalid", "model"))
	router := NewRouter(nil)
	reloader := NewConfigReloader(router, path, nil, nil)
	if _, err := reloader.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := router.SetProviderEnabled("model", false); err != nil {
		t.Fatal(err)
	}
	if err := router.SetModelCapabilities("model", []string{"text"}, []string{"text"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				router.ListRouteStatuses()
				router.ListModelInfos()
				router.ListProviderStatuses()
			}
		}()
	}
	for i := 0; i < 10; i++ {
		if _, err := reloader.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	s, _ := router.ProviderStatus("model")
	if s.Enabled {
		t.Fatal("reload enabled a disabled provider")
	}
	for _, info := range router.ListModelInfos() {
		if info.ID == "model" && (len(info.Capabilities) != 1 || info.Capabilities[0] != "text") {
			t.Fatalf("capability override lost: %+v", info)
		}
	}
	// A declared model with lost credentials must not replace an active client.
	t.Setenv("RELOAD_TEST_KEY", "")
	for _, id := range []string{"model", "renamed"} {
		body := fmt.Sprintf("providers:\n - name: test\n   type: openai\n   url: https://example.invalid\n   api_key_env: RELOAD_TEST_KEY\n   models: [{id: %s}]\n", id)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := reloader.Reload(); err == nil {
			t.Fatalf("lost credentials accepted after model ID %s", id)
		}
	}
}
