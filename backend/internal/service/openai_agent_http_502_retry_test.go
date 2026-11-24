package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
