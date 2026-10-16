package modelconfig

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

// ResolveModelInfo returns a private capability snapshot for a configured model.
// Capabilities come from the upstream name, then configured aliases for missing
// fields. Explicit configuration takes precedence over either lookup.
func ResolveModelInfo(name, modelType string, support *registry.ThinkingSupport, aliases ...string) *registry.ModelInfo {
	trimmedName := strings.TrimSpace(name)
	info := lookupModelCapabilities(trimmedName, aliases...)
	if info == nil {
		info = &registry.ModelInfo{}
	}
	info.ID = trimmedName
	info.Type = strings.TrimSpace(modelType)
	if support != nil {
		info.Thinking = NormalizeThinkingSupport(support)
	}
	info.UserDefined = false
	return info
}

func lookupModelCapabilities(name string, aliases ...string) *registry.ModelInfo {
	names := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if normalized := strings.TrimSpace(thinking.ParseSuffix(alias).ModelName); normalized != "" {
			names = append(names, normalized)
		}
	}
	return registry.LookupModelCapabilityInfo(strings.TrimSpace(thinking.ParseSuffix(name).ModelName), names...)
}

// NormalizeThinkingSupport clones and normalizes configured reasoning levels.
func NormalizeThinkingSupport(raw *registry.ThinkingSupport) *registry.ThinkingSupport {
	if raw == nil {
		return nil
	}
	normalized := *raw
	normalized.Levels = nil
	seen := make(map[string]struct{}, len(raw.Levels))
	for _, value := range raw.Levels {
		level := strings.ToLower(strings.TrimSpace(value))
		if level == "" {
			continue
		}
		switch level {
		case "none":
			normalized.ZeroAllowed = true
		case "auto":
			normalized.DynamicAllowed = true
		}
		if _, exists := seen[level]; exists {
			continue
		}
		seen[level] = struct{}{}
		normalized.Levels = append(normalized.Levels, level)
	}
	normalized.DefaultLevel = strings.ToLower(strings.TrimSpace(raw.DefaultLevel))
	if _, ok := seen[normalized.DefaultLevel]; !ok {
		normalized.DefaultLevel = ""
	}
	return &normalized
}
