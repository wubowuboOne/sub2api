package groupprompt

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPromptPerModelSelectionAndValidation(t *testing.T) {
	config := Config{Enabled: true, Content: "group", ModelPrompts: map[string]string{"model-a": "prompt a", "model-b": "prompt b", "bundled": ""}}
	for model, want := range map[string]string{"model-a": "prompt a", "model-b": "prompt b", " model-a ": "prompt a", "model-c": "group", "model-a-extra": "group", "": "group", "bundled": DefaultPrompt} {
		if got := config.Prompt(model); got != want {
			t.Fatalf("model %q: got %q", model, got)
		}
	}
	ctx := WithConfig(context.Background(), config)
	config.ModelPrompts["model-a"] = "mutated"
	if FromContext(ctx).Prompt("model-a") != "prompt a" {
		t.Fatal("context map changed through its caller")
	}
	fromContext := FromContext(ctx)
	fromContext.ModelPrompts["model-a"] = "mutated again"
	if FromContext(ctx).Prompt("model-a") != "prompt a" {
		t.Fatal("context map changed through its reader")
	}
	config.Enabled = false
	if config.Prompt("model-a") != "" {
		t.Fatal("disabled group used a model override")
	}
	for _, invalid := range []Config{
		{ModelPrompts: map[string]string{"": "text"}},
		{ModelPrompts: map[string]string{" model-a": "text"}},
		{ModelPrompts: map[string]string{"model-a": strings.Repeat("x", MaxPromptBytes)}},
		{Content: strings.Repeat("x", MaxPromptBytes-10), ModelPrompts: map[string]string{"model-a": "more text"}},
	} {
		if invalid.Validate() == nil {
			t.Fatal("invalid model config accepted")
		}
	}
	var legacy Config
	if err := json.Unmarshal([]byte("{\"enabled\":true,\"content\":\"legacy\"}"), &legacy); err != nil || legacy.Prompt("any") != "legacy" {
		t.Fatal("legacy config stopped working")
	}
}

func TestPrependPreservesContentAndIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name, format, body, want string
	}{
		{"responses", "responses", `{"model":"m","instructions":"client","input":"hello","stream":true}`, `"instructions":"prefix\n\nclient"`},
		{"messages string", "messages", `{"system":"client","messages":[{"role":"user","content":"hello"}]}`, `"system":"prefix\n\nclient"`},
		{"messages blocks", "messages", `{"system":[{"type":"text","text":"client","cache_control":{"type":"ephemeral"}}],"messages":[]}`, `"system":[{"text":"prefix","type":"text"},{"type":"text","text":"client","cache_control":{"type":"ephemeral"}}]`},
		{"chat", "chat", `{"messages":[{"role":"system","content":[{"type":"text","text":"client"}]},{"role":"user","content":"hello"}]}`, `"messages":[{"content":"prefix","role":"system"},{"role":"system","content":[{"type":"text","text":"client"}]}`},
		{"gemini", "gemini", `{"systemInstruction":{"role":"system","parts":[{"text":"client"}]},"contents":[]}`, `"parts":[{"text":"prefix"},{"text":"client"}]`},
		{"gemini snake", "gemini", `{"system_instruction":{"parts":[{"text":"client"}]},"contents":[]}`, `"system_instruction":{"parts":[{"text":"prefix"},{"text":"client"}]}`},
		{"new instructions", "responses", `{"input":"hello"}`, `"instructions":"prefix"`},
		{"null system", "messages", `{"system":null,"messages":[]}`, `"system":"prefix"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Prepend([]byte(tc.body), tc.format, "prefix")
			if err != nil || !strings.Contains(string(got), tc.want) {
				t.Fatalf("got %s, err %v; want %s", got, err, tc.want)
			}
			again, err := Prepend(got, tc.format, "prefix")
			if err != nil || string(again) != string(got) {
				t.Fatalf("repeated injection changed payload: %s (%v)", again, err)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(got, &obj); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"input", "stream", "contents", "model"} {
				var original map[string]json.RawMessage
				_ = json.Unmarshal([]byte(tc.body), &original)
				if string(obj[field]) != string(original[field]) {
					t.Fatalf("changed %s", field)
				}
			}
		})
	}
}

func TestPromptConfigAndInvalidRequests(t *testing.T) {
	if (Config{}).Prompt("") != "" {
		t.Fatal("disabled configuration injected text")
	}
	if DefaultPrompt == "" || (Config{Enabled: true}).Prompt("") != DefaultPrompt {
		t.Fatal("missing bundled default")
	}
	if (Config{Enabled: true, Content: "custom"}).Prompt("") != "custom" {
		t.Fatal("custom prompt ignored")
	}
	if (Config{Content: strings.Repeat("a", MaxPromptBytes+1)}).Validate() == nil {
		t.Fatal("oversized prompt accepted")
	}
	for _, body := range []string{`[]`, `null`, `{`, `{"instructions":[]}`} {
		if _, err := Prepend([]byte(body), "responses", "prefix"); err == nil {
			t.Fatalf("invalid body accepted: %s", body)
		}
	}
	body := []byte("untouched")
	got, err := Prepend(body, "responses", "")
	if err != nil || string(got) != string(body) {
		t.Fatal("disabled body changed")
	}
}

func TestPrependPreservesNonPromptBytes(t *testing.T) {
	body := " { \"model\" : \"first\", \"model\":\"last\", \"input\" : {\"n\": 1.00}, \"instructions\" : \"client\", \"metadata\": { \"unknown\" : true } } "
	got, err := Prepend([]byte(body), "responses", "prefix")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(body, "\"client\"", "\"prefix\\n\\nclient\"", 1)
	if string(got) != want {
		t.Fatalf("changed bytes outside prompt: got %s, want %s", got, want)
	}
}
