package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/websearch"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type openAIMessagesWebSearchKey struct{}

// IsOpenAIMessagesWebSearchRequest deliberately excludes mixed-tool model turns.
// Configuring a search account must not redirect ordinary conversations.
func IsOpenAIMessagesWebSearchRequest(body []byte) bool {
	if !isOnlyWebSearchToolInBody(body) {
		return false
	}
	choice := gjson.GetBytes(body, "tool_choice")
	return choice.String() != "none" && choice.Get("type").String() != "none"
}

// WithOpenAIMessagesWebSearch uses the same dedicated-account constraint as
// alpha/search. It is installed by the authenticated handler, never by JSON.
func WithOpenAIMessagesWebSearch(ctx context.Context, accountID int64, model string) context.Context {
	ctx = WithOpenAIWebSearchRouting(ctx, accountID, model)
	return context.WithValue(ctx, openAIMessagesWebSearchKey{}, true)
}

func (s *OpenAIGatewayService) forwardMessagesWebSearch(ctx context.Context, c *gin.Context, account *Account, body []byte, defaultMappedModel string) (*OpenAIForwardResult, error) {
	if !account.supportsOpenAIEndpointCapabilityForRequest(ctx, OpenAIEndpointCapabilityAlphaSearch) {
		return nil, denyOpenAITurn("web_search_account_unavailable")
	}
	var inbound apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &inbound); err != nil {
		return nil, fmt.Errorf("parse messages web search: %w", err)
	}
	query := messagesWebSearchQuery(body)
	if query == "" {
		writeMessagesWebSearchError(c, http.StatusBadRequest, "invalid_request_error", "Web search requires a final user message containing text")
		return nil, fmt.Errorf("messages web search: no query in final user message")
	}
	model := strings.TrimSpace(defaultMappedModel)
	if model == "" {
		model = inbound.Model
	}
	conversionInput := inbound
	conversionInput.Model = normalizeOpenAIModelForUpstream(account, account.GetMappedModel(model))
	converted, err := apicompat.AnthropicToResponsesWithMinOutputTokens(&conversionInput, configuredMinOutputTokens(s.cfg))
	if err != nil {
		writeMessagesWebSearchError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, fmt.Errorf("convert messages web search: %w", err)
	}
	converted.Model, converted.Stream = model, true
	store := false
	converted.Store = &store
	// Search is explicit here, rather than an optional tool on a model turn.
	converted.ToolChoice = json.RawMessage(`"required"`)
	responsesBody, err := json.Marshal(converted)
	if err != nil {
		return nil, err
	}
	tool := map[string]any{"type": "web_search"}
	settings := map[string]any{}
	requestedTool := gjson.GetBytes(body, "tools.0")
	if blocked := requestedTool.Get("blocked_domains"); blocked.IsArray() && len(blocked.Array()) > 0 {
		writeMessagesWebSearchError(c, http.StatusBadRequest, "invalid_request_error", "OpenAI web search does not support blocked_domains; use allowed_domains instead")
		return nil, fmt.Errorf("messages web search: blocked_domains is unsupported")
	}
	filters := map[string]json.RawMessage{}
	for _, key := range []string{"allowed_domains"} {
		if value := requestedTool.Get(key); value.IsArray() {
			filters[key] = json.RawMessage(value.Raw)
			settings[key] = json.RawMessage(value.Raw)
		}
	}
	if len(filters) > 0 {
		tool["filters"] = filters
	}
	if location := requestedTool.Get("user_location"); location.IsObject() {
		tool["user_location"] = json.RawMessage(location.Raw)
		settings["user_location"] = json.RawMessage(location.Raw)
	}
	responsesBody, err = sjson.SetBytes(responsesBody, "tools", []any{tool})
	if err != nil {
		return nil, err
	}
	policyBody, _, policyErr := ApplyOpenAIReasoningEffortPolicyFromContext(ctx, responsesBody)
	if policyErr != nil {
		if IsReasoningEffortPolicyDenied(policyErr) {
			MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
			writeMessagesWebSearchError(c, http.StatusForbidden, "forbidden_error", policyErr.Error())
		}
		return nil, policyErr
	}
	responsesBody = policyBody
	alphaBody, err := json.Marshal(map[string]any{
		"model": model, "input": json.RawMessage(converted.Input),
		"commands": map[string]any{"search_query": []any{map[string]string{"q": query}}},
		"settings": settings,
	})
	if err != nil {
		return nil, err
	}
	start := time.Now()
	clientModel := openAIClientResponseModel(c, inbound.Model)
	options := &openAIAlphaSearchForwardOptions{responsesBody: responsesBody, useResponsesWebSearch: true}
	options.writeResponse = func(status int, _ http.Header, payload []byte) error {
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			writeMessagesWebSearchError(c, status, "api_error", "Upstream web search failed")
			return fmt.Errorf("messages web search upstream status %d", status)
		}
		response, output, err := parseMessagesWebSearchResponse(payload, query)
		if err != nil {
			return err
		}
		if inbound.Stream {
			_, err = writeWebSearchStreamResponseWithText(c, query, response, clientModel, start, output)
		} else {
			_, err = writeWebSearchNonStreamResponseWithText(c, query, response, clientModel, start, output)
		}
		return err
	}
	SetActualOpenAIUpstreamEndpoint(c, "/v1/responses")
	result, err := s.forwardAlphaSearch(ctx, c, account, alphaBody, options)
	if result != nil {
		result.Model, result.Stream = inbound.Model, inbound.Stream
	}
	return result, err
}

func writeMessagesWebSearchError(c *gin.Context, status int, errType, message string) {
	if c.Writer.Written() && strings.HasPrefix(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
		_ = flushSSEJSON(c.Writer, "error", gin.H{"type": "error", "error": gin.H{"type": errType, "message": message}})
		return
	}
	writeAnthropicError(c, status, errType, message)
}

func messagesWebSearchQuery(body []byte) string {
	messages := gjson.GetBytes(body, "messages").Array()
	if len(messages) == 0 || messages[len(messages)-1].Get("role").String() != "user" {
		return ""
	}
	content := messages[len(messages)-1].Get("content")
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String())
	}
	var parts []string
	for _, block := range content.Array() {
		if block.Get("type").String() == "text" {
			if part := strings.TrimSpace(block.Get("text").String()); part != "" {
				parts = append(parts, part)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// A completed model answer alone is not proof of a successful search. Only
// return a search success (and bill it) after the hosted tool has completed.
func hasCompletedMessagesWebSearchCall(body []byte) bool {
	for _, block := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n\n") {
		event := gjson.Parse(openAIAlphaSearchSSEData(block))
		if event.Get("type").String() != "response.completed" {
			continue
		}
		for _, output := range event.Get("response.output").Array() {
			if output.Get("type").String() == "web_search_call" && output.Get("status").String() == "completed" {
				return true
			}
		}
	}
	return false
}

func parseMessagesWebSearchResponse(body []byte, query string) (*websearch.SearchResponse, string, error) {
	if !gjson.ValidBytes(body) {
		return nil, "", fmt.Errorf("messages web search: invalid search response")
	}
	output := gjson.GetBytes(body, "output")
	results := gjson.GetBytes(body, "results")
	if output.Type != gjson.String && !results.IsArray() {
		// An opaque encrypted_output cannot be decoded into a Messages result.
		return nil, "", fmt.Errorf("messages web search: response has no readable search output")
	}
	response := &websearch.SearchResponse{Query: query}
	seen := make(map[string]struct{})
	for _, result := range results.Array() {
		rawURL := strings.TrimSpace(result.Get("url").String())
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		if _, duplicate := seen[rawURL]; duplicate {
			continue
		}
		seen[rawURL] = struct{}{}
		title := strings.TrimSpace(result.Get("title").String())
		if title == "" {
			title = rawURL
		}
		snippet := result.Get("snippet").String()
		if snippet == "" {
			snippet = result.Get("page_content").String()
		}
		response.Results = append(response.Results, websearch.SearchResult{URL: rawURL, Title: title, Snippet: snippet, PageAge: result.Get("page_age").String()})
	}
	text := output.String()
	if strings.TrimSpace(text) == "" {
		text = buildTextSummary(query, response.Results)
	}
	return response, text, nil
}
