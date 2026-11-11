package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIAlphaSearchDedicatedAccountRoutingAndRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, mode string
		status     int
		wantCalls  int
	}{
		{"search_only_retries_configured_account", "only", 502, 3},
		{"search_rate_limit_retries_configured_account", "only", 429, 3},
		{"search_payload_error_retries_configured_account", "only", 413, 3},
		{"default_mode_retries_configured_account", "default", 502, 3},
		{"disabled_target_never_falls_back", "disabled", 502, 0},
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
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			cfg.Gateway.MaxAccountSwitches = 2
			upstream := &openAIConfiguredStatusRetryUpstream{status: tc.status, body: `{"error":{"message":"temporarily unavailable"}}`, contentType: "application/json"}
			billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, &openAIConfiguredStatusRetryCache{}, cfg, nil, nil,
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
				Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, WebSearchAccountID: 7}})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
			h.AlphaSearch(c)
			require.Len(t, upstream.accountIDs, tc.wantCalls, rec.Body.String())
			for i, id := range upstream.accountIDs {
				require.Equal(t, int64(7), id, "ordinary account 8 must never receive a dedicated search")
				require.JSONEq(t, string(body), string(upstream.requestBodies[i]), "retries preserve the query and search ID")
			}
			if tc.wantCalls == 0 {
				require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			}
		})
	}
}
