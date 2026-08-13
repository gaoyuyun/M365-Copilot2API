package chathub

import "testing"

func TestUsageFromRawAcceptsProviderCacheFields(t *testing.T) {
	u := usageFromRaw(map[string]any{"result": map[string]any{"usage": map[string]any{
		"prompt_tokens": 12, "completion_tokens": 4, "cache_creation_input_tokens": 7, "cache_read_input_tokens": 5,
	}}})
	if u.InputTokens != 12 || u.OutputTokens != 4 || u.TotalTokens != 16 || u.CacheCreationInputTokens != 7 || u.CacheReadInputTokens != 5 {
		t.Fatalf("usage=%+v", u)
	}
}

func TestUsageFromRawDoesNotInferCache(t *testing.T) {
	if got := usageFromRaw(map[string]any{"throttling": map[string]any{"tokens": 99}}); !got.Empty() {
		t.Fatalf("unexpected usage=%+v", got)
	}
}
