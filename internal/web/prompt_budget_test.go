package web

import (
	"testing"

	"m365-copilot2api/internal/chathub"
)

func TestSelectPromptMessagesKeepsToolLoopAndAnchor(t *testing.T) {
	t.Setenv("M365_INPUT_BUDGET_TOKENS", "9000")
	messages := []oaiMsg{
		{Role: "system", Content: "follow policy"},
		{Role: "user", Content: "Work in /workspace/project and deploy server 12"},
		{Role: "assistant", Content: "old context " + string(make([]byte, 1000))},
		{Role: "user", Content: "current task"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "exec", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "call_1", Content: "done"},
	}
	selected, stats := selectPromptMessages(messages, "gpt-5.5", []chathub.Tool{{Type: "function", Function: []byte(`{"name":"exec"}`)}}, nil, false)
	if stats.Exceeded {
		t.Fatalf("unexpected budget error: %s", stats.ExceededReason)
	}
	if len(selected) < 4 {
		t.Fatalf("selected=%d stats=%+v", len(selected), stats)
	}
	if selected[len(selected)-1].Role != "tool" || selected[len(selected)-2].Role != "assistant" {
		t.Fatalf("tool loop was split: %+v", selected)
	}
	foundAnchor := false
	for _, m := range selected {
		if contentToString(m.Content) == "Work in /workspace/project and deploy server 12" {
			foundAnchor = true
		}
	}
	if !foundAnchor {
		t.Fatal("durable task anchor was dropped")
	}
}
