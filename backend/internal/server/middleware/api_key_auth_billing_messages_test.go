//go:build unit

package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthBillingErrorsIncludeChinese(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"standard", "google"} {
		for _, tc := range []struct {
			name, code, chinese, english string
			balance                      float64
			subscription                 bool
			expired                      bool
		}{
			{name: "balance", code: "INSUFFICIENT_BALANCE", chinese: "用户余额不足", english: "Insufficient user balance"},
			{name: "no_active_subscription", code: "SUBSCRIPTION_NOT_FOUND", chinese: "当前分组没有有效订阅", english: "No active subscription found", balance: 10, subscription: true},
			{name: "expired_subscription", code: "SUBSCRIPTION_INVALID", chinese: "订阅已到期", english: "subscription has expired", balance: 10, subscription: true, expired: true},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				user := &service.User{ID: 10, Role: service.RoleUser, Status: service.StatusActive, Balance: tc.balance}
				group := &service.Group{ID: 42, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true}
				if tc.subscription {
					group.SubscriptionType = service.SubscriptionTypeSubscription
				}
				key := &service.APIKey{ID: 100, UserID: user.ID, Key: "billing-message-test", Status: service.StatusActive, User: user, Group: group, GroupID: &group.ID}
				repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
					copy := *key
					return &copy, nil
				}}
				cfg := &config.Config{RunMode: config.RunModeStandard}
				keyService := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
				var subscriptions *service.SubscriptionService
				if tc.subscription {
					subRepo := &stubUserSubscriptionRepo{getActive: func(context.Context, int64, int64) (*service.UserSubscription, error) {
						if !tc.expired {
							return nil, service.ErrSubscriptionNotFound
						}
						return &service.UserSubscription{ID: 20, UserID: user.ID, GroupID: group.ID, Status: service.SubscriptionStatusActive, ExpiresAt: time.Now().Add(-time.Hour)}, nil
					}}
					subscriptions = service.NewSubscriptionService(nil, subRepo, nil, nil, cfg)
					t.Cleanup(subscriptions.Stop)
				}
				router := gin.New()
				if protocol == "google" {
					router.Use(APIKeyAuthWithSubscriptionGoogle(keyService, subscriptions, cfg))
				} else {
					router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(keyService, subscriptions, cfg)))
				}
				called := false
				router.POST("/t", func(c *gin.Context) {
					called = true
					c.Status(http.StatusOK)
				})
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/t", nil)
				request.Header.Set("x-api-key", key.Key)
				request.Header.Set("x-goog-api-key", key.Key)
				router.ServeHTTP(response, request)
				require.Equal(t, http.StatusForbidden, response.Code)
				require.False(t, called)
				var body struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Error   struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				message := body.Message
				if protocol == "google" {
					require.Equal(t, http.StatusForbidden, body.Error.Code)
					message = body.Error.Message
				} else {
					require.Equal(t, tc.code, body.Code)
				}
				require.Contains(t, message, tc.chinese)
				require.Contains(t, message, tc.english)
				require.True(t, strings.HasPrefix(message, tc.chinese), "Chinese must appear first: %s", message)
			})
		}
	}
}
