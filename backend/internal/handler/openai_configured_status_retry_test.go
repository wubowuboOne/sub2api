package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

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
	requestBodies     [][]byte
	requestHeaders    []http.Header
	status            int
	body              string
	contentType       string
	upstreamRequestID string
	firstResponse     *http.Response
	bodyFactory       func() io.ReadCloser
	errorFactory      func() error
}

type openAIConfiguredStatusRetryCache struct {
	service.GatewayCache
	mu       sync.Mutex
	sessions map[string]int64
}

func (c *openAIConfiguredStatusRetryCache) GetSessionAccountID(_ context.Context, groupID int64, sessionHash string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id := c.sessions[fmt.Sprintf("%d:%s", groupID, sessionHash)]; id > 0 {
		return id, nil
	}
	return 0, service.ErrStickySessionNotFound
}

func (c *openAIConfiguredStatusRetryCache) SetSessionAccountID(_ context.Context, groupID int64, sessionHash string, accountID int64, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions == nil {
		c.sessions = make(map[string]int64)
	}
	c.sessions[fmt.Sprintf("%d:%s", groupID, sessionHash)] = accountID
	return nil
}

func (c *openAIConfiguredStatusRetryCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (c *openAIConfiguredStatusRetryCache) DeleteSessionAccountID(_ context.Context, groupID int64, sessionHash string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, fmt.Sprintf("%d:%s", groupID, sessionHash))
	return nil
}

func (u *openAIConfiguredStatusRetryUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.requestBodies = append(u.requestBodies, bodyBytes)
	u.requestHeaders = append(u.requestHeaders, req.Header.Clone())
	first := len(u.accountIDs) == 1
	u.mu.Unlock()
	if u.errorFactory != nil {
		if err := u.errorFactory(); err != nil {
			return nil, err
		}
	}
	if first && u.firstResponse != nil {
		return u.firstResponse, nil
	}
	body := io.NopCloser(strings.NewReader(u.body))
	if u.bodyFactory != nil {
		body = u.bodyFactory()
	}
	return &http.Response{
		StatusCode: u.status,
		Header:     http.Header{"Content-Type": []string{u.contentType}, "X-Request-Id": []string{u.upstreamRequestID}},
		Body:       body,
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
		secondAccount     bool
		transportErr      error
		retryStatus       int
		disablePool       bool
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
	for _, kind := range []string{"reasoning", "compaction"} {
		opaque := cases[0]
		opaque.name = "bps_opaque_" + kind
		opaque.accountID = 59327
		opaque.retryCount = 2
		opaque.secondAccount = true
		prelude := ""
		for _, event := range []string{"response.output_item.added", "response.output_item.done"} {
			prelude += fmt.Sprintf(`data: {"type":%q,"output_index":0,"item":{"id":"opaque_attempt","type":%q,"encrypted_content":"ciphertext","summary":[]}}

`, event, kind)
		}
		opaque.body = strings.Replace(opaque.body, "event: response.failed", prelude+"event: response.failed", 1)
		cases = append(cases, opaque)
	}
	for _, transport := range []struct {
		name        string
		err         error
		retries     int
		retryStatus int
		disablePool bool
		fallback    bool
	}{
		{name: "unexpected_eof_ten", err: io.ErrUnexpectedEOF, retries: 10, fallback: true},
		{name: "http2_protocol_error", err: fmt.Errorf("stream error: stream ID 1; PROTOCOL_ERROR; received from peer"), retries: 2, fallback: true},
		{name: "transport_502_not_configured", err: io.ErrUnexpectedEOF, retries: 10, retryStatus: http.StatusServiceUnavailable},
		{name: "transport_retry_disabled", err: io.ErrUnexpectedEOF},
		{name: "transport_non_pool", err: io.ErrUnexpectedEOF, retries: 10, disablePool: true},
	} {
		tt := cases[0]
		tt.name, tt.accountID, tt.model = transport.name, 59328, "gpt-6-sol"
		tt.transportErr = &url.Error{Op: "Post", URL: "https://vip.qimux.com/v1/responses", Err: transport.err}
		tt.retryCount, tt.retryStatus = transport.retries, transport.retryStatus
		tt.disablePool, tt.secondAccount = transport.disablePool, transport.fallback
		cases = append(cases, tt)
	}
	for _, tt := range cases {
		for _, passthrough := range []bool{false, true} {
			for _, queueHeartbeat := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/passthrough=%t/queue_heartbeat=%t", tt.name, passthrough, queueHeartbeat), func(t *testing.T) {
					groupID := int64(4205)
					retryStatus := tt.semanticStatus
					if tt.retryStatus != 0 {
						retryStatus = tt.retryStatus
					}
					accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{{
						ID: tt.accountID, Name: "configured-status-retry", Platform: service.PlatformOpenAI,
						Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1,
						GroupIDs: []int64{groupID},
						Credentials: map[string]any{
							"api_key": "sk-test", "base_url": "https://api.example.test",
							"pool_mode": !tt.disablePool, "pool_mode_retry_count": float64(tt.retryCount),
							"pool_mode_retry_status_codes": []any{float64(retryStatus)},
						},
						Extra: map[string]any{"openai_passthrough": passthrough, "openai_responses_supported": true},
					}}}
					var stickyCache service.GatewayCache
					if tt.secondAccount {
						stickyCache = &openAIConfiguredStatusRetryCache{}
						fallback := accountRepo.accounts[0]
						fallback.ID = tt.accountID + 100000
						fallback.Priority = 100
						fallback.Credentials = map[string]any{
							"api_key": "sk-fallback", "base_url": "https://api.example.test",
							"pool_mode": true, "pool_mode_retry_count": float64(0),
						}
						accountRepo.accounts = append(accountRepo.accounts, fallback)
					}
					cfg := &config.Config{RunMode: config.RunModeSimple}
					if tt.bodyLimitFirst {
						accountRepo.accounts[0].Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusRequestEntityTooLarge), float64(http.StatusBadGateway)}
					}
					cfg.Default.RateMultiplier = 1
					cfg.Gateway.MaxAccountSwitches = 1
					upstream := &openAIConfiguredStatusRetryUpstream{status: tt.status, body: tt.body, contentType: tt.contentType, upstreamRequestID: tt.upstreamRequestID}
					if tt.transportErr != nil {
						upstream.errorFactory = func() error {
							if tt.secondAccount {
								accountRepo.accounts[0].Priority = 100
								accountRepo.accounts[1].Priority = 1
							}
							return tt.transportErr
						}
					}
					if tt.secondAccount {
						upstream.bodyFactory = func() io.ReadCloser {
							// A fresh selection would now prefer the fallback; retries must
							// retain the original account through its sticky binding.
							accountRepo.accounts[0].Priority = 100
							accountRepo.accounts[1].Priority = 1
							return io.NopCloser(strings.NewReader(tt.body))
						}
					}
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
						accountRepo, nil, nil, nil, nil, nil, nil, stickyCache, cfg, nil, nil,
						service.NewBillingService(cfg, nil), nil, billingCache, upstream,
						&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
					)
					h := NewOpenAIGatewayHandler(
						gateway, service.NewConcurrencyService(nil), billingCache,
						service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg,
					)
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					requestBody, err := json.Marshal(map[string]any{"model": tt.model, "input": "hello", "stream": true, "prompt_cache_key": "stable-retry-cache"})
					require.NoError(t, err)
					c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(requestBody))
					c.Request.Header.Set("Content-Type", "application/json")
					c.Request.Header.Set("session_id", "stable-retry-session")
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
					bodies := append([][]byte(nil), upstream.requestBodies...)
					headers := append([]http.Header(nil), upstream.requestHeaders...)
					upstream.mu.Unlock()
					for i := 1; i < len(bodies); i++ {
						require.Equal(t, bodies[0], bodies[i], "same-account retries must preserve the body and prompt cache key")
						require.Equal(t, headers[0].Get("session_id"), headers[i].Get("session_id"))
					}
					wantRetries := tt.retryCount
					if tt.disablePool || retryStatus != tt.semanticStatus {
						wantRetries = 0
					}
					wantCalls := make([]int64, wantRetries+1)
					for i := range wantCalls {
						wantCalls[i] = tt.accountID
					}
					if tt.secondAccount {
						wantCalls = append(wantCalls, tt.accountID+100000)
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
						kind := "failover"
						status := tt.semanticStatus
						requestID := tt.upstreamRequestID
						if tt.transportErr != nil {
							kind, status, requestID = "request_error", 0, ""
						}
						require.Equal(t, kind, event.Kind)
						if tt.bodyLimitFirst && i == 0 {
							status = http.StatusRequestEntityTooLarge
						}
						require.Equal(t, status, event.UpstreamStatusCode)
						require.Equal(t, wantCalls[i], event.AccountID)
						require.Equal(t, requestID, event.UpstreamRequestID)
					}
				})
			}
		}
	}
}

func TestOpenAIMessagesConfiguredStatusRetriesSameAccount(t *testing.T) {
	for _, test := range []messagesRetryCase{
		{name: "transport_unexpected_eof_ten", retries: 10, retryStatus: http.StatusBadGateway, wantCalls: 11, transportErr: io.ErrUnexpectedEOF},
		{name: "transport_http2_recovers_after_two", retries: 10, retryStatus: http.StatusBadGateway, wantCalls: 3, successAfter: 2, transportErr: fmt.Errorf("stream error: stream ID 1; PROTOCOL_ERROR; received from peer")},
		{name: "transport_502_not_configured", retries: 10, retryStatus: http.StatusServiceUnavailable, wantCalls: 1, transportErr: io.ErrUnexpectedEOF},
		{name: "transport_retry_disabled", retryStatus: http.StatusBadGateway, wantCalls: 1, transportErr: io.ErrUnexpectedEOF},
		{name: "configured_ten", retries: 10, retryStatus: http.StatusBadGateway, wantCalls: 11},
		{name: "queue_heartbeat", retries: 2, retryStatus: http.StatusBadGateway, wantCalls: 3, queueHeartbeat: true},
		{name: "stream_heartbeat", retries: 2, retryStatus: http.StatusBadGateway, wantCalls: 3, streamHeartbeat: true},
		{name: "502_not_configured", retries: 10, retryStatus: http.StatusServiceUnavailable, wantCalls: 1},
		{name: "real_output", retries: 10, retryStatus: http.StatusBadGateway, wantCalls: 1, realOutput: true},
		{name: "recovers_after_two", retries: 10, retryStatus: http.StatusBadGateway, wantCalls: 3, successAfter: 2},
		{name: "opaque_preamble_recovers_after_two", retries: 10, retryStatus: http.StatusBadGateway, wantCalls: 3, successAfter: 2, opaquePrelude: true},
	} {
		t.Run(test.name, func(t *testing.T) { runMessagesRetryCase(t, test) })
	}
}

type messagesRetryCase struct {
	transportErr                                error
	successAfter                                int
	name                                        string
	retries, retryStatus, wantCalls             int
	queueHeartbeat, streamHeartbeat, realOutput bool
	opaquePrelude                               bool
}

type messagesRetryUsageRecorder struct {
	service.UsageLogRepository
	rows []service.UsageLog
}

func (r *messagesRetryUsageRecorder) Create(_ context.Context, usage *service.UsageLog) (bool, error) {
	r.rows = append(r.rows, *usage)
	return true, nil
}

type messagesRetryHeartbeatWriter struct {
	gin.ResponseWriter
	beat chan<- struct{}
}

func (w *messagesRetryHeartbeatWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil && bytes.Contains(p, []byte("event: ping")) {
		select {
		case w.beat <- struct{}{}:
		default:
		}
	}
	return n, err
}

func runMessagesRetryCase(t *testing.T, test messagesRetryCase) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(4205)
	accountID := int64(59328)
	accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{{
		ID: accountID, Name: "messages-configured-retry", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1,
		GroupIDs: []int64{groupID},
		Credentials: map[string]any{
			"api_key": "sk-test", "base_url": "https://api.example.test",
			"pool_mode": true, "pool_mode_retry_count": float64(test.retries),
			"pool_mode_retry_status_codes": []any{float64(test.retryStatus)},
		},
		Extra: map[string]any{"openai_responses_supported": true},
	}}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxAccountSwitches = 1
	upstream := &openAIConfiguredStatusRetryUpstream{
		status: http.StatusOK, contentType: "text/event-stream", upstreamRequestID: "29f351b8-a09e-4b48-9058-15642b01cb6f",
		body: `event: response.created
data: {"type":"response.created","response":{"id":"resp_bps_messages","status":"in_progress"}}

event: response.failed
data: {"response":{"created_at":1793894912,"error":{"code":"bps_stream_error","message":"Upstream request failed, please try again later","type":"bps_stream_error"},"id":"resp_02eb27c4c9368e16016aeca9fb97c487d2a1507e730fb38d7b","model":"gpt-6-luna","object":"response","status":"failed"},"type":"response.failed"}

`,
	}
	if test.transportErr != nil {
		upstream.errorFactory = func() error {
			upstream.mu.Lock()
			attempt := len(upstream.accountIDs)
			upstream.mu.Unlock()
			if test.successAfter == 0 || attempt <= test.successAfter {
				return &url.Error{Op: "Post", URL: "https://vip.qimux.com/v1/responses", Err: test.transportErr}
			}
			return nil
		}
	}
	if test.opaquePrelude {
		for _, event := range []string{"response.output_item.added", "response.output_item.done"} {
			prelude := fmt.Sprintf(`data: {"type":%q,"output_index":0,"item":{"id":"opaque_attempt","type":"reasoning","encrypted_content":"ciphertext","summary":[]}}

`, event)
			upstream.body = strings.Replace(upstream.body, "event: response.failed", prelude+"event: response.failed", 1)
		}
	}
	if test.realOutput {
		upstream.body = strings.Replace(upstream.body, "event: response.failed", "data: "+`{"type":"response.output_text.delta","delta":"partial"}`+"\n\nevent: response.failed", 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	beat := make(chan struct{}, 1)
	if test.successAfter > 0 {
		upstream.bodyFactory = func() io.ReadCloser {
			upstream.mu.Lock()
			attempt := len(upstream.accountIDs)
			upstream.mu.Unlock()
			body := upstream.body
			if attempt > test.successAfter {
				body = `data: {"type":"response.created","response":{"id":"resp_messages_success"}}

data: {"type":"response.output_text.delta","output_index":0,"delta":"recovered answer"}

data: {"type":"response.completed","response":{"id":"resp_messages_success","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}

`
			}
			return io.NopCloser(strings.NewReader(body))
		}
	}
	if test.streamHeartbeat {
		cfg.Gateway.StreamKeepaliveInterval = 1
		cfg.Gateway.StreamDataIntervalTimeout = 4
		upstream.bodyFactory = func() io.ReadCloser {
			reader, writer := io.Pipe()
			t.Cleanup(func() { _ = reader.Close() })
			go func() {
				defer writer.Close()
				select {
				case <-beat:
					_, _ = io.WriteString(writer, upstream.body)
				case <-ctx.Done():
					_ = writer.CloseWithError(ctx.Err())
				}
			}()
			return reader
		}
	}
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	usageRepo := &messagesRetryUsageRecorder{}
	gateway := service.NewOpenAIGatewayService(
		accountRepo, nil, usageRepo, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCache, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-6-sol","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request = c.Request.WithContext(ctx)
	if test.streamHeartbeat {
		c.Writer = &messagesRetryHeartbeatWriter{ResponseWriter: c.Writer, beat: beat}
	}
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID: 1805, GroupID: &groupID,
		User:  &service.User{ID: 1705, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowMessagesDispatch: true},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1705, Concurrency: 0})
	if test.queueHeartbeat {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		written, err := c.Writer.WriteString(string(SSEPingFormatClaude))
		require.NoError(t, err)
		recordGatewayStreamHeartbeat(c, written)
		c.Writer.Flush()
	}
	h.Messages(c)
	upstream.mu.Lock()
	calls := append([]int64(nil), upstream.accountIDs...)
	upstream.mu.Unlock()
	require.Len(t, calls, test.wantCalls, "Messages must honor retry count, status and output boundaries")
	for _, id := range calls {
		require.Equal(t, accountID, id)
	}
	wantKind := "failover"
	wantStatus := http.StatusBadGateway
	if test.transportErr != nil {
		wantKind, wantStatus = "request_error", 0
	}
	wantEvents := test.wantCalls
	if test.successAfter > 0 {
		wantEvents = test.successAfter
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), "recovered answer")
		require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_start"))
		require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_stop"))
		require.NotContains(t, rec.Body.String(), "resp_bps_messages")
		require.NotContains(t, rec.Body.String(), "event: error")
	} else if test.realOutput {
		wantKind = "http_error"
		require.Contains(t, rec.Body.String(), "partial")
	} else {
		require.NotContains(t, rec.Body.String(), "event: message_start")
	}
	if test.streamHeartbeat {
		require.Contains(t, rec.Body.String(), "event: ping")
		require.Contains(t, rec.Body.String(), "event: error")
	}
	value, ok := c.Get(service.OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := value.([]*service.OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, wantEvents)
	for _, event := range events {
		require.Equal(t, wantKind, event.Kind)
		require.Equal(t, wantStatus, event.UpstreamStatusCode)
		require.Equal(t, accountID, event.AccountID)
	}
	if test.successAfter > 0 {
		require.Len(t, usageRepo.rows, 1, "retry attempts must not create separate usage records")
		require.Equal(t, 1, usageRepo.rows[0].InputTokens)
		require.Equal(t, 1, usageRepo.rows[0].OutputTokens)
	} else if test.realOutput {
		require.Len(t, usageRepo.rows, 1, "a terminal failure may retain one partial usage record")
		require.Zero(t, usageRepo.rows[0].ActualCost)
	} else {
		require.Empty(t, usageRepo.rows, "pre-output retries and exhausted failover must not bill attempts")
	}
}
