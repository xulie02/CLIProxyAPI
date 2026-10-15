package modelconfig

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestUpstreamCatalogCachesExactCredentialAndModelMetadata(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/models" || r.Header.Get("X-Catalog-Test") != "required" {
			t.Errorf("unexpected request path or headers")
		}
		if r.Header.Get("Authorization") == "Bearer key-a" {
			fmt.Fprint(w, `{"data":[{"id":"actual-model","context_window":1048576,"effort":{"supported_levels":["low","high","max"],"default_level":"high"}},{"id":"auto"}]}`)
		} else {
			fmt.Fprint(w, `{"data":[{"id":"actual-model","effort":{"supported_levels":["high"],"default_level":"high"}}]}`)
		}
	}))
	defer server.Close()
	catalog := NewUpstreamCatalog()
	source := UpstreamSource{BaseURL: server.URL, APIKey: "key-a", Headers: map[string]string{"X-Catalog-Test": "required"}}
	if changed, err := catalog.Refresh(context.Background(), server.Client(), source); err != nil || !changed {
		t.Fatalf("refresh=(%v,%v)", changed, err)
	}
	capability := catalog.Lookup(source, "actual-model")
	if capability.ContextWindow != 1048576 || capability.Thinking == nil || capability.Thinking.DefaultLevel != "high" || !reflect.DeepEqual(capability.Thinking.Levels, []string{"low", "high", "max"}) {
		t.Fatalf("unexpected capability: %+v", capability)
	}
	capability.Thinking.Levels[0] = "modified"
	if catalog.Lookup(source, "actual-model").Thinking.Levels[0] != "low" {
		t.Fatal("cache leaked mutable storage")
	}
	if catalog.Lookup(source, "public-alias").Thinking != nil || catalog.Lookup(source, "auto").Thinking != nil {
		t.Fatal("model alias or metadata-free model inherited capabilities")
	}
	if changed, err := catalog.Refresh(context.Background(), server.Client(), source); changed || err != nil || calls.Load() != 1 {
		t.Fatal("cache TTL did not prevent redundant discovery")
	}
	source.APIKey = "key-b"
	source.BaseURL += "/v1"
	if _, err := catalog.Refresh(context.Background(), server.Client(), source); err != nil {
		t.Fatal(err)
	}
	if got := catalog.Lookup(source, "actual-model"); !reflect.DeepEqual(got.Thinking.Levels, []string{"high"}) {
		t.Fatal("credentials shared capability metadata")
	}
}

func TestUpstreamCatalogDropsMissingAndFailedMetadata(t *testing.T) {
	for _, response := range []struct {
		name, body string
		status     int
	}{
		{"standard model list", `{"data":[{"id":"model"}]}`, 200},
		{"removed model", `{"data":[]}`, 200},
		{"unauthorized", `{"error":"credential detail"}`, 401},
		{"unsupported", `{}`, 404},
		{"invalid schema", `{"data":"invalid"}`, 200},
		{"invalid JSON", `not json`, 200},
	} {
		t.Run(response.name, func(t *testing.T) {
			valid := true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if valid {
					fmt.Fprint(w, `{"data":[{"id":"model","context_window":64000,"effort":{"supported_levels":["high"]}}]}`)
					return
				}
				w.WriteHeader(response.status)
				fmt.Fprint(w, response.body)
			}))
			defer server.Close()
			catalog := NewUpstreamCatalog()
			now := time.Unix(1000, 0)
			catalog.now = func() time.Time { return now }
			source := UpstreamSource{BaseURL: server.URL, APIKey: "secret"}
			if _, err := catalog.Refresh(context.Background(), server.Client(), source); err != nil {
				t.Fatal(err)
			}
			valid = false
			now = now.Add(6 * time.Minute)
			changed, _ := catalog.Refresh(context.Background(), server.Client(), source)
			if !changed {
				t.Fatal("missing upstream capabilities did not invalidate old data")
			}
			got := catalog.Lookup(source, "model")
			if got.Thinking != nil || got.ContextWindow != 0 {
				t.Fatalf("stale metadata retained: %+v", got)
			}
		})
	}
}

func TestDiscoveryDoesNotFollowCredentialRedirects(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer origin.Close()
	_, err := NewUpstreamCatalog().Refresh(context.Background(), origin.Client(), UpstreamSource{BaseURL: origin.URL, APIKey: "private-key"})
	if err == nil || redirected.Load() {
		t.Fatal("discovery followed an upstream redirect")
	}
}

func TestExplicitThinkingAndContextOverrideUpstream(t *testing.T) {
	original := DefaultUpstreamCatalog
	DefaultUpstreamCatalog = NewUpstreamCatalog()
	defer func() { DefaultUpstreamCatalog = original }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"model","context_window":128000,"effort":{"supported_levels":["low","high","max"],"default_level":"high"}}]}`)
	}))
	defer server.Close()
	source := UpstreamSource{BaseURL: server.URL}
	if _, err := DefaultUpstreamCatalog.Refresh(context.Background(), server.Client(), source); err != nil {
		t.Fatal(err)
	}
	for _, override := range []*registry.ThinkingSupport{nil, {}, {Levels: []string{"max"}}} {
		info := &registry.ModelInfo{Thinking: override, ExplicitThinking: override != nil, ContextLength: 32000, MaxContextLength: 32000}
		ApplyUpstreamCapabilities(info, source, "model(high)")
		if info.ContextLength != 32000 {
			t.Fatal("explicit context lost priority")
		}
		if override != nil && !reflect.DeepEqual(info.Thinking, override) {
			t.Fatal("explicit thinking lost priority")
		}
		if override == nil && (info.Thinking == nil || info.Thinking.DefaultLevel != "high") {
			t.Fatal("upstream metadata was not applied")
		}
	}
	if info := ResolveModelInfo("claude-opus-4-6", "openai-compatibility", nil); info.Thinking == nil {
		t.Fatal("known model lost its internal catalog fallback")
	}
}

func TestUpstreamCapabilitiesFallBackPerFieldToInternalModel(t *testing.T) {
	original := DefaultUpstreamCatalog
	DefaultUpstreamCatalog = NewUpstreamCatalog()
	defer func() { DefaultUpstreamCatalog = original }()
	known := registry.LookupStaticModelInfo("claude-opus-4-6")
	if known == nil || known.Thinking == nil || known.ContextLength <= 0 {
		t.Fatal("test requires a known model with capabilities")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer override":
			fmt.Fprint(w, `{"data":[{"id":"claude-opus-4-6","context_window":123456,"effort":{"supported_levels":["high"],"default_level":"high"}}]}`)
		case "Bearer partial":
			fmt.Fprint(w, `{"data":[{"id":"claude-opus-4-6","context_window":123456}]}`)
		case "Bearer empty":
			fmt.Fprint(w, `{"data":[{"id":"claude-opus-4-6","effort":{"supported_levels":[]}}]}`)
		default:
			fmt.Fprint(w, `{"data":[{"id":"claude-opus-4-6"}]}`)
		}
	}))
	defer server.Close()
	for _, key := range []string{"override", "partial", "empty", "missing"} {
		t.Run(key, func(t *testing.T) {
			source := UpstreamSource{BaseURL: server.URL, APIKey: key}
			if _, err := DefaultUpstreamCatalog.Refresh(t.Context(), server.Client(), source); err != nil {
				t.Fatal(err)
			}
			info := &registry.ModelInfo{}
			ApplyUpstreamCapabilities(info, source, "claude-opus-4-6(high)")
			if key == "override" {
				if !reflect.DeepEqual(info.Thinking.Levels, []string{"high"}) || info.Thinking.DefaultLevel != "high" {
					t.Fatal("internal table overrode upstream thinking")
				}
			} else if key == "empty" {
				if info.Thinking == nil || len(info.Thinking.Levels) != 0 {
					t.Fatal("explicit empty upstream capability was overridden")
				}
			} else if !reflect.DeepEqual(info.Thinking, NormalizeThinkingSupport(known.Thinking)) {
				t.Fatal("missing upstream field did not fall back to internal thinking")
			}
			wantContext := known.ContextLength
			if key == "override" || key == "partial" {
				wantContext = 123456
			}
			if info.ContextLength != wantContext {
				t.Fatalf("context=%d, want %d", info.ContextLength, wantContext)
			}
			unknown := &registry.ModelInfo{ID: "claude-opus-4-6"}
			ApplyUpstreamCapabilities(unknown, source, "unknown-actual-upstream")
			if unknown.Thinking != nil || unknown.ContextLength != 0 {
				t.Fatal("unknown upstream inherited capabilities from a public alias")
			}
		})
	}
}
