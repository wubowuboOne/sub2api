package service

import (
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAIConfiguredRetryLargeOutput(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/success=%t", path, success), func(t *testing.T) {
				payload := strings.Repeat("large-output-", (9*1024*1024)/13)
				body := configuredRetryEvent(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_large","type":"message","role":"assistant","content":[]}}`) + configuredRetryEvent(fmt.Sprintf(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":%q}`, payload))
				if success {
					body += configuredRetryEvent(`{"type":"response.completed","response":{"id":"resp_large","status":"completed","output":[],"usage":{"input_tokens":19,"output_tokens":7,"input_tokens_details":{"cached_tokens":17}}}}`)
				} else {
					body += configuredRetryEvent(messagesBPSFailure)
				}
				c, rec := newOpenAIConfiguredRetryContext()
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				svc := &OpenAIGatewayService{cfg: &config.Config{}}
				account := newConfiguredStreamRetryAccount()
				var usage *OpenAIUsage
				var err error
				switch path {
				case "native":
					r, e := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
					err = e
					if r != nil {
						usage = r.usage
					}
				case "passthrough":
					r, e := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
					err = e
					if r != nil {
						usage = r.usage
					}
				default:
					var r *OpenAIForwardResult
					if path == "messages" {
						r, err = svc.handleAnthropicStreamingResponse(resp, c, account, "model", "model", "model", time.Now())
					} else {
						r, err = svc.handleChatStreamingResponse(resp, c, account, "model", "model", "model", time.Now(), 32)
					}
					if r != nil {
						usage = &r.Usage
					}
				}
				if !success {
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.True(t, failover.RetryableOnSameAccount)
					require.Contains(t, string(failover.ResponseBody), "Upstream request failed")
					require.Empty(t, rec.Body.String())
					return
				}
				require.NoError(t, err)
				require.Contains(t, rec.Body.String(), payload)
				require.NotNil(t, usage)
				require.Equal(t, 19, usage.InputTokens)
				require.Equal(t, 7, usage.OutputTokens)
				require.Equal(t, 17, usage.CacheReadInputTokens)
			})
		}
	}
}

func TestOpenAIConfiguredRetryLargeOpaquePreamble(t *testing.T) {
	for _, path := range []string{"native", "passthrough"} {
		for _, kind := range []string{"reasoning", "compaction"} {
			t.Run(path+"/"+kind, func(t *testing.T) {
				body := configuredRetryEvent(fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"id":"opaque_large","type":%q,"encrypted_content":%q,"summary":[]}}`, kind, strings.Repeat("x", 9*1024*1024))) + configuredRetryEvent(messagesBPSFailure)
				c, rec := newOpenAIConfiguredRetryContext()
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Contains(t, string(failover.ResponseBody), "Upstream request failed")
				require.Empty(t, rec.Body.String())
			})
		}
	}
}

func TestOpenAIConfiguredRetryStageScope(t *testing.T) {
	for _, policy := range []string{"pool", "non_pool", "zero", "not_502", "oauth", "grok"} {
		t.Run(policy, func(t *testing.T) {
			a := newConfiguredStreamRetryAccount()
			switch policy {
			case "non_pool":
				a.Credentials["pool_mode"] = false
			case "zero":
				a.Credentials["pool_mode_retry_count"] = float64(0)
			case "not_502":
				a.Credentials["pool_mode_retry_status_codes"] = []any{float64(429)}
			case "oauth":
				a.Type = AccountTypeOAuth
			case "grok":
				a.Platform = PlatformGrok
			}
			s := (&OpenAIGatewayService{}).newOpenAIStreamOutputStage(a)
			defer s.Close()
			want := int64(openAIFirstOutputStageMaxBytes)
			if policy == "pool" {
				want = config.DefaultOpenAIStreamRetryMaxBufferBytes
			}
			require.Equal(t, want, s.limit)
		})
	}
}

func TestOpenAIConfiguredRetryScannerBoundaryAndGuardRelease(t *testing.T) {
	var active atomic.Bool
	active.Store(true)
	split := openAIFirstOutputDynamicScanLines(&active, 64)
	for _, delimiter := range []string{"\n", "\r\n"} {
		data := []byte(strings.Repeat("x", 64))
		_, token, err := split(data, false)
		require.NoError(t, err)
		require.Nil(t, token)
		if delimiter == "\r\n" {
			_, token, err = split(append(data, '\r'), false)
			require.NoError(t, err)
			require.Nil(t, token)
		}
		_, token, err = split([]byte(string(data)+delimiter), false)
		require.NoError(t, err)
		require.Len(t, token, 64)
		_, _, err = split([]byte(strings.Repeat("x", 65)+delimiter), false)
		require.ErrorIs(t, err, errOpenAIFirstOutputScannerLimit)
	}
	active.Store(false)
	_, token, err := split([]byte(strings.Repeat("x", 65)+"\n"), false)
	require.NoError(t, err)
	require.Len(t, token, 65)
}

func TestOpenAIConfiguredRetryPreservesPostSemanticLineLimit(t *testing.T) {
	c, rec := newOpenAIConfiguredRetryContext()
	payload := strings.Repeat("x", 2*1024*1024)
	body := configuredRetryOutput("text") + configuredRetryEvent(fmt.Sprintf(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":%q}`, payload)) + configuredRetryEvent(`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIStreamRetryMaxLineBytes: 1024 * 1024}}}
	err := runConfiguredRetryStream(svc, c, resp, newConfiguredStreamRetryAccount(), "native")
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), payload)
}

func TestOpenAIConfiguredRetrySizeMarkerRequiresActiveBuffer(t *testing.T) {
	for _, retries := range []int{0, 3} {
		t.Run(fmt.Sprint(retries), func(t *testing.T) {
			a := newConfiguredStreamRetryAccount()
			a.Credentials["pool_mode_retry_count"] = float64(retries)
			c, rec := newOpenAIConfiguredRetryContext()
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", 2*1024*1024) + "\n\n"))}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1024 * 1024}}}, c, resp, a, "native")
			var f *UpstreamFailoverError
			require.ErrorAs(t, err, &f)
			require.True(t, f.SafeToFailoverAfterWrite)
			require.Equal(t, retries > 0, f.ConfiguredStreamRetry)
			require.Empty(t, rec.Body.String())
		})
	}
}
