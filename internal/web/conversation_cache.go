package web

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"m365-copilot2api/internal/chathub"
	"strconv"
	"sync"
	"time"
)

type cachedConversation struct {
	ConversationID string
	SessionID      string
	Tone           string
	TurnCount      int
	MessageCount   int
	CreatedAt      time.Time
	LastUsedAt     time.Time
	SystemPrompt   string
	// HistoryHash includes the assistant reply actually returned to the client.
	// MessageCount therefore points past that reply, not just past the request.
	HistoryHash string
}

type conversationCache struct {
	mu      sync.Mutex
	entries map[string]*cachedConversation
	maxAge  time.Duration
}

func newConversationCache() *conversationCache {
	return &conversationCache{
		entries: make(map[string]*cachedConversation),
		maxAge:  2 * time.Hour,
	}
}

// key scopes cached conversations by API-key tenant in addition to account and
// model, so two keys that share one upstream account never continue each
// other's cloud conversation.
func (c *conversationCache) key(tenant, accountID, model string) string {
	return tenant + "|" + accountID + "|" + model
}

func (c *conversationCache) Lookup(tenant, accountID, model string) *cachedConversation {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[c.key(tenant, accountID, model)]
	if entry == nil {
		return nil
	}
	if time.Since(entry.LastUsedAt) > c.maxAge {
		delete(c.entries, c.key(tenant, accountID, model))
		return nil
	}
	return entry
}

// Match returns the cached conversation only when the request is a strict
// continuation of the history that conversation has already seen: same
// tenant, account and model, and an identical message prefix (instructions
// included). Matching on the first system message alone let unrelated requests
// that merely share instructions — a Codex background title-generation request
// and the user's real turn, for example — continue each other's cloud
// conversation. The incremental tail then landed in the wrong context and the
// user's answer degraded into a stray task title.
func (c *conversationCache) Match(tenant, accountID, model string, messages []oaiMsg) *cachedConversation {
	entry := c.Lookup(tenant, accountID, model)
	if entry == nil {
		return nil
	}
	if entry.MessageCount <= 0 || len(messages) <= entry.MessageCount {
		return nil
	}
	prefix := messages[:entry.MessageCount]
	if entry.SystemPrompt != systemPromptHash(prefix) {
		return nil
	}
	if entry.HistoryHash == "" || entry.HistoryHash != historyFingerprint(prefix) {
		return nil
	}
	return entry
}

func (c *conversationCache) Store(tenant, accountID, model string, conv *cachedConversation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv.LastUsedAt = time.Now()
	c.entries[c.key(tenant, accountID, model)] = conv
}

func (c *conversationCache) Invalidate(tenant, accountID, model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, c.key(tenant, accountID, model))
}

func (c *conversationCache) GC() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.entries {
		if now.Sub(v.LastUsedAt) > c.maxAge {
			delete(c.entries, k)
		}
	}
}

func (c *conversationCache) Stats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"cached_conversations": len(c.entries)}
}

// systemPromptHash covers every system and developer message. Clients such as
// Codex send one shared system message followed by several developer messages
// that differ per request type, so hashing only the first instruction message
// cannot tell those requests apart.
func systemPromptHash(messages []oaiMsg) string {
	h := sha256.New()
	found := false
	for _, m := range messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		found = true
		writeHashField(h, m.Role)
		if !writeHashJSON(h, m.Content) {
			return ""
		}
	}
	if !found {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// historyFingerprint canonicalizes tool IDs by their position in the history.
// Renaming a call and its results together is harmless; changing which call a
// result belongs to changes the fingerprint. Ambiguous or dangling references
// cannot establish a reusable history.
func historyFingerprint(messages []oaiMsg) string {
	h := sha256.New()
	callIDs := make(map[string]string)
	for _, m := range messages {
		writeHashField(h, m.Role)
		writeHashField(h, m.Name)
		writeHashField(h, m.ReasoningContent)
		if !writeHashJSON(h, m.Content) {
			return ""
		}
		association := ""
		if m.ToolCallID != "" {
			var ok bool
			association, ok = callIDs[m.ToolCallID]
			if !ok {
				return ""
			}
		} else if m.Role == "tool" {
			return ""
		}
		writeHashField(h, association)
		for _, call := range m.ToolCalls {
			id, ok := call["id"].(string)
			if !ok || id == "" {
				return ""
			}
			if _, duplicate := callIDs[id]; duplicate {
				return ""
			}
			callIDs[id] = strconv.Itoa(len(callIDs) + 1)
			normalized := make(map[string]any, len(call))
			for key, value := range call {
				if key != "id" {
					normalized[key] = value
				}
			}
			if !writeHashJSON(h, normalized) {
				return ""
			}
		}
		writeHashField(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeHashField(h hash.Hash, v string) {
	writeHashBytes(h, []byte(v))
}

func writeHashJSON(h hash.Hash, v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	writeHashBytes(h, b)
	return true
}

func writeHashBytes(h hash.Hash, v []byte) {
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(v)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(v)
}

func extractLastUserMessage(messages []oaiMsg) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return contentToString(messages[i].Content)
		}
	}
	return ""
}

func completedConversationHistory(messages []oaiMsg, reply oaiMsg) []oaiMsg {
	history := make([]oaiMsg, len(messages)+1)
	copy(history, messages)
	history[len(messages)] = reply
	return history
}

func (s *Server) storeConvCache(tenant, accID, model string, res chathub.Result, tone string, messages []oaiMsg, reply oaiMsg) {
	if res.ConversationID == "" {
		return
	}
	history := completedConversationHistory(messages, reply)
	cached := s.convCache.Lookup(tenant, accID, model)
	entry := &cachedConversation{
		ConversationID: res.ConversationID,
		SessionID:      res.SessionID,
		Tone:           tone,
		MessageCount:   len(history),
		SystemPrompt:   systemPromptHash(history),
		HistoryHash:    historyFingerprint(history),
	}
	if cached != nil && cached.ConversationID == res.ConversationID {
		entry.TurnCount = cached.TurnCount + 1
	} else {
		entry.TurnCount = 1
	}
	s.convCache.Store(tenant, accID, model, entry)
}

func (s *Server) invalidateConvCache(tenant, accID, model string) {
	s.convCache.Invalidate(tenant, accID, model)
}
