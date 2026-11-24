package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type openAIAgentRetryProxy struct {
	id  int64
	url [32]byte
}

func openAIAgentHTTP502RetryProxy(account *Account, proxyURL string) *openAIAgentRetryProxy {
	if account.OpenAIRequestProxyEnabled() || (account.Proxy != nil &&
		(account.Proxy.FallbackMode == FallbackModeDirect || account.Proxy.FallbackMode == FallbackModeProxy)) {
		return nil
	}
	proxy := &openAIAgentRetryProxy{url: sha256.Sum256([]byte(proxyURL))}
	if account.ProxyID != nil {
		proxy.id = *account.ProxyID
	}
	return proxy
}

func (s *OpenAIGatewayService) newOpenAIAgentHTTP502RetryError(ctx context.Context, c *gin.Context, account *Account, req *http.Request, resp *http.Response, payload []byte, proxy *openAIAgentRetryProxy) *UpstreamFailoverError {
	const gatewayError = "error code: 502"
	if resp.StatusCode != http.StatusBadGateway || proxy == nil || !bytes.Equal(bytes.TrimSpace(payload), []byte(gatewayError)) {
		return nil
	}
	r := s.openAIAgentPinnedRetryRequest(ctx, c, account, http.StatusBadGateway)
	if r == nil || !account.IsOpenAIApiKey() || OpenAIImageGenerationIntentFromContext(ctx) || req.GetBody == nil {
		return nil
	}
	reader, err := req.GetBody()
	if err != nil {
		return nil
	}
	defer reader.Close()
	wireBody, err := io.ReadAll(reader)
	if err != nil || !openAIAgentHTTP502Replayable(wireBody) {
		return nil
	}
	return newOpenAIAgentPinnedHTTPRetryError(ctx, c, account, req, r, resp, payload, gatewayError, OpenAIAgentHTTP502RetryReason, proxy)
}

// A bare 502 does not prove the upstream rejected the request before running
// it. Limit replay to stateless inference with no server-side tool execution.
func openAIAgentHTTP502Replayable(body []byte) bool {
	if !utf8.Valid(body) || !openAIEncryptedJSONUnambiguous(body) || openAI409HasProviderState(body) {
		return false
	}
	root := gjson.ParseBytes(body)
	if root.Get("store").Type != gjson.False || (root.Get("background").Exists() && root.Get("background").Type != gjson.False) ||
		root.Get("prompt").Exists() || !openAIAgentHTTP502ClientTools(root.Get("tools")) ||
		!openAIAgentHTTP502ClientTools(root.Get("additional_tools")) {
		return false
	}
	if choice := root.Get("tool_choice"); choice.Exists() {
		switch choice.String() {
		case "auto", "none", "required":
		default:
			switch choice.Get("type").String() {
			case "function", "custom":
			case "allowed_tools":
				if !openAIAgentHTTP502ClientTools(choice.Get("tools")) {
					return false
				}
			default:
				return false
			}
		}
	}
	input := root.Get("input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		kind := item.Get("type")
		if !item.IsObject() || (kind.Exists() && kind.Type != gjson.String) {
			return false
		}
		switch kind.String() {
		case "", "message", "agent_message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "additional_tools":
		case "tool_search_call", "tool_search_output":
			if item.Get("execution").String() != "client" {
				return false
			}
		default:
			return false
		}
		if !openAIAgentHTTP502ClientTools(item.Get("tools")) {
			return false
		}
	}
	return true
}

func openAIAgentHTTP502ClientTools(tools gjson.Result) bool {
	if !tools.Exists() {
		return true
	}
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		execution := tool.Get("execution")
		if execution.Exists() && execution.String() != "client" {
			return false
		}
		switch tool.Get("type").String() {
		case "function", "custom":
		case "tool_search":
			if !execution.Exists() {
				return false
			}
		case "namespace":
			if !openAIAgentHTTP502ClientTools(tool.Get("tools")) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
