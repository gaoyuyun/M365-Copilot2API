package web

import (
	"m365-copilot2api/internal/chathub"
	"os"
	"strings"
)

func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func compatM365Metadata(res chathub.Result) map[string]any {
	m := map[string]any{
		"conversationId": res.ConversationID,
		"sessionId":      res.SessionID,
		"requestId":      res.RequestID,
		"usage_source":   "unavailable_from_chathub",
	}
	if res.Throttling != nil {
		m["throttling"] = res.Throttling
	}
	if len(res.SuggestedResponses) > 0 {
		m["suggestedResponses"] = res.SuggestedResponses
	}
	if res.Offense != "" {
		m["offense"] = res.Offense
	}
	if len(res.Scores) > 0 {
		m["scores"] = res.Scores
	}
	if res.ConversationTransferToken != "" {
		m["conversationTransferToken"] = res.ConversationTransferToken
	}
	if res.MeteringInformation != nil {
		m["meteringInformation"] = res.MeteringInformation
	}
	if res.SpokenText != "" {
		m["spokenText"] = res.SpokenText
	}
	if res.Timestamps.RequestSent != "" {
		m["timestamps"] = res.Timestamps
	}
	if res.StorageMessageID != "" {
		m["storageMessageId"] = res.StorageMessageID
	}
	if len(res.References) > 0 {
		citations := make([]map[string]any, 0, len(res.References))
		for key, ref := range res.References {
			c := map[string]any{"key": key}
			if ref.TargetLink != "" {
				c["url"] = ref.TargetLink
			}
			if ref.Title != "" {
				c["title"] = ref.Title
			}
			if ref.Snippet != "" {
				c["snippet"] = ref.Snippet
			}
			if ref.ProviderDisplayName != "" {
				c["provider"] = ref.ProviderDisplayName
			}
			citations = append(citations, c)
		}
		m["citations"] = citations
	}
	if !res.Usage.Empty() {
		m["usage_source"] = "upstream_chathub"
		m["usage_values_are_estimates"] = false
		m["cache_creation_input_tokens"] = res.Usage.CacheCreationInputTokens
		m["cache_read_input_tokens"] = res.Usage.CacheReadInputTokens
	} else {
		m["usage_values_are_estimates"] = true
		m["cache_source"] = "not_reported_by_upstream"
	}
	if envTrue("M365_INCLUDE_UPSTREAM_EVENTS") {
		m["events"] = res.Events
	}
	return m
}

func normalizedToolChoiceMode(choice any) string {
	if choice == nil {
		return "auto"
	}
	if s, ok := choice.(string); ok {
		return s
	}
	if m, ok := choice.(map[string]any); ok {
		if f, ok := m["function"].(map[string]any); ok {
			if n, ok := f["name"].(string); ok {
				return "named:" + n
			}
		}
		if n, ok := m["name"].(string); ok {
			return "named:" + n
		}
	}
	return "auto"
}
