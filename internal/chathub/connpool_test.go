package chathub

import (
	"errors"
	"testing"
	"time"
)

func TestTakeReturnsSocketBoundIdentity(t *testing.T) {
	options := optionsForRequest(Request{})
	want := &pooledConn{
		created:   time.Now(),
		handshook: true,
		options:   options,
		identity: connectionIdentity{
			requestID:      "request-from-upgrade",
			sessionID:      "session-from-upgrade",
			conversationID: "conversation-from-upgrade",
		},
	}
	pool := &ConnPool{
		conns:   map[string][]*pooledConn{"oid|tid": {want}},
		warming: map[string]int{},
	}

	got, ok := pool.Take("oid", "tid", options)
	if !ok || got != want {
		t.Fatalf("Take() = (%p, %v), want (%p, true)", got, ok, want)
	}
	if got.identity.requestID != "request-from-upgrade" || got.identity.sessionID != "session-from-upgrade" || got.identity.conversationID != "conversation-from-upgrade" {
		t.Fatalf("Take() lost the socket-bound identity: %#v", got.identity)
	}
	if len(pool.conns) != 0 {
		t.Fatalf("leased connection must be one-shot, pool=%#v", pool.conns)
	}
}

func TestTakeKeepsConnectionWhenOptionsDoNotMatch(t *testing.T) {
	wantOptions := optionsForRequest(Request{LicenseType: "Starter", Scenario: "OfficeWebIncludedCopilot"})
	pc := &pooledConn{created: time.Now(), handshook: true, options: wantOptions}
	pool := &ConnPool{
		conns:   map[string][]*pooledConn{"oid|tid": {pc}},
		warming: map[string]int{},
	}

	if got, ok := pool.Take("oid", "tid", optionsForRequest(Request{DisableMemory: true})); ok || got != nil {
		t.Fatalf("Take() returned an incompatible lease: (%p, %v)", got, ok)
	}
	if len(pool.conns["oid|tid"]) != 1 || pool.conns["oid|tid"][0] != pc {
		t.Fatal("an options mismatch must not consume the pooled connection")
	}
}

func TestCanUsePrewarmedOnlyForUnboundConversation(t *testing.T) {
	if !canUsePrewarmed(Request{}) {
		t.Fatal("new request should be eligible for a prewarmed identity")
	}
	if canUsePrewarmed(Request{SessionID: "existing"}) {
		t.Fatal("request with a session ID must dial its own URL")
	}
	if canUsePrewarmed(Request{ConversationID: "existing"}) {
		t.Fatal("request with a conversation ID must dial its own URL")
	}
}

func TestValidateResultRejectsInvalidRequest(t *testing.T) {
	for _, value := range []string{"", "Success", "success"} {
		if err := validateResult(value, "ok"); err != nil {
			t.Fatalf("validateResult(%q) = %v, want nil", value, err)
		}
	}

	err := validateResult("InvalidRequest", "Sorry, I wasn't able to respond to that.")
	var resultErr *ResultError
	if !errors.As(err, &resultErr) {
		t.Fatalf("validateResult() error = %T, want *ResultError", err)
	}
	if !IsInvalidRequestResult(err) {
		t.Fatalf("IsInvalidRequestResult(%v) = false", err)
	}
	if resultErr.Message == "" {
		t.Fatal("server-side diagnostic message was not retained")
	}
}
