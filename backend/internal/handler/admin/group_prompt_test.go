package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type promptAdminStub struct{ service.AdminService }

func (*promptAdminStub) GetGroup(_ context.Context, id int64) (*service.Group, error) {
	if id != 7 {
		return nil, service.ErrGroupNotFound
	}
	return &service.Group{ID: id}, nil
}

type promptHandlerRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r *promptHandlerRepo) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}
func (r *promptHandlerRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

func TestGroupPromptAdminReadWriteAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &promptHandlerRepo{values: map[string]string{}}
	settings := service.NewSettingService(repo, nil)
	h := NewGroupHandler(&promptAdminStub{}, nil, nil)
	r := gin.New()
	r.GET("/groups/:id/prepend-prompt", h.GroupPrompt(settings))
	r.PUT("/groups/:id/prepend-prompt", h.GroupPrompt(settings))
	request := func(method, id, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/groups/"+id+"/prepend-prompt", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	w := request(http.MethodGet, "7", "")
	require.Equal(t, http.StatusOK, w.Code)
	var result map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	data := result["data"].(map[string]any)
	require.Equal(t, false, data["enabled"])
	require.Equal(t, groupprompt.DefaultPrompt, data["default_content"])
	payload, _ := json.Marshal(groupprompt.Config{Enabled: true, Content: "custom", ModelPrompts: map[string]string{"model-a": "custom a", "model-b": ""}})
	require.Equal(t, http.StatusOK, request(http.MethodPut, "7", string(payload)).Code)
	w = request(http.MethodGet, "7", "")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	data = result["data"].(map[string]any)
	require.Equal(t, true, data["enabled"])
	require.Equal(t, "custom", data["content"])
	require.Equal(t, map[string]any{"model-a": "custom a", "model-b": ""}, data["model_prompts"])
	// Replacing the configuration without overrides removes earlier model rules.
	require.Equal(t, http.StatusOK, request(http.MethodPut, "7", `{"enabled":true,"content":"custom"}`).Code)
	w = request(http.MethodGet, "7", "")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	data = result["data"].(map[string]any)
	require.Empty(t, data["model_prompts"])
	require.Equal(t, http.StatusNotFound, request(http.MethodGet, "8", "").Code)
	require.Equal(t, http.StatusBadRequest, request(http.MethodGet, "invalid", "").Code)
	payload, _ = json.Marshal(groupprompt.Config{Content: strings.Repeat("x", groupprompt.MaxPromptBytes+1)})
	require.Equal(t, http.StatusBadRequest, request(http.MethodPut, "7", string(payload)).Code)
}

func TestGroupPromptAdminRejectsScopedKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewGroupHandler(&promptAdminStub{}, nil, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{}))
	})
	r.GET("/groups/:id/prepend-prompt", h.GroupPrompt(nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/groups/7/prepend-prompt", nil))
	require.Equal(t, http.StatusForbidden, w.Code)
}
