package web

import (
	"strings"
	"unicode/utf8"

	"m365-copilot2api/internal/chathub"
)

// toolFenceMarkers are the fence openings that may become a tool call and must
// therefore be withheld from the text stream until post-stream extraction runs.
// Everything else (```text, ```json, ```mermaid, prose diagrams, ...) is normal
// assistant output and must be emitted, otherwise legitimate answers get
// truncated at the first code block.
var toolFenceMarkers = []string{"```bash", "```sh", "```shell", "```powershell", "```cmd"}

func hasToolFenceCandidate(v string) bool {
	for _, m := range toolFenceMarkers {
		if strings.Contains(v, m) {
			return true
		}
	}
	// A bare {"command": ...} object (shell code emitted without a fence) is
	// also a tool-call candidate handled by fencedToolCalls after the stream.
	return strings.Contains(v, "\"command\"")
}

// streamEmitText incrementally forwards assistant text during streaming while
// correctly distinguishing tool-call fences from ordinary fenced content.
//
// Tool-candidate fences (```bash/sh/shell/powershell/cmd or a bare
// {"command":...} object) are withheld so post-stream extraction can convert
// them into tool calls. Any other completed fenced block — e.g. a ```text
// diagram — is emitted as normal text. Only an unclosed trailing fence (and a
// short tail used for fence detection) stays buffered in pending.
//
// Previously every fenced block was buffered and only the final pending value
// was flushed, which dropped ```text blocks and the prose between them; that
// truncated answers at the first code block.
func streamEmitText(ev chathub.StreamEvent, text, pending *strings.Builder, emitText func(string) error) error {
	text.WriteString(ev.Text)
	pending.WriteString(ev.Text)
	v := pending.String()

	// Withhold the whole buffer while a tool-candidate fence is present; the
	// post-stream fencedToolCalls pass decides whether it becomes a tool call
	// or is flushed back as text (illustrative example).
	if hasToolFenceCandidate(v) {
		return nil
	}

	// Emit every complete, non-tool fenced block together with the prose
	// around it. Stop at an unclosed fence and keep it buffered.
	for {
		i := strings.Index(v, "```")
		if i < 0 {
			break
		}
		j := strings.Index(v[i+3:], "```")
		if j < 0 {
			if err := emitText(v[:i]); err != nil {
				return err
			}
			pending.Reset()
			pending.WriteString(v[i:])
			return nil
		}
		closeIdx := i + 3 + j + 3
		if err := emitText(v[:closeIdx]); err != nil {
			return err
		}
		v = v[closeIdx:]
	}

	// No unclosed fence remains. Emit everything except a short tail so a fence
	// that is still arriving byte-by-byte ("`", "``", "```") can be detected on
	// the next delta.
	if runeCount := utf8.RuneCountInString(v); runeCount > 3 {
		cut := 0
		seen := 0
		for idx := range v {
			if seen == runeCount-3 {
				cut = idx
				break
			}
			seen++
		}
		if err := emitText(v[:cut]); err != nil {
			return err
		}
		v = v[cut:]
	}
	pending.Reset()
	pending.WriteString(v)
	return nil
}

// flushStreamText delivers the tail retained by streamEmitText and then drains
// the public-identity filter. Callers must invoke it only after deciding that
// the buffered text is not a tool call: tool-candidate fences intentionally
// remain withheld until that decision has been made.
func flushStreamText(pending *strings.Builder, identityFilter *publicIdentityStreamFilter, emitFiltered, emitRaw func(string) error) error {
	if pending != nil && pending.Len() > 0 {
		tail := pending.String()
		pending.Reset()
		if err := emitFiltered(tail); err != nil {
			return err
		}
	}
	if identityFilter != nil {
		if tail := identityFilter.Flush(); tail != "" {
			return emitRaw(tail)
		}
	}
	return nil
}
