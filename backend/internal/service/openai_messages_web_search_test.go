package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIMessagesWebSearchRoutingScope(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, true},
		{`{"tools":[{"type":"web_search_20260209"}]}`, true},
		{`{"tools":[{"name":"web_search"}]}`, true},
		{`{"tools":[{"type":"web_search_20250305"},{"name":"read_file"}]}`, false},
		{`{"tools":[]}`, false},
		{`{"messages":[{"role":"user","content":"hello"}]}`, false},
		{`{"tools":[{"type":"web_search_20250305"}],"tool_choice":{"type":"none"}}`, false},
		{`{"tools":[{"type":"web_search_20250305"}],"tool_choice":"none"}`, false},
	} {
		require.Equal(t, tc.want, IsOpenAIMessagesWebSearchRequest([]byte(tc.body)), tc.body)
	}
}

func TestOpenAIMessagesWebSearchProtocolsAndPromptPreservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		for _, route := range []string{"oauth_responses", "apikey_responses", "pat_responses"} {
			t.Run(fmt.Sprintf("%s/stream=%t", route, stream), func(t *testing.T) {
				account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
					Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://upstream.example", "model_mapping": map[string]any{"public-opus": "gpt-test"}},
					Extra:       map[string]any{OpenAIWebSearchModeKey: OpenAIWebSearchModeOnly}}
				if strings.HasPrefix(route, "oauth") || route == "pat_responses" {
					account.Type = AccountTypeOAuth
					account.Credentials["access_token"] = "fixture-oauth"
					account.Credentials["chatgpt_account_id"] = "fixture-account"
				}
				if route == "pat_responses" {
					account.Credentials["access_token"] = "at-fixture"
					account.Credentials["auth_mode"] = OpenAIAuthModePersonalAccessToken
				}
				searchResponse := func(status int, body string) *http.Response {
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
				}
				upstream := &httpUpstreamRecorder{responses: []*http.Response{searchResponse(200, messagesWebSearchResponsesSSE("Verified search output"))}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				body := []byte(fmt.Sprintf(`{"model":"public-opus","max_tokens":1024,"stream":%t,"system":"CLIENT-SYSTEM","messages":[{"role":"user","content":"latest news"}],"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["example.com"],"user_location":{"type":"approximate","country":"US"}}]}`, stream))
				body, err := groupprompt.Prepend(body, "messages", groupprompt.DefaultPrompt)
				require.NoError(t, err)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
				ctx := WithOpenAIMessagesWebSearch(context.Background(), account.ID, "public-opus")
				result, err := svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 1, result.WebSearchCalls)
				require.Equal(t, "public-opus", result.Model)
				require.Equal(t, "gpt-test", result.UpstreamModel)
				require.Equal(t, stream, result.Stream)
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Contains(t, recorder.Body.String(), "Verified search output")
				require.Contains(t, recorder.Body.String(), "web_search_tool_result")
				require.Contains(t, recorder.Body.String(), "https://example.com/news")
				require.Contains(t, recorder.Body.String(), "public-opus")
				require.NotContains(t, recorder.Body.String(), "gpt-test")
				if stream {
					require.Contains(t, recorder.Body.String(), "event: message_stop")
				}
				for _, payload := range upstream.bodies {
					require.Equal(t, "gpt-test", gjson.GetBytes(payload, "model").String())
					require.Equal(t, groupprompt.DefaultPrompt+"\n\nCLIENT-SYSTEM", gjson.GetBytes(payload, "input.0.content.0.text").String(), "the entire current group prompt must survive both search transports")
				}
				require.True(t, strings.HasSuffix(upstream.lastReq.URL.Path, "/responses"))
				require.Equal(t, "web_search", gjson.GetBytes(upstream.lastBody, "tools.0.type").String())
				require.Equal(t, "required", gjson.GetBytes(upstream.lastBody, "tool_choice").String())
				require.Equal(t, "example.com", gjson.GetBytes(upstream.lastBody, "tools.0.filters.allowed_domains.0").String())
				require.Equal(t, "US", gjson.GetBytes(upstream.lastBody, "tools.0.user_location.country").String())
				if route == "apikey_responses" {
					require.Equal(t, "https://upstream.example/v1/responses", upstream.lastReq.URL.String())
					require.NotEqual(t, "chatgpt.com", upstream.lastReq.Host)
					require.Equal(t, "Bearer fixture-key", upstream.lastReq.Header.Get("Authorization"))
					require.True(t, gjson.GetBytes(upstream.lastBody, "max_output_tokens").Exists())
				} else {
					require.Equal(t, chatgptCodexURL, upstream.lastReq.URL.String())
					require.False(t, gjson.GetBytes(upstream.lastBody, "max_output_tokens").Exists())
					require.True(t, gjson.GetBytes(upstream.lastBody, "instructions").Exists())
				}
				require.Len(t, upstream.requests, 1)
			})
		}
	}
}

func TestOpenAIMessagesWebSearchInvalidOutputIsNotBillable(t *testing.T) {
	for _, payload := range []string{`not JSON`, `{"encrypted_output":"private-cipher"}`, `{"error":{"message":"failed"}}`, alphaSearchResponsesSSE("answer without a search call"), "data: {\"type\":\"response.incomplete\"}\n\n"} {
		t.Run(payload, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			a := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "base_url": "https://upstream.example"}}
			body := []byte(`{"model":"gpt-test","max_tokens":64,"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20250305"}]}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
			result, err := svc.ForwardAsAnthropic(WithOpenAIMessagesWebSearch(context.Background(), 7, "gpt-test"), c, a, body, "", "")
			require.Error(t, err)
			require.Nil(t, result)
			require.False(t, c.Writer.Written())
			require.NotContains(t, rec.Body.String(), "private-cipher")
		})
	}
}

func messagesWebSearchResponsesSSE(output string) string {
	return strings.Replace(alphaSearchResponsesSSE(output), `"output":[{"type":"message"`, `"output":[{"type":"web_search_call","id":"ws_fixture","status":"completed","action":{"type":"search","query":"latest news"}},{"type":"message"`, 1)
}

func TestOpenAIMessagesWebSearchQueryRetainsAllTextBlocks(t *testing.T) {
	query := messagesWebSearchQuery([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"latest news"},{"type":"image","source":{}},{"type":"text","text":"only sources from this week"}]}]}`))
	require.Equal(t, "latest news\n\nonly sources from this week", query)
	require.Empty(t, messagesWebSearchQuery([]byte(`{"messages":[{"role":"assistant","content":"not a search query"}]}`)))
}

func TestOpenAIMessagesWebSearchDoesNotReuseStandaloneProbeFailure(t *testing.T) {
	a := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "base_url": "https://upstream.example"}, Extra: map[string]any{OpenAIWebSearchModeKey: OpenAIWebSearchModeOnly}}
	a.Extra["openai_web_search_probe"] = map[string]any{"supported": false, "model": "gpt-5.4", "route": openAIWebSearchRouteFingerprint(a)}
	require.False(t, openAIWebSearchAllowedForRequest(WithOpenAIWebSearchRouting(context.Background(), 7, "gpt-5.4"), a, OpenAIEndpointCapabilityAlphaSearch))
	ctx := WithOpenAIMessagesWebSearch(context.Background(), 7, "gpt-5.4")
	require.True(t, openAIWebSearchAllowedForRequest(ctx, a, OpenAIEndpointCapabilityAlphaSearch))
	require.False(t, openAIWebSearchAllowedForRequest(WithOpenAIMessagesWebSearch(context.Background(), 8, "gpt-5.4"), a, OpenAIEndpointCapabilityAlphaSearch))
	a.Extra[OpenAIWebSearchModeKey] = OpenAIWebSearchModeDisabled
	require.False(t, openAIWebSearchAllowedForRequest(ctx, a, OpenAIEndpointCapabilityAlphaSearch))
}

func TestOpenAIMessagesWebSearchMappedModelUsesReasoningSchema(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(messagesWebSearchResponsesSSE("answer")))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	a := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "base_url": "https://upstream.example", "model_mapping": map[string]any{"group-search-model": "gpt-5.4"}}}
	body := []byte(`{"model":"public-opus","max_tokens":1024,"temperature":0.7,"top_p":0.5,"messages":[{"role":"user","content":"latest news"}],"tools":[{"type":"web_search_20250305"}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	result, err := svc.ForwardAsAnthropic(WithOpenAIMessagesWebSearch(context.Background(), 7, "group-search-model"), c, a, body, "", "group-search-model")
	require.NoError(t, err)
	require.Equal(t, "gpt-5.4", result.UpstreamModel)
	require.Equal(t, "public-opus", result.Model)
	require.Equal(t, "gpt-5.4", gjson.GetBytes(upstream.lastBody, "model").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "temperature").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "top_p").Exists())
}

func TestOpenAIMessagesWebSearchRejectsUnsupportedOrEmptyInput(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5.4","max_tokens":1024,"messages":[{"role":"user","content":""}],"tools":[{"type":"web_search_20250305"}]}`,
		`{"model":"gpt-5.4","max_tokens":1024,"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305","blocked_domains":["example.com"]}]}`,
	} {
		upstream := &httpUpstreamRecorder{}
		svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		a := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture"}}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		result, err := svc.ForwardAsAnthropic(WithOpenAIMessagesWebSearch(context.Background(), 7, "gpt-5.4"), c, a, []byte(body), "", "")
		require.Error(t, err)
		require.Nil(t, result)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Empty(t, upstream.requests)
	}
}

func TestOpenAIMessagesWebSearchCitationNormalization(t *testing.T) {
	response, text, err := parseMessagesWebSearchResponse([]byte(`{"output":"","results":[{"url":"javascript:bad"},{"url":"https://example.com","snippet":"excerpt"},{"url":"https://example.com","title":"duplicate"}]}`), "query")
	require.NoError(t, err)
	require.Len(t, response.Results, 1)
	require.Equal(t, "https://example.com", response.Results[0].Title)
	require.Contains(t, text, "excerpt")
}

func TestOpenAIMessagesWebSearchUpstreamErrorAfterHeartbeat(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"unsupported tool"}}`))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	a := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "base_url": "https://upstream.example"}}
	body := []byte(`{"model":"gpt-5.4","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	c.Header("Content-Type", "text/event-stream")
	_, err := c.Writer.WriteString(": ping\n\n")
	require.NoError(t, err)
	c.Writer.Flush()
	result, err := svc.ForwardAsAnthropic(WithOpenAIMessagesWebSearch(context.Background(), 7, "gpt-5.4"), c, a, body, "", "")
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusOK, rec.Code, "headers were already sent with the heartbeat")
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error"))
	blocks := strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
	require.Len(t, blocks, 2)
	require.Equal(t, "api_error", gjson.Get(openAIAlphaSearchSSEData(blocks[1]), "error.type").String())
}
