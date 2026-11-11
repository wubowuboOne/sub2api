package service

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var openAIClientQuotedModelReferences = regexp.MustCompile("(?i)(?:'[^'\\r\\n]+'|\"[^\"\\r\\n]+\"|`[^`\\r\\n]+`)\\s+model\\b|\\bmodel\\s+(?:'[^'\\r\\n]+'|\"[^\"\\r\\n]+\"|`[^`\\r\\n]+`)")

// Public model identity is captured before API-key, composite and channel mapping.
// Use it only for wire responses, never for scheduling or usage attribution.
func openAIClientResponseModel(c *gin.Context, fallback string) string {
	if c != nil && c.Request != nil {
		if model, ok := RequestedPublicModelFromContext(c.Request.Context()); ok {
			return model
		}
		if model, ok := c.Request.Context().Value(ctxkey.Model).(string); ok && strings.TrimSpace(model) != "" {
			return model
		}
	}
	return fallback
}

// Only client copies are sanitized. Retry classification and ops retain the
// provider's original diagnostics, including its private model name.
func openAIClientReasoningErrorMessage(message, param string) string {
	lower := strings.ToLower(message)
	param = strings.ToLower(strings.TrimSpace(param))
	reasoning := param == "reasoning" || param == "reasoning.effort" || param == "reasoning_effort" ||
		((param == "" || param == "model") && strings.Contains(lower, "reasoning"))
	unsupported := strings.Contains(lower, "not support") || strings.Contains(lower, "unsupported") ||
		strings.Contains(lower, "not allowed") || strings.Contains(lower, "only support")
	if reasoning && unsupported {
		return "The requested reasoning effort is not supported by this model. Choose a supported reasoning effort and try again."
	}
	return openAIClientQuotedModelReferences.ReplaceAllString(message, "requested model")
}

func openAIClientErrorParam(payload []byte) string {
	return firstNonEmpty(
		gjson.GetBytes(payload, "response.error.param").String(),
		gjson.GetBytes(payload, "error.param").String(),
		gjson.GetBytes(payload, "param").String(),
	)
}

func sanitizeOpenAIClientReasoningError(payload []byte) ([]byte, bool) {
	if !bytes.Contains(payload, []byte(`"error"`)) || !gjson.ValidBytes(payload) {
		return payload, false
	}
	updated := payload
	for _, prefix := range []string{"error.", "response.error."} {
		message := gjson.GetBytes(updated, prefix+"message")
		if message.Type != gjson.String {
			continue
		}
		param := gjson.GetBytes(updated, prefix+"param").String()
		clientMessage := openAIClientReasoningErrorMessage(message.Str, param)
		if clientMessage != message.Str {
			next, err := sjson.SetBytes(updated, prefix+"message", clientMessage)
			if err != nil {
				return payload, false
			}
			updated = next
		}
	}
	// Some providers use a flat {type:"error", message, param} event.
	if gjson.GetBytes(updated, "type").String() == "error" {
		message := gjson.GetBytes(updated, "message")
		if message.Type == gjson.String {
			clientMessage := openAIClientReasoningErrorMessage(message.Str, gjson.GetBytes(updated, "param").String())
			if clientMessage != message.Str {
				next, err := sjson.SetBytes(updated, "message", clientMessage)
				if err != nil {
					return payload, false
				}
				updated = next
			}
		}
	}
	return updated, !bytes.Equal(payload, updated)
}
