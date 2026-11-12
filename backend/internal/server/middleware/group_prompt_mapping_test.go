package middleware

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGroupPromptMiddlewareAPIKeyMappingUsesClientModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, path, fields, promptField string
	}{
		{"messages", "/v1/messages", `"system":"CLIENT-SYSTEM","messages":[]`, "system"},
		{"messages count", "/v1/messages/count_tokens", `"system":"CLIENT-SYSTEM","messages":[]`, "system"},
		{"chat", "/v1/chat/completions", `"messages":[{"role":"system","content":"CLIENT-SYSTEM"}]`, "messages.0.content"},
		{"responses", "/v1/responses", `"instructions":"CLIENT-SYSTEM","input":[]`, "instructions"},
		{"compact", "/v1/responses/compact", `"instructions":"CLIENT-SYSTEM","input":[]`, "instructions"},
	} {
		for _, aliasPrompt := range []bool{true, false} {
			name := tc.name + "/group_default"
			if aliasPrompt {
				name = tc.name + "/client_override"
			}
			t.Run(name, func(t *testing.T) {
				config := groupprompt.Config{Enabled: true, Content: "GROUP-DEFAULT", ModelPrompts: map[string]string{
					"upstream-target": "WRONG-TARGET-PROMPT",
				}}
				want := "GROUP-DEFAULT"
				if aliasPrompt {
					config.ModelPrompts["client-alias"] = "CLIENT-ALIAS-PROMPT"
					want = "CLIENT-ALIAS-PROMPT"
				}
				id := int64(7)
				key := allowlistAPIKey(false)
				key.GroupID = &id
				key.ModelMapping = map[string]string{"client-alias": "upstream-target"}
				r := gin.New()
				r.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), key); c.Next() })
				// Keep the production order: admission and mapping precede injection.
				r.Use(GroupModelAllowlist(), APIKeyModelPolicy(), GroupPrompt(service.NewSettingService(&groupPromptSettingsStub{config: &config}, nil)))
				r.POST(tc.path, func(c *gin.Context) {
					body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
					require.NoError(t, err)
					require.Equal(t, "upstream-target", gjson.GetBytes(body, "model").String())
					original, ok := service.RequestedPublicModelFromContext(c.Request.Context())
					require.True(t, ok)
					require.Equal(t, "client-alias", original)
					require.Contains(t, gjson.GetBytes(body, tc.promptField).String(), want)
					require.NotContains(t, string(body), "WRONG-TARGET-PROMPT")
					require.Contains(t, string(body), "CLIENT-SYSTEM")
					c.Status(http.StatusOK)
				})
				w := doJSON(t, r, http.MethodPost, tc.path, `{"model":"client-alias",`+tc.fields+`}`)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			})
		}
	}
}

// The upstream per-key ceiling must see the complete injected request while
// prompt lookup continues to use the public model before API key mapping.
func TestGroupPromptMappingContextCeilingIncludesPrompt(t *testing.T) {
	for _, limit := range []int{10, 1000} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			id := int64(7)
			key := allowlistAPIKey(false)
			key.GroupID = &id
			key.AllowLongContext = true
			key.ContextTokenLimit = limit
			key.ModelMapping = map[string]string{"client-alias": "upstream-target"}
			prompt := groupprompt.Config{Enabled: true, ModelPrompts: map[string]string{
				"client-alias": "COMPLETE-CLIENT-PROMPT", "upstream-target": "WRONG-TARGET-PROMPT",
			}}
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), key); c.Next() })
			router.Use(GroupModelAllowlist(), APIKeyModelPolicy(), GroupPrompt(service.NewSettingService(&groupPromptSettingsStub{config: &prompt}, nil)))
			checked, forwarded := false, false
			router.Use(APIKeyLongContextPolicy(func(ctx context.Context, got *service.APIKey, body []byte, _ ...string) error {
				checked = true
				original, ok := service.RequestedPublicModelFromContext(ctx)
				require.True(t, ok)
				require.Equal(t, "client-alias", original)
				require.Equal(t, "upstream-target", gjson.GetBytes(body, "model").String())
				system := gjson.GetBytes(body, "system").String()
				require.Equal(t, "COMPLETE-CLIENT-PROMPT\n\nCLIENT-SYSTEM", system)
				if len(system) > got.ContextTokenLimit {
					return &service.APIKeyLongContextError{Code: "context_limit_exceeded", Limit: got.ContextTokenLimit}
				}
				return nil
			}))
			router.POST("/v1/messages", func(c *gin.Context) { forwarded = true; c.Status(http.StatusOK) })
			response := doJSON(t, router, http.MethodPost, "/v1/messages",
				`{"model":"client-alias","system":"CLIENT-SYSTEM","messages":[]}`)
			require.True(t, checked)
			if limit == 10 {
				require.False(t, forwarded)
				require.Equal(t, http.StatusBadRequest, response.Code)
				require.Contains(t, response.Body.String(), service.APIKeyContextLengthExceededCode)
			} else {
				require.True(t, forwarded)
				require.Equal(t, http.StatusOK, response.Code)
			}
		})
	}
}
