package modelconfig

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

// UpstreamSource identifies one configured upstream credential, never a public alias.
type UpstreamSource struct {
	BaseURL string
	APIKey  string
	Headers map[string]string
}

type UpstreamModelCapability struct {
	Thinking      *registry.ThinkingSupport
	ContextWindow int
	OutputTokens  int
}

type upstreamCatalogEntry struct {
	models      map[string]UpstreamModelCapability
	nextAttempt time.Time
	refreshing  bool
}

// UpstreamCatalog caches only capabilities explicitly advertised by the upstream.
// Network discovery is performed by the service worker, never by request resolution.
type UpstreamCatalog struct {
	mu      sync.RWMutex
	entries map[[32]byte]upstreamCatalogEntry
	now     func() time.Time
}

var DefaultUpstreamCatalog = NewUpstreamCatalog()

func NewUpstreamCatalog() *UpstreamCatalog {
	return &UpstreamCatalog{entries: make(map[[32]byte]upstreamCatalogEntry), now: time.Now}
}

func upstreamSourceKey(source UpstreamSource) [32]byte {
	value, _ := json.Marshal(struct {
		BaseURL, APIKey string
		Headers         map[string]string
	}{
		strings.TrimRight(strings.TrimSpace(source.BaseURL), "/"), source.APIKey, source.Headers,
	})
	return sha256.Sum256(value)
}

func (c *UpstreamCatalog) Lookup(source UpstreamSource, model string) UpstreamModelCapability {
	c.mu.RLock()
	entry := c.entries[upstreamSourceKey(source)]
	value := entry.models[strings.TrimSpace(model)]
	c.mu.RUnlock()
	value.Thinking = NormalizeThinkingSupport(value.Thinking)
	return value
}

// Refresh treats a successful list without capability fields as unknown.
// Failed discovery removes old metadata; resolution can then use the internal catalog.
func (c *UpstreamCatalog) Refresh(ctx context.Context, client *http.Client, source UpstreamSource) (bool, error) {
	key := upstreamSourceKey(source)
	c.mu.Lock()
	entry := c.entries[key]
	if entry.refreshing || c.now().Before(entry.nextAttempt) {
		c.mu.Unlock()
		return false, nil
	}
	entry.nextAttempt = c.now().Add(time.Minute)
	entry.refreshing = true
	c.entries[key] = entry
	c.mu.Unlock()
	models, err := fetchUpstreamCapabilities(ctx, client, source)
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := !reflect.DeepEqual(entry.models, models)
	if err == nil {
		entry.nextAttempt = c.now().Add(5 * time.Minute)
	}
	entry.models = models
	entry.refreshing = false
	c.entries[key] = entry
	return changed, err
}

func fetchUpstreamCapabilities(ctx context.Context, client *http.Client, source UpstreamSource) (map[string]UpstreamModelCapability, error) {
	base := strings.TrimRight(strings.TrimSpace(source.BaseURL), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid upstream model catalog URL")
	}
	if u.Path == "" || u.Path == "/" {
		base += "/v1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream model catalog request")
	}
	req.Header.Set("Accept", "application/json")
	if source.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+source.APIKey)
	}
	for name, value := range source.Headers {
		req.Header.Set(name, value)
	}
	// Never forward configured credentials to a redirect destination.
	transportClient := *client
	transportClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := transportClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream model catalog request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream model catalog returned HTTP %d", resp.StatusCode)
	}
	const limit = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		return nil, fmt.Errorf("invalid upstream model catalog body")
	}
	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			ContextWindow int    `json:"context_window"`
			OutputTokens  int    `json:"max_output_tokens"`
			Effort        *struct {
				SupportedLevels []string `json:"supported_levels"`
				DefaultLevel    string   `json:"default_level"`
			} `json:"effort"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Data == nil {
		return nil, fmt.Errorf("invalid upstream model catalog schema")
	}
	models := make(map[string]UpstreamModelCapability)
	for _, model := range payload.Data {
		if strings.TrimSpace(model.ID) == "" {
			continue
		}
		value := UpstreamModelCapability{}
		if model.ContextWindow > 0 {
			value.ContextWindow = model.ContextWindow
		}
		if model.OutputTokens > 0 {
			value.OutputTokens = model.OutputTokens
		}
		if model.Effort != nil && model.Effort.SupportedLevels != nil {
			value.Thinking = NormalizeThinkingSupport(&registry.ThinkingSupport{
				Levels: model.Effort.SupportedLevels, DefaultLevel: model.Effort.DefaultLevel,
			})
		}
		models[model.ID] = value
	}
	return models, nil
}

// ApplyUpstreamCapabilities resolves explicit configuration, upstream metadata,
// and then the internal catalog. Configured aliases only fill missing internal
// capability fields; discovery and outbound requests use the upstream ID.
func ApplyUpstreamCapabilities(info *registry.ModelInfo, source UpstreamSource, upstream string, aliases ...string) {
	baseName := thinking.ParseSuffix(upstream).ModelName
	capability := DefaultUpstreamCatalog.Lookup(source, baseName)
	fallback := lookupModelCapabilities(baseName, aliases...)
	if !info.ExplicitThinking {
		info.Thinking = capability.Thinking
		if info.Thinking == nil && fallback != nil {
			info.Thinking = NormalizeThinkingSupport(fallback.Thinking)
		}
	}
	if info.MaxContextLength <= 0 {
		info.ContextLength = capability.ContextWindow
		if info.ContextLength <= 0 && fallback != nil {
			info.ContextLength = fallback.ContextLength
		}
	}
	info.MaxCompletionTokens = capability.OutputTokens
	if info.MaxCompletionTokens <= 0 && fallback != nil {
		info.MaxCompletionTokens = fallback.MaxCompletionTokens
	}
	info.OutputTokenLimit = info.MaxCompletionTokens
	info.InputTokenLimit = info.ContextLength
}
