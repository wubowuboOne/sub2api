package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type openAIMessagesContinuationRetryKey struct{}

// This is a request repair, not permission to rotate accounts or replay output.
type openAIMessagesContinuationRetryError struct {
	unsupported bool
}

func (e *openAIMessagesContinuationRetryError) Error() string {
	return "openai messages: upstream continuation unavailable"
}

func newOpenAIMessagesContinuationRetryError(c *gin.Context, payload []byte, previousResponseID ...string) *openAIMessagesContinuationRetryError {
	if len(previousResponseID) == 0 || strings.TrimSpace(previousResponseID[0]) == "" ||
		openAIStreamClientCanceled(c) || IsResponseCommitted(c) || openAIStreamClientOutputStarted(c, false) {
		return nil
	}
	switch gjson.GetBytes(payload, "type").String() {
	case "response.failed", "error":
	default:
		return nil
	}
	errorFields := gjson.GetBytes(payload, "response.error")
	if !errorFields.IsObject() {
		errorFields = gjson.GetBytes(payload, "error")
	}
	if !errorFields.IsObject() {
		return nil
	}
	for _, path := range openAIStreamErrorStatusPaths {
		if status := int(gjson.GetBytes(payload, path).Int()); status >= http.StatusBadRequest && status != http.StatusBadRequest && status != http.StatusNotFound {
			return nil
		}
	}
	// Do not recover policy/auth/rate-limit failures merely mentioning this field.
	for _, field := range []string{"code", "type"} {
		switch strings.ToLower(strings.TrimSpace(errorFields.Get(field).String())) {
		case "", "invalid_request", "invalid_request_error", "previous_response_not_found", "unsupported_parameter", "unknown_parameter":
		default:
			return nil
		}
	}
	if hit, _, _ := detectOpenAICyberPolicy(payload); hit {
		return nil
	}
	message := errorFields.Get("message").String()
	errorBody := openAIStreamFailedEventPassthroughBody(payload, message)
	unsupported := isOpenAICompatPreviousResponseUnsupported(http.StatusBadRequest, message, errorBody)
	if !unsupported && !isOpenAICompatPreviousResponseNotFound(http.StatusBadRequest, message, errorBody) {
		return nil
	}
	return &openAIMessagesContinuationRetryError{unsupported: unsupported}
}
