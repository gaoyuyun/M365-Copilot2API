package web

import (
	"path/filepath"
	"testing"
)

func TestResponseStorePersistsContinuationNodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.json")
	s := &Server{responsePath: path, responseMessages: map[string]map[string]*RespNode{}}
	s.storeResponseNode("tenant\\x00session", "resp_1", &RespNode{Messages: []oaiMsg{{Role: "user", Content: "keep"}}})
	loaded := loadResponseMessages(path)
	if loaded["tenant\\x00session"]["resp_1"] == nil {
		t.Fatal("response node was not persisted")
	}
	if got := contentToString(loaded["tenant\\x00session"]["resp_1"].Messages[0].Content); got != "keep" {
		t.Fatalf("content=%q", got)
	}
}
