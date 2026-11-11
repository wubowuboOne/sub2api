package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	OpenAIWebSearchModeKey      = "openai_web_search_mode"
	OpenAIWebSearchModeDefault  = "default"
	OpenAIWebSearchModeOnly     = "only"
	OpenAIWebSearchModeDisabled = "disabled"
)

type openAIWebSearchRoutingKey struct{}

func ValidateOpenAIWebSearchExtra(extra map[string]any) error {
	raw, ok := extra[OpenAIWebSearchModeKey]
	if !ok || raw == nil {
		return nil
	}
	mode, ok := raw.(string)
	if ok && (mode == OpenAIWebSearchModeDefault || mode == OpenAIWebSearchModeOnly || mode == OpenAIWebSearchModeDisabled) {
		return nil
	}
	return infraerrors.BadRequest("INVALID_WEB_SEARCH_MODE", "openai_web_search_mode must be default, only, or disabled")
}

type openAIWebSearchRouting struct {
	AccountID int64
	Model     string
}

// WithOpenAIWebSearchRouting constrains only standalone search, never model turns.
func WithOpenAIWebSearchRouting(ctx context.Context, accountID int64, model string) context.Context {
	return context.WithValue(ctx, openAIWebSearchRoutingKey{}, openAIWebSearchRouting{accountID, model})
}

func (a *Account) GetOpenAIWebSearchMode() string {
	if a == nil || !a.IsOpenAI() {
		return OpenAIWebSearchModeDefault
	}
	switch a.GetExtraString(OpenAIWebSearchModeKey) {
	case OpenAIWebSearchModeOnly:
		return OpenAIWebSearchModeOnly
	case OpenAIWebSearchModeDisabled:
		return OpenAIWebSearchModeDisabled
	default:
		return OpenAIWebSearchModeDefault
	}
}

func openAIWebSearchModeAllows(a *Account, capability OpenAIEndpointCapability) bool {
	switch a.GetOpenAIWebSearchMode() {
	case OpenAIWebSearchModeOnly:
		return capability == OpenAIEndpointCapabilityAlphaSearch
	case OpenAIWebSearchModeDisabled:
		return capability != OpenAIEndpointCapabilityAlphaSearch
	default:
		return true
	}
}

func openAIWebSearchRouteFingerprint(a *Account) string {
	if a == nil {
		return ""
	}
	// OAuth access tokens rotate without changing the selected account identity.
	identity := a.GetCredential("api_key")
	if a.Type != AccountTypeAPIKey {
		identity = a.GetCredential("chatgpt_account_id")
	}
	b, _ := json.Marshal([]any{a.ID, a.Type, a.GetCredential("base_url"), identity, a.ProxyID, a.AuthConnectionID})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func openAIWebSearchAllowedForRequest(ctx context.Context, a *Account, capability OpenAIEndpointCapability) bool {
	if a == nil || !openAIWebSearchModeAllows(a, capability) {
		return false
	}
	if capability != OpenAIEndpointCapabilityAlphaSearch || ctx == nil {
		return true
	}
	route, ok := ctx.Value(openAIWebSearchRoutingKey{}).(openAIWebSearchRouting)
	if !ok {
		return true
	}
	if route.AccountID > 0 && route.AccountID != a.ID {
		return false
	}
	probe, _ := a.Extra["openai_web_search_probe"].(map[string]any)
	supported, known := probe["supported"].(bool)
	if !known || supported || probe["route"] != openAIWebSearchRouteFingerprint(a) {
		return true
	}
	model := normalizeOpenAIModelForUpstream(a, a.GetMappedModel(strings.TrimSpace(route.Model)))
	return probe["model"] != model
}

func (s *adminServiceImpl) validateGroupWebSearchAccount(ctx context.Context, group *Group, id int64) error {
	if id == 0 {
		return nil
	}
	if id < 0 || (group.Platform != PlatformOpenAI && group.Platform != PlatformComposite) {
		return infraerrors.BadRequest("INVALID_WEB_SEARCH_ACCOUNT", "dedicated web search requires an OpenAI or Composite group and a positive account ID")
	}
	accounts, err := s.accountRepo.ListByGroup(ctx, group.ID)
	if err != nil {
		return err
	}
	for i := range accounts {
		a := &accounts[i]
		if a.ID == id && a.IsOpenAI() && a.IsActive() && a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAlphaSearch) {
			return nil
		}
	}
	return infraerrors.BadRequest("INVALID_WEB_SEARCH_ACCOUNT", "select an active OpenAI account in this group with standalone web search enabled")
}
