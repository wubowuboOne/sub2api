package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type alphaSearchRoutingAccountRepo struct {
	*openAIWSFailoverHandlerAccountRepoStub
}

func (r alphaSearchRoutingAccountRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]service.Account, error) {
	return r.accounts, nil
}

func TestOpenAIAlphaSearchDedicatedAccountRoutingAndRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, mode   string
		status       int
		wantCalls    int
		unconfigured bool
	}{
		{name: "search_only_retries_configured_account", mode: "only", status: 502, wantCalls: 3},
		{name: "search_rate_limit_retries_configured_account", mode: "only", status: 429, wantCalls: 3},
		{name: "search_payload_error_retries_configured_account", mode: "only", status: 413, wantCalls: 3},
		{name: "default_mode_retries_configured_account", mode: "default", status: 502, wantCalls: 3},
		{name: "disabled_target_never_falls_back", mode: "disabled", status: 502},
		{name: "unconfigured_retries_in_group_search_account", mode: "only", status: 502, wantCalls: 3, unconfigured: true},
		{name: "unconfigured_does_not_use_other_groups", mode: "disabled", status: 502, unconfigured: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(42)
			repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{
				{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
					GroupIDs: []int64{groupID}, Priority: 100,
					Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://upstream.example", "pool_mode": true, "pool_mode_retry_count": float64(2), "pool_mode_retry_status_codes": []any{float64(tc.status)}},
					Extra:       map[string]any{service.OpenAIWebSearchModeKey: tc.mode}},
				{ID: 8, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
					GroupIDs: []int64{groupID}, Priority: 1, Credentials: map[string]any{"api_key": "sk-other", "base_url": "https://other.example"}},
			}}
			searchAccountID := int64(7)
			if tc.unconfigured {
				searchAccountID = 0
				repo.accounts[1].Extra = map[string]any{service.OpenAIWebSearchModeKey: service.OpenAIWebSearchModeDisabled}
				otherGroup := repo.accounts[0]
				otherGroup.ID, otherGroup.GroupIDs, otherGroup.Priority = 9, []int64{groupID + 1}, 0
				otherGroup.Extra = map[string]any{service.OpenAIWebSearchModeKey: service.OpenAIWebSearchModeOnly}
				repo.accounts = append(repo.accounts, otherGroup)
			}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			cfg.Gateway.MaxAccountSwitches = 2
			upstream := &openAIConfiguredStatusRetryUpstream{status: tc.status, body: `{"error":{"message":"temporarily unavailable"}}`, contentType: "application/json"}
			billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingCache.Stop)
			gateway := service.NewOpenAIGatewayService(alphaSearchRoutingAccountRepo{repo}, nil, nil, nil, nil, nil, nil, &openAIConfiguredStatusRetryCache{}, cfg, nil, nil,
				service.NewBillingService(cfg, nil), nil, billingCache, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache,
				service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := []byte(`{"id":"stable-search","model":"gpt-test","commands":{"search_query":[{"q":"OpenAI"}]}}`)
			c.Request = httptest.NewRequest("POST", "/v1/alpha/search", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, GroupID: &groupID,
				User:  &service.User{ID: 1, Status: service.StatusActive},
				Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, WebSearchAccountID: searchAccountID}})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
			h.AlphaSearch(c)
			require.Len(t, upstream.accountIDs, tc.wantCalls, rec.Body.String())
			for i, id := range upstream.accountIDs {
				require.Equal(t, int64(7), id, "search must stay within the eligible accounts in this group")
				require.JSONEq(t, string(body), string(upstream.requestBodies[i]), "retries preserve the query and search ID")
			}
			if tc.wantCalls == 0 {
				require.Equal(t, http.StatusServiceUnavailable, rec.Code)
				if tc.unconfigured {
					require.Contains(t, rec.Body.String(), "No available web search accounts in this group")
				}
			}
		})
	}
}
