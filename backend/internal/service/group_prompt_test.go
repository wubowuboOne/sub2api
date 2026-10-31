package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/stretchr/testify/require"
)

func TestGroupPromptModelOverridesCacheAndWebSocket(t *testing.T) {
	ctx := context.Background()
	svc := NewSettingService(&groupPromptRepoStub{values: map[string]string{}}, nil)
	config := groupprompt.Config{Enabled: true, Content: "group", ModelPrompts: map[string]string{"model-a": "prompt a", "model-b": "prompt b"}}
	require.NoError(t, svc.SetGroupPrompt(ctx, 1, config))
	config.ModelPrompts["model-a"] = "caller mutation"
	got, err := svc.GetGroupPrompt(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, "prompt a", got.Prompt("model-a"))
	got.ModelPrompts["model-a"] = "reader mutation"
	got, err = svc.GetGroupPrompt(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, "prompt a", got.Prompt("model-a"))
	wsctx := groupprompt.WithConfig(ctx, got)
	for model, want := range map[string]string{"model-a": "prompt a", "model-b": "prompt b", "other": "group"} {
		// A missing frame model inherits the client model resolved by WS ingress;
		// the upstream alias must not select a different prompt.
		for _, bodyModel := range []string{"", "upstream-alias"} {
			body, _ := json.Marshal(map[string]any{"type": "response.create", "model": bodyModel, "input": []any{}})
			out, err := applyOpenAIWSGroupPrompt(wsctx, body, model)
			require.NoError(t, err)
			var result map[string]any
			require.NoError(t, json.Unmarshal(out, &result))
			require.Equal(t, want, result["instructions"])
		}
	}
}

type groupPromptRepoStub struct {
	SettingRepository
	values map[string]string
	fail   bool
}

func (r *groupPromptRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if r.fail {
		return "", errors.New("offline")
	}
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}
func (r *groupPromptRepoStub) Set(_ context.Context, key, value string) error {
	if r.fail {
		return errors.New("offline")
	}
	r.values[key] = value
	return nil
}

func TestGroupPromptPersistenceIsolationAndCache(t *testing.T) {
	ctx := context.Background()
	repo := &groupPromptRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, nil)
	initial, err := svc.GetGroupPrompt(ctx, 1)
	require.NoError(t, err)
	require.False(t, initial.Enabled)
	config := groupprompt.Config{Enabled: true, Content: "custom"}
	require.NoError(t, svc.SetGroupPrompt(ctx, 1, config))
	got, err := svc.GetGroupPrompt(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, config, got)
	other, err := svc.GetGroupPrompt(ctx, 2)
	require.NoError(t, err)
	require.False(t, other.Enabled)
	fresh := NewSettingService(repo, nil)
	got, err = fresh.GetGroupPrompt(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, config, got)
	require.NoError(t, svc.SetGroupPrompt(ctx, 1, groupprompt.Config{}))
	got, err = svc.GetGroupPrompt(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, got.Prompt(""))
	fresh.groupPromptCache.Store(int64(1), &cachedGroupPrompt{config: config, expires: time.Now().Add(-time.Second)})
	got, err = fresh.GetGroupPrompt(ctx, 1)
	require.NoError(t, err)
	require.False(t, got.Enabled)
	repo.fail = true
	_, err = svc.GetGroupPrompt(ctx, 3)
	require.Error(t, err)
	require.Error(t, svc.SetGroupPrompt(ctx, 1, config))
}

func TestGroupPromptWSFrames(t *testing.T) {
	ctx := groupprompt.WithConfig(context.Background(), groupprompt.Config{Enabled: true, Content: "prefix"})
	for _, input := range []string{`{"type":"response.create","input":[]}`, `{"type":"response.create","instructions":"client","input":[]}`} {
		got, err := applyOpenAIWSGroupPrompt(ctx, []byte(input), "")
		require.NoError(t, err)
		require.Contains(t, string(got), "prefix")
		again, err := applyOpenAIWSGroupPrompt(ctx, got, "")
		require.NoError(t, err)
		require.Equal(t, string(got), string(again))
	}
	other := []byte(`{"type":"response.cancel"}`)
	got, err := applyOpenAIWSGroupPrompt(ctx, other, "")
	require.NoError(t, err)
	require.Equal(t, other, got)
	_, err = applyOpenAIWSGroupPrompt(ctx, []byte(`{"instructions":123}`), "")
	require.Error(t, err)
	require.False(t, strings.Contains(string(other), "prefix"))
}

func TestGroupPromptGatewayStorageFailureIsolation(t *testing.T) {
	ctx := context.Background()
	repo := &groupPromptRepoStub{values: map[string]string{}, fail: true}
	svc := NewSettingService(repo, nil)
	require.False(t, svc.GroupPromptForRequest(ctx, 1).Enabled)
	config := groupprompt.Config{Enabled: true, Content: "last known", ModelPrompts: map[string]string{"model-a": "last model prompt"}}
	svc.groupPromptCache.Store(int64(1), &cachedGroupPrompt{config: config, expires: time.Now().Add(-time.Second)})
	got := svc.GroupPromptForRequest(ctx, 1)
	require.Equal(t, "last model prompt", got.Prompt("model-a"))
	got.ModelPrompts["model-a"] = "mutation"
	require.Equal(t, "last model prompt", svc.GroupPromptForRequest(ctx, 1).Prompt("model-a"))
	// Optional gateway fallback must never turn a failed admin read/save into success.
	_, err := svc.GetGroupPrompt(ctx, 1)
	require.Error(t, err)
	require.Error(t, svc.SetGroupPrompt(ctx, 1, config))
}

func TestGroupPromptWSBypassAndErrorPreservePayload(t *testing.T) {
	ctx := context.Background()
	untouched := []byte("original payload, not JSON")
	require.NoError(t, prependOpenAIWSGroupPrompt(ctx, &untouched, "model-a"))
	require.Equal(t, "original payload, not JSON", string(untouched))
	ctx = groupprompt.WithConfig(ctx, groupprompt.Config{Enabled: true, Content: "prefix"})
	invalid := []byte(`{"type":"response.create","instructions":123}`)
	original := string(invalid)
	err := prependOpenAIWSGroupPrompt(ctx, &invalid, "model-a")
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, original, string(invalid))
}
