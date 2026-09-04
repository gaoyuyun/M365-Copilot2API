package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

type historyExchange struct {
	ConversationID string
	Prompt         string
}

// Exercise the real HTTP handlers and ChatHub frame parser. All upstream
// connections are redirected to this local WebSocket fixture, using fake tokens.
func newHistoryPipeline(t *testing.T, replies ...string) (*Server, <-chan historyExchange) {
	t.Helper()
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "false")
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USAGE_LOG", filepath.Join(dir, "usage.jsonl"))
	store, err := auth.OpenStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(auth.TokenSet{HomeOID: "fixture-account", TenantID: "fixture-tenant", AccessToken: "fixture-token", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	outputs := make(chan string, len(replies))
	for _, reply := range replies {
		outputs <- reply
	}
	close(outputs)
	exchanges := make(chan historyExchange, len(replies)+1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte("{}\x1e"))
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				t.Error(err)
				return
			}
			var payload struct {
				Type      int `json:"type"`
				Arguments []struct {
					Message struct {
						Text string `json:"text"`
					} `json:"message"`
				} `json:"arguments"`
			}
			for _, part := range bytes.Split(frame, []byte("\x1e")) {
				if len(part) == 0 {
					continue
				}
				if err := json.Unmarshal(part, &payload); err != nil {
					t.Error(err)
					return
				}
				if payload.Type == 4 {
					break
				}
			}
			if payload.Type != 4 {
				continue
			}
			if len(payload.Arguments) != 1 {
				t.Errorf("unexpected ChatHub arguments: %s", frame)
				return
			}
			exchanges <- historyExchange{r.URL.Query().Get("ConversationId"), payload.Arguments[0].Message.Text}
			reply, ok := <-outputs
			if !ok {
				t.Error("unexpected extra upstream call")
				return
			}
			// Two cumulative updates exercise both streamed text and the retained
			// fence-detection tail before the final result arrives.
			runes := []rune(reply)
			for _, snapshot := range []string{string(runes[:len(runes)/2]), reply} {
				frame := map[string]any{"type": 1, "target": "update", "arguments": []any{map[string]any{"messages": []any{map[string]any{"author": "bot", "text": snapshot}}}}}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(mustJSON(frame)+"\x1e"))
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(mustJSON(map[string]any{"type": 2, "item": map[string]any{"result": map[string]any{"value": "Success", "message": reply}}})+"\x1e"))
			_ = conn.WriteMessage(websocket.TextMessage, []byte("{\"type\":3}\x1e"))
			return
		}
	}))
	t.Cleanup(upstream.Close)
	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second, NetDialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}}
	cfg := defaultRuntimeSettings()
	cfg.ToolPlanningMode = "native"
	cfg.ChatTimeoutSeconds = 5
	s := &Server{
		accountPool: newAccountHealth(), accountConcurrency: newAccountConcurrency(),
		tokens: store, chat: &chathub.Client{Dialer: dialer, HTTPHeader: make(http.Header)},
		settings: &settingsStore{v: cfg}, apiKeys: &apiKeyStore{},
		convCache: newConversationCache(), sessionResolver: openSessionResolver(),
		sessions: openSessionStore(), userSessions: openUserSessionStore(time.Hour),
		conversationManager: openConversationManager(), usage: openUsageLog(),
		responseMessages: make(map[string]map[string]*RespNode),
	}
	return s, exchanges
}

func historyChat(t *testing.T, s *Server, body oaiReq, headers ...map[string]string) oaiMsg {
	t.Helper()
	encoded, _ := json.Marshal(body)
	r := historyTestRequest()
	for _, values := range headers {
		for name, value := range values {
			r.Header.Set(name, value)
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
	}
	if !body.Stream {
		var response struct {
			Choices []struct {
				Message oaiMsg `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Choices) != 1 {
			t.Fatalf("invalid chat response: %s", w.Body.String())
		}
		return response.Choices[0].Message
	}
	reply := oaiMsg{Role: "assistant"}
	var content strings.Builder
	calls := make(map[int]*responsesToolCallState)
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		if chunk["error"] != nil {
			t.Fatalf("chat stream error: %v", chunk["error"])
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		if text, ok := delta["content"].(string); ok {
			content.WriteString(text)
		}
		if reasoning, ok := delta["reasoning_content"].(string); ok {
			reply.ReasoningContent += reasoning
		}
		if toolCalls, ok := delta["tool_calls"].([]any); ok {
			for _, call := range toolCalls {
				accumulateResponsesToolCall(calls, call.(map[string]any))
			}
		}
	}
	if content.Len() > 0 {
		reply.Content = content.String()
	}
	for i := 0; i < len(calls); i++ {
		call := calls[i]
		reply.ToolCalls = append(reply.ToolCalls, map[string]any{"id": call.ID, "type": call.Type, "function": map[string]any{"name": call.Name, "arguments": call.Args}})
	}
	return reply
}

func TestConversationPipelineBranchesAndContinues(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			s, exchanges := newHistoryPipeline(t, "Use alpha.", "Use beta.", "完整回答尾部。")
			first := codexStyleMessages("Answer in detail.", "Choose a directory.")
			reply := historyChat(t, s, oaiReq{Messages: first, Stream: stream})
			original := <-exchanges
			if reply.Content != "Use alpha." {
				t.Fatalf("reply=%+v", reply)
			}
			reply.Content = "Use beta."
			branch := append(completedConversationHistory(first, reply), oaiMsg{Role: "user", Content: "Use the selected directory."})
			reply = historyChat(t, s, oaiReq{Messages: branch, Stream: stream})
			branched := <-exchanges
			if branched.ConversationID == original.ConversationID || !strings.Contains(branched.Prompt, "Answer in detail.") || !strings.Contains(branched.Prompt, "Use beta.") {
				t.Fatalf("branch must send full history to a fresh cloud conversation: %+v", branched)
			}
			next := append(completedConversationHistory(branch, reply), oaiMsg{Role: "user", Content: "Explain the next step."})
			reply = historyChat(t, s, oaiReq{Messages: next, Stream: stream})
			continued := <-exchanges
			if continued.ConversationID != branched.ConversationID || continued.Prompt != "[user]\nExplain the next step." {
				t.Fatalf("valid continuation must send only the new turn: %+v", continued)
			}
			if reply.Content != "完整回答尾部。" {
				t.Fatalf("lost response tail: %+v", reply)
			}
		})
	}
}

func TestConversationPipelineToolReplyHistory(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			s, exchanges := newHistoryPipeline(t, "```bash\npwd\n```", "The directory is /repo.")
			first := []oaiMsg{{Role: "user", Content: "Find the directory using bash."}}
			tools := []chathub.Tool{{Type: "function", Function: json.RawMessage(`{"name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}`)}}
			reply := historyChat(t, s, oaiReq{Messages: first, Tools: tools, Stream: stream})
			original := <-exchanges
			if len(reply.ToolCalls) != 1 {
				t.Fatalf("expected structured tool reply: %+v", reply)
			}
			next := completedConversationHistory(first, reply)
			next = append(next, oaiMsg{Role: "tool", ToolCallID: reply.ToolCalls[0]["id"].(string), Content: "/repo"})
			answer := historyChat(t, s, oaiReq{Messages: next, Stream: stream})
			continued := <-exchanges
			if continued.ConversationID != original.ConversationID || strings.Contains(continued.Prompt, "Find the directory") {
				t.Fatalf("emitted tool call was not stored as history: %+v", continued)
			}
			if answer.Content != "The directory is /repo." {
				t.Fatalf("answer=%+v", answer)
			}
		})
	}
}

func TestConversationPipelineRequiredStreamToolCannotReturnText(t *testing.T) {
	s, exchanges := newHistoryPipeline(t, "/mnt/data", "/mnt/data")
	tools := []chathub.Tool{{Type: "function", Function: json.RawMessage(`{"name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}`)}}
	body := oaiReq{
		Messages:   []oaiMsg{{Role: "user", Content: "Use the declared bash tool."}},
		Tools:      tools,
		ToolChoice: "required",
		Stream:     true,
	}
	encoded, _ := json.Marshal(body)
	r := historyTestRequest()
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)
	first := <-exchanges
	if first.Prompt == "" {
		t.Fatal("required-tool request did not reach upstream")
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"code":"invalid_tool_call"`) {
		t.Fatalf("missing required-tool error: status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
		t.Fatal("required-tool refusal was reported as a successful text completion")
	}
}

func TestConversationPipelineRequiredNonStreamToolCannotReturnText(t *testing.T) {
	s, exchanges := newHistoryPipeline(t, "Here is a textual answer instead of a tool call.", `{"calls":[]}`)
	tools := []chathub.Tool{{Type: "function", Function: json.RawMessage(`{"name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}`)}}
	body := oaiReq{
		Messages:   []oaiMsg{{Role: "user", Content: "Use the declared bash tool."}},
		Tools:      tools,
		ToolChoice: "required",
	}
	encoded, _ := json.Marshal(body)
	r := historyTestRequest()
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)
	first := <-exchanges
	if first.Prompt == "" {
		t.Fatal("required-tool request did not reach upstream")
	}
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"code":"invalid_tool_call"`) {
		t.Fatalf("missing required-tool error: status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
		t.Fatal("required-tool refusal was reported as a successful text completion")
	}
}

func historyResponses(t *testing.T, s *Server, body map[string]any) (string, string) {
	t.Helper()
	encoded, _ := json.Marshal(body)
	r := historyTestRequest()
	r.URL.Path = "/v1/responses"
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	w := httptest.NewRecorder()
	s.responses(w, r)
	var output strings.Builder
	id := ""
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		switch event["type"] {
		case "response.output_text.delta":
			output.WriteString(event["delta"].(string))
		case "response.completed":
			id, _ = event["response"].(map[string]any)["id"].(string)
		case "response.failed", "error":
			t.Fatalf("Responses stream failed: %s", w.Body.String())
		}
	}
	if w.Code != http.StatusOK || id == "" {
		t.Fatalf("incomplete Responses stream: status=%d body=%s", w.Code, w.Body.String())
	}
	return id, output.String()
}

func TestConversationPipelineResponsesSeparatesTitleAndMain(t *testing.T) {
	s, exchanges := newHistoryPipeline(t, "说明当前模型", "这是一段完整的模型介绍。", "这里是后续的详细解释。")
	title := codexStyleMessages("Generate a task title only.", "Title this task: describe the model.")
	_, text := historyResponses(t, s, map[string]any{"stream": true, "input": title})
	if text != "说明当前模型" {
		t.Fatalf("title tail was lost: %q", text)
	}
	titleExchange := <-exchanges
	main := make([]any, 0)
	for _, message := range codexStyleMessages("Answer the actual question in detail.", "Describe the model.") {
		main = append(main, message)
	}
	main = append(main,
		map[string]any{"type": "function_call", "call_id": "local-call", "name": "exec", "arguments": `{"cmd":"pwd"}`},
		map[string]any{"type": "function_call_output", "call_id": "local-call", "output": "/repo"},
	)
	id, text := historyResponses(t, s, map[string]any{"stream": true, "input": main})
	if text != "这是一段完整的模型介绍。" {
		t.Fatalf("main reply=%q", text)
	}
	mainExchange := <-exchanges
	if titleExchange.ConversationID == mainExchange.ConversationID || !strings.Contains(mainExchange.Prompt, "Answer the actual question in detail.") {
		t.Fatalf("main request continued the title conversation: title=%+v main=%+v", titleExchange, mainExchange)
	}
	_, text = historyResponses(t, s, map[string]any{"stream": true, "previous_response_id": id, "input": "Please explain further."})
	continued := <-exchanges
	if text != "这里是后续的详细解释。" || continued.ConversationID != mainExchange.ConversationID || strings.Contains(continued.Prompt, "Answer the actual question in detail.") {
		t.Fatalf("Responses continuation failed: text=%q request=%+v", text, continued)
	}
}

func TestConversationPipelineRebuildsLegacySession(t *testing.T) {
	s, exchanges := newHistoryPipeline(t, "A complete answer.")
	r := historyTestRequest()
	main := codexStyleMessages("Answer in detail.", "Describe the model.")
	history := completedConversationHistory(main, oaiMsg{Role: "assistant", Content: "Describe current model"})
	legacy := sessionBinding{
		SessionID: "legacy-session", ConversationID: "title-conversation", AccountID: "fixture-account",
		CreatedAt: time.Now(), LastUsedAt: time.Now(), ContextHistory: history,
		Tenant: tenantFromRequest(r), IPFingerprint: clientIPFingerprint(r), ExplicitID: "logical-session",
	}
	b, _ := json.Marshal([]sessionBinding{legacy})
	if err := os.WriteFile(s.sessionResolver.path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	s.sessionResolver = openSessionResolver()
	s.userSessions.Put(tenantFromRequest(r), "legacy-user", legacy.ConversationID, legacy.SessionID, legacy.AccountID)
	next := append(history, oaiMsg{Role: "user", Content: "Please answer fully."})
	reply := historyChat(t, s, oaiReq{User: "legacy-user", Messages: next}, map[string]string{sessionHeaderName: "logical-session"})
	got := <-exchanges
	if got.ConversationID == legacy.ConversationID || !strings.Contains(got.Prompt, "Answer in detail.") || !strings.Contains(got.Prompt, "Describe the model.") || reply.Content != "A complete answer." {
		t.Fatalf("upgrade did not rebuild the full conversation: request=%+v reply=%+v", got, reply)
	}
	if _, ok := s.sessionResolver.GetConversationForAdmin(legacy.ConversationID); !ok {
		t.Fatal("the old transcript was lost during upgrade recovery")
	}
}

func TestConversationPipelineStoresNormalizedJSONReply(t *testing.T) {
	s, exchanges := newHistoryPipeline(t, "```json\n{\"path\":\"alpha\"}\n```", "Acknowledged.")
	first := []oaiMsg{{Role: "user", Content: "Choose a directory and return JSON."}}
	reply := historyChat(t, s, oaiReq{Messages: first, ResponseFormat: &responseFormat{Type: "json_object"}})
	original := <-exchanges
	if reply.Content != `{"path":"alpha"}` {
		t.Fatalf("reply was not normalized: %+v", reply)
	}
	next := append(completedConversationHistory(first, reply), oaiMsg{Role: "user", Content: "Use that directory."})
	historyChat(t, s, oaiReq{Messages: next})
	continued := <-exchanges
	if continued.ConversationID != original.ConversationID || continued.Prompt != "[user]\nUse that directory." {
		t.Fatalf("cache used the raw upstream reply instead of the returned JSON: %+v", continued)
	}
}

func TestConversationPipelineResponsesRetainsToolOrder(t *testing.T) {
	toolReply := "```json\n" + `{"tool_calls":[{"id":"a","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}},{"id":"b","function":{"name":"read_file","arguments":"{\"path\":\"b.txt\"}"}}]}` + "\n```"
	s, exchanges := newHistoryPipeline(t, toolReply, "Both files contain text.")
	s.settings.v.MaxToolCallsPerTurn = 4
	id, _ := historyResponses(t, s, map[string]any{
		"stream": true, "input": "Read a.txt and b.txt.",
		"tools": []any{map[string]any{
			"type": "function", "name": "read_file",
			"parameters": map[string]any{
				"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"},
			},
		}},
	})
	original := <-exchanges
	r := historyTestRequest()
	node := s.responseMessages[responseNamespace(tenantFromRequest(r), "")][id]
	if node == nil || len(node.ToolCalls) != 2 {
		t.Fatalf("expected two pending tools: %+v", node)
	}
	calls := node.Messages[len(node.Messages)-1].ToolCalls
	var input []any
	for i, path := range []string{"a.txt", "b.txt"} {
		fn := calls[i]["function"].(map[string]any)
		if fn["arguments"] != `{"path":"`+path+`"}` {
			t.Fatalf("tool call order changed: %+v", calls)
		}
		input = append(input, map[string]any{"type": "function_call_output", "call_id": calls[i]["id"], "output": "contents of " + path})
	}
	_, answer := historyResponses(t, s, map[string]any{"stream": true, "previous_response_id": id, "input": input})
	continued := <-exchanges
	if continued.ConversationID != original.ConversationID || answer != "Both files contain text." {
		t.Fatalf("Responses changed the emitted tool history: request=%+v answer=%q", continued, answer)
	}
}
