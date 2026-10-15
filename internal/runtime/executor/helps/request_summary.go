package helps

import (
	"context"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// logUpstreamRequestSummary inspects the final outbound payload, independently of
// full request-body logging. Absence of an effort is not equivalent to "none".
func logUpstreamRequestSummary(ctx context.Context, info UpstreamRequestLog) {
	if !log.IsLevelEnabled(log.InfoLevel) {
		return
	}
	model := strings.TrimSpace(gjson.GetBytes(info.Body, "model").String())
	if model == "" {
		model = strings.TrimSpace(gjson.GetBytes(info.Body, "request.model").String())
	}
	if model == "" {
		if parsed, err := url.Parse(info.URL); err == nil {
			if _, tail, ok := strings.Cut(parsed.Path, "/models/"); ok {
				model, _, _ = strings.Cut(tail, ":")
			}
		}
	}
	if model == "" {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(info.Provider))
	switch provider {
	case "aistudio", "vertex":
		provider = "gemini"
	case "claude", "gemini", "antigravity", "codex", "xai", "openai-response", "kimi", "kimi-ai", "kimi.ai", "kimi.com", "interactions":
	default:
		provider = "openai"
	}
	effort := thinking.ExtractTranslatedReasoningEffort(info.Body, provider)
	if effort == "" {
		effort = "unspecified"
	}
	requestedEffort := usage.ReasoningEffortFromContext(ctx)
	if requestedEffort == "" {
		requestedEffort = "unspecified"
	}
	requestedModel := usage.RequestedModelAliasFromContext(ctx)
	if requestedModel == "" {
		requestedModel = "unknown"
	}
	LogWithRequestID(ctx).WithFields(log.Fields{
		"provider":                   info.Provider,
		"model":                      model,
		"requested_model":            requestedModel,
		"requested_reasoning_effort": requestedEffort,
		"upstream_reasoning_effort":  effort,
	}).Info("upstream request")
}
