package chathub

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Usage contains billing-style token counters when ChatHub includes them in a
// SignalR frame. ChatHub does not document a stable usage envelope, so the
// parser accepts the field spellings used by OpenAI, Anthropic and camel-case
// variants without inventing values when they are absent.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	TotalTokens              int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

func (u Usage) Empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 &&
		u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0
}

func (u Usage) Map() map[string]any {
	return map[string]any{
		"input_tokens":                u.InputTokens,
		"output_tokens":               u.OutputTokens,
		"total_tokens":                u.TotalTokens,
		"cache_creation_input_tokens": u.CacheCreationInputTokens,
		"cache_read_input_tokens":     u.CacheReadInputTokens,
	}
}

func usageNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), n >= 0 && n == float64(int64(n))
	case json.Number:
		i, err := strconv.ParseInt(string(n), 10, 64)
		return i, err == nil && i >= 0
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i, err == nil && i >= 0
	default:
		return 0, false
	}
}

func usageField(u *Usage, key string, value any) {
	n, ok := usageNumber(value)
	if !ok {
		return
	}
	switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_")) {
	case "input_tokens", "inputtokens", "input_token_count", "inputtokencount", "prompt_tokens", "prompttokens":
		u.InputTokens = n
	case "output_tokens", "outputtokens", "output_token_count", "outputtokencount", "completion_tokens", "completiontokens":
		u.OutputTokens = n
	case "total_tokens", "totaltokens", "total_token_count", "totaltokencount":
		u.TotalTokens = n
	case "cache_creation_input_tokens", "cachecreationinputtokens", "cache_creation_input_token_count", "cachecreationinputtokencount", "cache_creation_tokens", "cachecreationtokens":
		u.CacheCreationInputTokens = n
	case "cache_read_input_tokens", "cachereadinputtokens", "cache_read_input_token_count", "cachereadinputtokencount", "cache_read_tokens", "cachereadtokens", "cached_tokens", "cachedtokens":
		u.CacheReadInputTokens = n
	}
}

// usageFromRaw recursively searches a decoded ChatHub frame. The recursive
// walk is intentional: result envelopes have changed shape over time and may
// put usage below result, metadata, or usageDetails.
func usageFromRaw(v any) Usage {
	var out Usage
	var walk func(any)
	walk = func(x any) {
		switch z := x.(type) {
		case []any:
			for _, item := range z {
				walk(item)
			}
		case map[string]any:
			for key, value := range z {
				usageField(&out, key, value)
				walk(value)
			}
		}
	}
	walk(v)
	if out.TotalTokens == 0 && (out.InputTokens != 0 || out.OutputTokens != 0) {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	return out
}

func mergeUsage(dst, src Usage) Usage {
	if src.InputTokens != 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens != 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.TotalTokens != 0 {
		dst.TotalTokens = src.TotalTokens
	}
	if src.CacheCreationInputTokens != 0 {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
	if src.CacheReadInputTokens != 0 {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
	return dst
}
