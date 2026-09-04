package web

import (
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// collectStream feeds text fragments through the same incremental and final
// flush helpers used by the live SSE loop.
func collectStream(fragments []string) string {
	var text, pending strings.Builder
	var emitted strings.Builder
	identityFilter := newPublicIdentityStreamFilter()
	emitRaw := func(part string) error {
		emitted.WriteString(part)
		return nil
	}
	emitFiltered := func(part string) error {
		return emitRaw(identityFilter.Push(part))
	}
	for _, fragment := range fragments {
		_ = streamEmitText(chathub.StreamEvent{Kind: "text", Text: fragment}, &text, &pending, emitFiltered)
	}
	_ = flushStreamText(&pending, identityFilter, emitFiltered, emitRaw)
	return emitted.String()
}

// streamDuringAndPending returns (text emitted mid-stream, residual pending).
func streamDuringAndPending(fragments []string) (string, string) {
	var text, pending strings.Builder
	var emitted strings.Builder
	emit := func(part string) error {
		emitted.WriteString(part)
		return nil
	}
	for _, f := range fragments {
		_ = streamEmitText(chathub.StreamEvent{Kind: "text", Text: f}, &text, &pending, emit)
	}
	return emitted.String(), pending.String()
}

// A ```text diagram followed by more prose and a second ```text block must be
// delivered in full. The old buffering logic dropped everything after the first
// completed fence, so the client answer was truncated at the diagram — exactly
// the clash.yaml explanation regression.
func TestStreamEmitsTextFencesAndTrailingProse(t *testing.T) {
	full := "开头说明。\n\n```text\n浏览器 → Clash → 互联网\n```\n\n配置文件地址：\n\n```text\nhttp://1.2.3.4:8080/x/clash.yaml\n```\n\n其中 8080 只提供配置文件，443 才承载流量。结束。"
	// Split into many small deltas to simulate token-by-token streaming.
	var frags []string
	for _, r := range full {
		frags = append(frags, string(r))
	}
	got := collectStream(frags)
	if got != full {
		t.Fatalf("streamed output truncated.\n want %q\n  got %q", full, got)
	}
}

// A tool-candidate fence (```bash) is withheld from the text stream mid-flight
// so the post-stream extractor can convert or re-flush it; it must stay in the
// pending buffer rather than being emitted as live text.
func TestStreamWithholdsToolFence(t *testing.T) {
	emitted, pending := streamDuringAndPending([]string{"运行：\n```bash\nls -la\n```"})
	if strings.Contains(emitted, "ls -la") {
		t.Fatalf("tool-candidate fence must not be emitted mid-stream, got %q", emitted)
	}
	if !strings.Contains(pending, "ls -la") {
		t.Fatalf("tool-candidate fence should remain buffered in pending, got %q", pending)
	}
}

// Ordinary prose with no fences streams through unchanged.
func TestStreamPlainProse(t *testing.T) {
	full := "这是一段没有代码块的普通回答，应该原样输出。"
	var frags []string
	for _, r := range full {
		frags = append(frags, string(r))
	}
	if got := collectStream(frags); got != full {
		t.Fatalf("plain prose altered.\n want %q\n  got %q", full, got)
	}
}

func TestStreamFlushesRetainedThreeRuneTail(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "false")
	full := "介绍你自己。"
	if got := collectStream([]string{"介绍", "你", "自己。"}); got != full {
		t.Fatalf("final stream tail was lost: got %q want %q", got, full)
	}
}
