package apicompat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
)

func TestGroupPromptPreservedAcrossGatewayProtocolConversions(t *testing.T) {
	for _, useDefault := range []bool{false, true} {
		prompt := "Always append GPCHK-CLAUDE-7F29."
		label := "custom-marker"
		if useDefault {
			prompt = groupprompt.DefaultPrompt
			label = "bundled-default"
		}
		for _, input := range []struct{ label, body string }{
			{"string-system", `{"model":"test-model","max_tokens":64,"system":"CLIENT-SYSTEM-KEEP","messages":[{"role":"user","content":"hello"}]}`},
			{"array-system", `{"model":"test-model","max_tokens":64,"system":[{"type":"text","text":"CLIENT-SYSTEM-KEEP"}],"messages":[{"role":"user","content":"hello"}]}`},
		} {
			for _, secondEnabled := range []bool{false, true} {
				caseName := label + "/" + input.label + "/second-disabled"
				if secondEnabled {
					caseName = label + "/" + input.label + "/second-enabled"
				}
				t.Run(caseName, func(t *testing.T) {
					aBody, err := groupprompt.Prepend([]byte(input.body), "messages", prompt)
					if err != nil {
						t.Fatal(err)
					}
					var aRequest apicompat.AnthropicRequest
					if err := json.Unmarshal(aBody, &aRequest); err != nil {
						t.Fatal(err)
					}
					bRequest, err := apicompat.AnthropicToResponses(&aRequest)
					if err != nil {
						t.Fatal(err)
					}
					var items []apicompat.ResponsesInputItem
					if err := json.Unmarshal(bRequest.Input, &items); err != nil {
						t.Fatal(err)
					}
					if len(items) == 0 || items[0].Role != "developer" {
						t.Fatal("expected first developer input")
					}
					var parts []apicompat.ResponsesContentPart
					if err := json.Unmarshal(items[0].Content, &parts); err != nil {
						t.Fatal(err)
					}
					var textParts []string
					for _, part := range parts {
						textParts = append(textParts, part.Text)
					}
					developerText := strings.Join(textParts, "\n\n")
					if !strings.Contains(developerText, prompt) {
						t.Fatal("group prompt lost entering second layer")
					}
					if !strings.Contains(developerText, "CLIENT-SYSTEM-KEEP") {
						t.Fatal("client system lost")
					}
					bBody, err := json.Marshal(bRequest)
					if err != nil {
						t.Fatal(err)
					}
					secondPrompt := groupprompt.Config{Enabled: secondEnabled, Content: "SECOND-GROUP-KEEP"}.Prompt("")
					bBody, err = groupprompt.Prepend(bBody, "responses", secondPrompt)
					if err != nil {
						t.Fatal(err)
					}
					var bAfter apicompat.ResponsesRequest
					if err := json.Unmarshal(bBody, &bAfter); err != nil {
						t.Fatal(err)
					}
					if string(bAfter.Input) != string(bRequest.Input) {
						t.Fatal("second group changed existing developer input")
					}
					if secondEnabled && bAfter.Instructions != secondPrompt {
						t.Fatal("second prompt missing")
					}
					// If layer B forwards Responses back to a Claude account, developer must return to system.
					finalAnthropic, err := apicompat.ResponsesToAnthropicRequest(&bAfter)
					if err != nil {
						t.Fatal(err)
					}
					var finalSystem string
					if err := json.Unmarshal(finalAnthropic.System, &finalSystem); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(finalSystem, prompt) {
						t.Fatal("group prompt lost in reverse bridge")
					}
					if !strings.Contains(finalSystem, "CLIENT-SYSTEM-KEEP") {
						t.Fatal("client system lost in reverse bridge")
					}
					if secondEnabled && !strings.Contains(finalSystem, secondPrompt) {
						t.Fatal("second group prompt lost in reverse bridge")
					}
					t.Logf("PASS: %d prompt bytes preserved in developer input and reverse bridge; second group enabled=%v", len(prompt), secondEnabled)
				})
			}
		}
	}
}
