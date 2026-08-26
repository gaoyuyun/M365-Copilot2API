package web

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"m365-copilot2api/internal/chathub"
)

const (
	defaultInputBudgetTokens      int64 = 96000
	promptBudgetReserveTokens     int64 = 2048
	minimumPromptBudgetTokens     int64 = 8192
	defaultContinuingHistoryShare       = 60
	maxTaskAnchorTokens                 = 4096
)

var durableTaskAnchorPattern = regexp.MustCompile(`(?i)([a-z]:\\|\\\\[a-z0-9_.-]+\\|https?://|\.lnk\b|\bserver\s*#?\s*\d+\b|\d+\s*号服务器|(?:^|[\s"'])/(?:opt|home|root|srv|var|workspace|app|mnt)/)`)

type promptBudgetStats struct {
	OriginalMessages, SelectedMessages, DroppedMessages, AnchoredMessages int
	PromptTokens, TaskAnchorTokens, ToolTokens, AttachmentTokens          int64
	PromptBudget                                                          int64
	Continuing, Exceeded                                                  bool
	ExceededReason                                                        string
}

type promptBudgetUnit struct {
	messages []oaiMsg
	tokens   int64
	instruct bool
	hasUser  bool
}

func configuredContinuingHistoryShare() int64 {
	if raw := strings.TrimSpace(os.Getenv("M365_CONTINUING_HISTORY_SHARE")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n <= 100 {
			return int64(n)
		}
	}
	return defaultContinuingHistoryShare
}

func configuredInputBudgetTokens(model string) int64 {
	limit := defaultInputBudgetTokens
	if raw := strings.TrimSpace(os.Getenv("M365_INPUT_BUDGET_TOKENS")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= minimumPromptBudgetTokens+promptBudgetReserveTokens {
			limit = n
		}
	}
	modelLimit := int64(configuredModelLimits().MaxInputTokens)
	if modelLimit > 0 && limit > modelLimit {
		limit = modelLimit
	}
	return limit
}

func promptBudgetTokens(model string, value any) int64 {
	count, _ := tokenEstimator(model)
	return int64(serializedTokenCount(value, count))
}

func promptAttachmentBudgetTokens(model string, attachments []chathub.Attachment) int64 {
	serialized := mustJSON(attachments)
	// Base64/data URLs do not benefit from an exact BPE pass and can otherwise
	// monopolize a CPU on large multimodal requests.
	if len(serialized) > 32<<10 {
		return int64((len(serialized) + 1) / 2)
	}
	return promptBudgetTokens(model, attachments)
}

func promptMessageBudgetTokens(model string, message oaiMsg) int64 {
	tokens := int64(estimateMessageTokens(message, func(text string) int { return int(promptBudgetTokens(model, text)) }))
	_, attachments := parseContent(message.Content)
	if len(attachments) > 0 {
		tokens += promptAttachmentBudgetTokens(model, attachments)
	}
	return tokens
}

func buildPromptBudgetUnits(messages []oaiMsg, model string) []promptBudgetUnit {
	units := make([]promptBudgetUnit, 0, len(messages))
	for i := 0; i < len(messages); {
		m := messages[i]
		u := promptBudgetUnit{messages: []oaiMsg{m}, tokens: promptMessageBudgetTokens(model, m)}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		u.instruct, u.hasUser = role == "system" || role == "developer", role == "user"
		i++
		if len(m.ToolCalls) > 0 {
			for i < len(messages) && strings.EqualFold(strings.TrimSpace(messages[i].Role), "tool") {
				u.messages = append(u.messages, messages[i])
				u.tokens += promptMessageBudgetTokens(model, messages[i])
				i++
			}
		}
		units = append(units, u)
	}
	return units
}

func durablePromptAnchor(u promptBudgetUnit) bool {
	if u.instruct || len(u.messages) != 1 {
		return false
	}
	m := u.messages[0]
	role := strings.ToLower(strings.TrimSpace(m.Role))
	return (role == "user" || role == "assistant") && len(m.ToolCalls) == 0 && durableTaskAnchorPattern.MatchString(contentToString(m.Content))
}

// selectPromptMessages bounds the complete request, including tool schemas and
// attachments. Tool-call/result pairs remain atomic and durable paths/URLs are
// retained so long-running Codex and Claude Code tasks do not lose their target.
func selectPromptMessages(messages []oaiMsg, model string, tools []chathub.Tool, attachments []chathub.Attachment, continuing bool) ([]oaiMsg, promptBudgetStats) {
	stats := promptBudgetStats{OriginalMessages: len(messages), Continuing: continuing}
	if len(messages) == 0 {
		return nil, stats
	}
	stats.ToolTokens = promptBudgetTokens(model, tools)
	stats.AttachmentTokens = promptAttachmentBudgetTokens(model, attachments)
	stats.PromptBudget = configuredInputBudgetTokens(model) - stats.ToolTokens - stats.AttachmentTokens - promptBudgetReserveTokens
	if stats.PromptBudget < minimumPromptBudgetTokens {
		stats.Exceeded, stats.ExceededReason = true, "tool definitions or attachments leave too little prompt budget"
		return nil, stats
	}
	units := buildPromptBudgetUnits(messages, model)
	selected := make([]bool, len(units))
	used := int64(0)
	for i := range units {
		if !units[i].instruct {
			continue
		}
		if used+units[i].tokens > stats.PromptBudget {
			stats.Exceeded, stats.ExceededReason = true, "system/developer instructions exceed the configured input budget"
			return nil, stats
		}
		selected[i], used = true, used+units[i].tokens
	}
	lastUser := -1
	for i := range units {
		if units[i].hasUser {
			lastUser = i
		}
	}
	if lastUser < 0 {
		lastUser = len(units) - 1
	}
	current := int64(0)
	for i := lastUser; i < len(units); i++ {
		if !selected[i] {
			current += units[i].tokens
		}
	}
	if used+current > stats.PromptBudget {
		stats.Exceeded, stats.ExceededReason = true, "current user turn exceeds the configured input budget"
		return nil, stats
	}
	for i := lastUser; i < len(units); i++ {
		if !selected[i] {
			selected[i], used = true, used+units[i].tokens
		}
	}
	anchors := []int{}
	for i := 0; i < lastUser; i++ {
		if durablePromptAnchor(units[i]) {
			anchors = append(anchors, i)
		}
	}
	if len(anchors) > 0 {
		chosen := []int{anchors[0]}
		for i := len(anchors) - 1; i >= 0 && len(chosen) < 4; i-- {
			if anchors[i] != anchors[0] {
				chosen = append(chosen, anchors[i])
			}
		}
		sort.Ints(chosen)
		for _, i := range chosen {
			if selected[i] || stats.TaskAnchorTokens+units[i].tokens > maxTaskAnchorTokens || used+units[i].tokens > stats.PromptBudget {
				continue
			}
			selected[i], used = true, used+units[i].tokens
			stats.TaskAnchorTokens += units[i].tokens
			stats.AnchoredMessages += len(units[i].messages)
		}
	}
	historyLimit := stats.PromptBudget
	if continuing {
		historyLimit = stats.PromptBudget * configuredContinuingHistoryShare() / 100
	}
	for i := lastUser - 1; i >= 0; i-- {
		if selected[i] || units[i].instruct || used+units[i].tokens > historyLimit {
			continue
		}
		selected[i], used = true, used+units[i].tokens
	}
	out := make([]oaiMsg, 0, len(messages))
	for i, u := range units {
		if selected[i] {
			out = append(out, u.messages...)
		}
	}
	stats.SelectedMessages, stats.DroppedMessages, stats.PromptTokens = len(out), len(messages)-len(out), used
	return out, stats
}
