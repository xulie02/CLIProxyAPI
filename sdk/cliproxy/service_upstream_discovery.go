package cliproxy

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

func discoveredCompatibilityModels(models []*ModelInfo, compat *config.OpenAICompatibility, auth *coreauth.Auth) []*ModelInfo {
	if compat == nil || auth == nil {
		return models
	}
	source := modelconfig.UpstreamSource{BaseURL: compat.BaseURL, APIKey: auth.Attributes["api_key"], Headers: compat.Headers}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		clone := *model
		if clone.Type != registry.OpenAIImageModelType {
			modelconfig.ApplyUpstreamCapabilities(&clone, source, clone.MetadataModelID, model.ID)
		}
		out = append(out, &clone)
	}
	return out
}

func (s *Service) requestUpstreamDiscovery() {
	s.cfgMu.RLock()
	wake := s.upstreamDiscoveryWake
	s.cfgMu.RUnlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (s *Service) startUpstreamDiscovery(ctx context.Context) {
	wake := make(chan struct{}, 1)
	s.cfgMu.Lock()
	s.upstreamDiscoveryWake = wake
	s.cfgMu.Unlock()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-ticker.C:
			}
			s.refreshUpstreamDiscovery(ctx)
		}
	}()
	s.requestUpstreamDiscovery()
}

func (s *Service) refreshUpstreamDiscovery(ctx context.Context) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || cfg.Home.Enabled {
		return
	}
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for _, provider := range cfg.OpenAICompatibility {
		if provider.Disabled {
			continue
		}
		for _, credential := range provider.APIKeyEntries {
			if credential.Weight != nil && *credential.Weight <= 0 {
				continue
			}
			select {
			case <-ctx.Done():
				wg.Wait()
				return
			case limit <- struct{}{}:
			}
			wg.Add(1)
			go func(provider config.OpenAICompatibility, credential config.OpenAICompatibilityAPIKey) {
				defer wg.Done()
				defer func() { <-limit }()
				proxyURL := credential.ProxyURL
				if proxyURL == "" {
					proxyURL = cfg.ProxyURL
				}
				transport, _, err := proxyutil.BuildHTTPTransport(proxyURL)
				if err != nil {
					return
				}
				if transport != nil {
					defer transport.CloseIdleConnections()
				}
				client := &http.Client{}
				if transport != nil {
					client.Transport = transport
				}
				changed, err := modelconfig.DefaultUpstreamCatalog.Refresh(ctx, client, modelconfig.UpstreamSource{
					BaseURL: provider.BaseURL, APIKey: credential.APIKey, Headers: provider.Headers,
				})
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					log.WithField("provider", provider.Name).Debug("upstream model capabilities unavailable; configure model capabilities explicitly")
				}
				if changed && s.coreManager != nil {
					s.coreManager.RefreshAPIKeyModelAlias()
					var affected []*coreauth.Auth
					for _, auth := range s.coreManager.List() {
						if auth == nil || auth.Disabled || auth.Attributes["api_key"] != credential.APIKey {
							continue
						}
						if !strings.EqualFold(auth.Attributes["compat_name"], provider.Name) && !strings.EqualFold(auth.Provider, provider.Name) {
							continue
						}
						if strings.TrimRight(auth.Attributes["base_url"], "/") != strings.TrimRight(provider.BaseURL, "/") {
							continue
						}
						affected = append(affected, auth)
					}
					s.registerModelsForAuthBatch(ctx, affected)
				}
			}(provider, credential)
		}
	}
	wg.Wait()
}
