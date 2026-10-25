package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/metrics"
	"github.com/nightgopher/easygateway/internal/provider"
)

func TestAdminProviderControlsExposeAndUpdateRuntimeState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	router.RegisterProvider("first", &adminTestProvider{name: "first"})
	router.RegisterProvider("second", &adminTestProvider{name: "second"})
	router.RegisterChain("coding", []string{"first", "second"})
	handler := NewAdminHandler(router, metrics.NewCollector(), logger, nil)

	engine := gin.New()
	engine.GET("/admin/providers", handler.HandleProviders)
	engine.PATCH("/admin/providers/:name", handler.HandleUpdateProvider)
	engine.PUT("/admin/routes/:model", handler.HandleUpdateRoute)
	engine.PUT("/admin/models/:model", handler.HandleUpdateModel)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/providers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET providers status = %d: %s", w.Code, w.Body.String())
	}
	var initial struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode providers: %v", err)
	}
	if len(initial.Providers) != 2 || initial.Providers[0]["state"] != string(provider.CircuitClosed) {
		t.Fatalf("initial provider state = %#v", initial.Providers)
	}

	w = httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/admin/providers/first", bytes.NewBufferString(`{"enabled":false}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH provider status = %d: %s", w.Code, w.Body.String())
	}
	if status, ok := router.ProviderStatus("first"); !ok || status.Enabled {
		t.Fatalf("provider should be disabled, status = %#v", status)
	}

	w = httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/admin/routes/coding", bytes.NewBufferString(`{"providers":["second","first"]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT route status = %d: %s", w.Code, w.Body.String())
	}
	var updatedRoute provider.RouteStatus
	if err := json.Unmarshal(w.Body.Bytes(), &updatedRoute); err != nil {
		t.Fatalf("decode updated route: %v", err)
	}
	if updatedRoute.Model != "coding" || len(updatedRoute.Providers) != 2 {
		t.Fatalf("updated route payload = %#v", updatedRoute)
	}
	if routes := router.ListRouteStatuses(); len(routes) != 1 || routes[0].Providers[0].Name != "second" {
		t.Fatalf("route order was not updated: %#v", routes)
	}

	w = httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/admin/models/coding", bytes.NewBufferString(`{"capabilities":["text","vision"],"input_modalities":["text","image"]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT model status = %d: %s", w.Code, w.Body.String())
	}
	info := router.ListModelInfos()
	if len(info) != 1 || len(info[0].Capabilities) != 2 || info[0].Capabilities[1] != provider.CapabilityVision {
		t.Fatalf("model capabilities were not updated: %#v", info)
	}
}

func TestAdminHealthDoesNotTreatUnprobedProvidersAsDegraded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	router.RegisterProvider("unprobed", &adminTestProvider{name: "unprobed"})
	handler := NewAdminHandler(router, metrics.NewCollector(), logger, nil)

	engine := gin.New()
	engine.GET("/admin/health", handler.HandleHealth)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET health status = %d: %s", response.Code, response.Body.String())
	}

	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if payload.Status != "ok" {
		t.Fatalf("unprobed provider health status = %q, want ok", payload.Status)
	}
}

type adminTestProvider struct {
	name string
}

func (p *adminTestProvider) Name() string                     { return p.name }
func (p *adminTestProvider) URL() string                      { return "http://example.com" }
func (p *adminTestProvider) APIKey() string                   { return "" }
func (p *adminTestProvider) UseBearer() bool                  { return true }
func (p *adminTestProvider) IsHealthy(_ context.Context) bool { return true }
func (p *adminTestProvider) ForwardRequest(_ context.Context, _ *provider.Request) (*provider.Response, error) {
	return &provider.Response{Model: p.name}, nil
}
