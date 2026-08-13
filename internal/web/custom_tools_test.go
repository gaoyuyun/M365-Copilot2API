package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func containsJSON(v []byte, key string) bool { return strings.Contains(string(v), `"`+key+`"`) }

func customCallSource() map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"tool_calls": []any{map[string]any{
					"id":   "call_exec",
					"type": "custom",
					"function": map[string]any{
						"name":      "exec",
						"arguments": `{"input":"uname -s"}`,
					},
				}},
			},
		}},
	}
}

func TestResponsesResultWritesCustomToolCall(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "m", false, customCallSource())
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	output := response["output"].([]any)
	call := output[0].(map[string]any)
	if call["type"] != "custom_tool_call" || call["name"] != "exec" || call["input"] != "uname -s" {
		t.Fatalf("custom output=%#v", call)
	}
}

func TestResponsesResultPreservesNestedPathInCustomExecInput(t *testing.T) {
	input := "const r = await tools.apply_patch(`*** Begin Patch\n*** Update File: internal/web/protocol_compat.go\n*** End Patch`);\ntext(r);"
	src := customCallSource()
	msg, _ := openAIChoice(src)
	call := msg["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	fn["arguments"] = mustJSON(map[string]any{"input": input})

	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "m", false, src)
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	got := response["output"].([]any)[0].(map[string]any)["input"]
	if got != input {
		t.Fatalf("custom input changed across Responses conversion:\ngot  %q\nwant %q", got, input)
	}
}

func TestResponsesStreamWritesCustomToolEvents(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "m", true, customCallSource())
	body := rr.Body.String()
	for _, want := range []string{"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", `"input":"uname -s"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in stream: %s", want, body)
		}
	}
}
