package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testWrapper(caps []string, url string) *BoundModelProviderWrapper {
	base := NewOpenAIProvider(&Config{Name: "siliconflow", URL: url, APIKey: "sk-probe-key", UseBearer: true})
	return NewBoundModelProviderWrapper(base, "test/model", caps)
}

func TestProbeCapability_Upstream404IsOffline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	w := testWrapper([]string{CapabilityEmbedding}, upstream.URL+"/")
	result := w.Probe(context.Background())
	if result.Status != ProbeOffline || result.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %s code=%d, want offline/404", result.Status, result.StatusCode)
	}
}
