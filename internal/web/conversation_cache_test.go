package web

import (
	"testing"

	"m365-copilot2api/internal/chathub"
)

// codexStyleMessages mirrors the shape Codex sends through /v1/responses: the
// gateway's shared system message, a request-specific developer message, an
// environment block and the user turn.
func codexStyleMessages(developer, user string) []oaiMsg {
	return []oaiMsg{
		{Role: "system", Content: "You are operating through the caller's local Codex execution bridge."},
		{Role: "developer", Content: developer},
		{Role: "user", Content: "<environment_context><cwd>/home/grayson/tests</cwd></environment_context>"},
		{Role: "user", Content: user},
	}
}

func cacheEntryFor(id string, messages []oaiMsg) *cachedConversation {
	return &cachedConversation{
		ConversationID: id,
		MessageCount:   len(messages),
		SystemPrompt:   systemPromptHash(messages),
		HistoryHash:    historyFingerprint(messages),
	}
}

// Regression for the production incident: Codex's background title-generation
// request and the user's real turn share the gateway's system message. The
// old system-prompt-only check routed the user's continuation into the title
// conversation, and the answer to "你是什么模型" became the stray title
// "说明当前模型".
func TestConvCacheRejectsDifferentHistoryBehindSharedSystemPrompt(t *testing.T) {
	c := newConversationCache()
	title := codexStyleMessages(
		"Title generation instructions.",
		"Produce a single-line task title. Do not answer the request.\n\nUser prompt:\n你好，请问你是什么模型",
	)
	c.Store("tenant-a", "acc", "gpt-5.6-sol", cacheEntryFor("title-conv", title))

	main := codexStyleMessages("You are Codex, an agent based on GPT-5.", "你好，请问你是什么模型")
	main = append(main,
		oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "exec", "arguments": `{"cmd":"pwd"}`}}}},
		oaiMsg{Role: "tool", ToolCallID: "call_1", Content: "/home/grayson/tests"},
	)
	if got := c.Match("tenant-a", "acc", "gpt-5.6-sol", main); got != nil {
		t.Fatalf("unrelated request with a shared system prompt reused conversation %q", got.ConversationID)
	}
}

func TestConvCacheMatchesStrictContinuationOnly(t *testing.T) {
	c := newConversationCache()
	first := codexStyleMessages("You are Codex.", "你好，请问你是什么模型")
	c.Store("tenant-a", "acc", "gpt-5.6-sol", cacheEntryFor("conv-1", first))

	next := append(cloneMessages(first),
		oaiMsg{Role: "assistant", Content: "我是 GPT-5 系列模型。"},
		oaiMsg{Role: "user", Content: "你能做什么？"},
	)
	got := c.Match("tenant-a", "acc", "gpt-5.6-sol", next)
	if got == nil || got.ConversationID != "conv-1" {
		t.Fatalf("strict continuation was not matched: %+v", got)
	}
	if c.Match("tenant-a", "acc", "gpt-5.6-sol", first) != nil {
		t.Fatal("a request with no new messages must not be treated as a continuation")
	}

	edited := cloneMessages(next)
	edited[3] = oaiMsg{Role: "user", Content: "帮我写一个脚本"}
	if c.Match("tenant-a", "acc", "gpt-5.6-sol", edited) != nil {
		t.Fatal("a request whose earlier turn differs must not reuse the conversation")
	}
}

func TestConvCacheIsTenantScoped(t *testing.T) {
	c := newConversationCache()
	first := codexStyleMessages("You are Codex.", "hello")
	c.Store("tenant-a", "acc", "gpt-5.6-sol", cacheEntryFor("conv-a", first))
	next := append(cloneMessages(first), oaiMsg{Role: "assistant", Content: "hi"}, oaiMsg{Role: "user", Content: "more"})
	if c.Match("tenant-b", "acc", "gpt-5.6-sol", next) != nil {
		t.Fatal("another API key must not continue this tenant's conversation")
	}
	if c.Match("tenant-a", "acc", "gpt-5.6-sol", next) == nil {
		t.Fatal("owning tenant should still match")
	}
	c.Invalidate("tenant-a", "acc", "gpt-5.6-sol")
	if c.Lookup("tenant-a", "acc", "gpt-5.6-sol") != nil {
		t.Fatal("invalidate should drop the tenant's entry")
	}
}

func TestConvCacheIgnoresRegeneratedToolCallIDs(t *testing.T) {
	c := newConversationCache()
	first := codexStyleMessages("You are Codex.", "list files")
	first = append(first,
		oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_old", "type": "function", "function": map[string]any{"name": "exec", "arguments": `{"cmd":"ls"}`}}}},
		oaiMsg{Role: "tool", ToolCallID: "call_old", Content: "a.txt"},
	)
	c.Store("tenant-a", "acc", "gpt-5.6-sol", cacheEntryFor("conv-1", first))

	replayed := cloneMessages(first)
	replayed[4] = oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_new", "type": "function", "function": map[string]any{"name": "exec", "arguments": `{"cmd":"ls"}`}}}}
	replayed[5] = oaiMsg{Role: "tool", ToolCallID: "call_new", Content: "a.txt"}
	replayed = append(replayed, oaiMsg{Role: "assistant", Content: "There is one file."}, oaiMsg{Role: "user", Content: "open it"})
	if c.Match("tenant-a", "acc", "gpt-5.6-sol", replayed) == nil {
		t.Fatal("regenerated tool-call ids must not break continuation matching")
	}

	changed := cloneMessages(replayed)
	changed[4] = oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_new", "type": "function", "function": map[string]any{"name": "exec", "arguments": `{"cmd":"rm -rf"}`}}}}
	if c.Match("tenant-a", "acc", "gpt-5.6-sol", changed) != nil {
		t.Fatal("different tool arguments must not match")
	}
}

func TestConvCacheFingerprintCoversCompleteContentAndToolSemantics(t *testing.T) {
	c := newConversationCache()
	first := codexStyleMessages("You are Codex.", "inspect this image")
	first[3].Content = []any{map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:image/png;base64,AA==", "detail": "high"},
	}}
	first = append(first, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{
		"id":   "call_old",
		"type": "custom",
		"function": map[string]any{
			"name":      "exec",
			"arguments": `{"input":"pwd"}`,
		},
	}}})
	c.Store("tenant-a", "acc", "gpt-5.6-sol", cacheEntryFor("conv-1", first))

	continuation := append(cloneMessages(first), oaiMsg{Role: "tool", ToolCallID: "call_new", Content: "/repo"})
	continuation[4] = oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{
		"id":   "call_new",
		"type": "custom",
		"function": map[string]any{
			"arguments": `{"input":"pwd"}`,
			"name":      "exec",
		},
	}}}
	if c.Match("tenant-a", "acc", "gpt-5.6-sol", continuation) == nil {
		t.Fatal("map key order and regenerated call ids must not change the fingerprint")
	}

	differentDetail := cloneMessages(continuation)
	differentDetail[3] = oaiMsg{Role: "user", Content: []any{map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:image/png;base64,AA==", "detail": "low"},
	}}}
	if c.Match("tenant-a", "acc", "gpt-5.6-sol", differentDetail) != nil {
		t.Fatal("different image detail must not reuse a conversation")
	}

	differentToolType := cloneMessages(continuation)
	differentToolType[4] = oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{
		"id":   "call_new",
		"type": "function",
		"function": map[string]any{
			"name":      "exec",
			"arguments": `{"input":"pwd"}`,
		},
	}}}
	if c.Match("tenant-a", "acc", "gpt-5.6-sol", differentToolType) != nil {
		t.Fatal("different tool-call type must not reuse a conversation")
	}
}

func TestConvCacheDisablesReuseForUnhashableContent(t *testing.T) {
	messages := codexStyleMessages("You are Codex.", "hello")
	messages[3].Content = make(chan int)
	entry := cacheEntryFor("conv-1", messages)
	if entry.HistoryHash != "" {
		t.Fatalf("unhashable content produced fingerprint %q", entry.HistoryHash)
	}
	c := newConversationCache()
	c.Store("tenant-a", "acc", "gpt-5.6-sol", entry)
	continuation := append(cloneMessages(messages), oaiMsg{Role: "user", Content: "more"})
	if c.Match("tenant-a", "acc", "gpt-5.6-sol", continuation) != nil {
		t.Fatal("a request without a stable fingerprint must not be reused")
	}
}

func TestSystemPromptHashCoversEveryInstructionMessage(t *testing.T) {
	a := []oaiMsg{{Role: "system", Content: "S"}, {Role: "developer", Content: "A"}, {Role: "user", Content: "q"}}
	b := []oaiMsg{{Role: "system", Content: "S"}, {Role: "developer", Content: "B"}, {Role: "user", Content: "q"}}
	if systemPromptHash(a) == systemPromptHash(b) {
		t.Fatal("system prompt hash ignored a differing developer message")
	}
	if systemPromptHash(a) != systemPromptHash(cloneMessages(a)) {
		t.Fatal("system prompt hash must be deterministic")
	}
	if systemPromptHash([]oaiMsg{{Role: "user", Content: "q"}}) != "" {
		t.Fatal("requests without instructions should hash to empty")
	}
}

func TestStoreConvCacheRecordsHistoryFingerprint(t *testing.T) {
	s := &Server{convCache: newConversationCache()}
	first := codexStyleMessages("You are Codex.", "你好，请问你是什么模型")
	reply := oaiMsg{Role: "assistant", Content: "我是 GPT-5。"}
	s.storeConvCache("tenant-a", "acc", "gpt-5.6-sol", chathub.Result{ConversationID: "conv-1", SessionID: "sess-1"}, "tone", first, reply)

	continuation := append(cloneMessages(first), oaiMsg{Role: "assistant", Content: "我是 GPT-5。"}, oaiMsg{Role: "user", Content: "谢谢"})
	got := s.convCache.Match("tenant-a", "acc", "gpt-5.6-sol", continuation)
	if got == nil || got.ConversationID != "conv-1" || got.MessageCount != len(first)+1 || got.TurnCount != 1 {
		t.Fatalf("stored entry did not match its own continuation: %+v", got)
	}

	other := codexStyleMessages("Title generation instructions.", "Produce a title for: 你好，请问你是什么模型")
	other = append(other, oaiMsg{Role: "user", Content: "extra"})
	if s.convCache.Match("tenant-a", "acc", "gpt-5.6-sol", other) != nil {
		t.Fatal("a different request must not continue the stored conversation")
	}

	s.storeConvCache("tenant-a", "acc", "gpt-5.6-sol", chathub.Result{ConversationID: "conv-1", SessionID: "sess-1"}, "tone", continuation, oaiMsg{Role: "assistant", Content: "不客气"})
	if got := s.convCache.Lookup("tenant-a", "acc", "gpt-5.6-sol"); got == nil || got.TurnCount != 2 || got.MessageCount != len(continuation)+1 {
		t.Fatalf("second turn should extend the same entry: %+v", got)
	}
}
