package middleware

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
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

type groupPromptSettingsStub struct {
	service.SettingRepository
	config *groupprompt.Config
	fail   bool
	reads  int
}

func (s *groupPromptSettingsStub) GetValue(_ context.Context, key string) (string, error) {
	s.reads++
	if s.fail {
		return "", errors.New("settings unavailable")
	}
	if key == "group_prepend_prompt:7" {
		config := groupprompt.Config{Enabled: true, Content: "group prefix"}
		if s.config != nil {
			config = *s.config
		}
		data, _ := json.Marshal(config)
		return string(data), nil
	}
	return "", service.ErrSettingNotFound
}

func TestGroupPromptMiddlewareBypassPreservesRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []string{"disabled", "unconfigured", "no key", "no service", "unrelated route", "storage failure"} {
		t.Run(scenario, func(t *testing.T) {
			config := groupprompt.Config{Enabled: scenario != "disabled", Content: "prefix"}
			repo := &groupPromptSettingsStub{config: &config, fail: scenario == "storage failure"}
			settings := service.NewSettingService(repo, nil)
			if scenario == "no service" {
				settings = nil
			}
			path := "/v1/responses"
			if scenario == "unrelated route" {
				path = "/v1/images/generations"
			}
			r := gin.New()
			id := int64(7)
			if scenario == "unconfigured" {
				id = 8
			}
			r.Use(func(c *gin.Context) {
				if scenario != "no key" {
					c.Set(string(ContextKeyAPIKey), &service.APIKey{GroupID: &id})
				}
			})
			r.Use(GroupPrompt(settings))
			// Even malformed, compressed and non-JSON bodies must be untouched
			// when this feature is disabled, unavailable or outside its routes.
			original := "original bytes: not JSON or gzip"
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(original))
			req.Header.Set("Content-Encoding", "gzip")
			req.Header.Set("X-Client-Test", "original")
			bodyBefore, headersBefore, lengthBefore := req.Body, req.Header.Clone(), req.ContentLength
			r.POST(path, func(c *gin.Context) {
				require.True(t, bodyBefore == c.Request.Body, "bypass replaced the request body")
				require.Equal(t, headersBefore, c.Request.Header)
				require.Equal(t, lengthBefore, c.Request.ContentLength)
				got, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				require.Equal(t, original, string(got))
				c.Status(http.StatusOK)
			})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			if scenario == "unrelated route" || scenario == "no key" || scenario == "no service" {
				require.Zero(t, repo.reads)
			}
		})
	}
}

func TestGroupPromptMiddlewarePerModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	config := groupprompt.Config{Enabled: true, Content: "group fallback", ModelPrompts: map[string]string{
		"model-a": "model a prompt", "model-b": "model b prompt",
	}}
	for _, tc := range []struct {
		name, route, path, body, want string
	}{
		{"chat model a", "/v1/chat/completions", "/v1/chat/completions", `{"model":"model-a","messages":[]}`, "model a prompt"},
		{"responses model b", "/v1/responses", "/v1/responses", `{"model":"model-b","input":[]}`, "model b prompt"},
		{"messages fallback", "/v1/messages", "/v1/messages", `{"model":"other","messages":[]}`, "group fallback"},
		{"exact model name", "/v1/responses", "/v1/responses", `{"model":"model-a-suffix","input":[]}`, "group fallback"},
		{"gemini URL model", "/v1beta/models/*modelAction", "/v1beta/models/model-a:generateContent", `{"model":"model-b","contents":[]}`, "model a prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			id := int64(7)
			r.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{GroupID: &id}) })
			r.Use(GroupPrompt(service.NewSettingService(&groupPromptSettingsStub{config: &config}, nil)))
			r.POST(tc.route, func(c *gin.Context) {
				body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
				require.NoError(t, err)
				format := groupPromptFormat(c)
				// Reapplying the expected prompt must be a no-op, proving the
				// matching prompt was selected without any additional prefix.
				again, err := groupprompt.Prepend(body, format, tc.want)
				require.NoError(t, err)
				require.JSONEq(t, string(body), string(again))
				require.Contains(t, string(body), tc.want)
				c.Status(http.StatusOK)
			})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		})
	}
}

func TestGroupPromptMiddlewareProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{
		"/v1/messages", "/v1/responses", "/responses/compact",
		"/backend-api/codex/responses", "/antigravity/v1/messages",
		"/v1/chat/completions", "/v1beta/models/*modelAction",
	} {
		t.Run(path, func(t *testing.T) {
			r := gin.New()
			id := int64(7)
			r.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{GroupID: &id}) })
			r.Use(GroupPrompt(service.NewSettingService(&groupPromptSettingsStub{}, nil)))
			r.POST(path, func(c *gin.Context) {
				body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
				require.NoError(t, err)
				require.Contains(t, string(body), "group prefix")
				c.Status(http.StatusOK)
			})
			requestPath := strings.ReplaceAll(path, "*modelAction", "m:generateContent")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, requestPath, strings.NewReader("{}")))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		})
	}
}

func TestGroupPromptMiddlewareDisabledAndCompressed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, enabled := range []bool{false, true} {
		r := gin.New()
		id := int64(8)
		if enabled {
			id = 7
		}
		r.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{GroupID: &id}) })
		r.Use(GroupPrompt(service.NewSettingService(&groupPromptSettingsStub{}, nil)))
		r.POST("/v1/responses", func(c *gin.Context) {
			body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
			require.NoError(t, err)
			require.Equal(t, enabled, strings.Contains(string(body), "group prefix"))
			c.Status(http.StatusOK)
		})
		var compressed bytes.Buffer
		z := gzip.NewWriter(&compressed)
		_, _ = z.Write([]byte("{}"))
		require.NoError(t, z.Close())
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", &compressed)
		req.Header.Set("Content-Encoding", "gzip")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}
}

func TestGroupPromptMiddlewareWebSocketContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	id := int64(7)
	r.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{GroupID: &id}) })
	r.Use(GroupPrompt(service.NewSettingService(&groupPromptSettingsStub{}, nil)))
	r.GET("/v1/responses", func(c *gin.Context) {
		require.Equal(t, "group prefix", groupprompt.FromContext(c.Request.Context()).Prompt(""))
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	require.Equal(t, http.StatusOK, w.Code)
}
