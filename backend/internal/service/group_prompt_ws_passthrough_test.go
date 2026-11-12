package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Exercise the actual passthrough ingress so merged admission hooks still see
// the original client frame, while every upstream turn gets its model's prefix.
func TestGroupPromptWSPassthroughPreservesClientAdmissionAcrossTurns(t *testing.T) {
	controlCtx, cancel := context.WithCancelCause(groupprompt.WithConfig(context.Background(), groupprompt.Config{
		Enabled: true,
		Content: "FIRST-GROUP-PROMPT",
		ModelPrompts: map[string]string{
			"client-first": "FIRST-GROUP-PROMPT", "client-second": "SECOND-GROUP-PROMPT",
			"gpt-5.1": "WRONG-UPSTREAM-PROMPT", "gpt-5.2": "WRONG-UPSTREAM-PROMPT",
		},
	}))
	defer cancel(context.Canceled)
	upstream := newStagedPassthroughConn()
	svc := newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream)
	type admission struct {
		turn                   int
		payload, clientPayload []byte
		model                  string
	}
	admissions := make(chan admission, 2)
	legacyAdmissions := make(chan int, 2)
	hooks := &OpenAIWSIngressHooks{
		InitialRequestModel: "client-first",
		APIKeyModelMapping:  true,
		MapRequestModel: func(_ int, model string) (string, error) {
			switch model {
			case "client-first":
				return "gpt-5.1", nil
			case "client-second":
				return "gpt-5.2", nil
			default:
				return model, nil
			}
		},
		BeforeRequestWithClientPayload: func(turn int, payload, clientPayload []byte, model string) error {
			admissions <- admission{turn, append([]byte(nil), payload...), append([]byte(nil), clientPayload...), model}
			return nil
		},
		BeforeRequest: func(turn int, _ []byte, _ string) error {
			legacyAdmissions <- turn
			return nil
		},
	}
	server, serverErr := startPassthroughLifecycleServer(t, controlCtx, svc, passthroughLifecycleAccount(), hooks)
	defer server.Close()
	clientConn := dialPassthroughLifecycleClientWithPayload(t, server,
		`{"type":"response.create","model":"client-first","instructions":"client first","input":[],"stream":false}`)
	defer func() { _ = clientConn.CloseNow() }()
	first := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	require.Equal(t, "gpt-5.1", gjson.GetBytes(first, "model").String())
	require.Equal(t, "FIRST-GROUP-PROMPT\n\nclient first", gjson.GetBytes(first, "instructions").String())
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	_, err := readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
	require.NoError(t, err)

	rawSecond := []byte(`{"type":"response.create","model":"client-second","instructions":"client second","input":[],"stream":false}`)
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, rawSecond)
	cancelWrite()
	require.NoError(t, err)
	second := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	require.Equal(t, "gpt-5.2", gjson.GetBytes(second, "model").String())
	instructions := gjson.GetBytes(second, "instructions").String()
	require.Equal(t, "SECOND-GROUP-PROMPT\n\nclient second", instructions)
	require.Equal(t, 1, strings.Count(instructions, "SECOND-GROUP-PROMPT"))
	select {
	case got := <-admissions:
		require.Equal(t, 2, got.turn)
		require.Equal(t, "client-second", got.model)
		require.Equal(t, rawSecond, got.clientPayload)
		require.Equal(t, instructions, gjson.GetBytes(got.payload, "instructions").String())
	case <-time.After(3 * time.Second):
		t.Fatal("client-aware admission hook was skipped")
	}
	select {
	case turn := <-legacyAdmissions:
		require.Equal(t, 2, turn)
	case <-time.After(3 * time.Second):
		t.Fatal("legacy admission hook was skipped")
	}
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_second","model":"gpt-5.2","usage":{"input_tokens":1,"output_tokens":1}}}`)
	_, err = readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
	require.NoError(t, err)
	// Omitting a model uses the original session model, even though the first
	// frame and the most recent explicit turn were both rewritten upstream.
	rawThird := []byte(`{"type":"response.create","instructions":"client third","input":[],"stream":false}`)
	writeCtx, cancelWrite = context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, rawThird)
	cancelWrite()
	require.NoError(t, err)
	third := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
	require.Equal(t, "gpt-5.1", gjson.GetBytes(third, "model").String())
	require.Equal(t, "FIRST-GROUP-PROMPT\n\nclient third", gjson.GetBytes(third, "instructions").String())
	select {
	case got := <-admissions:
		require.Equal(t, 3, got.turn)
		require.Equal(t, "client-first", got.model)
		require.Equal(t, rawThird, got.clientPayload)
	case <-time.After(3 * time.Second):
		t.Fatal("model-less turn skipped admission")
	}
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_third","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	_, err = readPassthroughLifecycleFrame(t, clientConn, 3*time.Second)
	require.NoError(t, err)
	_ = clientConn.CloseNow()
	cancel(context.Canceled)
	select {
	case <-serverErr:
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough did not exit after client close")
	}
}
