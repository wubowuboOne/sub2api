package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGroupPromptUpstreamBodyBudgetAndCancellation(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1beta/models/test:generateContent"} {
		for _, mode := range []string{"overload", "canceled", "available"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				router := gin.New()
				budget := int64(1 << 20)
				if mode == "overload" {
					budget = 1
				}
				router.Use(RequestBodyLimit(2048, httputil.NewBodyMemoryBudget(budget)))
				id := int64(7)
				router.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{GroupID: &id}); c.Next() })
				cfg := groupprompt.Config{Enabled: true, Content: "COMPLETE-GROUP-PROMPT"}
				router.Use(GroupPrompt(service.NewSettingService(&groupPromptSettingsStub{config: &cfg}, nil)))
				route := path
				if strings.Contains(path, "/models/") {
					route = "/v1beta/models/*modelAction"
				}
				forwarded := false
				router.POST(route, func(c *gin.Context) {
					forwarded = true
					body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
					require.NoError(t, err)
					require.Contains(t, string(body), "COMPLETE-GROUP-PROMPT")
					c.Status(http.StatusOK)
				})
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
				if mode == "canceled" {
					ctx, cancel := context.WithCancel(request.Context())
					cancel()
					request = request.WithContext(ctx)
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				switch mode {
				case "overload":
					require.False(t, forwarded)
					require.Equal(t, http.StatusServiceUnavailable, response.Code)
					require.Equal(t, "1", response.Header().Get("Retry-After"))
					require.Contains(t, response.Body.String(), "buffering capacity")
				case "canceled":
					require.False(t, forwarded)
					require.Equal(t, 499, response.Code)
				default:
					require.True(t, forwarded)
					require.Equal(t, http.StatusOK, response.Code)
				}
			})
		}
	}
}
