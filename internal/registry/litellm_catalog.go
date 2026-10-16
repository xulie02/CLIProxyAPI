package registry

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

const liteLLMCatalogURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

//go:embed models/litellm_models.json
var embeddedLiteLLMModels []byte

type liteLLMModel struct {
	Provider          string   `json:"litellm_provider"`
	Mode              string   `json:"mode"`
	InputTokens       int      `json:"max_input_tokens"`
	OutputTokens      int      `json:"max_output_tokens"`
	SupportsReasoning *bool    `json:"supports_reasoning"`
	ReasoningLevels   []string `json:"reasoning_effort_levels"`
	DefaultReasoning  string   `json:"default_reasoning_effort"`
	Modalities        []string `json:"supported_modalities"`
	OutputModalities  []string `json:"supported_output_modalities"`
}

var liteLLMCatalog = struct {
	sync.RWMutex
	models map[string]liteLLMModel
}{}

func init() {
	_, err := publishLiteLLMCatalog(embeddedLiteLLMModels)
	if err != nil {
		log.WithError(err).Warn("invalid embedded LiteLLM catalog; retaining CPA catalog fallback")
	}
}

func parseLiteLLMCatalog(data []byte) (map[string]liteLLMModel, error) {
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("invalid LiteLLM catalog JSON")
	}
	models := make(map[string]liteLLMModel)
	for id, raw := range entries {
		var model liteLLMModel
		if err := json.Unmarshal(raw, &model); err != nil {
			continue
		}
		if model.Provider == "" || (model.Mode != "chat" && model.Mode != "responses" && model.Mode != "completion") {
			continue
		}
		if model.InputTokens < 0 || model.OutputTokens < 0 {
			return nil, fmt.Errorf("invalid LiteLLM token limits for %q", id)
		}
		models[id] = model
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("LiteLLM catalog contains no chat model metadata")
	}
	return models, nil
}

func publishLiteLLMCatalog(data []byte) ([]string, error) {
	models, err := parseLiteLLMCatalog(data)
	if err != nil {
		return nil, err
	}
	liteLLMCatalog.Lock()
	changed := !reflect.DeepEqual(liteLLMCatalog.models, models)
	liteLLMCatalog.models = models
	liteLLMCatalog.Unlock()
	if changed {
		return []string{"litellm"}, nil
	}
	return nil, nil
}

func lookupLiteLLMModel(modelID string) (liteLLMModel, bool) {
	liteLLMCatalog.RLock()
	defer liteLLMCatalog.RUnlock()
	if model, ok := liteLLMCatalog.models[modelID]; ok {
		return model, true
	}
	// Only canonical native-provider prefixes can be omitted. Do not collapse
	// reseller/hosting records with potentially different limits into a bare ID.
	var found liteLLMModel
	matched := false
	for _, provider := range []string{"openai", "anthropic", "deepseek", "zai", "zhipuai", "gemini", "moonshot", "mistral", "cohere", "xai"} {
		model, ok := liteLLMCatalog.models[provider+"/"+modelID]
		if !ok || model.Provider != provider {
			continue
		}
		if matched && !reflect.DeepEqual(found, model) {
			return liteLLMModel{}, false
		}
		found, matched = model, true
	}
	return found, matched
}

// LookupModelCapabilityInfo uses the current LiteLLM catalog for available
// fields, retaining CPA-specific metadata and missing fields from its catalog.
// Configured aliases may fill missing capability fields only; the primary ID
// and all already-known fields remain authoritative.
func LookupModelCapabilityInfo(modelID string, aliases ...string) *ModelInfo {
	info := lookupModelCapabilityInfo(modelID)
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" || alias == modelID {
			continue
		}
		fallback := lookupModelCapabilityInfo(alias)
		if fallback == nil {
			continue
		}
		if info == nil {
			info = &ModelInfo{ID: modelID, Object: "model"}
		}
		if info.ContextLength <= 0 {
			info.ContextLength = fallback.ContextLength
		}
		if info.InputTokenLimit <= 0 {
			info.InputTokenLimit = fallback.InputTokenLimit
		}
		if info.MaxCompletionTokens <= 0 {
			info.MaxCompletionTokens = fallback.MaxCompletionTokens
		}
		if info.OutputTokenLimit <= 0 {
			info.OutputTokenLimit = fallback.OutputTokenLimit
		}
		if info.Thinking == nil {
			info.Thinking = cloneModelInfo(fallback).Thinking
		}
		if len(info.SupportedInputModalities) == 0 {
			info.SupportedInputModalities = append([]string(nil), fallback.SupportedInputModalities...)
		}
		if len(info.SupportedOutputModalities) == 0 {
			info.SupportedOutputModalities = append([]string(nil), fallback.SupportedOutputModalities...)
		}
	}
	return info
}

func lookupModelCapabilityInfo(modelID string) *ModelInfo {
	info := LookupStaticModelInfo(modelID)
	model, ok := lookupLiteLLMModel(modelID)
	if !ok {
		return info
	}
	if info == nil {
		info = &ModelInfo{ID: modelID, Object: "model"}
	}
	// max_input_tokens is a prompt budget. max_tokens is often an output alias;
	// neither it nor input+output is used to invent a larger context window.
	if model.InputTokens > 0 {
		info.ContextLength = model.InputTokens
		info.InputTokenLimit = model.InputTokens
	}
	if model.OutputTokens > 0 {
		info.MaxCompletionTokens = model.OutputTokens
		info.OutputTokenLimit = model.OutputTokens
	}
	if len(model.Modalities) > 0 {
		info.SupportedInputModalities = append([]string(nil), model.Modalities...)
	}
	if len(model.OutputModalities) > 0 {
		info.SupportedOutputModalities = append([]string(nil), model.OutputModalities...)
	}
	if model.ReasoningLevels != nil {
		support := &ThinkingSupport{}
		seen := make(map[string]bool)
		for _, raw := range model.ReasoningLevels {
			level := strings.ToLower(strings.TrimSpace(raw))
			if level == "" || seen[level] {
				continue
			}
			seen[level] = true
			support.Levels = append(support.Levels, level)
			if level == "none" {
				support.ZeroAllowed = true
			}
			if level == "auto" {
				support.DynamicAllowed = true
			}
		}
		if seen[model.DefaultReasoning] {
			support.DefaultLevel = model.DefaultReasoning
		}
		info.Thinking = support
	} else if model.SupportsReasoning != nil && !*model.SupportsReasoning {
		info.Thinking = &ThinkingSupport{}
	}
	// A supports_reasoning=true flag alone does not declare any effort levels.
	return info
}

var liteLLMCatalogUpdater = &catalogUpdater{
	fetch:   catalogFetcher(embeddedLiteLLMModels, []string{liteLLMCatalogURL}, func(data []byte) error { _, err := parseLiteLLMCatalog(data); return err }),
	publish: publishLiteLLMCatalog,
}
