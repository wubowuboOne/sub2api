package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIResponsesEncryptedContentPoolRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const rejection = `{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"The encrypted content AAAA-private-ciphertext-ZZZ1 could not be verified. Reason: Encrypted content could not be decrypted or parsed."}`
	for _, failure := range []struct {
		name, body, contentType string
		status                  int
	}{
		{"http", `{"error":` + rejection + `}`, "application/json", 400},
		{"sse_failed", "data: {\"type\":\"response.created\",\"response\":{\"id\":\"rejected-attempt\"}}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":" + rejection + "}}\n\n", "text/event-stream", 200},
		{"sse_error", "data: {\"type\":\"error\",\"error\":" + rejection + "}\n\n", "text/event-stream", 200},
	} {
		for _, passthrough := range []bool{false, true} {
			for _, streaming := range []bool{false, true} {
				for _, recover := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/passthrough=%t/stream=%t/recover=%t", failure.name, passthrough, streaming, recover), func(t *testing.T) {
						const accountID = int64(59327)
						groupID := int64(4205)
						accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{{
							ID: accountID, Name: "encrypted-retry", Platform: service.PlatformOpenAI,
							Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1,
							GroupIDs: []int64{groupID}, Credentials: map[string]any{
								"api_key": "sk-test", "base_url": "https://api.example.test", "pool_mode": true,
								"pool_mode_retry_count": float64(3), "pool_mode_retry_status_codes": []any{float64(400), float64(502)},
							}, Extra: map[string]any{"openai_passthrough": passthrough, "openai_responses_supported": true},
						}}}
						fallback := accountRepo.accounts[0]
						fallback.ID, fallback.Priority = accountID+100000, 100
						accountRepo.accounts = append(accountRepo.accounts, fallback)
						cfg := &config.Config{RunMode: config.RunModeSimple}
						cfg.Default.RateMultiplier = 1
						cfg.Gateway.MaxAccountSwitches = 2
						upstream := &openAIConfiguredStatusRetryUpstream{status: failure.status, contentType: failure.contentType, upstreamRequestID: "encrypted-attempt"}
						upstream.bodyFactory = func() io.ReadCloser {
							accountRepo.accounts[0].Priority, accountRepo.accounts[1].Priority = 100, 1
							if recover && len(upstream.accountIDs) >= 3 {
								// Recover after two failures on the original account.
								upstream.status = 200
								upstream.contentType = "text/event-stream"
								return io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"recovered-attempt\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-6.1-sol\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"))
							}
							return io.NopCloser(strings.NewReader(failure.body))
						}
						billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
						t.Cleanup(billingCache.Stop)
						usageRepo := &messagesRetryUsageRecorder{}
						gateway := service.NewOpenAIGatewayService(
							accountRepo, nil, usageRepo, nil, nil, nil, nil, &openAIConfiguredStatusRetryCache{}, cfg, nil, nil,
							service.NewBillingService(cfg, nil), nil, billingCache, upstream,
							&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
						)
						h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache,
							service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
						mappedStatus, mappedMessage := 502, "Upstream service temporarily unavailable"
						h.errorPassthroughService = service.NewErrorPassthroughService(&configuredHTTPPassthroughRepo{rules: []*model.ErrorPassthroughRule{{
							ID: 4, Enabled: true, ErrorCodes: []int{400}, MatchMode: model.MatchModeAny,
							ResponseCode: &mappedStatus, CustomMessage: &mappedMessage,
						}}}, nil)
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						service.BindErrorPassthroughService(c, h.errorPassthroughService)
						requestBody := []byte(fmt.Sprintf(`{"model":"gpt-6.1-sol","stream":%t,"prompt_cache_key":"stable-encrypted-cache","input":[{"type":"reasoning","encrypted_content":"AAAA-private-ciphertext-ZZZ1","summary":[{"type":"summary_text","text":"keep summary"}]},{"type":"compaction","encrypted_content":"compacted-history"},{"type":"function_call","call_id":"call-original","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call-original","output":"keep tool result"}]}`, streaming))
						c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(requestBody))
						c.Request.Header.Set("Content-Type", "application/json")
						c.Request.Header.Set("session_id", "stable-encrypted-session")
						c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
							ID: 1805, GroupID: &groupID, AllowLongContext: true, User: &service.User{ID: 1705, Status: service.StatusActive},
							Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
						})
						c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1705})

						h.Responses(c)
						require.NotEmpty(t, upstream.accountIDs, "request rejected before forwarding: %s", rec.Body.String())

						wantCalls := 4
						if recover {
							wantCalls = 3
							require.Equal(t, 200, rec.Code)
							require.Contains(t, rec.Body.String(), "recovered-attempt")
							require.Len(t, usageRepo.rows, 1, "only the recovered attempt is settled")
							require.Equal(t, 10, usageRepo.rows[0].InputTokens)
							require.Equal(t, 2, usageRepo.rows[0].OutputTokens)
						} else {
							require.Equal(t, mappedStatus, rec.Code)
							require.Contains(t, rec.Body.String(), mappedMessage)
							require.Empty(t, usageRepo.rows)
						}
						require.NotContains(t, rec.Body.String(), "rejected-attempt")
						require.NotContains(t, rec.Body.String(), "private-ciphertext")
						require.Len(t, upstream.accountIDs, wantCalls)
						for i, id := range upstream.accountIDs {
							require.Equal(t, accountID, id, "encrypted failures must never rotate accounts")
							require.Equal(t, upstream.requestBodies[0], upstream.requestBodies[i], "same-account retries must retain the exact outbound body and cache key")
							require.Contains(t, string(upstream.requestBodies[i]), "AAAA-private-ciphertext-ZZZ1")
							require.Contains(t, string(upstream.requestBodies[i]), "compacted-history")
							require.Contains(t, string(upstream.requestBodies[i]), "keep tool result")
							require.Equal(t, upstream.requestHeaders[0].Get("session_id"), upstream.requestHeaders[i].Get("session_id"))
							require.Equal(t, upstream.requestHeaders[0].Get("Authorization"), upstream.requestHeaders[i].Get("Authorization"))
						}
						value, ok := c.Get(service.OpsUpstreamErrorsKey)
						require.True(t, ok)
						events, ok := value.([]*service.OpsUpstreamErrorEvent)
						require.True(t, ok)
						wantFailures := wantCalls
						if recover {
							wantFailures--
						}
						require.Len(t, events, wantFailures)
						for _, event := range events {
							require.Equal(t, string(service.OpenAIEncryptedContentRetryReason), event.Reason)
							require.Equal(t, accountID, event.AccountID)
							require.Equal(t, "encrypted-attempt", event.UpstreamRequestID)
							raw, err := json.Marshal(event)
							require.NoError(t, err)
							require.NotContains(t, string(raw), "private-ciphertext")
						}
					})
				}
			}
		}
	}
}

func TestOpenAIEncryptedContentPoolRetryCompatibilityHandlers(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			runMessagesRetryCase(t, messagesRetryCase{
				name: path, retries: 10, retryStatus: 502, wantCalls: 3, successAfter: 2,
				failureCode: "invalid_encrypted_content", chat: path == "chat",
				responses: path == "native" || path == "passthrough", passthrough: path == "passthrough",
			})
		})
	}
	t.Run("full_ten_retry_budget", func(t *testing.T) {
		runMessagesRetryCase(t, messagesRetryCase{
			name: "encrypted_ten", retries: 10, retryStatus: 502, wantCalls: 11,
			failureCode: "invalid_encrypted_content", responses: true,
		})
	})
}
