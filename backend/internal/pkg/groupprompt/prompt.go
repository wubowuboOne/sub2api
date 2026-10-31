// Package groupprompt adds an optional group prefix without changing client messages.
package groupprompt

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/sjson"
)

//go:embed system-prompt.md
var DefaultPrompt string

const MaxPromptBytes = 1 << 20

type Config struct {
	Enabled      bool              `json:"enabled"`
	Content      string            `json:"content"`                 // Empty uses the bundled system-prompt.md.
	ModelPrompts map[string]string `json:"model_prompts,omitempty"` // Exact client model name -> override.
}

func (c Config) Prompt(model string) string {
	if !c.Enabled {
		return ""
	}
	content := c.Content
	if override, ok := c.ModelPrompts[strings.TrimSpace(model)]; ok {
		content = override
	}
	if strings.TrimSpace(content) == "" {
		return DefaultPrompt
	}
	return content
}

// Clone keeps cached and request-bound maps private to their owner.
func (c Config) Clone() Config {
	c.ModelPrompts = maps.Clone(c.ModelPrompts)
	return c
}

func (c Config) Validate() error {
	if !utf8.ValidString(c.Content) || len(c.Content) > MaxPromptBytes {
		return errors.New("prompt must be valid UTF-8 and at most 1 MiB")
	}
	total := len(c.Content)
	for model, content := range c.ModelPrompts {
		if model == "" || model != strings.TrimSpace(model) || len(model) > 512 || !utf8.ValidString(model) {
			return errors.New("model names must be non-empty, trimmed UTF-8 strings of at most 512 bytes")
		}
		total += len(model) + len(content)
		if !utf8.ValidString(content) || total > MaxPromptBytes {
			return errors.New("all prompt content and model names combined must be valid UTF-8 and at most 1 MiB")
		}
	}
	return nil
}

type contextKey struct{}

func WithConfig(ctx context.Context, config Config) context.Context {
	return context.WithValue(ctx, contextKey{}, config.Clone())
}

func FromContext(ctx context.Context) Config {
	if ctx == nil {
		return Config{}
	}
	config, _ := ctx.Value(contextKey{}).(Config)
	return config.Clone()
}

// Prepend preserves unknown fields, existing instructions and structured blocks.
// Calling it again with the same prefix is a no-op (including gateway retries).
func Prepend(body []byte, format, prompt string) ([]byte, error) {
	if prompt == "" {
		return body, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, errors.New("prompt requires a JSON object request")
	}
	field := ""
	switch format {
	case "responses":
		field = "instructions"
		var existing string
		if err := decode(obj["instructions"], &existing); err != nil {
			return nil, fmt.Errorf("invalid instructions: %w", err)
		}
		obj["instructions"] = raw(join(prompt, existing))
	case "messages":
		field = "system"
		var existing string
		if err := decode(obj["system"], &existing); err == nil {
			obj["system"] = raw(join(prompt, existing))
		} else {
			var blocks []json.RawMessage
			if err := json.Unmarshal(obj["system"], &blocks); err != nil {
				return nil, fmt.Errorf("invalid system: %w", err)
			}
			if !startsWithText(blocks, prompt) {
				blocks = append([]json.RawMessage{raw(map[string]string{"type": "text", "text": prompt})}, blocks...)
			}
			obj["system"] = raw(blocks)
		}
	case "chat":
		field = "messages"
		var messages []json.RawMessage
		if err := decode(obj["messages"], &messages); err != nil {
			return nil, fmt.Errorf("invalid messages: %w", err)
		}
		var first struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if len(messages) == 0 || json.Unmarshal(messages[0], &first) != nil || first.Role != "system" || first.Content != prompt {
			messages = append([]json.RawMessage{raw(map[string]string{"role": "system", "content": prompt})}, messages...)
		}
		obj["messages"] = raw(messages)
	case "gemini":
		field = "systemInstruction"
		if _, ok := obj["system_instruction"]; ok {
			field = "system_instruction"
		}
		var instruction map[string]json.RawMessage
		if err := decode(obj[field], &instruction); err != nil {
			return nil, fmt.Errorf("invalid systemInstruction: %w", err)
		}
		if instruction == nil {
			instruction = make(map[string]json.RawMessage)
		}
		var parts []json.RawMessage
		if err := decode(instruction["parts"], &parts); err != nil {
			return nil, fmt.Errorf("invalid systemInstruction.parts: %w", err)
		}
		if !startsWithText(parts, prompt) {
			parts = append([]json.RawMessage{raw(map[string]string{"text": prompt})}, parts...)
		}
		instruction["parts"] = raw(parts)
		obj[field] = raw(instruction)
	default:
		return body, nil
	}
	// Only replace the prompt field. Preserve all other bytes, including duplicate
	// model keys, numeric formatting, unknown fields and their original order.
	return sjson.SetRawBytes(body, field, obj[field])
}

func join(prefix, existing string) string {
	if existing == prefix || strings.HasPrefix(existing, prefix+"\n\n") {
		return existing
	}
	if existing == "" {
		return prefix
	}
	return prefix + "\n\n" + existing
}

func decode(data json.RawMessage, out any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func raw(value any) json.RawMessage {
	data, _ := json.Marshal(value) // Only already-validated JSON and strings are passed here.
	return data
}

func startsWithText(blocks []json.RawMessage, prompt string) bool {
	if len(blocks) == 0 {
		return false
	}
	var first struct {
		Text string `json:"text"`
	}
	return json.Unmarshal(blocks[0], &first) == nil && first.Text == prompt
}
