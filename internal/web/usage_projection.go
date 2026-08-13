package web

import (
	"encoding/json"
	"strconv"
	"strings"

	"m365-copilot2api/internal/chathub"
)

func usageValue(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err == nil {
			return i
		}
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err == nil {
			return i
		}
	}
	return 0
}

// openAIUsage projects upstream counters when ChatHub reports them and uses
// the supplied local estimate otherwise.
func openAIUsage(res chathub.Result, input, output int64) (map[string]any, bool) {
	if !res.Usage.Empty() {
		u := res.Usage
		if u.TotalTokens == 0 {
			u.TotalTokens = u.InputTokens + u.OutputTokens
		}
		return map[string]any{
			"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens, "total_tokens": u.TotalTokens,
			"prompt_tokens_details":       map[string]any{"cached_tokens": u.CacheReadInputTokens},
			"cache_creation_input_tokens": u.CacheCreationInputTokens, "cache_read_input_tokens": u.CacheReadInputTokens,
		}, true
	}
	return map[string]any{
		"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output,
		"prompt_tokens_details":       map[string]any{"cached_tokens": int64(0)},
		"cache_creation_input_tokens": int64(0), "cache_read_input_tokens": int64(0),
	}, false
}

func anthropicUsage(src map[string]any) (map[string]any, bool) {
	if u, ok := src["usage"].(map[string]any); ok {
		known := false
		if m, ok := src["m365"].(map[string]any); ok {
			known = m["usage_source"] == "upstream_chathub"
		}
		return map[string]any{
			"input_tokens": usageValue(u["prompt_tokens"]), "output_tokens": usageValue(u["completion_tokens"]),
			"cache_creation_input_tokens": usageValue(u["cache_creation_input_tokens"]), "cache_read_input_tokens": usageValue(u["cache_read_input_tokens"]),
		}, known
	}
	return map[string]any{"input_tokens": int64(0), "output_tokens": int64(0), "cache_creation_input_tokens": int64(0), "cache_read_input_tokens": int64(0)}, false
}

func responsesUsageFromOpenAI(src map[string]any) (map[string]any, bool) {
	meta, _ := src["m365"].(map[string]any)
	if meta["usage_source"] != "upstream_chathub" {
		return nil, false
	}
	u, _ := src["usage"].(map[string]any)
	if u == nil {
		return nil, false
	}
	return map[string]any{
		"input_tokens": usageValue(u["prompt_tokens"]), "output_tokens": usageValue(u["completion_tokens"]), "total_tokens": usageValue(u["total_tokens"]),
		"input_tokens_details":        u["prompt_tokens_details"],
		"cache_creation_input_tokens": usageValue(u["cache_creation_input_tokens"]), "cache_read_input_tokens": usageValue(u["cache_read_input_tokens"]),
	}, true
}

func responsesUsageFromLegacyUsage(u map[string]any, upstream bool) (map[string]any, bool) {
	if !upstream || u == nil {
		return nil, false
	}
	return map[string]any{
		"input_tokens": usageValue(u["prompt_tokens"]), "output_tokens": usageValue(u["completion_tokens"]), "total_tokens": usageValue(u["total_tokens"]),
		"input_tokens_details":        u["prompt_tokens_details"],
		"cache_creation_input_tokens": usageValue(u["cache_creation_input_tokens"]), "cache_read_input_tokens": usageValue(u["cache_read_input_tokens"]),
	}, true
}
