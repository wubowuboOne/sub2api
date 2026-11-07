//go:build unit

package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type groupDiagnosticsRouter struct {
	available bool
	models    []string
}

func (r *groupDiagnosticsRouter) ProbeAPIKeyGroup(_ context.Context, _ *service.Group, model, _ string) (service.APIKeyGroupAvailability, error) {
	r.models = append(r.models, model)
	return service.APIKeyGroupAvailability{Available: r.available}, nil
}

func TestSelectAPIKeyRequestGroupFailureRetainsRequestMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name      string
		google    bool
		available bool
		stream    bool
	}{
		{"openai_failure", false, false, true},
		{"openai_nonstream_failure", false, false, false},
		{"google_failure", true, false, true},
		{"google_nonstream_failure", true, false, false},
		{"successful_selection", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform := service.PlatformOpenAI
			path := "/v1/responses"
			action := "public-model:generateContent"
			if tc.stream {
				action = "public-model:streamGenerateContent"
			}
			if tc.google {
				platform, path = service.PlatformGemini, "/v1beta/models/"+action
			}
			primary := int64(1)
			groups := map[int64]*service.Group{
				1: {ID: 1, Platform: platform, Status: service.StatusActive, RateMultiplier: 0.5},
				2: {ID: 2, Platform: platform, Status: service.StatusActive, RateMultiplier: 1},
			}
			cfg := &config.Config{RunMode: config.RunModeStandard}
			key := &service.APIKey{
				ID: 7, UserID: 8, GroupID: &primary, GroupIDs: []int64{1, 2}, Group: groups[1],
				ModelMapping: map[string]string{"public-model": "routed-model"},
				User:         &service.User{ID: 8, Status: service.StatusActive, Balance: 10},
			}
			keys := service.NewAPIKeyService(nil, nil, creditFallbackGroups{groups: groups}, nil, nil, nil, cfg)
			probe := &groupDiagnosticsRouter{available: tc.available}
			keys.SetGroupRouter(probe)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			body := `{"model":"public-model","stream":true,"input":"private prompt"}`
			if !tc.stream {
				body = `{"model":"public-model","stream":false,"input":"private prompt"}`
			}
			if tc.google {
				body = `{"contents":[{"parts":[{"text":"private prompt"}]}]}`
			}
			c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.google {
				c.AddParam("modelAction", action)
			}
			selected, err := selectAPIKeyRequestGroup(c, keys, nil, cfg, key, tc.google)
			if tc.available {
				require.NoError(t, err)
				require.NotNil(t, selected)
				require.False(t, c.IsAborted())
				require.Empty(t, c.GetString("ops_model"))
				require.Nil(t, c.Request.Context().Value(ctxkey.Model))
			} else {
				require.Nil(t, selected)
				require.Equal(t, "NO_AVAILABLE_API_KEY_GROUP", infraerrors.Reason(err))
				require.True(t, c.IsAborted())
				require.Equal(t, http.StatusServiceUnavailable, w.Code)
				require.Equal(t, "public-model", c.GetString("ops_model"))
				require.Equal(t, "public-model", c.Request.Context().Value(ctxkey.Model))
				require.Equal(t, tc.stream, c.GetBool("ops_stream"))
				require.Contains(t, w.Body.String(), "No selected group is currently available for this request")
				require.NotContains(t, w.Body.String(), "private prompt")
			}
			wantProbeModel := "routed-model"
			if tc.google {
				wantProbeModel = "public-model"
			}
			require.NotEmpty(t, probe.models)
			for _, model := range probe.models {
				require.Equal(t, wantProbeModel, model)
			}
			restored, readErr := io.ReadAll(c.Request.Body)
			require.NoError(t, readErr)
			require.Equal(t, body, string(restored))
			require.Equal(t, primary, *key.GroupID)
			require.Equal(t, map[string]string{"public-model": "routed-model"}, key.ModelMapping)
		})
	}
}
