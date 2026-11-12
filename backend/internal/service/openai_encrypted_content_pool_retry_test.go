package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const encryptedPoolRetryFailure = `{"type":"response.failed","response":{"id":"rejected-attempt","status":"failed","error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"Encrypted content rejected: private-ciphertext"}}}`

func TestOpenAIEncryptedContentPoolRetryStream(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for _, event := range []string{encryptedPoolRetryFailure, `{"type":"error","error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"Encrypted content rejected: private-ciphertext"}}`} {
			t.Run(path+"/"+gjson.Get(event, "type").String(), func(t *testing.T) {
				account := newConfiguredStreamRetryAccount()
				account.Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusBadRequest)}
				c, rec := newOpenAIConfiguredRetryContext()
				policy := openAIEncryptedContentRecoveryPolicy{body: []byte(`{"input":[{"type":"reasoning","encrypted_content":"private-ciphertext","summary":[]}]}`)}
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), openAIEncryptedContentRecoveryContextKey{}, policy))
				body := configuredRetryEvent(`{"type":"response.created","response":{"id":"rejected-attempt"}}`) + configuredRetryEvent(event)
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"encrypted-attempt"}}, Body: io.NopCloser(strings.NewReader(body))}
				err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, account, path)
				var retry *UpstreamFailoverError
				require.ErrorAs(t, err, &retry)
				require.True(t, retry.RetryableOnSameAccount)
				require.False(t, retry.ShouldRetryNextAccount())
				require.False(t, retry.ShouldReportAccountScheduleFailure())
				require.Equal(t, http.StatusBadRequest, retry.StatusCode)
				require.Contains(t, string(retry.ResponseBody), "invalid_encrypted_content")
				require.NotContains(t, string(retry.ResponseBody), "private-ciphertext")
				require.Equal(t, "encrypted-attempt", retry.ResponseHeaders.Get("x-request-id"))
				require.Empty(t, rec.Body.String(), "failed attempt metadata must stay private")
				require.False(t, IsResponseCommitted(c))
				require.False(t, takeOpenAIStreamInvalidEncryptedContent(c), "configured retries must not poison lineage and strip the next attempt")
			})
		}
	}
}

func TestOpenAIEncryptedContentPoolRetryPolicy(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		payload string
		change  func(*Account)
		want    bool
	}{
		{name: "http_400", status: 400, payload: `{"error":{"code":"invalid_encrypted_content"}}`, want: true},
		{name: "sse_400", payload: encryptedPoolRetryFailure, want: true},
		{name: "sse_unknown_status_502", payload: encryptedSSERetryFailure, want: true},
		{name: "http_502", status: 502, payload: `{"error":{"code":"invalid_encrypted_content"}}`, want: true},
		{name: "signature_alias", payload: `{"type":"error","code":"thinking_signature_invalid","message":"The encrypted content could not be verified because it could not be decrypted or parsed"}`, want: true},
		{name: "signature_without_decrypt_evidence", payload: `{"type":"error","code":"thinking_signature_invalid","message":"invalid signature"}`},
		{name: "plain_message", payload: `{"type":"response.failed","response":{"error":{"code":"invalid_value","message":"invalid_encrypted_content"}}}`},
		{name: "other_invalid_request", payload: `{"type":"error","error":{"code":"invalid_value","type":"invalid_request_error"}}`},
		{name: "success_event", payload: `{"type":"response.completed","response":{"error":{"code":"invalid_encrypted_content"}}}`},
		{name: "malformed", payload: `{"type":"error","code":"invalid_encrypted_content"`},
		{name: "non_pool", payload: encryptedPoolRetryFailure, change: func(a *Account) { a.Credentials["pool_mode"] = false }},
		{name: "zero_budget", payload: encryptedPoolRetryFailure, change: func(a *Account) { a.Credentials["pool_mode_retry_count"] = float64(0) }},
		{name: "unconfigured_400", payload: encryptedPoolRetryFailure, change: func(a *Account) { a.Credentials["pool_mode_retry_status_codes"] = []any{float64(502)} }},
		{name: "unconfigured_502", payload: encryptedSSERetryFailure, change: func(a *Account) { a.Credentials["pool_mode_retry_status_codes"] = []any{float64(400)} }},
		{name: "other_platform", payload: encryptedPoolRetryFailure, change: func(a *Account) { a.Platform = PlatformGrok }},
		{name: "oauth", payload: encryptedPoolRetryFailure, change: func(a *Account) { a.Type = AccountTypeOAuth }},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := newConfiguredStreamRetryAccount()
			account.Credentials["pool_mode_retry_status_codes"] = []any{float64(400), float64(502)}
			if test.change != nil {
				test.change(account)
			}
			require.Equal(t, test.want, openAIConfiguredEncryptedContentRetry(account, test.status, []byte(test.payload)))
		})
	}
	require.False(t, openAIConfiguredEncryptedContentRetry(nil, 400, []byte(encryptedPoolRetryFailure)))
}

func TestOpenAIEncryptedContentPoolRetryDoesNotReplayCommittedOutput(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for _, output := range []string{"text", "tool"} {
			t.Run(fmt.Sprintf("%s/%s", path, output), func(t *testing.T) {
				account := newConfiguredStreamRetryAccount()
				// A configured 400 retry must not turn on the existing whole-attempt
				// 502 buffer or replay semantic output already sent downstream.
				account.Credentials["pool_mode_retry_status_codes"] = []any{float64(400)}
				c, _ := newOpenAIConfiguredRetryContext()
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(configuredRetryOutput(output) + configuredRetryEvent(encryptedPoolRetryFailure)))}
				err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, account, path)
				var retry *UpstreamFailoverError
				require.NotErrorAs(t, err, &retry)
				require.True(t, c.Writer.Written())
			})
		}
	}
}

func TestOpenAIEncryptedContentPoolRetryCanceled(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			account := newConfiguredStreamRetryAccount()
			account.Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusBadRequest)}
			c, _ := newOpenAIConfiguredRetryContext()
			ctx, cancel := context.WithCancel(c.Request.Context())
			c.Request = c.Request.WithContext(ctx)
			cancel()
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(configuredRetryEvent(encryptedPoolRetryFailure)))}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, account, path)
			var retry *UpstreamFailoverError
			require.NotErrorAs(t, err, &retry)
		})
	}
}

func TestOpenAIEncryptedContentPoolRetryWSHTTPBridge(t *testing.T) {
	for _, failure := range []struct {
		name, body, contentType string
		status                  int
	}{
		{"http", `{"error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"rejected"}}`, "application/json", 400},
		{"sse_failed", configuredRetryEvent(encryptedPoolRetryFailure), "text/event-stream", 200},
		{"sse_error", configuredRetryEvent(`{"type":"error","error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"rejected"}}`), "text/event-stream", 200},
	} {
		for _, turn := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/turn=%d", failure.name, turn), func(t *testing.T) {
				account := newConfiguredStreamRetryAccount()
				account.Status, account.Schedulable = StatusActive, true
				account.Credentials["pool_mode_retry_status_codes"] = []any{float64(400)}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: failure.status,
					Header: http.Header{"Content-Type": {failure.contentType}}, Body: io.NopCloser(strings.NewReader(failure.body))}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				c, _ := newOpenAIConfiguredRetryContext()
				payload := []byte(`{"type":"response.create","model":"gpt-5","input":"continue"}`)
				var writes [][]byte
				_, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "sk-test", payload, len(payload), "gpt-5", "", "", "", "", turn, func(message []byte) error {
					writes = append(writes, append([]byte(nil), message...))
					return nil
				})
				var retry *UpstreamFailoverError
				if turn == 1 {
					require.ErrorAs(t, err, &retry)
					require.Equal(t, OpenAIEncryptedContentRetryReason, retry.Reason)
					require.True(t, retry.RetryableOnSameAccount)
					require.False(t, retry.ShouldRetryNextAccount())
					require.Empty(t, writes)
				} else {
					require.NotErrorAs(t, err, &retry, "later turns must retain the existing replay boundary")
					require.NotEmpty(t, writes, "deliver the terminal on the existing connection")
				}
			})
		}
	}
}
