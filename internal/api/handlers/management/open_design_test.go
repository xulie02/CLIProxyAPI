package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOpenDesignDeviceTokenSavedWithoutRuntimeKey(t *testing.T) {
	dir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, nil)
	h.tokenStore = sdkauth.NewFileTokenStore()
	state := "open-design-success-fixture"
	RegisterOAuthSession(state, "open-design")
	t.Cleanup(func() { CancelOAuthSession(state) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/cli/device-authorizations/fixture-device/token" {
			t.Errorf("unexpected device request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["deviceSecret"] != "fixture-device-secret" {
			t.Error("device secret was not sent in the request body")
		}
		_, _ = w.Write([]byte(`{"status":"approved","controlKey":"fixture-control-key","runtimeKey":"fixture-runtime-key","user":{"id":"fixture-user","email":"fixture@example.invalid"}}`))
	}))
	defer server.Close()
	h.completeOpenDesignLogin(context.Background(), openDesignClient{baseURL: server.URL, http: server.Client()},
		openDesignDevice{DeviceID: "fixture-device", DeviceSecret: "fixture-device-secret", ExpiresAt: time.Now().Add(time.Minute)}, state)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("saved credentials: %v, %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "fixture-control-key") || strings.Contains(string(data), "fixture-runtime-key") ||
		strings.Contains(string(data), "access_token") || strings.Contains(string(data), "fixture-device-secret") {
		t.Fatalf("incorrect account credential fields")
	}
	_, _, _, _, completed, ok := GetOAuthSessionDetails(state)
	if !ok || !completed {
		t.Fatal("login did not complete")
	}
}

type openDesignQuotaTransport struct {
	status int
	body   string
	t      *testing.T
}

func (tr openDesignQuotaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() != openDesignAPI+"/v1/workspaces/fixture/billing/preflight" || req.Header.Get("Authorization") != "Bearer fixture-control-key" {
		tr.t.Error("control credential was not confined to the preflight endpoint")
	}
	return &http.Response{StatusCode: tr.status, Body: io.NopCloser(strings.NewReader(tr.body)), Header: make(http.Header)}, nil
}

func TestOpenDesignPreflightWorkspaceAndAuthenticationBoundary(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{ID: "open-design-fixture.json", FileName: "open-design-fixture.json", Provider: "open-design", Metadata: map[string]any{"control_key": "fixture-control-key"}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	router := gin.New()
	router.GET("/quota", h.OpenDesignQuota)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, tc := range []struct {
		upstream int
		body     string
		status   int
	}{
		{401, `{"error":"fixture-control-key"}`, 200},
		{200, `{"workspaceId":"other","codingPlan":{}}`, 502},
		{200, `{"workspaceId":"fixture","codingPlan":{"workspaceId":"other"}}`, 502},
		{200, `{"workspaceId":"fixture","balanceUsd":"1.50","codingPlan":{"workspaceId":"fixture","eligible":true,"windows":[]}}`, 200},
	} {
		http.DefaultTransport = openDesignQuotaTransport{tc.upstream, tc.body, t}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/quota?workspace_id=fixture&name=open-design-fixture.json", nil))
		if w.Code != tc.status || strings.Contains(w.Body.String(), "fixture-control-key") {
			t.Fatalf("unsafe quota response: %d %s", w.Code, w.Body.String())
		}
		if tc.upstream == 401 && !strings.Contains(w.Body.String(), `"status_code":401`) {
			t.Fatal("supplier 401 was confused with management authorization")
		}
	}
}

func TestOpenDesignCancelledDuringExchangeDoesNotSave(t *testing.T) {
	dir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, nil)
	h.tokenStore = sdkauth.NewFileTokenStore()
	state := "open-design-cancelled-fixture"
	RegisterOAuthSession(state, "open-design")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		CancelOAuthSession(state)
		_, _ = w.Write([]byte(`{"status":"approved","controlKey":"fixture-secret","user":{"id":"fixture-user"}}`))
	}))
	defer server.Close()
	h.completeOpenDesignLogin(context.Background(), openDesignClient{baseURL: server.URL, http: server.Client()},
		openDesignDevice{DeviceID: "fixture-device", DeviceSecret: "fixture-secret", ExpiresAt: time.Now().Add(time.Minute)}, state)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("cancelled authorization saved account keys")
	}
}

func TestOpenDesignErrorsDoNotExposeUpstreamSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"fixture-secret-token"}`))
	}))
	defer server.Close()
	var token openDesignToken
	_, err := (openDesignClient{baseURL: server.URL, http: server.Client()}).post(context.Background(), "/token", map[string]string{}, &token)
	if err == nil || strings.Contains(err.Error(), "fixture-secret-token") {
		t.Fatal("unsafe upstream error")
	}
}

func TestOpenDesignQuotaRequiresExplicitSafeWorkspaceAndAccount(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	router := gin.New()
	router.GET("/quota", h.OpenDesignQuota)
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"?name=fixture", 400}, {"?workspace_id=../other&name=fixture", 400},
		{"?workspace_id=fixture&name=open-design-fixture.json", 404},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/quota"+tc.query, nil))
		if w.Code != tc.status {
			t.Fatalf("%s: status %d", tc.query, w.Code)
		}
	}
}
