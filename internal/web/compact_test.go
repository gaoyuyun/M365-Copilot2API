package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompactionEnvelopeRoundTrip(t *testing.T) {
	raw, err := encodeCompaction("preserve internal/web/compact.go and the pending test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, compactionEnvelopePrefix) {
		t.Fatalf("raw=%q", raw)
	}
	got, ok := decodeCompaction(raw)
	if !ok || got != "preserve internal/web/compact.go and the pending test" {
		t.Fatalf("got=%q ok=%t", got, ok)
	}
}

func TestResponsesCompactionTriggerDetection(t *testing.T) {
	if hasCompactionTrigger([]any{map[string]any{"type": "message"}}) {
		t.Fatal("message was treated as compaction trigger")
	}
	if hasCompactionTrigger([]any{
		map[string]any{"type": "compaction_trigger"},
		map[string]any{"type": "message"},
	}) {
		t.Fatal("non-final compaction trigger was accepted")
	}
	if !hasCompactionTrigger([]any{map[string]any{"type": "compaction_trigger"}}) {
		t.Fatal("missing compaction trigger")
	}
}

func TestCompactionResourceMatchesResponsesContract(t *testing.T) {
	item, _, err := compactionItem("summary")
	if err != nil {
		t.Fatal(err)
	}
	resource := compactionResource("gpt-5.6-sol", item, "resp_compact", map[string]any{"input_tokens": 1, "output_tokens": 2, "total_tokens": 3})
	if resource["object"] != "response.compaction" || resource["id"] != "resp_compact" {
		t.Fatalf("resource=%#v", resource)
	}
	if _, ok := resource["model"]; ok {
		t.Fatal("compact resource must not include a model field")
	}
	if _, err := json.Marshal(resource); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesInputAcceptsCompactionItemAndTrigger(t *testing.T) {
	encoded, err := json.Marshal(responsesRequest{Model: "gpt-5.6-sol", Input: []any{
		map[string]any{"type": "compaction", "encrypted_content": mustTestCompaction(t, "old summary")},
		map[string]any{"type": "compaction_trigger"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var body responsesRequest
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	o, err := body.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 1 || !strings.Contains(contentToString(o.Messages[0].Content), "old summary") {
		t.Fatalf("messages=%#v", o.Messages)
	}
}

func TestCompactionStreamEvents(t *testing.T) {
	item, _, err := compactionItem("streamed summary")
	if err != nil {
		t.Fatal(err)
	}
	resource := compactionResource("gpt-5.6-sol", item, "resp_compact", map[string]any{"total_tokens": 3})
	r := httptest.NewRequest("POST", "/v1/responses/compact", nil)
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	writeCompactionStream(w, r, resource, item)
	body := w.Body.String()
	for _, event := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_item.done",
		"event: response.completed",
		"m365_compaction_v1:",
	} {
		if !strings.Contains(body, event) {
			t.Fatalf("stream missing %q: %s", event, body)
		}
	}
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type=%q", w.Header().Get("Content-Type"))
	}
}

func TestStoreResponseHistoryEvictsExpiredAndOldest(t *testing.T) {
	now := time.Now()
	s := &Server{responseMessages: map[string]map[string]respHistory{"tenant": {}}}
	bucket := s.responseMessages["tenant"]
	bucket["expired"] = respHistory{At: now.Add(-2 * time.Hour)}
	for i := 0; i < maxResponsesPerTenant; i++ {
		bucket[fmt.Sprintf("current-%03d", i)] = respHistory{At: now.Add(time.Duration(i) * time.Second)}
	}

	s.storeResponseHistory("tenant", "new", []oaiMsg{{Role: "user", Content: "keep"}})

	if _, ok := bucket["expired"]; ok {
		t.Fatal("expired response history was not removed")
	}
	if _, ok := bucket["current-000"]; ok {
		t.Fatal("oldest response history was not evicted at the tenant limit")
	}
	if _, ok := bucket["new"]; !ok {
		t.Fatal("new response history was not stored")
	}
	if len(bucket) != maxResponsesPerTenant {
		t.Fatalf("history size=%d, want %d", len(bucket), maxResponsesPerTenant)
	}
}

func mustTestCompaction(t *testing.T, text string) string {
	t.Helper()
	raw, err := encodeCompaction(text)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
