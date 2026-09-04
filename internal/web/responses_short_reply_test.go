package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This exercises the real Responses streaming adapter and its internal
// chat-completions pipe without touching the network. The public-identity path
// provides a deterministic answer before account selection.
func TestResponsesStreamPreservesPublicIdentityReply(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "true")
	s := &Server{}

	// Build a minimal /v1/responses streaming request where the input triggers
	// the publicIdentityAnswer early-return in openaiChat.
	body := map[string]any{
		"model":  "gpt-5.6-sol",
		"stream": true,
		"input":  "你是什么模型",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	s.responses(rw, req)

	result := rw.Body.String()

	// Parse SSE events
	var textDelta strings.Builder
	completed := false
	failed := false
	scanner := bufio.NewScanner(strings.NewReader(result))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		var obj map[string]any
		if json.Unmarshal([]byte(data), &obj) != nil {
			continue
		}
		switch obj["type"] {
		case "response.output_text.delta":
			if d, ok := obj["delta"].(string); ok {
				textDelta.WriteString(d)
			}
		case "response.completed":
			completed = true
		case "response.failed":
			failed = true
			t.Logf("response.failed: %v", obj)
		}
	}

	if failed {
		t.Fatal("response.failed received; openaiChat inner request failed to reach publicIdentityAnswer path")
	}
	if !completed {
		t.Fatal("response.completed not received")
	}
	want := publicIdentityAnswerForModel("gpt-5.6-sol", "zh")
	if got := textDelta.String(); got != want {
		t.Fatalf("Responses stream altered identity reply: got %q want %q", got, want)
	}
}

func TestOpenAIChatStreamPreservesPublicIdentityReply(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "true")
	s := &Server{}

	body := oaiReq{
		Model:  "gpt-5.6-sol",
		Stream: true,
		Messages: []oaiMsg{
			{Role: "user", Content: "你是什么模型"},
		},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()

	s.openaiChat(rw, req)

	result := rw.Body.String()

	var collected strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(result))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(line[6:]), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if c, ok := delta["content"].(string); ok {
			collected.WriteString(c)
		}
	}

	want := publicIdentityAnswerForModel("gpt-5.6-sol", "zh")
	if got := collected.String(); got != want {
		t.Fatalf("chat stream altered identity reply: got %q want %q", got, want)
	}
}
