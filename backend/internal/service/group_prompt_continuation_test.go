package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Inspect the actual Responses HTTP payload, including a continuation that
// changes the client alias while both aliases map to the same upstream model.
func TestGroupPromptMappedMessagesContinuationKeepsCurrentSystem(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, structured := range []bool{false, true} {
		name := "string"
		if structured {
			name = "structured"
		}
		t.Run(name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{
				httpUpstream: upstream,
				cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
			}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.openai.com/v1",
					"model_mapping": map[string]any{"client-a": "gpt-5.3-codex", "client-b": "gpt-5.3-codex"}},
			}
			promptConfig := groupprompt.Config{Enabled: true, Content: "GROUP-FALLBACK", ModelPrompts: map[string]string{
				"client-a": "PROMPT-A", "client-b": "", // Empty selects the complete bundled prompt.
			}}
			for turn, model := range []string{"client-a", "client-b"} {
				var system any = "CLIENT-SYSTEM"
				if structured {
					system = []map[string]any{{"type": "text", "text": "CLIENT-SYSTEM", "cache_control": map[string]string{"type": "ephemeral"}}}
				}
				messages := []map[string]string{{"role": "user", "content": "first"}}
				if turn == 1 {
					messages = append(messages, map[string]string{"role": "assistant", "content": "ok"}, map[string]string{"role": "user", "content": "second"})
				}
				body, err := json.Marshal(map[string]any{"model": model, "system": system, "max_tokens": 16, "messages": messages, "stream": false})
				require.NoError(t, err)
				body, err = groupprompt.Prepend(body, "messages", promptConfig.Prompt(model))
				require.NoError(t, err)
				responseID := []string{"resp_first", "resp_second"}[turn]
				upstream.resp = openAICompatSSECompletedResponse(responseID, "gpt-5.3-codex")
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "stable-cache-key", "")
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, "gpt-5.3-codex", gjson.GetBytes(upstream.lastBody, "model").String())
				var systemTexts []string
				for _, item := range gjson.GetBytes(upstream.lastBody, "input").Array() {
					if item.Get("role").String() == "developer" || item.Get("role").String() == "system" {
						var parts []string
						for _, part := range item.Get("content").Array() {
							parts = append(parts, part.Get("text").String())
						}
						systemTexts = append(systemTexts, strings.Join(parts, "\n\n"))
					}
				}
				want := promptConfig.Prompt(model) + "\n\nCLIENT-SYSTEM"
				require.True(t, slices.Contains(systemTexts, want), "the complete current group and client system must reach the upstream (prompt bytes: %d)", len(promptConfig.Prompt(model)))
				if turn == 1 {
					require.Equal(t, "resp_first", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
					require.NotContains(t, systemTexts, "PROMPT-A\n\nCLIENT-SYSTEM")
					require.Equal(t, int64(3), gjson.GetBytes(upstream.lastBody, "input.#").Int()) // Guard, current system, latest user.
					require.Equal(t, "second", gjson.GetBytes(upstream.lastBody, "input.2.content.0.text").String())
				}
			}
		})
	}
}

func TestGroupPromptContinuationKeepsSystemAndToolPairs(t *testing.T) {
	items := []apicompat.ResponsesInputItem{
		{Type: "message", Role: "system", Content: json.RawMessage(`"GROUP-SYSTEM"`)},
		{Type: "message", Role: "developer", Content: json.RawMessage(`"CLIENT-DEVELOPER"`)},
		{Type: "message", Role: "user", Content: json.RawMessage(`"old user"`)},
		{Type: "function_call", CallID: "call_one", Name: "lookup", Arguments: `{}`},
		{Type: "function_call", CallID: "call_two", Name: "lookup", Arguments: `{}`},
		{Type: "function_call_output", CallID: "call_one", Output: "one"},
		{Type: "function_call_output", CallID: "call_two", Output: "two"},
		{Type: "message", Role: "user", Content: json.RawMessage(`"latest user"`)},
	}
	input, err := json.Marshal(items)
	require.NoError(t, err)
	req := &apicompat.ResponsesRequest{Input: input}
	trimAnthropicCompatResponsesInputToLatestTurn(req)
	want := append(append([]apicompat.ResponsesInputItem(nil), items[:2]...), items[3:]...)
	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	require.JSONEq(t, string(wantJSON), string(req.Input))
	// Repeating the operation must preserve every instruction exactly once.
	firstTrim := string(req.Input)
	trimAnthropicCompatResponsesInputToLatestTurn(req)
	require.Equal(t, firstTrim, string(req.Input))
}
