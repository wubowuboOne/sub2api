package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const AccountTestModeWebSearch = "web_search"

func openAIWebSearchProbeResult(account *Account, status int, body []byte, pat bool) (*bool, string) {
	known := func(v bool) *bool { return &v }
	if status >= 200 && status < 300 {
		if pat {
			_, sources, err := parseOpenAIResponsesSSEForAlphaSearch(body)
			if err == nil && len(sources) > 0 {
				return known(true), ""
			}
			return nil, "Responses search completed without verifiable search sources, or the stream did not complete"
		}
		if gjson.ValidBytes(body) && gjson.GetBytes(body, "error").Type == gjson.Null &&
			(gjson.GetBytes(body, "encrypted_output").Type == gjson.String && strings.TrimSpace(gjson.GetBytes(body, "encrypted_output").String()) != "" ||
				gjson.GetBytes(body, "output").Type == gjson.String && strings.TrimSpace(gjson.GetBytes(body, "output").String()) != "" ||
				gjson.GetBytes(body, "results").IsArray() && len(gjson.GetBytes(body, "results").Array()) > 0) {
			return known(true), ""
		}
		return nil, "Upstream returned 2xx without a valid standalone search result"
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if message == "" {
		message = fmt.Sprintf("Search probe returned HTTP %d", status)
	}
	// A missing model is not evidence that the endpoint itself is missing.
	if !pat && isOpenAIAlphaSearchEndpointUnsupported(account, status) &&
		!strings.Contains(strings.ToLower(message), "model") {
		return known(false), truncateString(message, 1024)
	}
	return nil, truncateString(message, 1024)
}

func (s *AccountTestService) testOpenAIWebSearchConnection(c *gin.Context, account *Account, model, query string) error {
	gateway := s.openaiGatewayService
	if gateway == nil {
		return s.sendErrorAndEnd(c, "OpenAI search gateway is unavailable")
	}
	if model = strings.TrimSpace(model); model == "" {
		model = openai.DefaultTestModel
	}
	if query = strings.TrimSpace(query); query == "" {
		query = "OpenAI official website"
	}
	upstreamModel := normalizeOpenAIModelForUpstream(account, account.GetMappedModel(model))
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Probing the standalone web search route..."})
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	var supported *bool
	status := 0
	message := ""
	persisted := false
	var persistErr error
	persist := func() error {
		if persisted {
			return persistErr
		}
		persisted = true
		persistCtx, stop := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
		defer stop()
		fresh, err := s.accountRepo.GetByID(persistCtx, account.ID)
		if err != nil {
			persistErr = fmt.Errorf("load account before saving search probe: %w", err)
			return persistErr
		}
		if openAIWebSearchRouteFingerprint(fresh) != openAIWebSearchRouteFingerprint(account) {
			persistErr = fmt.Errorf("account route changed during the search probe; run the probe again")
			return persistErr
		}
		probe := map[string]any{"route": openAIWebSearchRouteFingerprint(account), "model": upstreamModel,
			"checked_at": time.Now().UTC().Format(time.RFC3339), "status": status, "error": message}
		// Transient failures update diagnostics without erasing earlier evidence.
		if old, ok := fresh.Extra["openai_web_search_probe"].(map[string]any); ok && old["route"] == probe["route"] && old["model"] == upstreamModel {
			if value, ok := old["supported"].(bool); ok {
				probe["supported"] = value
			}
		}
		if supported != nil {
			probe["supported"] = *supported
		}
		persistErr = s.accountRepo.UpdateExtra(persistCtx, account.ID, map[string]any{"openai_web_search_probe": probe})
		return persistErr
	}
	defer persist()
	fail := func(err error) error {
		message = truncateString(sanitizeUpstreamErrorMessage(err.Error()), 1024)
		persist()
		return s.sendErrorAndEnd(c, message)
	}
	if _, err := gateway.prepareCodexAccountIdentitySource(ctx, c, account); err != nil {
		return fail(err)
	}
	token, _, err := gateway.GetAccessToken(ctx, account)
	if err != nil {
		return fail(err)
	}
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	if err := gateway.ensureOpenAIAlphaSearchAuthMetadata(ctx, account, token, proxy); err != nil {
		return fail(err)
	}
	body, err := json.Marshal(map[string]any{"id": fmt.Sprintf("search-probe-%d-%d", account.ID, time.Now().UnixNano()),
		"model": upstreamModel, "commands": map[string]any{"search_query": []any{map[string]any{"q": query}}}})
	if err != nil {
		return fail(err)
	}
	var req *http.Request
	pat := account.IsOpenAIPersonalAccessToken()
	if pat {
		var responsesBody []byte
		responsesBody, err = buildOpenAIAlphaSearchResponsesWebSearchBody(body, upstreamModel)
		if err == nil {
			req, err = gateway.buildOpenAIAlphaSearchResponsesWebSearchRequest(ctx, c, account, body, responsesBody, token)
		}
	} else {
		req, err = gateway.buildOpenAIAlphaSearchRequest(ctx, c, account, body, token)
	}
	if err != nil {
		return fail(err)
	}
	resp, err := gateway.doOpenAIUpstream(req, proxy, account)
	if err != nil {
		return fail(fmt.Errorf("Search probe transport failed: %w", err))
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return fail(err)
	}
	if len(responseBody) > 2<<20 {
		return fail(fmt.Errorf("search probe response exceeded 2 MiB"))
	}
	supported, message = openAIWebSearchProbeResult(account, status, responseBody, pat)
	if err := persist(); err != nil {
		return s.sendErrorAndEnd(c, "Search probe result could not be saved; please retry the probe")
	}
	if supported == nil || !*supported {
		return s.sendErrorAndEnd(c, message)
	}
	s.sendEvent(c, TestEvent{Type: "content", Text: "Standalone web search is supported"})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}
