package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestTransientCooldownKeepsModelInCatalogWithoutBypassingRouting(t *testing.T) {
	withQuotaCooldownEnabled(t)
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	const authID, modelID = "catalog-transient-auth", "catalog-transient-model"
	if _, err := m.Register(ctx, &Auth{ID: authID, Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatal(err)
	}
	r := registry.GetGlobalRegistry()
	r.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelID}})
	t.Cleanup(func() { r.UnregisterClient(authID) })
	m.MarkResult(ctx, Result{AuthID: authID, Provider: "codex", Model: modelID,
		Error: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "temporary upstream failure"}})
	snapshot, _ := m.GetByID(authID)
	if blocked, _, _ := isAuthBlockedForModel(snapshot, modelID, time.Now()); !blocked {
		t.Fatal("temporary cooldown must still block routing")
	}
	found := false
	for _, model := range r.GetAvailableModels("openai") {
		if model["id"] == modelID {
			found = true
		}
	}
	if !found {
		t.Fatal("temporary 503 cooldown removed the configured model from the catalog")
	}
	providerFound := false
	for _, model := range r.GetAvailableModelsByProvider("codex") {
		providerFound = providerFound || model.ID == modelID
	}
	if !providerFound {
		t.Fatal("temporary cooldown removed the model from the provider catalog")
	}
	state := snapshot.ModelStates[modelID]
	projection := m.clientModelProjectionForAuth(snapshot, modelID, state.NextRetryAfter.Add(time.Second))
	if projection.Suspended {
		t.Fatal("an expired transient deadline must not keep the model suspended")
	}
	// An explicit administrative disable must remain authoritative.
	snapshot.Disabled = true
	projection = m.clientModelProjectionForAuth(snapshot, modelID, time.Now())
	if !projection.Suspended || projection.SuspendReason == "cooldown" {
		t.Fatal("an explicitly disabled credential was classified as temporarily cooling")
	}
}
