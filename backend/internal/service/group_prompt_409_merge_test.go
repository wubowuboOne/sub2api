//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Exercise the merge boundary: prompts chosen using the public model survive
// the explicit 409 omission path, its replay normalization and the send fence.
func TestGroupPrompt409OmissionPreservesMappedInstructions(t *testing.T) {
	for _, format := range []string{"responses", "messages"} {
		for _, mode := range []string{"group_default", "model_override", "bundled_override"} {
			t.Run(format+"/"+mode, func(t *testing.T) {
				const publicModel, upstreamModel = "claude-client-alias", "gpt-6-astra"
				policy := groupprompt.Config{Enabled: true, Content: "GROUP-DEFAULT", ModelPrompts: map[string]string{upstreamModel: "WRONG-UPSTREAM-PROMPT"}}
				want := policy.Content
				switch mode {
				case "model_override":
					policy.ModelPrompts[publicModel] = "PUBLIC-MODEL-PROMPT"
					want = "PUBLIC-MODEL-PROMPT"
				case "bundled_override":
					policy.ModelPrompts[publicModel] = ""
					want = groupprompt.DefaultPrompt
				}
				body := []byte(strings.TrimSuffix(issue715OpaqueBody, "]}") + `,{"role":"user","content":"continue with visible history"}],"instructions":"CLIENT-INSTRUCTIONS","store":false}`)
				var err error
				body, err = sjson.SetBytes(body, "model", publicModel)
				require.NoError(t, err)
				if format == "responses" {
					body, err = groupprompt.Prepend(body, format, policy.Prompt(publicModel))
					require.NoError(t, err)
				} else {
					inbound, err := groupprompt.Prepend([]byte(`{"model":"claude-client-alias","max_tokens":16,"system":"CLIENT-SYSTEM","messages":[{"role":"user","content":"hello"}]}`), format, policy.Prompt(publicModel))
					require.NoError(t, err)
					var anthropic apicompat.AnthropicRequest
					require.NoError(t, json.Unmarshal(inbound, &anthropic))
					converted, err := apicompat.AnthropicToResponses(&anthropic)
					require.NoError(t, err)
					var input []json.RawMessage
					require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(body, "input").Raw), &input))
					developer := json.RawMessage(gjson.GetBytes(converted.Input, "0").Raw)
					require.Equal(t, "developer", gjson.GetBytes(developer, "role").String())
					input = append([]json.RawMessage{developer}, input...)
					body, err = sjson.SetBytes(body, "input", input)
					require.NoError(t, err)
				}
				forwardBody, err := sjson.SetBytes(body, "model", upstreamModel)
				require.NoError(t, err)
				svc := &OpenAIGatewayService{cfg: &config.Config{}, settingService: NewSettingService(&codexPolicyMigrationRepoStub{values: map[string]string{SettingKeyOpenAI409OmitEncryptedHistoryEnabled: "true"}}, nil)}
				issue715LearnRoute(t, svc, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, issue715OpaqueBody)
				c, _ := newTurnStateTestContext(t, 715, "fixture-session")
				c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
				ctx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, body, "fixture-session", upstreamModel)
				next, clean, changed := svc.TryOmitOpenAI409History(ctx, c, body, "fixture-session", upstreamModel, openAIAgentRecoveryError("owner_unavailable"))
				require.True(t, changed)
				// Match the handler: preserve the mapped model and replace only input.
				wire, err := sjson.SetRawBytes(forwardBody, "input", []byte(gjson.GetBytes(clean, "input").Raw))
				require.NoError(t, err)
				require.Equal(t, upstreamModel, gjson.GetBytes(wire, "model").String())
				require.Equal(t, gjson.GetBytes(body, "instructions").String(), gjson.GetBytes(wire, "instructions").String())
				if format == "responses" {
					require.Equal(t, want+"\n\nCLIENT-INSTRUCTIONS", gjson.GetBytes(wire, "instructions").String())
				} else {
					require.JSONEq(t, gjson.GetBytes(body, "input.0").Raw, gjson.GetBytes(wire, "input.0").Raw)
					require.Equal(t, want+"\n\nCLIENT-SYSTEM", gjson.GetBytes(wire, "input.0.content.0.text").String())
				}
				require.NotContains(t, string(wire), "WRONG-UPSTREAM-PROMPT")
				require.NotContains(t, string(wire), "fixture-opaque-reference")
				require.Contains(t, string(wire), "Historical encrypted content omitted")
				require.Equal(t, "continue with visible history", gjson.GetBytes(wire, "input").Array()[len(gjson.GetBytes(wire, "input").Array())-1].Get("content").String())
				req, err := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", bytes.NewReader(wire))
				require.NoError(t, err)
				finish, err := svc.prepareOpenAIAgentRecoveryAttempt(next, &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, req)
				defer finish(false)
				require.NoError(t, err, "the full prompt and mapped model must pass the native send fence")
			})
		}
	}
}
