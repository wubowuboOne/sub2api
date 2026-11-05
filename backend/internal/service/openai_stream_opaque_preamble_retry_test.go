package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOpaquePreambleBPSFailureRemainsRetryable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"reasoning", "compaction"} {
		for _, size := range []int{32, 1536 * 1024} {
			for _, path := range []string{"native", "passthrough", "messages"} {
				t.Run(fmt.Sprintf("%s/%s/%d", path, kind, size), func(t *testing.T) {
					svc := &OpenAIGatewayService{cfg: &config.Config{}}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					item := fmt.Sprintf(`{"id":"opaque_pending","type":%q,"encrypted_content":%q,"summary":[]}`, kind, strings.Repeat("x", size))
					body := `data: {"type":"response.created","response":{"id":"resp_pending"}}` + "\n\n"
					for _, event := range []string{"response.output_item.added", "response.output_item.done"} {
						body += fmt.Sprintf(`data: {"type":%q,"output_index":0,"item":%s}`+"\n\n", event, item)
					}
					body += "data: " + messagesBPSFailure + "\n\n"
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": []string{"1451c097-042c-45c1-8fc0-cb10b4a24ad2"}}, Body: io.NopCloser(strings.NewReader(body))}
					account := &Account{ID: 59327, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
						"pool_mode": true, "pool_mode_retry_count": float64(10),
						"pool_mode_retry_status_codes": []any{float64(http.StatusBadGateway)},
					}}
					err := runOpenAIOpaquePreambleStream(svc, c, resp, account, path)
					var failoverErr *UpstreamFailoverError
					require.ErrorAs(t, err, &failoverErr)
					require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
					require.True(t, failoverErr.RetryableOnSameAccount)
					require.False(t, c.Writer.Written(), "opaque attempt state must remain private until usable output or success")
					require.Empty(t, rec.Body.String())
				})
			}
		}
	}
}

func TestOpenAIOpaquePreambleCommitsOnSuccessAndPreservesOutputBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"reasoning", "compaction"} {
		for _, path := range []string{"native", "passthrough", "messages"} {
			for _, completed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/success=%t", path, kind, completed), func(t *testing.T) {
					logSink, restore := captureStructuredLog(t)
					defer restore()
					svc := &OpenAIGatewayService{cfg: &config.Config{}}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					item := fmt.Sprintf(`{"id":"rs_kept","type":%q,"encrypted_content":"opaque_kept","summary":[]}`, kind)
					body := `data: {"type":"response.created","response":{"id":"resp_success","status":"in_progress"}}` + "\n\n"
					body += fmt.Sprintf(`data: {"type":"response.output_item.added","output_index":0,"item":%s}`+"\n\n", item)
					if completed {
						body += fmt.Sprintf(`data: {"type":"response.completed","response":{"id":"resp_success","status":"completed","output":[%s],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n", item)
					} else {
						body += `data: {"type":"response.output_text.delta","output_index":1,"delta":"answer already delivered"}` + "\n\ndata: " + messagesBPSFailure + "\n\n"
					}
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
					account := &Account{ID: 59327, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
						"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(http.StatusBadGateway)},
					}}
					err := runOpenAIOpaquePreambleStream(svc, c, resp, account, path)
					if completed {
						require.NoError(t, err)
						if path != "messages" {
							require.Contains(t, rec.Body.String(), "opaque_kept")
							require.Contains(t, rec.Body.String(), "response.completed")
						} else {
							require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_start"))
							require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_stop"))
						}
					} else {
						var failoverErr *UpstreamFailoverError
						require.Error(t, err)
						require.NotErrorAs(t, err, &failoverErr)
						require.Contains(t, rec.Body.String(), "answer already delivered")
						require.True(t, logSink.ContainsMessage("openai.same_account_retry_suppressed"))
						require.True(t, logSink.ContainsField("first_output_item_type"))
					}
				})
			}
		}
	}
}

func TestOpenAIPassthroughOpaquePreambleHasBoundedStaging(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	line := `data: {"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"` + strings.Repeat("x", openAIFirstOutputStageMaxBytes/2) + `","summary":[]}}` + "\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(line + line))}
	err := runOpenAIOpaquePreambleStream(svc, c, resp, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "passthrough")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Contains(t, string(failoverErr.ResponseBody), "staging limit")
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIOpaquePreamblePreservesUpstreamProgress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"reasoning", "compaction"} {
		for _, path := range []string{"native", "passthrough"} {
			t.Run(path+"/"+kind, func(t *testing.T) {
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1}}}
				budget := &openAIHTTPBudget{progress: make(chan struct{})}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(context.WithValue(context.Background(), openAIHTTPBudgetKey{}, budget))
				reader, writer := io.Pipe()
				defer reader.Close()
				finished := make(chan struct{})
				progressBeforeOutput := make(chan bool, 1)
				go func() {
					defer close(finished)
					defer writer.Close()
					_, _ = fmt.Fprintf(writer, `data: {"type":"response.output_item.added","item":{"id":"opaque_pending","type":%q,"encrypted_content":"ciphertext","summary":[]}}`+"\n\n", kind)
					select {
					case <-budget.progress:
						progressBeforeOutput <- true
					case <-time.After(500 * time.Millisecond):
						progressBeforeOutput <- false
					}
					time.Sleep(1100 * time.Millisecond)
					_, _ = io.WriteString(writer, `data: {"type":"response.output_text.delta","delta":"answer"}`+"\n\n"+`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
				}()
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
				err := runOpenAIOpaquePreambleStream(svc, c, resp, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, path)
				_ = reader.Close()
				<-finished
				require.NoError(t, err, "opaque upstream progress must not become a first-output timeout")
				require.True(t, <-progressBeforeOutput, "the HTTP pre-output budget must stop before visible output arrives")
				require.Contains(t, rec.Body.String(), "answer")
			})
		}
	}
}

func TestOpenAIOpaquePreambleKeepsScannerFailureRetryable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"reasoning", "compaction"} {
		t.Run(kind, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 64 * 1024}}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			body := fmt.Sprintf(`data: {"type":"response.output_item.added","item":{"type":%q,"encrypted_content":"private_ciphertext","summary":[]}}`+"\n\n", kind)
			body += `data: {"type":"response.created","padding":"` + strings.Repeat("x", 128*1024) + `"}` + "\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
				"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(http.StatusBadGateway)},
			}}
			err := runOpenAIOpaquePreambleStream(svc, c, resp, account, "native")
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.True(t, failoverErr.RetryableOnSameAccount)
			require.False(t, c.Writer.Written(), "upstream progress must not commit staged output on a scanner error")
			require.Empty(t, rec.Body.String())
		})
	}
}

type openAIOpaquePartialWriteRecorder struct {
	*httptest.ResponseRecorder
}

func (w *openAIOpaquePartialWriteRecorder) Write(p []byte) (int, error) {
	n, _ := w.ResponseRecorder.Write(p[:len(p)/2])
	return n, io.ErrClosedPipe
}

func TestOpenAIOpaquePreamblePartialWriteDoesNotAuthorizeRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 64 * 1024}}}
	rec := &openAIOpaquePartialWriteRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := `data: {"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"private_ciphertext","summary":[]}}` + "\n\n"
	body += `data: {"type":"response.output_text.delta","delta":"answer"}` + "\n\n"
	body += `data: {"type":"response.created","padding":"` + strings.Repeat("x", 128*1024) + `"}` + "\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	err := runOpenAIOpaquePreambleStream(svc, c, resp, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "native")
	var failoverErr *UpstreamFailoverError
	require.Error(t, err)
	require.NotErrorAs(t, err, &failoverErr, "a partially committed attempt must never be replayed")
	require.Positive(t, c.Writer.Size())
}

func TestOpenAIOpaqueCompletedItemSurvivesThinSuccessfulTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"reasoning", "compaction"} {
		for _, path := range []string{"native", "passthrough", "messages"} {
			t.Run(path+"/"+kind, func(t *testing.T) {
				svc := &OpenAIGatewayService{cfg: &config.Config{}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				body := `data: {"type":"response.created","response":{"id":"resp_thin"}}` + "\n\n"
				body += fmt.Sprintf(`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"opaque_done","type":%q,"encrypted_content":"ciphertext","summary":[]}}`+"\n\n", kind)
				body += `data: {"type":"response.completed","response":{"id":"resp_thin","status":"completed","output":[]}}` + "\n\n"
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				err := runOpenAIOpaquePreambleStream(svc, c, resp, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, path)
				require.NoError(t, err, "a completed opaque item is not an empty response")
				if path != "messages" {
					require.Equal(t, 1, strings.Count(rec.Body.String(), "response.output_item.done"))
					require.Equal(t, 1, strings.Count(rec.Body.String(), "response.completed"))
					require.Contains(t, rec.Body.String(), "ciphertext")
				} else {
					require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_stop"))
				}
			})
		}
	}
}

func runOpenAIOpaquePreambleStream(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response, account *Account, path string) error {
	switch path {
	case "passthrough":
		_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6.1-sol")
		return err
	case "messages":
		_, err := svc.handleAnthropicStreamingResponse(resp, c, account, "gpt-6-astra", "gpt-6-astra", "gpt-6.1-sol", time.Now())
		return err
	default:
		_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6.1-sol")
		return err
	}
}
