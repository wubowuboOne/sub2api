package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const configuredHTTPInvalidURLBody = `{"error":{"code":"invalid_value","message":"Upstream rejected the request","param":"url","type":"invalid_request_error"}}`

func TestOpenAIConfiguredHTTP400ExhaustionPreservesError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, streaming := range []bool{false, true} {
		name := "json"
		if streaming {
			name = "heartbeat_committed"
		}
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if streaming {
				c.Writer.Header().Set("Content-Type", "text/event-stream")
				c.Writer.Flush()
			}
			(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
				StatusCode: http.StatusBadRequest, ResponseBody: []byte(configuredHTTPInvalidURLBody), RetryableOnSameAccount: true,
			}, streaming)

			if streaming {
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed"))
				require.Contains(t, rec.Body.String(), `"code":"invalid_value"`)
				require.Contains(t, rec.Body.String(), "Upstream rejected the request")
			} else {
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.JSONEq(t, configuredHTTPInvalidURLBody, rec.Body.String())
			}
		})
	}
}

func TestOpenAIHTTP400ExhaustionKeepsOtherErrorPolicies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{name: "transient_processing", status: 400, body: `{"error":{"type":"invalid_request_error","message":"An error occurred while processing your request"}}`, want: 502},
		{name: "capacity", status: 400, body: `{"error":{"type":"invalid_request_error","message":"Selected model is at capacity"}}`, want: 502},
		{name: "credential", status: 400, body: `{"error":{"type":"invalid_request_error","code":"account_deactivated","message":"Account deactivated"}}`, want: 502},
		{name: "cyber_policy", status: 400, body: `{"error":{"type":"invalid_request_error","code":"cyber_policy","message":"Request blocked"}}`, want: 502},
		{name: "session_blocked", status: 400, body: `{"error":{"type":"invalid_request_error","code":"session_blocked_by_cyber_policy","message":"Session blocked"}}`, want: 502},
		{name: "context_window", status: 400, body: `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Input exceeds the context window"}}`, want: 502},
		{name: "untyped_400", status: 400, body: `{"error":{"message":"Upstream failed"}}`, want: 502},
		{name: "server_error_400", status: 400, body: `{"error":{"type":"server_error","message":"Upstream failed"}}`, want: 502},
		{name: "unauthorized", status: 401, body: configuredHTTPInvalidURLBody, want: 502},
		{name: "rate_limit", status: 429, body: configuredHTTPInvalidURLBody, want: 429},
		{name: "server_error", status: 502, body: configuredHTTPInvalidURLBody, want: 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
				StatusCode: test.status, ResponseBody: []byte(test.body), RetryableOnSameAccount: true,
			}, false)
			require.Equal(t, test.want, rec.Code)
		})
	}
}

type configuredHTTPPassthroughRepo struct {
	service.ErrorPassthroughRepository
	rules []*model.ErrorPassthroughRule
}

func (r *configuredHTTPPassthroughRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return r.rules, nil
}

func TestOpenAIHTTP400ExhaustionHonorsPassthroughRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	status, message := http.StatusUnprocessableEntity, "custom request rejection"
	h := &OpenAIGatewayHandler{errorPassthroughService: service.NewErrorPassthroughService(
		&configuredHTTPPassthroughRepo{rules: []*model.ErrorPassthroughRule{{
			ID: 1, Enabled: true, ErrorCodes: []int{400}, MatchMode: model.MatchModeAny,
			ResponseCode: &status, CustomMessage: &message,
		}}}, nil,
	)}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusBadRequest, ResponseBody: []byte(configuredHTTPInvalidURLBody), RetryableOnSameAccount: true,
	}, false)
	require.Equal(t, status, rec.Code)
	require.Contains(t, rec.Body.String(), message)
}

func TestOpenAIConfiguredHTTP400ExhaustionSanitizesMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusBadRequest, RetryableOnSameAccount: true,
		ResponseBody: []byte(`{"error":{"type":"invalid_request_error","code":"invalid_value","param":"url","message":"Invalid URL: https://image.example.test/?key=must-not-leak"}}`),
	}, false)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.NotContains(t, rec.Body.String(), "must-not-leak")
	require.Contains(t, rec.Body.String(), "key=***")
	recorded, ok := c.Get(service.OpsUpstreamErrorMessageKey)
	require.True(t, ok)
	require.NotContains(t, recorded, "must-not-leak")
}

func TestOpenAIConfiguredHTTP400ExhaustionHasFallbackMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusBadRequest, ResponseBody: []byte(`{"error":{"type":"invalid_request_error"}}`), RetryableOnSameAccount: true,
	}, true)
	require.Contains(t, rec.Body.String(), "Upstream rejected the request")
	require.Contains(t, rec.Body.String(), "event: response.failed")
}

func TestOpenAIHTTP400ExhaustionKeepsOtherPlatforms(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{service.PlatformGrok, service.PlatformAnthropic, service.PlatformGemini, service.PlatformDeepseek} {
		t.Run(platform, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: platform}})
			(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
				StatusCode: http.StatusBadRequest, ResponseBody: []byte(configuredHTTPInvalidURLBody), RetryableOnSameAccount: true,
			}, false)
			require.Equal(t, http.StatusBadGateway, rec.Code)
		})
	}
}

func TestOpenAIHTTP400ExhaustionKeepsNonRetryableMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusBadRequest, ResponseBody: []byte(configuredHTTPInvalidURLBody),
	}, false)
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestOpenAIHTTP400ExhaustionUsesResolvedCompositePlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		platform string
		want     int
	}{{service.PlatformOpenAI, http.StatusBadRequest}, {service.PlatformGrok, http.StatusBadGateway}} {
		t.Run(test.platform, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), test.platform))
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}})
			(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
				StatusCode: http.StatusBadRequest, ResponseBody: []byte(configuredHTTPInvalidURLBody), RetryableOnSameAccount: true,
			}, false)
			require.Equal(t, test.want, rec.Code)
		})
	}
}
