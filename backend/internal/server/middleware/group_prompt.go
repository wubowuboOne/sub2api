package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// GroupPrompt is mounted after authentication and before protocol conversion.
// Only text inference/counting routes are changed; disabled groups keep their body.
func GroupPrompt(settings *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		format := groupPromptFormat(c)
		if settings == nil || format == "" {
			c.Next()
			return
		}
		key, ok := GetAPIKeyFromContext(c)
		if !ok || key == nil || key.GroupID == nil {
			c.Next()
			return
		}
		config := settings.GroupPromptForRequest(c.Request.Context(), *key.GroupID)
		if !config.Enabled {
			c.Next()
			return
		}
		if isResponsesWebSocketRoute(c) {
			c.Request = c.Request.WithContext(groupprompt.WithConfig(c.Request.Context(), config))
			c.Next()
			return
		}
		// Match the gateway's lenient JSON handling, including compressed bodies.
		body, err := httputil.ReadLenientJSONRequestBodyWithPrealloc(c.Request, 0)
		if err == nil {
			model := groupModelAllowlistModelFromParams(c)
			if model == "" {
				model = requestmodel.FromBodyForRoute(c.FullPath(), c.GetHeader("Content-Type"), body)
			}
			body, err = groupprompt.Prepend(body, format, config.Prompt(model))
		}
		if err != nil {
			status := http.StatusBadRequest
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				status = http.StatusRequestEntityTooLarge
			}
			groupPromptError(c, status, "Failed to apply group prompt to request")
			return
		}
		requestmodel.ResetRequestBody(c.Request, body)
		c.Next()
	}
}

func groupPromptFormat(c *gin.Context) string {
	if isResponsesWebSocketRoute(c) {
		return "responses"
	}
	if c.Request.Method != http.MethodPost {
		return ""
	}
	path := strings.TrimRight(c.Request.URL.Path, "/")
	switch {
	case strings.HasSuffix(path, "/messages"), strings.HasSuffix(path, "/messages/count_tokens"):
		return "messages"
	case strings.HasSuffix(path, "/chat/completions"):
		return "chat"
	case strings.HasSuffix(path, "/responses"), strings.HasSuffix(path, "/responses/compact"), strings.HasSuffix(path, "/responses/input_tokens"):
		return "responses"
	case strings.Contains(c.FullPath(), "/v1beta/models/") &&
		(strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent") || strings.HasSuffix(path, ":countTokens")):
		return "gemini"
	default:
		return ""
	}
}

func groupPromptError(c *gin.Context, status int, message string) {
	groupModelAllowlistErrorWriter(c)(c, status, message)
	c.Abort()
}
