package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"m365-copilot2api/internal/chathub"
)

const compactionEnvelopePrefix = "m365_compaction_v1:"

// compactEnvelope is deliberately opaque to the client while remaining
// replayable by this gateway. ChatHub returns ordinary text, so it cannot
// produce OpenAI's provider-owned encrypted_content directly.
type compactEnvelope struct {
	Version   int    `json:"version"`
	CreatedAt int64  `json:"created_at"`
	Text      string `json:"text"`
}

func encodeCompaction(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("ChatHub returned an empty compaction summary")
	}
	b, err := json.Marshal(compactEnvelope{Version: 1, CreatedAt: time.Now().Unix(), Text: text})
	if err != nil {
		return "", err
	}
	return compactionEnvelopePrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCompaction(raw string) (string, bool) {
	if !strings.HasPrefix(raw, compactionEnvelopePrefix) {
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, compactionEnvelopePrefix))
	if err != nil {
		return "", false
	}
	var envelope compactEnvelope
	if json.Unmarshal(b, &envelope) != nil || envelope.Version != 1 || strings.TrimSpace(envelope.Text) == "" {
		return "", false
	}
	return envelope.Text, true
}

func hasCompactionTrigger(input any) bool {
	items, ok := input.([]any)
	if !ok || len(items) == 0 {
		return false
	}
	item, _ := items[len(items)-1].(map[string]any)
	typ, _ := item["type"].(string)
	return typ == "compaction_trigger"
}

func compactPrompt(messages []oaiMsg) string {
	prompt, _ := flattenPromptMessages(messages, nil)
	return strings.TrimSpace(prompt)
}

func buildCompactionPrompt(messages []oaiMsg) string {
	return `You are the context compaction engine for a coding agent. Produce one concise, durable context summary for the next model turn. Preserve the user's goal, constraints, decisions, relevant files and identifiers, completed tool results, failures, and pending work. Preserve exact commands, paths, API names, and quoted values when they matter. Do not claim an action happened unless the conversation contains completed evidence. Do not answer the user or add commentary. Return only the summary text.

CONVERSATION TO COMPACT:
` + compactPrompt(messages)
}

func compactionItem(summary string) (map[string]any, string, error) {
	encrypted, err := encodeCompaction(summary)
	if err != nil {
		return nil, "", err
	}
	return map[string]any{
		"type":              "compaction",
		"id":                "cmp_" + uuid.NewString(),
		"encrypted_content": encrypted,
	}, encrypted, nil
}

func compactionResource(model string, item map[string]any, id string, usage map[string]any) map[string]any {
	return map[string]any{
		"id":         id,
		"object":     "response.compaction",
		"created_at": time.Now().Unix(),
		"output":     []any{item},
		"usage":      usage,
	}
}

func (s *Server) compactResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponsesError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body responsesRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20)).Decode(&body); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if body.PreviousResponseID != "" {
		tenant := extractAPIKey(r)
		s.responseMu.Lock()
		bucket := s.responseMessages[tenant]
		prior, ok := bucket[body.PreviousResponseID]
		s.responseMu.Unlock()
		if !ok || len(prior.Messages) == 0 {
			writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "unknown previous_response_id")
			return
		}
		o.Messages = append(append([]oaiMsg(nil), prior.Messages...), o.Messages...)
	}
	if len(o.Messages) == 0 {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "input or previous_response_id required")
		return
	}

	startedAt := time.Now()
	acc, err := s.resolveAccount(o.AccountID)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "account_error", err.Error())
		return
	}
	if acc.OID == "" || acc.TID == "" {
		if oid, tid := extractOIDTID(acc.AccessToken); oid != "" {
			acc.OID, acc.TID = oid, tid
		}
	}
	if acc.OID == "" || acc.TID == "" {
		writeResponsesError(w, http.StatusBadRequest, "account_error", "account missing oid/tid")
		return
	}
	tone, toneErr := reasoningTone(body.Model, firstNonEmpty(o.ReasoningEffort, "medium"))
	if toneErr != nil {
		tone = "magic"
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	res, err := s.chatWithAccount(ctx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, chathub.Request{
		Text: buildCompactionPrompt(o.Messages), Tone: tone,
	})
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	item, _, err := compactionItem(res.Text)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	model := firstNonEmpty(body.Model, "m365-copilot")
	usage := estimateResponsesUsage(model, o.Messages, nil, nil, res.Text).Values
	id := "resp_" + uuid.NewString()
	resource := compactionResource(model, item, id, usage)
	// A compact response is also a valid previous_response_id anchor. Store
	// the decoded form so the next request does not need to understand our
	// envelope and does not replay the full pre-compaction history.
	if encrypted, ok := item["encrypted_content"].(string); ok {
		if summary, decoded := decodeCompaction(encrypted); decoded {
			s.storeResponseHistory(extractAPIKey(r), id, []oaiMsg{{Role: "system", Content: "[compacted context]\n" + summary}})
		}
	}
	s.usage.record(UsageRecord{Time: time.Now(), APIKeyPrefix: extractAPIKey(r), AccountEmail: acc.Email, Model: model, Endpoint: "/v1/responses/compact", InputTokens: int64(usage["input_tokens"].(int)), OutputTokens: int64(usage["output_tokens"].(int)), DurationMs: time.Since(startedAt).Milliseconds(), Status: http.StatusOK})
	if body.Stream {
		writeCompactionStream(w, r, resource, item)
		return
	}
	jsonOut(w, resource)
}

func (s *Server) storeResponseHistory(tenant, id string, messages []oaiMsg) {
	s.responseMu.Lock()
	defer s.responseMu.Unlock()
	bucket := s.responseMessages[tenant]
	if bucket == nil {
		bucket = map[string]*respHistory{}
		s.responseMessages[tenant] = bucket
	}
	for key, history := range bucket {
		if history == nil {
			delete(bucket, key)
			continue
		}
		if time.Since(history.At) > time.Hour {
			delete(bucket, key)
		}
	}
	if len(bucket) >= maxResponsesPerTenant {
		var oldestKey string
		var oldestAt time.Time
		for key, history := range bucket {
			if oldestKey == "" || history.At.Before(oldestAt) {
				oldestKey, oldestAt = key, history.At
			}
		}
		delete(bucket, oldestKey)
	}
	bucket[id] = &respHistory{At: time.Now(), Messages: append([]oaiMsg(nil), messages...)}
}

func writeCompactionStream(w http.ResponseWriter, r *http.Request, resource map[string]any, item map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	id, _ := resource["id"].(string)
	emit := func(name string, value any) { _ = writeSSE(r, w, flusher, name, value) }
	emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response.compaction", "status": "in_progress", "output": []any{}}})
	emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "compaction", "id": item["id"], "status": "in_progress"}})
	emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	emit("response.completed", map[string]any{"type": "response.completed", "response": resource})
}
