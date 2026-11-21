//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAnthropicStopMatcher(t *testing.T) {
	for _, tc := range []struct {
		name, want, stop string
		stops, chunks    []string
	}{
		{name: "split", stops: []string{"END"}, chunks: []string{"Hello E", "N", "D hidden"}, want: "Hello ", stop: "END"},
		{name: "prefix only", stops: []string{"END"}, chunks: []string{"Hello E", "N"}, want: "Hello EN"},
		{name: "prefix diverges", stops: []string{"END"}, chunks: []string{"E", "Next"}, want: "ENext"},
		{name: "earliest match", stops: []string{"later", "first"}, chunks: []string{"ok first later"}, want: "ok ", stop: "first"},
		{name: "first completed stop", stops: []string{"ABC", "B"}, chunks: []string{"ABC tail"}, want: "A", stop: "B"},
		{name: "first completed stop split", stops: []string{"ABC", "B"}, chunks: []string{"A", "B", "C tail"}, want: "A", stop: "B"},
		{name: "unicode", stops: []string{"\u7ed3\u675f"}, chunks: []string{"answer \u7ed3", "\u675f hidden"}, want: "answer ", stop: "\u7ed3\u675f"},
		{name: "overlap", stops: []string{"ABAB", "XYZ"}, chunks: []string{"A", "BA", "B tail"}, stop: "ABAB"},
		{name: "ordinary text", stops: []string{"END"}, chunks: []string{"hello", " world"}, want: "hello world"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := anthropicStopMatcher{stops: tc.stops}
			var output strings.Builder
			matched := ""
			for _, chunk := range tc.chunks {
				text, stop := m.push(chunk)
				output.WriteString(text)
				require.LessOrEqual(t, len(m.pending), 4)
				if stop != "" {
					matched = stop
					break
				}
			}
			output.WriteString(m.pending)
			require.Equal(t, tc.want, output.String())
			require.Equal(t, tc.stop, matched)
		})
	}
	t.Run("normal text is released immediately", func(t *testing.T) {
		m := anthropicStopMatcher{stops: []string{"END"}}
		text, stop := m.push(strings.Repeat("hello ", 10000))
		require.Len(t, text, 60000)
		require.Empty(t, m.pending)
		require.Empty(t, stop)
	})
	t.Run("long repetitive stop", func(t *testing.T) {
		m := anthropicStopMatcher{stops: []string{strings.Repeat("a", 100000) + "b"}}
		input := strings.Repeat("a", 100000) + "c"
		text, stop := m.push(input)
		require.Equal(t, input, text)
		require.Empty(t, stop)
		require.Empty(t, m.pending)
	})
}

func anthropicStopFixtureSSE(chunks ...string) string {
	var output strings.Builder
	write := func(data string) {
		fmt.Fprintf(&output, "event: %s\ndata: %s\n\n", gjson.Get(data, "type").String(), data)
	}
	write(`{"type":"message_start","message":{"id":"msg_fixture","type":"message","model":"claude-sonnet-4-20250514","role":"assistant","content":[],"usage":{"input_tokens":12,"cache_read_input_tokens":6,"output_tokens":1}},"extension":"keep"}`)
	write(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"END in reasoning","signature":"signed-END"}}`)
	write(`{"type":"content_block_stop","index":0}`)
	write(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
	for _, chunk := range chunks {
		data, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]string{"type": "text_delta", "text": chunk}})
		write(string(data))
	}
	write(`{"type":"content_block_stop","index":1}`)
	write(`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool_hidden","name":"Read","input":{}}}`)
	write(`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"END\"}"}}`)
	write(`{"type":"content_block_stop","index":2}`)
	write(`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9},"extension":"keep"}`)
	write(`{"type":"message_stop"}`)
	return output.String()
}

func anthropicStopEvents(t *testing.T, body string) []gjson.Result {
	t.Helper()
	var events []gjson.Result
	for _, line := range strings.Split(body, "\n") {
		if data, ok := extractAnthropicSSEDataLine(strings.TrimSuffix(line, "\r")); ok {
			require.True(t, json.Valid([]byte(data)), data)
			events = append(events, gjson.Parse(data))
		}
	}
	return events
}

func TestAnthropicStopSequenceWriterStream(t *testing.T) {
	for _, chunkSize := range []int{1, 7, 4096} {
		t.Run(fmt.Sprint(chunkSize), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			w, restore := installAnthropicStopSequenceWriter(c, []string{"END"}, true)
			defer restore()
			body := anthropicStopFixtureSSE("Hello E", "N", "D hidden", "more hidden")
			for offset := 0; offset < len(body); offset += chunkSize {
				part := body[offset:min(offset+chunkSize, len(body))]
				n, err := w.WriteString(part)
				require.NoError(t, err)
				require.Equal(t, len(part), n)
				w.Flush()
			}
			require.NoError(t, w.finish())
			var text strings.Builder
			stopped, terminal := false, false
			for _, event := range anthropicStopEvents(t, rec.Body.String()) {
				if event.Get("delta.type").String() == "text_delta" {
					text.WriteString(event.Get("delta.text").String())
				}
				require.NotEqual(t, int64(2), event.Get("index").Int())
				if event.Get("type").String() == "message_delta" {
					require.Equal(t, "stop_sequence", event.Get("delta.stop_reason").String())
					require.Equal(t, "END", event.Get("delta.stop_sequence").String())
					require.Equal(t, int64(9), event.Get("usage.output_tokens").Int())
					require.Equal(t, "keep", event.Get("extension").String())
					stopped = true
				}
				terminal = terminal || event.Get("type").String() == "message_stop"
			}
			require.Equal(t, "Hello ", text.String())
			require.True(t, stopped)
			require.True(t, terminal)
			require.Contains(t, rec.Body.String(), "signed-END")
		})
	}
}

func TestAnthropicStopSequenceWriterFlushesUnmatchedPrefixAndPreservesTools(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	w, restore := installAnthropicStopSequenceWriter(c, []string{"END"}, true)
	defer restore()
	_, err := w.WriteString(anthropicStopFixtureSSE("hello E", "N"))
	require.NoError(t, err)
	require.NoError(t, w.finish())
	var text strings.Builder
	for _, event := range anthropicStopEvents(t, rec.Body.String()) {
		if event.Get("delta.type").String() == "text_delta" {
			text.WriteString(event.Get("delta.text").String())
		}
		if event.Get("type").String() == "message_delta" {
			require.Equal(t, "tool_use", event.Get("delta.stop_reason").String())
		}
	}
	require.Equal(t, "hello EN", text.String())
	require.Contains(t, rec.Body.String(), "tool_hidden")
}

func TestAnthropicStopSequenceWriterJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Header("Content-Length", "1000")
	w, restore := installAnthropicStopSequenceWriter(c, []string{"END"}, false)
	defer restore()
	_, err := w.WriteString(`{"type":"message","model":"client-model","content":[{"type":"thinking","thinking":"END","signature":"opaque"},{"type":"text","text":"hello END hidden","citations":[]},{"type":"tool_use","id":"hidden"}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":9},"extension":"keep"}`)
	require.NoError(t, err)
	require.NoError(t, w.finish())
	got := gjson.Parse(rec.Body.String())
	require.Equal(t, "hello ", got.Get("content.1.text").String())
	require.Len(t, got.Get("content").Array(), 2)
	require.True(t, got.Get("content.1.citations").IsArray())
	require.Equal(t, "opaque", got.Get("content.0.signature").String())
	require.Equal(t, "stop_sequence", got.Get("stop_reason").String())
	require.Equal(t, "END", got.Get("stop_sequence").String())
	require.Equal(t, int64(9), got.Get("usage.output_tokens").Int())
	require.Equal(t, "keep", got.Get("extension").String())
	require.Empty(t, rec.Header().Get("Content-Length"))
}

func stopCompatHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"X-Request-Id": []string{"stop-fixture"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestAnthropicStopSequenceCompatibilityRetry(t *testing.T) {
	request := []byte(`{"model":"fixture","stop_sequences":["END"],"messages":[{"role":"user","content":"hello"}]}`)
	rejection := `{"error":{"type":"invalid_request_error","message":"` + anthropicStopSequencesUnsupported + `"},"type":"error"}`
	for _, tc := range []struct {
		name, request, rejection string
		status, calls            int
	}{
		{name: "explicit incompatibility", request: string(request), rejection: rejection, status: 400, calls: 2},
		{name: "ordinary invalid request", request: string(request), rejection: `{"error":{"message":"invalid tool input"}}`, status: 400, calls: 1},
		{name: "malformed error", request: string(request), rejection: strings.TrimSuffix(rejection, "}"), status: 400, calls: 1},
		{name: "large error", request: string(request), rejection: rejection + strings.Repeat(" ", 100000), status: 400, calls: 1},
		{name: "unauthorized", request: string(request), rejection: rejection, status: 401, calls: 1},
		{name: "no stop parameter", request: `{"model":"fixture"}`, rejection: rejection, status: 400, calls: 1},
		{name: "invalid stop parameter", request: `{"stop_sequences":[1]}`, rejection: rejection, status: 400, calls: 1},
		{name: "empty stop parameter", request: `{"stop_sequences":[""]}`, rejection: rejection, status: 400, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			resp, stops, err := doAnthropicStopSequenceCompatibleRequest(context.Background(), nil, newAnthropicAPIKeyAccountForTest(), []byte(tc.request), func(body []byte) (*http.Response, error) {
				calls++
				if calls == 1 {
					require.JSONEq(t, tc.request, string(body))
					return stopCompatHTTPResponse(tc.status, tc.rejection), nil
				}
				require.False(t, gjson.GetBytes(body, "stop_sequences").Exists())
				require.Equal(t, "hello", gjson.GetBytes(body, "messages.0.content").String())
				// A second rejection must not recursively retry.
				return stopCompatHTTPResponse(400, tc.rejection), nil
			})
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, tc.calls, calls)
			if calls == 2 {
				require.Equal(t, []string{"END"}, stops)
			} else {
				require.Empty(t, stops)
			}
			payload, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tc.rejection, string(payload))
		})
	}
}

func TestAnthropicStopSequenceCompatibilityPreservesReadError(t *testing.T) {
	want := errors.New("upstream read failed")
	resp, _, err := doAnthropicStopSequenceCompatibleRequest(context.Background(), nil, newAnthropicAPIKeyAccountForTest(), []byte(`{"stop_sequences":["END"]}`), func([]byte) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: &streamReadCloser{payload: []byte(`{"error":`), err: want}}, nil
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	require.ErrorIs(t, err, want)
	require.Equal(t, `{"error":`, string(payload))
}

type anthropicStopHTTPUpstream struct {
	responses []*http.Response
	bodies    [][]byte
	accounts  []int64
}

func (u *anthropicStopHTTPUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.bodies = append(u.bodies, body)
	u.accounts = append(u.accounts, accountID)
	if len(u.responses) == 0 {
		return nil, errors.New("unexpected extra request")
	}
	resp := u.responses[0]
	u.responses = u.responses[1:]
	return resp, nil
}

func (u *anthropicStopHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestGatewayAnthropicStopSequenceCompatibility(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"}],"max_tokens":256,"stream":%t,"stop_sequences":["END"],"temperature":0.3}`, stream))
			rejection := `{"type":"error","error":{"type":"invalid_request_error","message":"` + anthropicStopSequencesUnsupported + `"}}`
			response := `{"type":"message","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Hello END hidden"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":9}}`
			if stream {
				response = anthropicStopFixtureSSE("Hello E", "ND hidden")
			}
			upstream := &anthropicStopHTTPUpstream{responses: []*http.Response{stopCompatHTTPResponse(400, rejection), stopCompatHTTPResponse(200, response)}}
			svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream, rateLimitService: &RateLimitService{}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			account := newAnthropicAPIKeyAccountForTest()
			result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, account, body, "claude-sonnet-4-20250514", "claude-sonnet-4-20250514", stream, time.Now())
			require.NoError(t, err)
			require.Equal(t, 200, rec.Code)
			require.Len(t, upstream.bodies, 2)
			require.Equal(t, []int64{account.ID, account.ID}, upstream.accounts)
			require.True(t, gjson.GetBytes(upstream.bodies[0], "stop_sequences").Exists())
			require.False(t, gjson.GetBytes(upstream.bodies[1], "stop_sequences").Exists())
			require.Equal(t, "hello", gjson.GetBytes(upstream.bodies[1], "messages.0.content").String())
			require.Equal(t, 0.3, gjson.GetBytes(upstream.bodies[1], "temperature").Float())
			require.Equal(t, 9, result.Usage.OutputTokens)
			require.NotContains(t, rec.Body.String(), "hidden")
			if !stream {
				require.Equal(t, "stop_sequence", gjson.Get(rec.Body.String(), "stop_reason").String())
				require.Equal(t, "Hello ", gjson.Get(rec.Body.String(), "content.0.text").String())
			}
		})
	}
}

func TestOpenAINativeAnthropicStopSequenceCompatibility(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"k3","max_tokens":128,"stream":%t,"messages":[{"role":"user","content":"hello"}],"stop_sequences":["END"]}`, stream))
			rejection := `{"type":"error","error":{"message":"` + anthropicStopSequencesUnsupported + `"}}`
			response := `{"type":"message","model":"k3","content":[{"type":"text","text":"Hello END hidden"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":9}}`
			if stream {
				response = anthropicStopFixtureSSE("Hello E", "ND hidden")
			}
			upstream := &anthropicStopHTTPUpstream{responses: []*http.Response{stopCompatHTTPResponse(400, rejection), stopCompatHTTPResponse(200, response)}}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			account := nativeAnthropicTestAccount()
			result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
			require.NoError(t, err)
			require.Len(t, upstream.bodies, 2)
			require.Equal(t, []int64{account.ID, account.ID}, upstream.accounts)
			require.Equal(t, 9, result.Usage.OutputTokens)
			require.NotContains(t, rec.Body.String(), "hidden")
			require.Contains(t, rec.Body.String(), "stop_sequence")
		})
	}
}

func TestForwardAsAnthropicResponsesStopSequenceCompatibility(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"public-model","max_tokens":128,"stream":%t,"messages":[{"role":"user","content":"hello"}],"stop_sequences":["END"]}`, stream))
			sse := strings.Join([]string{
				`data: {"type":"response.created","response":{"id":"resp_stop","object":"response","model":"gpt-4.1","status":"in_progress","output":[]}}`, "",
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"message_stop","role":"assistant","status":"in_progress","content":[]}}`, "",
				`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello E"}`, "",
				`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"ND hidden"}`, "",
				`data: {"type":"response.completed","response":{"id":"resp_stop","object":"response","model":"gpt-4.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello END hidden"}]}],"usage":{"input_tokens":12,"output_tokens":9}}}`, "", "",
			}, "\n")
			upstream := &anthropicStopHTTPUpstream{responses: []*http.Response{stopCompatHTTPResponse(200, sse)}}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			account := rawChatCompletionsTestAccount()
			account.Credentials["model_mapping"] = map[string]any{"public-model": "gpt-4.1"}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
			require.NoError(t, err)
			require.Len(t, upstream.bodies, 1)
			require.False(t, gjson.GetBytes(upstream.bodies[0], "stop_sequences").Exists())
			require.Equal(t, "gpt-4.1", gjson.GetBytes(upstream.bodies[0], "model").String())
			require.Equal(t, "public-model", result.Model)
			require.Equal(t, 9, result.Usage.OutputTokens)
			require.NotContains(t, rec.Body.String(), "hidden")
			require.NotContains(t, rec.Body.String(), "gpt-4.1")
			require.Contains(t, rec.Body.String(), "stop_sequence")
		})
	}
}

func TestGatewayAnthropicNativeStopSupportIsUnchanged(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stop_sequences":["END"]}`)
	response := `{"type":"message","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"native response"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":9},"extension":"unchanged"}`
	upstream := &anthropicStopHTTPUpstream{responses: []*http.Response{stopCompatHTTPResponse(200, response)}}
	svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream, rateLimitService: &RateLimitService{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	_, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, newAnthropicAPIKeyAccountForTest(), body, "claude-sonnet-4-20250514", "claude-sonnet-4-20250514", false, time.Now())
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Equal(t, "END", gjson.GetBytes(upstream.bodies[0], "stop_sequences.0").String())
	require.Equal(t, response, rec.Body.String())
}

func TestAnthropicStopSequencesDoNotBreakTokenCounting(t *testing.T) {
	plain := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`)
	withStop := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}],"stop_sequences":["END"]}`)
	account := rawChatCompletionsTestAccount()
	base, err := prepareOpenAIInputTokensCountRequest(plain, account, "")
	require.NoError(t, err)
	prepared, err := prepareOpenAIInputTokensCountRequest(withStop, account, "")
	require.NoError(t, err)
	require.Equal(t, base, prepared)
	baseCount, err := estimateAnthropicCountTokensLocally(plain)
	require.NoError(t, err)
	count, err := estimateAnthropicCountTokensLocally(withStop)
	require.NoError(t, err)
	require.Equal(t, baseCount, count)
}

func TestOpenAIMessagesWebSearchStopSequenceCompatibility(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			account := rawChatCompletionsTestAccount()
			account.Extra = map[string]any{OpenAIWebSearchModeKey: OpenAIWebSearchModeOnly}
			body := []byte(fmt.Sprintf(`{"model":"gpt-4.1","max_tokens":128,"stream":%t,"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305","name":"web_search"}],"stop_sequences":["END"]}`, stream))
			upstream := &anthropicStopHTTPUpstream{responses: []*http.Response{stopCompatHTTPResponse(200, messagesWebSearchResponsesSSE("Verified search END hidden"))}}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			result, err := svc.ForwardAsAnthropic(WithOpenAIMessagesWebSearch(context.Background(), account.ID, "gpt-4.1"), c, account, body, "", "")
			require.NoError(t, err)
			require.Equal(t, 1, result.WebSearchCalls)
			require.Len(t, upstream.bodies, 1)
			require.Contains(t, rec.Body.String(), "web_search_tool_result")
			require.Contains(t, rec.Body.String(), "Verified search ")
			require.NotContains(t, rec.Body.String(), "hidden")
			require.Contains(t, rec.Body.String(), "stop_sequence")
		})
	}
}

func TestAnthropicStopSequenceCompatibilityHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	resp, stops, err := doAnthropicStopSequenceCompatibleRequest(ctx, nil, newAnthropicAPIKeyAccountForTest(), []byte(`{"stop_sequences":["END"]}`), func([]byte) (*http.Response, error) {
		calls++
		cancel()
		return stopCompatHTTPResponse(400, `{"error":{"message":"`+anthropicStopSequencesUnsupported+`"}}`), nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
	require.Empty(t, stops)
	_ = resp.Body.Close()
}

func TestAnthropicStopSequencePersistsAcrossPermittedOutputRetry(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	w, restore := installAnthropicStopSequenceWriter(c, []string{"END"}, true)
	_, err := w.WriteString("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"Hello END hidden\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	require.NoError(t, err)
	restore()
	c.Request = c.Request.WithContext(WithOpenAIStreamRetryAccount(c.Request.Context(), 201))
	w, restore = installAnthropicStopSequenceWriter(c, []string{"END"}, true)
	defer restore()
	_, err = w.WriteString(anthropicStopFixtureSSE("retried hidden answer"))
	require.NoError(t, err)
	require.NoError(t, w.finish())
	require.NotContains(t, rec.Body.String(), "hidden")
	require.Contains(t, rec.Body.String(), `"stop_reason":"stop_sequence"`)
}

func TestGatewayAnthropicStopSequenceDisconnectPreservesUsage(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hello"}],"stop_sequences":["END"]}`)
	upstream := &anthropicStopHTTPUpstream{responses: []*http.Response{
		stopCompatHTTPResponse(400, `{"error":{"message":"`+anthropicStopSequencesUnsupported+`"}}`),
		stopCompatHTTPResponse(200, anthropicStopFixtureSSE("hello END hidden")),
	}}
	svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream, rateLimitService: &RateLimitService{}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}
	result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, newAnthropicAPIKeyAccountForTest(), body, "claude-sonnet-4-20250514", "claude-sonnet-4-20250514", true, time.Now())
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, result.ClientDisconnect)
	require.Equal(t, 9, result.Usage.OutputTokens)
}

func TestAnthropicStopSequenceWriterCRLFAndFinalFrame(t *testing.T) {
	for _, trailingBoundary := range []bool{false, true} {
		t.Run(fmt.Sprint(trailingBoundary), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			w, restore := installAnthropicStopSequenceWriter(c, []string{"END"}, true)
			defer restore()
			body := strings.ReplaceAll(anthropicStopFixtureSSE("hello END hidden"), "\n", "\r\n")
			if !trailingBoundary {
				body = strings.TrimRight(body, "\r\n")
			}
			_, err := w.WriteString(body)
			require.NoError(t, err)
			require.NoError(t, w.finish())
			require.NotContains(t, rec.Body.String(), "hidden")
			events := anthropicStopEvents(t, rec.Body.String())
			require.Equal(t, "message_stop", events[len(events)-1].Get("type").String())
		})
	}
}

func TestAnthropicStopSequenceWriterPreservesError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			w, restore := installAnthropicStopSequenceWriter(c, []string{"END"}, stream)
			defer restore()
			body := `{"type":"error","error":{"type":"api_error","message":"END is in an error"}}`
			if stream {
				body = "event: error\ndata: " + body + "\n\n"
			} else {
				c.Status(http.StatusBadRequest)
			}
			_, err := w.WriteString(body)
			require.NoError(t, err)
			require.NoError(t, w.finish())
			require.Equal(t, body, rec.Body.String())
		})
	}
}
