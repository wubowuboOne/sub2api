package handler

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIMessagesWebSearchDedicatedAccountAndBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                                   string
		stream, mixed, none, unconfigured      bool
		noTools, restricted, paused, composite bool
		mode                                   string
		status, wantCalls                      int
		wantAccount                            int64
	}{
		{name: "dedicated_search_only", mode: "only", status: 200, wantCalls: 1, wantAccount: 7},
		{name: "dedicated_stream", stream: true, mode: "only", status: 200, wantCalls: 1, wantAccount: 7},
		{name: "dedicated_normal_account", mode: "default", status: 200, wantCalls: 1, wantAccount: 7},
		{name: "configured_retries_stay_on_search_account", mode: "only", status: 502, wantCalls: 3, wantAccount: 7},
		{name: "disabled_search_fails_closed", mode: "disabled", status: 200, wantCalls: 0},
		{name: "mixed_tools_keep_normal_routing", mixed: true, mode: "only", status: 200, wantCalls: 1, wantAccount: 8},
		{name: "explicit_none_keeps_normal_routing", none: true, mode: "only", status: 200, wantCalls: 1, wantAccount: 8},
		{name: "unconfigured_keeps_normal_routing", unconfigured: true, mode: "only", status: 200, wantCalls: 1, wantAccount: 8},
		{name: "ordinary_chat_keeps_normal_routing", noTools: true, mode: "only", status: 200, wantCalls: 1, wantAccount: 8},
		{name: "paused_search_fails_closed", paused: true, mode: "only", status: 200, wantCalls: 0},
		{name: "restricted_key_keeps_context_guard", restricted: true, mode: "only", status: 200, wantCalls: 0},
		{name: "composite_resolved_to_openai", composite: true, mode: "only", status: 200, wantCalls: 1, wantAccount: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(42)
			repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{
				{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Priority: 100,
					Credentials: map[string]any{"api_key": "fixture-search", "base_url": "https://search.example", "pool_mode": true, "pool_mode_retry_count": float64(2), "pool_mode_retry_status_codes": []any{float64(502)}},
					Extra:       map[string]any{service.OpenAIWebSearchModeKey: tc.mode}},
				{ID: 8, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Priority: 1,
					Credentials: map[string]any{"api_key": "fixture-normal", "base_url": "https://normal.example"}, Extra: map[string]any{"openai_responses_supported": true}},
			}}
			if tc.paused {
				repo.accounts[0].Schedulable = false
			}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			cfg.Gateway.MaxAccountSwitches = 2
			wire := "data: " + `{"type":"response.completed","response":{"status":"completed","output":[{"type":"web_search_call","id":"ws_fixture","status":"completed"},{"type":"message","content":[{"type":"output_text","text":"Verified search output","annotations":[{"type":"url_citation","url":"https://example.com/news","title":"Example News"}]}]}]}}` + "\n\n"
			contentType := "text/event-stream"
			if tc.wantAccount == 8 {
				wire = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_normal\",\"status\":\"completed\",\"model\":\"gpt-6-sol\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Normal answer\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
				contentType = "text/event-stream"
			}
			if tc.status >= 400 {
				wire = `{"error":{"message":"temporarily unavailable"}}`
			}
			upstream := &openAIConfiguredStatusRetryUpstream{status: tc.status, body: wire, contentType: contentType, upstreamRequestID: "fixture-search-request"}
			billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingCache.Stop)
			usage := &messagesRetryUsageRecorder{}
			gateway := service.NewOpenAIGatewayService(repo, nil, usage, nil, nil, nil, nil, &openAIConfiguredStatusRetryCache{}, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, billingCache, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
			tools := `[{"type":"web_search_20250305","name":"web_search"}]`
			if tc.mixed {
				tools = `[{"type":"web_search_20250305","name":"web_search"},{"name":"read_file","input_schema":{"type":"object","properties":{}}}]`
			}
			if tc.noTools {
				tools = `[]`
			}
			choice := ""
			if tc.none {
				choice = `,"tool_choice":{"type":"none"}`
			}
			body := []byte(fmt.Sprintf(`{"model":"gpt-6-sol","max_tokens":64,"stream":%t,"messages":[{"role":"user","content":"latest news"}],"tools":%s%s}`, tc.stream, tools, choice))
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowMessagesDispatch: true, WebSearchAccountID: 7, RateMultiplier: 1}
			if tc.composite {
				group.Platform = service.PlatformComposite
				c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformOpenAI))
			}
			if tc.unconfigured {
				group.WebSearchAccountID = 0
			}
			// Hosted tools require permission for unverifiable/long context.
			// Preserve this existing policy while testing normal model routing.
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, AllowLongContext: !tc.restricted, GroupID: &groupID, User: &service.User{ID: 1, Status: service.StatusActive}, Group: group})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
			h.Messages(c)
			require.Len(t, upstream.accountIDs, tc.wantCalls, rec.Body.String())
			for _, id := range upstream.accountIDs {
				require.Equal(t, tc.wantAccount, id, "dedicated searches must not fall back to an ordinary account")
			}
			if tc.wantAccount == 7 && tc.status == 200 {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "web_search_tool_result")
				require.Contains(t, rec.Body.String(), "Verified search output")
				require.Len(t, usage.rows, 1, "one successful search must create exactly one usage record")
				require.InDelta(t, 0.01, usage.rows[0].TotalCost, 1e-9)
			} else if tc.wantAccount == 8 {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "Normal answer")
				require.NotContains(t, rec.Body.String(), "web_search_tool_result")
			} else {
				require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
				require.Empty(t, usage.rows, "failed searches and retries must not be billed")
				if tc.restricted {
					require.Contains(t, rec.Body.String(), "context_length_exceeded")
				}
			}
		})
	}
}
