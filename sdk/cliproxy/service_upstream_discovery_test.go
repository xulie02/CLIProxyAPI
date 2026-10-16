package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestUpstreamDiscoveryPublishesCapabilitiesThroughAliases(t *testing.T) {
	original := modelconfig.DefaultUpstreamCatalog
	modelconfig.DefaultUpstreamCatalog = modelconfig.NewUpstreamCatalog()
	t.Cleanup(func() { modelconfig.DefaultUpstreamCatalog = original })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("wrong catalog path or credential")
		}
		fmt.Fprint(w, `{"data":[{"id":"upstream-model","context_window":1048576,"effort":{"supported_levels":["low","high","max"],"default_level":"high"}},{"id":"auto"}]}`)
	}))
	defer server.Close()
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "catalog-test", BaseURL: server.URL,
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "test-key"}},
		Models: []config.OpenAICompatibilityModel{
			{Name: "upstream-model", Alias: "discovered-public"},
			{Name: "auto", Alias: "gpt-5.6-sol"},
			{Name: "opaque-real-name", Alias: "claude-opus-4-6"},
			{Name: "upstream-model", Alias: "manual-public", Thinking: &registry.ThinkingSupport{Levels: []string{"max"}}, MaxContextLength: 32000},
		},
	}}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth := &coreauth.Auth{ID: "discovery-test-auth", Provider: "catalog-test", Status: coreauth.StatusActive, Attributes: map[string]string{
		coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey, "api_key": "test-key", "compat_name": "catalog-test", "provider_key": "catalog-test", "base_url": server.URL,
	}}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	service := &Service{cfg: cfg, coreManager: manager}
	service.refreshUpstreamDiscovery(context.Background())
	registered := GlobalModelRegistry().GetModelsForClient(auth.ID)
	if len(registered) != 4 {
		t.Fatalf("registered models = %d, want 4", len(registered))
	}
	input := []map[string]any{{"id": "discovered-public"}, {"id": "manual-public"}, {"id": "gpt-5.6-sol"}, {"id": "claude-opus-4-6"}}
	response := codexmodels.BuildResponseForClient(input, func(string) []string { return []string{"catalog-test"} }, false, "")
	for _, entry := range response["models"].([]map[string]any) {
		slug := entry["slug"].(string)
		var levels []string
		for _, raw := range entry["supported_reasoning_levels"].([]any) {
			levels = append(levels, raw.(map[string]any)["effort"].(string))
		}
		switch slug {
		case "claude-opus-4-6", "gpt-5.6-sol":
			known := registry.LookupModelCapabilityInfo(slug)
			if !reflect.DeepEqual(levels, known.Thinking.Levels) || entry["context_window"] != known.ContextLength {
				t.Fatal("configured alias capabilities did not reach the client catalog")
			}
		case "discovered-public":
			if !reflect.DeepEqual(levels, []string{"low", "high", "max"}) || entry["default_reasoning_level"] != "high" || entry["context_window"] != 1048576 {
				t.Fatalf("discovered catalog metadata = %+v", entry)
			}
		case "manual-public":
			if !reflect.DeepEqual(levels, []string{"max"}) || entry["context_window"] != 32000 {
				t.Fatalf("manual override lost priority: %+v", entry)
			}
		default:
			t.Fatalf("unexpected model %s", slug)
		}
	}
}
