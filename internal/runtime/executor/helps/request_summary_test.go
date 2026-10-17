package helps

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

func TestUpstreamRequestSummaryUsesFinalPayloadWithoutBodyLogging(t *testing.T) {
	hook := setupResponseModelLoggerHook(t)
	log.SetLevel(log.InfoLevel)
	ctx := usage.WithRequestedModelAlias(context.Background(), "public-alias")
	ctx = usage.WithReasoningEffort(ctx, "high")
	ctx = logging.WithClientRequestMetadata(ctx, logging.ClientRequestMetadata{
		ClientIP: "172.18.0.6", ResolvedClientIP: "203.0.113.5",
	})
	tests := []struct {
		name, provider, body, url, model, effort, thinkingMode string
		websocket                                              bool
	}{
		{name: "HTTP configured override", provider: "custom-channel", body: `{"model":"upstream-model","reasoning_effort":"low","messages":[{"content":"private prompt"}]}`, model: "upstream-model", effort: "low"},
		{name: "Responses", provider: "custom-channel", body: `{"model":"upstream-model","reasoning":{"effort":"xhigh"}}`, model: "upstream-model", effort: "xhigh"},
		{name: "removed parameter", provider: "openai", body: `{"model":"upstream-model"}`, model: "upstream-model", effort: "unspecified"},
		{name: "DeepSeek thinking disabled without effort", provider: "custom-channel", body: `{"model":"deepseek-flash","thinking":{"type":"disabled"}}`, model: "deepseek-flash", effort: "unspecified", thinkingMode: "disabled"},
		{name: "DeepSeek thinking enabled without effort", provider: "custom-channel", body: `{"model":"deepseek-flash","thinking":{"type":"enabled"}}`, model: "deepseek-flash", effort: "unspecified", thinkingMode: "enabled"},
		{name: "DeepSeek enabled with selected effort", provider: "custom-channel", body: `{"model":"deepseek-flash","thinking":{"type":"enabled"},"reasoning_effort":"max"}`, model: "deepseek-flash", effort: "max", thinkingMode: "enabled"},
		{name: "explicitly disabled", provider: "openai", body: `{"model":"upstream-model","reasoning_effort":"none"}`, model: "upstream-model", effort: "none"},
		{name: "websocket", provider: "codex", body: `{"type":"response.create","model":"upstream-model","reasoning":{"effort":"medium"}}`, model: "upstream-model", effort: "medium", websocket: true},
		{name: "Gemini URL model", provider: "gemini", url: "https://example.com/v1beta/models/gemini-model:generateContent?key=secret-key", body: `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`, model: "gemini-model", effort: "high"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hook.Reset()
			info := UpstreamRequestLog{Provider: tc.provider, URL: tc.url, Body: []byte(tc.body), AuthValue: "secret-key"}
			if tc.websocket {
				RecordAPIWebsocketRequest(ctx, &config.Config{}, info)
			} else {
				RecordAPIRequest(ctx, &config.Config{}, info)
			}
			entries := hook.AllEntries()
			if len(entries) != 1 {
				t.Fatalf("log entries = %d, want 1", len(entries))
			}
			entry := entries[0]
			if entry.Message != "模型请求" {
				t.Errorf("message = %q, want localized model request message", entry.Message)
			}
			wantThinkingMode := tc.thinkingMode
			if wantThinkingMode == "" {
				wantThinkingMode = "unspecified"
			}
			for key, want := range map[string]string{
				"client_ip": "203.0.113.5",
				"model":     tc.model, "requested_model": "public-alias",
				"requested_reasoning_effort": "high", "upstream_reasoning_effort": tc.effort,
				"upstream_thinking": wantThinkingMode,
			} {
				if entry.Data[key] != want {
					t.Errorf("%s = %v, want %s", key, entry.Data[key], want)
				}
			}
			formatted, err := (&logging.LogFormatter{}).Format(entry)
			if err != nil {
				t.Fatal(err)
			}
			line := string(formatted)
			for _, want := range []string{"模型请求", `来源IP="203.0.113.5"`, `请求模型="public-alias"`, `请求思考等级="高(high)"`, `上游模型="` + tc.model + `"`, "上游思考等级=", "上游思考状态="} {
				if !strings.Contains(line, want) {
					t.Errorf("formatted log missing %s: %s", want, line)
				}
			}
			if strings.Contains(line, "private prompt") || strings.Contains(line, "secret-key") {
				t.Fatalf("summary leaked request content or credentials: %s", line)
			}
		})
	}
}

func TestUpstreamRequestSummaryClientIP(t *testing.T) {
	hook := setupResponseModelLoggerHook(t)
	log.SetLevel(log.InfoLevel)
	tests := []struct {
		name     string
		metadata logging.ClientRequestMetadata
		want     string
	}{
		{name: "resolved address", metadata: logging.ClientRequestMetadata{ClientIP: "172.18.0.6", ResolvedClientIP: "203.0.113.5"}, want: "203.0.113.5"},
		{name: "IPv6 peer fallback", metadata: logging.ClientRequestMetadata{ClientIP: "2001:db8::1"}, want: "2001:db8::1"},
		{name: "untrusted forwarded header is ignored", metadata: logging.ClientRequestMetadata{ClientIP: "192.0.2.10", XForwardedFor: "203.0.113.99"}, want: "192.0.2.10"},
		{name: "missing address", want: "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hook.Reset()
			ctx := logging.WithClientRequestMetadata(context.Background(), tc.metadata)
			RecordAPIRequest(ctx, &config.Config{}, UpstreamRequestLog{Body: []byte(`{"model":"auto"}`)})
			entries := hook.AllEntries()
			if len(entries) != 1 {
				t.Fatalf("log entries = %d, want 1", len(entries))
			}
			if got := entries[0].Data["client_ip"]; got != tc.want {
				t.Errorf("client_ip = %v, want %q", got, tc.want)
			}
		})
	}
}
