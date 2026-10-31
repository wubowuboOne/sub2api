package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

const groupPromptCacheTTL = 5 * time.Second

type cachedGroupPrompt struct {
	config  groupprompt.Config
	expires time.Time
}

func groupPromptKey(id int64) string { return fmt.Sprintf("group_prepend_prompt:%d", id) }

// GroupPromptForRequest isolates this optional feature from gateway availability.
// A failed read keeps the last known config, or skips injection on a cold cache.
// Admin reads/writes still return errors instead of claiming a successful save.
func (s *SettingService) GroupPromptForRequest(ctx context.Context, id int64) groupprompt.Config {
	lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	config, err := s.GetGroupPrompt(lookupCtx, id)
	if err == nil {
		return config
	}
	slog.WarnContext(ctx, "group prompt lookup failed; keeping gateway available", "group_id", id, "error", err)
	if cached, ok := s.groupPromptCache.Load(id); ok {
		return cached.(*cachedGroupPrompt).config.Clone()
	}
	return groupprompt.Config{}
}

// GetGroupPrompt uses the existing settings table; group schemas and auth snapshots
// need no migration. Other server instances observe changes within five seconds.
func (s *SettingService) GetGroupPrompt(ctx context.Context, id int64) (groupprompt.Config, error) {
	var config groupprompt.Config
	if s == nil || s.settingRepo == nil || id <= 0 {
		return config, nil
	}
	if cached, ok := s.groupPromptCache.Load(id); ok {
		entry := cached.(*cachedGroupPrompt)
		if time.Now().Before(entry.expires) {
			return entry.config.Clone(), nil
		}
	}
	value, err := s.settingRepo.GetValue(ctx, groupPromptKey(id))
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return config, err
	}
	if err == nil && value != "" {
		if err := json.Unmarshal([]byte(value), &config); err != nil {
			return config, fmt.Errorf("decode group prompt: %w", err)
		}
		if err := config.Validate(); err != nil {
			return config, err
		}
	}
	// LoadOrStore avoids an in-flight old read replacing a concurrent local save.
	entry := &cachedGroupPrompt{config: config.Clone(), expires: time.Now().Add(groupPromptCacheTTL)}
	if old, loaded := s.groupPromptCache.LoadOrStore(id, entry); loaded {
		if time.Now().Before(old.(*cachedGroupPrompt).expires) {
			return old.(*cachedGroupPrompt).config.Clone(), nil
		}
		if !s.groupPromptCache.CompareAndSwap(id, old, entry) {
			if current, ok := s.groupPromptCache.Load(id); ok {
				return current.(*cachedGroupPrompt).config.Clone(), nil
			}
		}
	}
	return config, nil
}

func (s *SettingService) SetGroupPrompt(ctx context.Context, id int64, config groupprompt.Config) error {
	if s == nil || s.settingRepo == nil || id <= 0 {
		return errors.New("group prompt storage is unavailable")
	}
	config = config.Clone()
	if err := config.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if err := s.settingRepo.Set(ctx, groupPromptKey(id), string(data)); err != nil {
		return err
	}
	s.groupPromptCache.Store(id, &cachedGroupPrompt{config: config, expires: time.Now().Add(groupPromptCacheTTL)})
	return nil
}

// applyOpenAIWSGroupPrompt runs on each response.create in both native and
// passthrough ingress, using the group policy bound at the WS handshake.
func applyOpenAIWSGroupPrompt(ctx context.Context, payload []byte, model string) ([]byte, error) {
	prompt := groupprompt.FromContext(ctx).Prompt(model)
	if prompt == "" {
		return payload, nil
	}
	if event := gjson.GetBytes(payload, "type").String(); event != "" && event != "response.create" {
		return payload, nil
	}
	return groupprompt.Prepend(payload, "responses", prompt)
}

// Keep the three existing ingress call sites limited to one call and error return.
func prependOpenAIWSGroupPrompt(ctx context.Context, payload *[]byte, model string) error {
	next, err := applyOpenAIWSGroupPrompt(ctx, *payload, model)
	if err != nil {
		return NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "invalid group prompt request", err)
	}
	*payload = next
	return nil
}
