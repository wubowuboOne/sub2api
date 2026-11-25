package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIAgentHTTP502GatewayMessage(t *testing.T) {
	const unavailable = "Upstream service temporarily unavailable"
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"plain", "  error code: 502\n", "error code: 502"},
		{"json", `{"error":{"message":"Upstream service temporarily unavailable","type":"upstream_error"}}`, unavailable},
		{"json_reordered", ` { "error": { "type":"upstream_error", "message":"Upstream service temporarily unavailable" } } `, unavailable},
		{"unknown_message", `{"error":{"type":"upstream_error","message":"Upstream request failed"}}`, ""},
		{"different_type", `{"error":{"type":"invalid_request_error","message":"Upstream service temporarily unavailable"}}`, ""},
		{"provider_code", `{"error":{"type":"upstream_error","message":"Upstream service temporarily unavailable","code":"policy_violation"}}`, ""},
		{"response_state", `{"error":{"type":"upstream_error","message":"Upstream service temporarily unavailable"},"id":"resp_fixture"}`, ""},
		{"ambiguous_type", `{"error":{"type":"upstream_error","message":"Upstream service temporarily unavailable","type":"invalid_request_error"}}`, ""},
		{"trailing_value", `{"error":{"type":"upstream_error","message":"Upstream service temporarily unavailable"}}[]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, openAIAgentHTTP502GatewayMessage([]byte(tc.body)))
		})
	}
}

func TestOpenAIAgentHTTP502Replayable(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"stateless", `{"store":false,"input":[{"type":"agent_message"}]}`, true},
		{"client_catalog", `{"store":false,"background":false,"tool_choice":"auto","tools":[{"type":"tool_search","execution":"client"}],"input":[{"type":"agent_message"},{"type":"additional_tools","tools":[{"type":"namespace","tools":[{"type":"custom"}]}]},{"type":"tool_search_output","execution":"client","tools":[{"type":"function"}]}]}`, true},
		{"default_store", `{"input":[]}`, false},
		{"ambiguous_store", `{"store":false,"store":true,"input":[]}`, false},
		{"stored_response", `{"store":false,"previous_response_id":"resp_fixture","input":[]}`, false},
		{"conversation", `{"store":false,"conversation":{"id":"conv_fixture"},"input":[]}`, false},
		{"background", `{"store":false,"background":true,"input":[]}`, false},
		{"stored_prompt", `{"store":false,"prompt":{"id":"pmpt_fixture"},"input":[]}`, false},
		{"item_reference", `{"store":false,"input":[{"type":"item_reference","id":"msg_fixture"}]}`, false},
		{"hosted_choice", `{"store":false,"tool_choice":{"type":"web_search"},"input":[]}`, false},
		{"mcp_approval", `{"store":false,"input":[{"type":"mcp_approval_response","approve":true}]}`, false},
		{"unknown_tool", `{"store":false,"tools":[{"type":"future_tool"}],"input":[]}`, false},
		{"search_execution_unknown", `{"store":false,"tools":[{"type":"tool_search"}],"input":[]}`, false},
		{"catalog_with_hosted_tool", `{"store":false,"input":[{"type":"tool_search_output","execution":"client","tools":[{"type":"namespace","tools":[{"type":"mcp"}]}]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, openAIAgentHTTP502Replayable([]byte(tc.body)))
		})
	}
}
