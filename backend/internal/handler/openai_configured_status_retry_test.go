package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openAIConfiguredStatusRetryUpstream struct {
	service.HTTPUpstream
	mu                sync.Mutex
	accountIDs        []int64
	status            int
	body              string
	contentType       string
	upstreamRequestID string
	firstResponse     *http.Response
}

func (u *openAIConfiguredStatusRetryUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	first := len(u.accountIDs) == 1
	u.mu.Unlock()
	if first && u.firstResponse != nil {
		return u.firstResponse, nil
	}
	return &http.Response{
		StatusCode: u.status,
		Header:     http.Header{"Content-Type": []string{u.contentType}, "X-Request-Id": []string{u.upstreamRequestID}},
		Body:       io.NopCloser(strings.NewReader(u.body)),
	}, nil
}

func TestOpenAIResponsesConfiguredStatusRetriesSameAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name              string
		accountID         int64
		retryCount        int
		model             string
		upstreamRequestID string
		status            int
		semanticStatus    int
		body              string
		contentType       string
		bodyLimitFirst    bool
	}{
		{
			name:              "bps_stream_error",
			accountID:         59323,
			retryCount:        1,
			model:             "gpt-6-luna",
			upstreamRequestID: "dff2539f-5616-495f-84d6-f6fe9eca2058",
			status:            http.StatusOK,
			semanticStatus:    http.StatusBadGateway,
			contentType:       "text/event-stream",
			body: `event: response.created
data: {"type":"response.created","response":{"id":"resp_retry"}}

event: response.failed
data: {"response":{"created_at":1793885885,"error":{"code":"bps_stream_error","message":"Upstream request failed, please try again later","type":"bps_stream_error"},"id":"resp_retry","model":"gpt-6-luna","object":"response","status":"failed"},"type":"response.failed"}

`,
		},
		{
			name:              "bps_stream_error_mouubox_58365",
			accountID:         58365,
			retryCount:        10,
			model:             "gpt-6.1-sol",
			upstreamRequestID: "ec293e2b-e75a-47ab-93e0-258a36168c17",
			status:            http.StatusOK,
			semanticStatus:    http.StatusBadGateway,
			contentType:       "text/event-stream",
			body: `event: response.created
data: {"type":"response.created","response":{"id":"resp_07ba0a18ac829dbb016aec8151a59487d1b6af2570ebe3e887"}}

event: response.failed
data: {"response":{"created_at":1793884529,"error":{"code":"bps_stream_error","message":"Upstream request failed, please try again later","type":"bps_stream_error"},"id":"resp_07ba0a18ac829dbb016aec8151a59487d1b6af2570ebe3e887","model":"gpt-6.1-sol","object":"response","status":"failed"},"type":"response.failed"}

`,
		},
		{
			name:              "request_payload_too_large",
			accountID:         59323,
			retryCount:        1,
			model:             "gpt-6-luna",
			upstreamRequestID: "3db01e58-ca5b-4451-aaee-697e132571d9",
			status:            http.StatusRequestEntityTooLarge,
			semanticStatus:    http.StatusRequestEntityTooLarge,
			contentType:       "application/json",
			body:              `{"error":{"message":"Request payload is too large","type":"invalid_request_error"}}`,
		},
	}
	for _, prelude := range []struct {
		name string
		wire string
	}{
		{name: "metadata"},
		{name: "empty_reasoning_done", wire: `data: {"type":"response.output_item.done","item":{"id":"rs_empty","type":"reasoning","summary":[]}}

`},
		{name: "empty_message_done", wire: `data: {"type":"response.output_item.done","item":{"id":"msg_empty","type":"message","content":[{"type":"output_text","text":""}]}}

`},
	} {
		mixed := cases[1]
		mixed.name = "413_then_bps_" + prelude.name
		mixed.accountID = 59328
		mixed.bodyLimitFirst = true
		mixed.body = strings.Replace(mixed.body, "event: response.failed", prelude.wire+"event: response.failed", 1)
		cases = append(cases, mixed)
	}
	for _, tt := range cases {
		for _, passthrough := range []bool{false, true} {
			for _, queueHeartbeat := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/passthrough=%t/queue_heartbeat=%t", tt.name, passthrough, queueHeartbeat), func(t *testing.T) {
					groupID := int64(4205)
					accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{{
						ID: tt.accountID, Name: "configured-status-retry", Platform: service.PlatformOpenAI,
						Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1,
						GroupIDs: []int64{groupID},
						Credentials: map[string]any{
							"api_key": "sk-test", "base_url": "https://api.example.test",
							"pool_mode": true, "pool_mode_retry_count": float64(tt.retryCount),
							"pool_mode_retry_status_codes": []any{float64(tt.semanticStatus)},
						},
						Extra: map[string]any{"openai_passthrough": passthrough, "openai_responses_supported": true},
					}}}
					cfg := &config.Config{RunMode: config.RunModeSimple}
					if tt.bodyLimitFirst {
						accountRepo.accounts[0].Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusRequestEntityTooLarge), float64(http.StatusBadGateway)}
					}
					cfg.Default.RateMultiplier = 1
					cfg.Gateway.MaxAccountSwitches = 1
					upstream := &openAIConfiguredStatusRetryUpstream{status: tt.status, body: tt.body, contentType: tt.contentType, upstreamRequestID: tt.upstreamRequestID}
					if tt.bodyLimitFirst {
						upstream.firstResponse = &http.Response{
							StatusCode: http.StatusRequestEntityTooLarge,
							Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{tt.upstreamRequestID}},
							Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Request payload is too large","type":"invalid_request_error"}}`)),
						}
					}
					billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
					t.Cleanup(billingCache.Stop)
					gateway := service.NewOpenAIGatewayService(
						accountRepo, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
						service.NewBillingService(cfg, nil), nil, billingCache, upstream,
						&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
					)
					h := NewOpenAIGatewayHandler(
						gateway, service.NewConcurrencyService(nil), billingCache,
						service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg,
					)
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					requestBody, err := json.Marshal(map[string]any{"model": tt.model, "input": "hello", "stream": true})
					require.NoError(t, err)
					c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(requestBody))
					c.Request.Header.Set("Content-Type", "application/json")
					c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
						ID: 1805, GroupID: &groupID,
						User:  &service.User{ID: 1705, Status: service.StatusActive},
						Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
					})
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1705, Concurrency: 0})
					if queueHeartbeat {
						c.Writer.Header().Set("Content-Type", "text/event-stream")
						written, err := c.Writer.WriteString(string(SSEPingFormatComment))
						require.NoError(t, err)
						recordGatewayStreamHeartbeat(c, written)
						c.Writer.Flush()
					}

					h.Responses(c)

					upstream.mu.Lock()
					calls := append([]int64(nil), upstream.accountIDs...)
					upstream.mu.Unlock()
					wantCalls := make([]int64, tt.retryCount+1)
					for i := range wantCalls {
						wantCalls[i] = tt.accountID
					}
					require.Equal(t, wantCalls, calls, "configured retries must reuse the same account and then stop")
					if tt.bodyLimitFirst {
						require.NotContains(t, rec.Body.String(), "response.created", "failed attempt metadata must stay buffered")
						require.NotContains(t, rec.Body.String(), "rs_empty")
						require.NotContains(t, rec.Body.String(), "msg_empty")
					}
					value, ok := c.Get(service.OpsUpstreamErrorsKey)
					require.True(t, ok)
					events, ok := value.([]*service.OpsUpstreamErrorEvent)
					require.True(t, ok)
					require.Len(t, events, len(wantCalls))
					for i, event := range events {
						require.Equal(t, "failover", event.Kind)
						status := tt.semanticStatus
						if tt.bodyLimitFirst && i == 0 {
							status = http.StatusRequestEntityTooLarge
						}
						require.Equal(t, status, event.UpstreamStatusCode)
						require.Equal(t, tt.accountID, event.AccountID)
						require.Equal(t, tt.upstreamRequestID, event.UpstreamRequestID)
					}
				})
			}
		}
	}
}
