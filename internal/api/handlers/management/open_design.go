package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const openDesignAPI = "https://amr-api.open-design.ai/api"

type openDesignDevice struct {
	DeviceID      string    `json:"deviceId"`
	DeviceSecret  string    `json:"deviceSecret"`
	UserCode      string    `json:"userCode"`
	ActivationURL string    `json:"activationUrl"`
	ExpiresAt     time.Time `json:"expiresAt"`
	PollInterval  int       `json:"pollIntervalSeconds"`
}

type openDesignToken struct {
	Status     string `json:"status"`
	ControlKey string `json:"controlKey"`
	User       struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

type openDesignClient struct {
	baseURL string
	http    *http.Client
}

func (s openDesignClient) post(ctx context.Context, path string, body any, result any) (int, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CLIProxyAPI-OpenDesign")
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, errors.New("OpenDesign authorization request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		return resp.StatusCode, errors.New("OpenDesign authorization unavailable")
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil {
		return resp.StatusCode, errors.New("Invalid OpenDesign authorization response")
	}
	return resp.StatusCode, nil
}

func (h *Handler) openDesignClient() openDesignClient {
	client := util.SetProxy(&h.cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second})
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return openDesignClient{baseURL: openDesignAPI, http: client}
}

func (h *Handler) RequestOpenDesignToken(c *gin.Context) {
	client := h.openDesignClient()
	var device openDesignDevice
	status, err := client.post(c.Request.Context(), "/v1/cli/device-authorizations", map[string]string{
		"host": "CLIProxyAPI", "cliVersion": "0.1.3", "profile": "prod",
	}, &device)
	activation, errURL := url.Parse(device.ActivationURL)
	validURL := errURL == nil && activation.Scheme == "https" && activation.Host == "open-design.ai" &&
		activation.Path == "/cloud/cli/activate" && activation.User == nil && activation.Fragment == "" &&
		activation.Query().Get("deviceId") == device.DeviceID && activation.Query().Get("userCode") == device.UserCode
	if err != nil || status != http.StatusCreated || device.DeviceID == "" || device.DeviceSecret == "" ||
		device.UserCode == "" || !validURL || !device.ExpiresAt.After(time.Now()) ||
		device.ExpiresAt.After(time.Now().Add(30*time.Minute)) {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to start OpenDesign device authorization"})
		return
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate state"})
		return
	}
	RegisterOAuthSession(state, "open-design")
	ctx := PopulateAuthContext(context.Background(), c)
	go h.completeOpenDesignLogin(ctx, client, device, state)
	c.JSON(http.StatusOK, gin.H{"url": device.ActivationURL, "state": state, "user_code": device.UserCode,
		"flow": "device", "expires_in": int(time.Until(device.ExpiresAt).Seconds())})
}

func (h *Handler) completeOpenDesignLogin(ctx context.Context, client openDesignClient, device openDesignDevice, state string) {
	ctx, cancel := context.WithDeadline(ctx, device.ExpiresAt)
	defer cancel()
	go watchOAuthSessionCancel(ctx, cancel, state, "open-design")
	interval := time.Duration(device.PollInterval) * time.Second
	if interval < time.Second || interval > 30*time.Second {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil || !IsOAuthSessionPending(state, "open-design") {
			if IsOAuthSessionPending(state, "open-design") {
				SetOAuthSessionError(state, "OpenDesign authorization expired")
			}
			return
		}
		var token openDesignToken
		status, err := client.post(ctx, "/v1/cli/device-authorizations/"+url.PathEscape(device.DeviceID)+"/token",
			map[string]string{"deviceSecret": device.DeviceSecret}, &token)
		if err != nil {
			SetOAuthSessionError(state, "OpenDesign authorization request failed; start login again")
			return
		}
		if (status == http.StatusOK || status == http.StatusAccepted) && token.Status == "pending" {
			select {
			case <-ctx.Done():
			case <-ticker.C:
			}
			continue
		}
		if status != http.StatusOK || (token.Status != "approved" && token.Status != "consumed") ||
			strings.TrimSpace(token.ControlKey) == "" || token.User.ID == "" {
			SetOAuthSessionError(state, "OpenDesign authorization denied, expired or invalid")
			return
		}
		digest := sha256.Sum256([]byte(token.User.ID))
		name := "open-design-" + hex.EncodeToString(digest[:12]) + ".json"
		record := &coreauth.Auth{ID: name, FileName: name, Provider: "open-design", Metadata: map[string]any{
			"type": "open-design", "auth_kind": "oauth", "control_key": token.ControlKey,
			"account_id": token.User.ID, "email": token.User.Email,
		}}
		// This control credential is only for account queries, never model routing.
		if guardOAuthSessionPendingForSave(state, "open-design") != nil {
			return
		}
		if _, err := h.saveTokenRecord(ctx, record); err != nil {
			SetOAuthSessionError(state, "Failed to save OpenDesign account credentials")
			return
		}
		CompleteOAuthSession(state)
		return
	}
}

var openDesignWorkspacePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (h *Handler) OpenDesignQuota(c *gin.Context) {
	workspace := strings.TrimSpace(c.Query("workspace_id"))
	name := strings.TrimSpace(c.Query("name"))
	if !openDesignWorkspacePattern.MatchString(workspace) || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace_id and credential name are required"})
		return
	}
	var auth *coreauth.Auth
	if h.authManager != nil {
		for _, entry := range h.authManager.List() {
			if entry != nil && entry.Provider == "open-design" && (entry.FileName == name || entry.ID == name) {
				auth = entry
				break
			}
		}
	}
	if auth == nil || auth.Disabled {
		c.JSON(http.StatusNotFound, gin.H{"error": "OpenDesign credential unavailable"})
		return
	}
	key, _ := auth.Metadata["control_key"].(string)
	if strings.TrimSpace(key) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OpenDesign login required"})
		return
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet,
		fmt.Sprintf("%s/v1/workspaces/%s/billing/preflight", openDesignAPI, workspace), nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid workspace"})
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-vela-workspace-id", workspace)
	client := util.SetProxy(&h.cfg.SDKConfig, &http.Client{})
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "OpenDesign quota query failed"})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Supplier authentication failures must not expire the CPA management login.
		c.JSON(http.StatusOK, gin.H{"status_code": resp.StatusCode, "error": "OpenDesign account access expired or unavailable; sign in again"})
		return
	}
	var payload map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload) != nil || payload["workspaceId"] != workspace {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Invalid OpenDesign quota response"})
		return
	}
	plan, ok := payload["codingPlan"].(map[string]any)
	if !ok || plan["workspaceId"] != workspace {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Invalid OpenDesign plan response"})
		return
	}
	plan["balanceUsd"] = payload["balanceUsd"]
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, plan)
}
