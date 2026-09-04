package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

func TestConversationListAndDetailUseCompleteLocalHistory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	store, err := auth.OpenStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store, sessionResolver: openSessionResolver()}

	oldCloudClient := m365CloudClient
	m365CloudClient = nil
	defer func() { m365CloudClient = oldCloudClient }()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer detail-key")
	req.Header.Set(sessionHeaderName, "session-detail")
	body := &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "show the complete answer"},
		{Role: "assistant", Content: "complete body", ReasoningContent: "complete reasoning"},
	}}
	s.sessionResolver.Bind("", "conversation-detail", "account-a", body, "", req)

	listRecorder := httptest.NewRecorder()
	listRequest := httptest.NewRequest(http.MethodGet, "/api/m365/conversations", nil)
	s.handleM365Conversations(listRecorder, listRequest)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list struct {
		Count int              `json:"count"`
		Data  []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Count != 1 || list.Data[0]["messageCount"] != float64(2) || list.Data[0]["historyAvailable"] != true || list.Data[0]["source"] != "gateway" {
		t.Fatalf("list response=%s", listRecorder.Body.String())
	}

	detailRecorder := httptest.NewRecorder()
	detailRequest := httptest.NewRequest(http.MethodGet, "/api/m365/conversations/detail?id=conversation-detail", nil)
	s.handleM365ConversationDetail(detailRecorder, detailRequest)
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail struct {
		ConversationID string   `json:"conversationId"`
		Messages       []oaiMsg `json:"messages"`
	}
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.ConversationID != "conversation-detail" || len(detail.Messages) != 2 {
		t.Fatalf("detail response=%s", detailRecorder.Body.String())
	}
	if detail.Messages[1].ReasoningContent != "complete reasoning" || contentToString(detail.Messages[1].Content) != "complete body" {
		t.Fatalf("assistant message=%#v", detail.Messages[1])
	}
}

func TestConversationListAndDetailPreferLatestLocalBinding(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	store, err := auth.OpenStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store, sessionResolver: openSessionResolver()}

	oldCloudClient := m365CloudClient
	m365CloudClient = nil
	defer func() { m365CloudClient = oldCloudClient }()

	oldRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	oldRequest.Header.Set("Authorization", "Bearer tenant-old")
	s.sessionResolver.Bind("session-old", "shared-conversation", "account-old", &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "old question"},
	}}, "", oldRequest)

	newRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	newRequest.Header.Set("Authorization", "Bearer tenant-new")
	s.sessionResolver.Bind("session-new", "shared-conversation", "account-new", &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "new question"},
		{Role: "assistant", Content: "new answer"},
	}}, "", newRequest)

	// Make the ordering deterministic rather than relying on two time.Now calls.
	s.sessionResolver.mu.Lock()
	oldBinding := s.sessionResolver.sessions["session-old"]
	oldBinding.LastUsedAt = time.Now().Add(-time.Hour)
	s.sessionResolver.sessions["session-old"] = oldBinding
	newBinding := s.sessionResolver.sessions["session-new"]
	newBinding.LastUsedAt = time.Now()
	s.sessionResolver.sessions["session-new"] = newBinding
	s.sessionResolver.mu.Unlock()

	listRecorder := httptest.NewRecorder()
	s.handleM365Conversations(listRecorder, httptest.NewRequest(http.MethodGet, "/api/m365/conversations", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0]["sessionId"] != "session-new" || list.Data[0]["messageCount"] != float64(2) {
		t.Fatalf("list did not select latest binding: %s", listRecorder.Body.String())
	}

	detailRecorder := httptest.NewRecorder()
	s.handleM365ConversationDetail(detailRecorder, httptest.NewRequest(http.MethodGet, "/api/m365/conversations/detail?id=shared-conversation", nil))
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail struct {
		SessionID string   `json:"sessionId"`
		Messages  []oaiMsg `json:"messages"`
	}
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.SessionID != "session-new" || len(detail.Messages) != 2 {
		t.Fatalf("detail did not select latest binding: %s", detailRecorder.Body.String())
	}
}

func TestConversationDetailPageContainsCompleteViews(t *testing.T) {
	body, err := os.ReadFile("../../web/conversation.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, needle := range []string{
		`id="conversationView"`,
		`id="jsonView"`,
		"reasoning_content",
		"tool_calls",
		"/api/m365/conversations/detail?id=",
	} {
		if !strings.Contains(page, needle) {
			t.Fatalf("conversation page missing %q", needle)
		}
	}
	embedded, err := os.ReadFile("web/conversation.html")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, embedded) {
		t.Fatal("web/conversation.html and its embedded copy are not byte-identical")
	}
}

func TestConversationListPageHandlesCloudOnlyRows(t *testing.T) {
	body, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, needle := range []string{
		"x.historyAvailable===true",
		`data-history="${hasHistory?'1':'0'}"`,
		"Cloud only",
		"No local transcript: only conversations created through this gateway keep their history",
	} {
		if !strings.Contains(page, needle) {
			t.Fatalf("conversation list page missing %q", needle)
		}
	}
}

func TestConversationTimestampPrefersUpdateTime(t *testing.T) {
	created := time.Now().Add(-time.Hour).UnixMilli()
	updated := time.Now().UnixMilli()
	if got := conversationTimestamp(map[string]any{"createTimeUtc": created, "updateTimeUtc": updated}); got != updated {
		t.Fatalf("timestamp=%d want %d", got, updated)
	}
}
