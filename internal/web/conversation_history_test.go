package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

func historyTestRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer history-fixture-key")
	r.Header.Set("User-Agent", "history-fixture-client")
	return r
}

func copyHistoryForTest(t *testing.T, messages []oaiMsg) []oaiMsg {
	t.Helper()
	b, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	var copy []oaiMsg
	if err := json.Unmarshal(b, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

func TestConversationHistoryRejectsBranchesThroughBothResolvers(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	r := historyTestRequest()
	first := []oaiMsg{
		{Role: "developer", Content: "Inspect the project."},
		{Role: "user", Content: "Read both files."},
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "call-a", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"a.txt"}`}},
			{"id": "call-b", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"b.txt"}`}},
		}},
		{Role: "tool", ToolCallID: "call-a", Content: "alpha"},
		{Role: "tool", ToolCallID: "call-b", Content: "beta"},
	}
	reply := oaiMsg{Role: "assistant", Content: "Use directory alpha.", ReasoningContent: "I checked both files."}
	history := completedConversationHistory(first, reply)
	s := &Server{convCache: newConversationCache(), sessionResolver: openSessionResolver()}
	result := chathub.Result{ConversationID: "conversation-alpha", SessionID: "session-alpha", Text: reply.Content.(string)}
	s.storeConvCache(tenantFromRequest(r), "account", defaultPublicModelName, result, "", first, reply)
	s.sessionResolver.Bind(result.SessionID, result.ConversationID, "account", &oaiReq{Messages: history}, "", r)

	tests := []struct {
		name string
		edit func([]oaiMsg)
		want bool
	}{
		{"unchanged", func([]oaiMsg) {}, true},
		{"assistant branch", func(m []oaiMsg) { m[5].Content = "Use directory beta." }, false},
		{"reasoning branch", func(m []oaiMsg) { m[5].ReasoningContent = "I checked only a.txt." }, false},
		{"tool result association", func(m []oaiMsg) { m[3].ToolCallID, m[4].ToolCallID = m[4].ToolCallID, m[3].ToolCallID }, false},
		{"tool type", func(m []oaiMsg) { m[2].ToolCalls[0]["type"] = "custom" }, false},
		{"tool arguments", func(m []oaiMsg) { m[2].ToolCalls[0]["function"].(map[string]any)["arguments"] = `{"path":"c.txt"}` }, false},
		{"message name", func(m []oaiMsg) { m[3].Name = "other-tool" }, false},
		{"instructions", func(m []oaiMsg) { m[0].Content = "Generate a title only." }, false},
		{"renamed IDs", func(m []oaiMsg) {
			m[2].ToolCalls[0]["id"], m[2].ToolCalls[1]["id"] = "new-a", "new-b"
			m[3].ToolCallID, m[4].ToolCallID = "new-a", "new-b"
		}, true},
		{"dangling ID", func(m []oaiMsg) { m[3].ToolCallID = "unknown" }, false},
		{"duplicate ID", func(m []oaiMsg) { m[2].ToolCalls[1]["id"] = "call-a" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := copyHistoryForTest(t, history)
			tt.edit(next)
			next = append(next, oaiMsg{Role: "user", Content: "Create the configuration in that directory."})
			if got := s.convCache.Match(tenantFromRequest(r), "account", defaultPublicModelName, next); (got != nil) != tt.want {
				t.Fatalf("fallback cache match=%v want %v", got != nil, tt.want)
			}
			if got := s.sessionResolver.Resolve(r, &oaiReq{Messages: next}); !got.IsNew != tt.want {
				t.Fatalf("session resolver=%+v want match=%v", got, tt.want)
			}
			if got := s.sessionResolver.CanContinue(r, &oaiReq{Messages: next}, result.ConversationID); got != tt.want {
				t.Fatalf("session/user alias match=%v want %v", got, tt.want)
			}
		})
	}
}

func TestConversationHistoryContinuesPendingToolCalls(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	r := historyTestRequest()
	first := []oaiMsg{{Role: "user", Content: "Read a.txt."}}
	reply := toolResponseMessage([]detectedToolCall{{ID: "old-id", Name: "read", Arguments: json.RawMessage(`{"path":"a.txt"}`)}}, chathub.Result{})
	s := &Server{convCache: newConversationCache(), sessionResolver: openSessionResolver()}
	res := chathub.Result{ConversationID: "conversation", SessionID: "session"}
	s.storeConvCache(tenantFromRequest(r), "account", defaultPublicModelName, res, "", first, reply)
	s.sessionResolver.Bind(res.SessionID, res.ConversationID, "account", &oaiReq{Messages: completedConversationHistory(first, reply)}, "", r)
	next := copyHistoryForTest(t, completedConversationHistory(first, reply))
	next[1].ToolCalls[0]["id"] = "new-id"
	next = append(next, oaiMsg{Role: "tool", ToolCallID: "new-id", Content: "alpha"})
	if err := validateToolConversation(next); err != nil {
		t.Fatal(err)
	}
	if got := s.convCache.Match(tenantFromRequest(r), "account", defaultPublicModelName, next); got == nil || got.MessageCount != 2 {
		t.Fatalf("pending tool continuation not matched: %+v", got)
	}
	if got := s.sessionResolver.Resolve(r, &oaiReq{Messages: next}); got.IsNew || got.HistoryLen != 2 {
		t.Fatalf("resolver must allow results after an already emitted tool call: %+v", got)
	}
}

func TestConversationHistoryUpgradeRetiresLegacyBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	t.Setenv("M365_SESSION_CACHE", path)
	r := historyTestRequest()
	r.Header.Set(sessionHeaderName, "logical-session")
	main := codexStyleMessages("Answer the actual question in detail.", "Describe the model.")
	history := completedConversationHistory(main, oaiMsg{Role: "assistant", Content: "Describe current model"})
	// This is the on-disk format written by the previous release, after its
	// cache routed a main request into a title-only cloud conversation.
	legacy := []map[string]any{{
		"sessionId": "old-session", "conversationId": "title-conversation", "accountId": "account",
		"createdAt": time.Now(), "lastUsedAt": time.Now(), "tenant": tenantFromRequest(r),
		"ipFingerprint": clientIPFingerprint(r), "explicitId": "logical-session", "contextHistory": history,
	}}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sr := openSessionResolver()
	next := append(copyHistoryForTest(t, history), oaiMsg{Role: "user", Content: "Please answer fully."})
	for _, explicit := range []string{"logical-session", ""} {
		r.Header.Set(sessionHeaderName, explicit)
		if got := sr.Resolve(r, &oaiReq{Messages: next}); !got.IsNew {
			t.Fatalf("legacy binding reused via %q: %+v", explicit, got)
		}
	}
	if sr.CanContinue(r, &oaiReq{Messages: next}, "title-conversation") {
		t.Fatal("legacy user/session alias bypassed migration")
	}
	if got, ok := sr.GetConversationForAdmin("title-conversation"); !ok || len(got.ContextHistory) != len(history) {
		t.Fatal("upgrade must preserve the old transcript for administrator inspection")
	}
	// After rebuilding a real cloud conversation, the verified binding survives
	// restart and remains reusable, including the complete assistant reply.
	r.Header.Set(sessionHeaderName, "logical-session")
	sr.Bind("new-session", "main-conversation", "account", &oaiReq{Messages: next}, "A complete model description.", r)
	if err := sr.persist.flushNowBlocking(); err != nil {
		t.Fatal(err)
	}
	sr = openSessionResolver()
	continuation := append(copyHistoryForTest(t, next), oaiMsg{Role: "assistant", Content: "A complete model description."}, oaiMsg{Role: "user", Content: "Continue."})
	if got := sr.Resolve(r, &oaiReq{Messages: continuation}); got.IsNew || got.ConversationID != "main-conversation" || got.SessionID != "new-session" || got.MatchedBy != "explicit" {
		t.Fatalf("verified history did not survive restart: %+v", got)
	}
	if _, ok := sr.GetConversationForAdmin("title-conversation"); !ok {
		t.Fatal("rebuilding an explicit session must preserve the old transcript")
	}
	sr.DeleteSession(tenantFromRequest(r), "old-session")
	if got := sr.Resolve(r, &oaiReq{Messages: continuation}); got.IsNew || got.SessionID != "new-session" || got.MatchedBy != "explicit" {
		t.Fatalf("deleting the legacy binding removed the new explicit alias: %+v", got)
	}
}

func TestConversationHistoryExplicitIndexIsIndependentOfDiskOrder(t *testing.T) {
	sr := newTenantResolver(t)
	r := historyTestRequest()
	r.Header.Set(sessionHeaderName, "logical-session")
	history := []oaiMsg{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "world"}}
	sr.Bind("new-session", "new-conversation", "account", &oaiReq{Messages: history}, "", r)
	current, _ := sr.GetSession(tenantFromRequest(r), "new-session")
	legacy := current
	legacy.SessionID, legacy.ConversationID = "old-session", "old-conversation"
	legacy.HistoryVersion = 0
	legacy.LastUsedAt = current.LastUsedAt.Add(-time.Minute)
	for _, bindings := range [][]sessionBinding{{legacy, current}, {current, legacy}} {
		b, _ := json.Marshal(bindings)
		if err := os.WriteFile(sr.path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		reloaded := openSessionResolver()
		next := append(copyHistoryForTest(t, history), oaiMsg{Role: "user", Content: "continue"})
		if got := reloaded.Resolve(r, &oaiReq{Messages: next}); got.IsNew || got.SessionID != "new-session" || got.MatchedBy != "explicit" {
			t.Fatalf("disk order selected the legacy alias: %+v", got)
		}
	}
}

func TestConversationHistoryRetirementIsTenantScoped(t *testing.T) {
	sr := newTenantResolver(t)
	a := historyTestRequest()
	b := historyTestRequest()
	b.Header.Set("Authorization", "Bearer other-fixture-key")
	history := []oaiMsg{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "world"}}
	sr.Bind("session-a", "shared-conversation", "account", &oaiReq{Messages: history}, "", a)
	sr.Bind("session-b", "shared-conversation", "account", &oaiReq{Messages: history}, "", b)
	sr.RetireConversation(tenantFromRequest(a), "shared-conversation")
	next := append(copyHistoryForTest(t, history), oaiMsg{Role: "user", Content: "continue"})
	if got := sr.Resolve(a, &oaiReq{Messages: next}); !got.IsNew {
		t.Fatalf("retired history was still reused: %+v", got)
	}
	if got := sr.Resolve(b, &oaiReq{Messages: next}); got.IsNew {
		t.Fatalf("another tenant's history was retired: %+v", got)
	}
}

func TestConversationHistoryExplicitSessionCannotBypassChecks(t *testing.T) {
	sr := newTenantResolver(t)
	r := historyTestRequest()
	r.Header.Set(sessionHeaderName, "shared-session")
	first := codexStyleMessages("Answer the question.", "Choose a directory.")
	sr.Bind("session", "conversation", "account", &oaiReq{Messages: first}, "Use alpha.", r)
	history := completedConversationHistory(first, oaiMsg{Role: "assistant", Content: "Use alpha."})
	next := append(copyHistoryForTest(t, history), oaiMsg{Role: "user", Content: "Continue."})
	if got := sr.Resolve(r, &oaiReq{Messages: next}); got.IsNew {
		t.Fatalf("valid explicit continuation did not match: %+v", got)
	}
	next[len(history)-1].Content = "Use beta."
	if got := sr.Resolve(r, &oaiReq{Messages: next}); !got.IsNew {
		t.Fatalf("explicit session bypassed history mismatch: %+v", got)
	}
	for _, body := range []oaiReq{{Messages: history, Model: "other-model"}, {Messages: history, AccountID: "other-account"}} {
		body.Messages = append(copyHistoryForTest(t, history), oaiMsg{Role: "user", Content: "Continue."})
		if got := sr.Resolve(r, &body); !got.IsNew {
			t.Fatalf("explicit session bypassed account/model scope: %+v", got)
		}
	}
}

func TestConversationHistoryDoesNotUseTruncatedSuffix(t *testing.T) {
	sr := newTenantResolver(t)
	r := historyTestRequest()
	history := []oaiMsg{{Role: "developer", Content: "Generate titles only."}, {Role: "user", Content: "hello"}, {Role: "assistant", Content: "Greeting"}}
	sr.Bind("session", "title-conversation", "account", &oaiReq{Messages: history}, "", r)
	changed := copyHistoryForTest(t, history)
	changed[0].Content = "Answer questions in detail."
	if got := sr.Resolve(r, &oaiReq{Messages: changed}); !got.IsNew {
		t.Fatalf("shared tail bypassed changed instructions: %+v", got)
	}
}

func TestConversationHistoryRetainsFullFingerprintBeyondTranscriptLimit(t *testing.T) {
	sr := newTenantResolver(t)
	r := historyTestRequest()
	history := []oaiMsg{{Role: "developer", Content: "Answer in detail."}}
	for i := 0; i < 260; i++ {
		history = append(history, oaiMsg{Role: "user", Content: fmt.Sprintf("question %d", i)}, oaiMsg{Role: "assistant", Content: fmt.Sprintf("answer %d", i)})
	}
	sr.Bind("session", "conversation", "account", &oaiReq{Messages: history}, "", r)
	if err := sr.persist.flushNowBlocking(); err != nil {
		t.Fatal(err)
	}
	sr = openSessionResolver()
	next := append(copyHistoryForTest(t, history), oaiMsg{Role: "user", Content: "Continue."})
	if got := sr.Resolve(r, &oaiReq{Messages: next}); got.IsNew || got.HistoryLen != len(history) {
		t.Fatalf("full history length was lost when the transcript was capped: %+v", got)
	}
	next[0].Content = "Generate a title only."
	if got := sr.Resolve(r, &oaiReq{Messages: next}); !got.IsNew {
		t.Fatalf("an edit outside the saved transcript bypassed the full hash: %+v", got)
	}
}
