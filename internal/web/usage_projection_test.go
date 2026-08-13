package web

import (
	"encoding/json"
	"m365-copilot2api/internal/chathub"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIUsageProjectsCacheCounters(t *testing.T) {
	u, known := openAIUsage(chathub.Result{Usage: chathub.Usage{InputTokens: 10, OutputTokens: 3, CacheCreationInputTokens: 4, CacheReadInputTokens: 6}}, 1, 2)
	if !known || u["prompt_tokens"] != int64(10) || u["cache_creation_input_tokens"] != int64(4) || u["cache_read_input_tokens"] != int64(6) {
		t.Fatalf("usage=%#v known=%v", u, known)
	}
}

func TestOpenAIUsageMarksFallbackShape(t *testing.T) {
	u, known := openAIUsage(chathub.Result{}, 8, 2)
	if known || u["prompt_tokens"] != int64(8) || u["cache_read_input_tokens"] != int64(0) {
		t.Fatalf("usage=%#v known=%v", u, known)
	}
}

func TestAnthropicUsageIncludesCacheCounters(t *testing.T) {
	src := map[string]any{
		"usage":   map[string]any{"prompt_tokens": int64(10), "completion_tokens": int64(2), "cache_creation_input_tokens": int64(3), "cache_read_input_tokens": int64(4)},
		"m365":    map[string]any{"usage_source": "upstream_chathub"},
		"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
	}
	rr := httptest.NewRecorder()
	writeAnthropicResult(rr, "m", false, src)
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	usage, _ := got["usage"].(map[string]any)
	if usage["cache_creation_input_tokens"] != float64(3) || usage["cache_read_input_tokens"] != float64(4) {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestAnthropicStreamUsageIncludesCacheCounters(t *testing.T) {
	src := map[string]any{
		"usage":   map[string]any{"prompt_tokens": int64(10), "completion_tokens": int64(2), "cache_creation_input_tokens": int64(3), "cache_read_input_tokens": int64(4)},
		"m365":    map[string]any{"usage_source": "upstream_chathub"},
		"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
	}
	rr := httptest.NewRecorder()
	writeAnthropicResult(rr, "m", true, src)
	if !strings.Contains(rr.Body.String(), `"cache_creation_input_tokens":3`) || !strings.Contains(rr.Body.String(), `"cache_read_input_tokens":4`) {
		t.Fatalf("stream=%s", rr.Body.String())
	}
}
