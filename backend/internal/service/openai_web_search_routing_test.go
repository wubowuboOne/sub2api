package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func webSearchTestAccount(id int64, mode string) Account {
	return Account{ID: id, Name: fmt.Sprint(id), Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Extra: map[string]any{OpenAIWebSearchModeKey: mode},
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://upstream.example"}}
}

func TestOpenAIWebSearchRoutingModes(t *testing.T) {
	for _, mode := range []string{"", "default", "only", "disabled"} {
		for _, capability := range []OpenAIEndpointCapability{"", OpenAIEndpointCapabilityChatCompletions, OpenAIEndpointCapabilityResponses, OpenAIEndpointCapabilityResponsesCompact, OpenAIEndpointCapabilityEmbeddings, OpenAIEndpointCapabilityLive, OpenAIEndpointCapabilityAlphaSearch} {
			t.Run(mode+"/"+string(capability), func(t *testing.T) {
				a := webSearchTestAccount(1, mode)
				baseline := webSearchTestAccount(1, "default")
				want := baseline.SupportsOpenAIEndpointCapability(capability)
				if mode == "only" && capability != OpenAIEndpointCapabilityAlphaSearch || mode == "disabled" && capability == OpenAIEndpointCapabilityAlphaSearch {
					want = false
				}
				require.Equal(t, want, a.SupportsOpenAIEndpointCapability(capability))
				require.Equal(t, want, a.supportsOpenAIEndpointCapabilityForRequest(context.Background(), capability))
			})
		}
	}
}

func TestOpenAIWebSearchRoutingDedicatedAndProbe(t *testing.T) {
	a := webSearchTestAccount(7, "only")
	ctx := WithOpenAIWebSearchRouting(context.Background(), 7, "public-model")
	require.True(t, a.supportsOpenAIEndpointCapabilityForRequest(ctx, OpenAIEndpointCapabilityAlphaSearch))
	other := webSearchTestAccount(8, "default")
	require.False(t, other.supportsOpenAIEndpointCapabilityForRequest(ctx, OpenAIEndpointCapabilityAlphaSearch))
	// Search binding has no effect on ordinary accounts' model requests.
	require.True(t, other.supportsOpenAIEndpointCapabilityForRequest(ctx, OpenAIEndpointCapabilityChatCompletions))
	a.Credentials["model_mapping"] = map[string]any{"public-model": "actual-model"}
	a.Extra["openai_web_search_probe"] = map[string]any{"supported": false, "route": openAIWebSearchRouteFingerprint(&a), "model": "actual-model"}
	require.False(t, a.supportsOpenAIEndpointCapabilityForRequest(ctx, OpenAIEndpointCapabilityAlphaSearch))
	require.True(t, a.supportsOpenAIEndpointCapabilityForRequest(WithOpenAIWebSearchRouting(context.Background(), 7, "another-model"), OpenAIEndpointCapabilityAlphaSearch))
	a.Credentials["base_url"] = "https://new-upstream.example"
	require.True(t, a.supportsOpenAIEndpointCapabilityForRequest(ctx, OpenAIEndpointCapabilityAlphaSearch), "stale route evidence must not exclude the changed route")
}

func TestOpenAIWebSearchRoutingDoesNotBypassBPSOrAdvancedChecks(t *testing.T) {
	a := webSearchTestAccount(7, "only")
	a.Extra["openai_excel_bps"] = true
	require.False(t, accountSupportsOpenAICapabilitiesForRequest(context.Background(), &a, "gpt-test", OpenAIEndpointCapabilityChatCompletions, ""))
	scheduler := &defaultOpenAIAccountScheduler{}
	ok, reason := scheduler.isAccountRequestCompatibleReason(context.Background(), &a, OpenAIAccountScheduleRequest{RequiredCapability: OpenAIEndpointCapabilityChatCompletions})
	require.False(t, ok)
	require.Equal(t, "capability_mismatch", reason)
}

func TestOpenAIWebSearchProbeClassification(t *testing.T) {
	a := webSearchTestAccount(7, "disabled")
	for _, tc := range []struct {
		name             string
		status           int
		body             string
		known, supported bool
	}{
		{"success", 200, `{"output":"search result","encrypted_output":"cipher"}`, true, true},
		{"success_with_null_error", 200, `{"output":"search result","error":null}`, true, true},
		{"html", 200, `<html>not a search endpoint</html>`, false, false},
		{"empty", 200, `{}`, false, false},
		{"non_string_cipher", 200, `{"encrypted_output":true}`, false, false},
		{"error_in_200", 200, `{"error":{"message":"unavailable"}}`, false, false},
		{"missing_endpoint", 404, `{"error":{"message":"Not Found"}}`, true, false},
		{"missing_model", 404, `{"error":{"message":"model not found"}}`, false, false},
		{"method_not_allowed", 405, ``, true, false},
		{"limited", 429, `{"error":{"message":"rate limited"}}`, false, false},
		{"bad_gateway", 502, ``, false, false},
		{"timeout", 524, ``, false, false},
		{"auth", 401, ``, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := openAIWebSearchProbeResult(&a, tc.status, []byte(tc.body), false)
			require.Equal(t, tc.known, got != nil)
			if got != nil {
				require.Equal(t, tc.supported, *got)
			}
		})
	}
	got, _ := openAIWebSearchProbeResult(&a, 200, []byte(alphaSearchResponsesSSE("verified sources")), true)
	require.NotNil(t, got)
	require.True(t, *got)
	got, _ = openAIWebSearchProbeResult(&a, 200, []byte("data: [DONE]\n\n"), true)
	require.Nil(t, got)
}

type webSearchProbeRepo struct {
	AccountRepository
	accounts []Account
	saved    map[string]any
	saveErr  error
}

func (r *webSearchProbeRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			return &r.accounts[i], nil
		}
	}
	return nil, fmt.Errorf("not found")
}
func (r *webSearchProbeRepo) ListByGroup(context.Context, int64) ([]Account, error) {
	return r.accounts, nil
}
func (r *webSearchProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.saved = updates
	return r.saveErr
}

func TestOpenAIWebSearchProbePersistsWithoutChangingAccountHealth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{200, 404, 429, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a := webSearchTestAccount(7, "disabled")
			if status == 429 || status == 502 {
				a.Extra["openai_web_search_probe"] = map[string]any{"supported": true, "route": openAIWebSearchRouteFingerprint(&a), "model": "gpt-test"}
			}
			repo := &webSearchProbeRepo{accounts: []Account{a}}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":"search result"}`))}}
			gateway := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			svc := &AccountTestService{accountRepo: repo, openaiGatewayService: gateway}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/admin/accounts/7/test", nil)
			err := svc.testOpenAIWebSearchConnection(c, &repo.accounts[0], "gpt-test", "test query")
			if status == 200 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NotNil(t, repo.saved)
			probe := repo.saved["openai_web_search_probe"].(map[string]any)
			require.Equal(t, status != 404, probe["supported"])
			require.Equal(t, StatusActive, repo.accounts[0].Status)
			require.Equal(t, OpenAIWebSearchModeDisabled, repo.accounts[0].GetOpenAIWebSearchMode(), "manual probing must not enable a disabled account")
			require.Equal(t, "/v1/alpha/search", upstream.lastReq.URL.Path)
		})
	}
}

func TestOpenAIWebSearchProbeSaveFailureDoesNotReportSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := webSearchTestAccount(7, "default")
	repo := &webSearchProbeRepo{accounts: []Account{a}, saveErr: fmt.Errorf("database unavailable")}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":"search result"}`))}}
	svc := &AccountTestService{accountRepo: repo, openaiGatewayService: &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/admin/accounts/7/test", nil)
	require.Error(t, svc.testOpenAIWebSearchConnection(c, &a, "gpt-test", "query"))
	require.Contains(t, rec.Body.String(), "could not be saved")
	require.NotContains(t, rec.Body.String(), `"success":true`)
}

func TestOpenAIWebSearchGroupValidation(t *testing.T) {
	for _, tc := range []struct {
		name, mode, platform string
		id                   int64
		valid                bool
	}{
		{"default", "default", PlatformOpenAI, 0, true},
		{"member", "only", PlatformOpenAI, 7, true},
		{"composite", "default", PlatformComposite, 7, true},
		{"wrong_group", "default", PlatformOpenAI, 9, false},
		{"disabled", "disabled", PlatformOpenAI, 7, false},
		{"other_platform", "default", PlatformAnthropic, 7, false},
		{"negative", "default", PlatformOpenAI, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &adminServiceImpl{accountRepo: &webSearchProbeRepo{accounts: []Account{webSearchTestAccount(7, tc.mode)}}}
			err := s.validateGroupWebSearchAccount(context.Background(), &Group{ID: 42, Platform: tc.platform}, tc.id)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	for _, invalid := range []any{true, 1, "invalid"} {
		require.Error(t, ValidateOpenAIWebSearchExtra(map[string]any{OpenAIWebSearchModeKey: invalid}))
	}
}

func TestOpenAIWebSearchSettingsDoNotValidateOtherPlatforms(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGrok, PlatformGemini} {
		t.Run(platform, func(t *testing.T) {
			extra := map[string]any{OpenAIWebSearchModeKey: true, "provider_setting": "preserve"}
			normalized, err := normalizeOpenAILongContextBillingExtra(platform, extra)
			require.NoError(t, err)
			require.Equal(t, extra, normalized)
			a := webSearchTestAccount(7, "default")
			a.Platform = platform
			repo := &webSearchProbeRepo{accounts: []Account{a}}
			svc := &adminServiceImpl{accountRepo: repo}
			require.NoError(t, svc.UpdateAccountExtra(context.Background(), a.ID, extra))
			require.Equal(t, true, repo.saved[OpenAIWebSearchModeKey])
		})
	}
	openaiextra := map[string]any{OpenAIWebSearchModeKey: true}
	_, err := normalizeOpenAILongContextBillingExtra(PlatformOpenAI, openaiextra)
	require.Error(t, err)
	a := webSearchTestAccount(7, "default")
	repo := &webSearchProbeRepo{accounts: []Account{a}}
	require.Error(t, (&adminServiceImpl{accountRepo: repo}).UpdateAccountExtra(context.Background(), a.ID, openaiextra))
	require.Nil(t, repo.saved)
}

func TestOpenAIWebSearchDefaultSchedulingRegression(t *testing.T) {
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
	for _, advanced := range []string{"false", "true"} {
		for _, tc := range []struct {
			name       string
			mode       string
			binding    int64
			capability OpenAIEndpointCapability
			wantID     int64
		}{
			{"normal_unconfigured", "", 0, OpenAIEndpointCapabilityChatCompletions, 7},
			{"normal_search_disabled", "disabled", 0, OpenAIEndpointCapabilityChatCompletions, 7},
			{"responses_search_disabled", "disabled", 0, OpenAIEndpointCapabilityResponses, 7},
			{"embeddings_search_disabled", "disabled", 0, OpenAIEndpointCapabilityEmbeddings, 7},
			{"normal_with_search_binding", "default", 8, OpenAIEndpointCapabilityChatCompletions, 7},
			{"search_unconfigured", "", 0, OpenAIEndpointCapabilityAlphaSearch, 7},
			{"search_disabled_skipped", "disabled", 0, OpenAIEndpointCapabilityAlphaSearch, 8},
			{"search_binding_overrides_priority", "default", 8, OpenAIEndpointCapabilityAlphaSearch, 8},
		} {
			t.Run(advanced+"/"+tc.name, func(t *testing.T) {
				groupID := int64(42)
				preferred := webSearchTestAccount(7, tc.mode)
				other := webSearchTestAccount(8, "default")
				preferred.Priority, other.Priority = 0, 100
				preferred.GroupIDs, other.GroupIDs = []int64{42}, []int64{42}
				cfg := &config.Config{}
				cfg.Gateway.OpenAIWS.LBTopK = 1
				svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{preferred, other}},
					cache: &schedulerTestGatewayCache{}, cfg: cfg,
					rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(advanced), concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
				ctx := WithOpenAIWebSearchRouting(context.Background(), tc.binding, "gpt-test")
				selection, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, &groupID, "", "", "gpt-test", nil, OpenAIUpstreamTransportHTTPSSE, tc.capability, false, false, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, tc.wantID, selection.Account.ID)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			})
		}
	}
}

func TestOpenAIWebSearchActualSchedulersHonorModesAndBinding(t *testing.T) {
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
	for _, advanced := range []string{"false", "true"} {
		for _, capability := range []OpenAIEndpointCapability{OpenAIEndpointCapabilityAlphaSearch, OpenAIEndpointCapabilityChatCompletions} {
			t.Run(advanced+"/"+string(capability), func(t *testing.T) {
				groupID := int64(42)
				search := webSearchTestAccount(7, "only")
				ordinary := webSearchTestAccount(8, "default")
				search.Priority, ordinary.Priority = 0, 100
				search.GroupIDs, ordinary.GroupIDs = []int64{42}, []int64{42}
				svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{search, ordinary}},
					cache: &schedulerTestGatewayCache{}, cfg: &config.Config{},
					rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(advanced),
					concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
				ctx := WithOpenAIWebSearchRouting(context.Background(), 7, "gpt-test")
				selection, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, &groupID, "", "", "gpt-test", nil, OpenAIUpstreamTransportHTTPSSE, capability, false, false, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				if capability == OpenAIEndpointCapabilityAlphaSearch {
					require.Equal(t, int64(7), selection.Account.ID)
				} else {
					require.Equal(t, int64(8), selection.Account.ID)
				}
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			})
		}
	}
}
