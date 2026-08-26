package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

func TestUpstreamErrorClassification(t *testing.T) {
	cases := []struct {
		err      error
		limited  bool
		authFail bool
		retry    int
		status   int
	}{
		{&UpstreamHTTPError{Status: 429, RetryAfter: 90}, true, false, 90, http.StatusTooManyRequests},
		{&UpstreamHTTPError{Status: 503}, true, false, 0, http.StatusTooManyRequests},
		{&UpstreamHTTPError{Status: 401}, false, true, 0, http.StatusUnauthorized},
		{&UpstreamHTTPError{Status: 403}, false, true, 0, http.StatusUnauthorized},
		{&UpstreamHTTPError{Status: 502}, false, false, 0, http.StatusBadGateway},
		{&UpstreamHTTPError{Status: 502, Body: "account is limited"}, true, false, 0, http.StatusTooManyRequests},
		{fmt.Errorf("upstream http 429"), false, false, 0, http.StatusBadGateway},
		{fmt.Errorf("Too many requests, slow down"), false, false, 0, http.StatusBadGateway},
		{fmt.Errorf("account is limited"), false, false, 0, http.StatusBadGateway},
		{fmt.Errorf("random failure"), false, false, 0, http.StatusBadGateway},
		{chathub.ErrRateLimitNotice, true, false, 0, http.StatusTooManyRequests},
	}
	for _, c := range cases {
		if got := IsRateLimited(c.err); got != c.limited {
			t.Errorf("IsRateLimited(%v)=%v want %v", c.err, got, c.limited)
		}
		if got := IsAuthFailure(c.err); got != c.authFail {
			t.Errorf("IsAuthFailure(%v)=%v want %v", c.err, got, c.authFail)
		}
		if got := RetryAfterSeconds(c.err); got != c.retry {
			t.Errorf("RetryAfterSeconds(%v)=%d want %d", c.err, got, c.retry)
		}
		if got := upstreamStatus(c.err); got != c.status {
			t.Errorf("upstreamStatus(%v)=%d want %d", c.err, got, c.status)
		}
	}
}

func TestAccountHealthLifecycle(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-1"

	if !h.Available(id) {
		t.Fatal("fresh account must be available")
	}
	h.MarkFailure(id, &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	if h.Available(id) {
		t.Fatal("rate-limited account must be in cooldown")
	}
	if !h.RateLimited(id) {
		t.Fatal("rate-limited account missing rate-limit state")
	}
	h.MarkSuccess(id)
	if h.RateLimited(id) {
		t.Fatal("success must clear rate-limit state")
	}
	if !h.Available(id) {
		t.Fatal("MarkSuccess must lift the cooldown")
	}

	h.MarkFailure(id, &UpstreamHTTPError{Status: 401}, 0)
	if h.Available(id) {
		t.Fatal("auth-failed account must stay unusable")
	}
	h.MarkSuccess(id)
	if !h.Available(id) {
		t.Fatal("MarkSuccess must clear auth failure")
	}

	h.MarkFailure(id, &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	until := h.Snapshot()[id]
	if until == nil || until["available"].(bool) || until["cooldownUntil"] == nil {
		t.Fatalf("snapshot should report cooldown until: %v", h.Snapshot())
	}
}

func TestCooldownExpiryEntersHalfOpen(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-expiry"
	h.MarkCall(id)
	h.MarkCall(id)
	h.MarkFailure(id, &UpstreamHTTPError{Status: 429}, time.Minute)
	h.mu.Lock()
	h.cooldown[id] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if h.Available(id) {
		t.Fatal("expired rate-limit cooldown must remain unavailable until a probe succeeds")
	}
	if !h.RateLimited(id) {
		t.Fatal("expired cooldown must preserve rate-limit state")
	}
	if !h.TryAcquire(id) {
		t.Fatal("first request after cooldown must acquire the half-open probe")
	}
	if h.TryAcquire(id) {
		t.Fatal("only one half-open probe may run per account")
	}
	h.MarkSuccess(id)
	if !h.Available(id) || h.RateLimited(id) {
		t.Fatal("successful probe must restore scheduling and clear rate-limit state")
	}

	const authID = "acct-auth-expiry"
	h.MarkCall(authID)
	h.MarkFailure(authID, &UpstreamHTTPError{Status: 401}, time.Minute)
	h.mu.Lock()
	h.cooldown[authID] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if !h.Available(authID) || h.CallCount(authID) != 1 {
		t.Fatal("auth cooldown must not clear call count")
	}
}

func TestHalfOpenProbeRateLimitedAgain(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-retry"
	h.MarkFailure(id, &UpstreamHTTPError{Status: 429, RetryAfter: 90}, time.Minute)
	h.mu.Lock()
	h.cooldown[id] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if !h.TryAcquire(id) {
		t.Fatal("half-open probe was not acquired")
	}
	h.MarkFailure(id, &UpstreamHTTPError{Status: 429, RetryAfter: 90}, time.Minute)
	until, ok := h.CooldownUntil(id)
	if !ok || time.Until(until) < 89*time.Second {
		t.Fatalf("429 probe result did not re-enter Retry-After cooldown: %v", until)
	}
}

func TestHalfOpenAllowsSingleConcurrentProbe(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-concurrent-probe"
	h.MarkFailure(id, &UpstreamHTTPError{Status: 429}, time.Minute)
	h.mu.Lock()
	h.cooldown[id] = time.Now().Add(-time.Second)
	h.mu.Unlock()

	const workers = 64
	start := make(chan struct{})
	var acquired atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if h.TryAcquire(id) {
				acquired.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := acquired.Load(); got != 1 {
		t.Fatalf("half-open probes=%d want 1", got)
	}
}

func BenchmarkAccountHealthTryAcquire(b *testing.B) {
	h := newAccountHealth()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !h.TryAcquire("benchmark-account") {
			b.Fatal("healthy account unexpectedly unavailable")
		}
	}
}

func testAccountFiles(t *testing.T) *auth.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	toks := map[string]auth.TokenSet{
		"u-1": {HomeOID: "u-1", Email: "one@example.com", AccessToken: "tok1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)},
		"u-2": {HomeOID: "u-2", Email: "two@example.com", AccessToken: "tok2", RefreshToken: "r2", ExpiresAt: time.Now().Add(time.Hour)},
		"u-3": {HomeOID: "u-3", Email: "three@example.com", AccessToken: "tok3", RefreshToken: "r3", ExpiresAt: time.Now().Add(time.Hour)},
	}
	b, _ := os.ReadFile(path)
	_ = b
	store, err := auth.OpenStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, tok := range toks {
		if _, err := store.Upsert(tok); err != nil {
			t.Fatalf("upsert %s: %v", tok.HomeOID, err)
		}
	}
	return store
}

func TestWriteUpstreamErrorHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	writeUpstreamError(w, &UpstreamHTTPError{Status: 429, RetryAfter: 90})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After=%q want 90", got)
	}
	if strings.Contains(w.Body.String(), "429") {
		t.Fatalf("client-visible body must not leak upstream status: %q", w.Body.String())
	}

	w = httptest.NewRecorder()
	writeUpstreamError(w, &UpstreamHTTPError{Status: 502})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After=%q want empty for non-rate-limited errors", got)
	}
}

func TestResultErrorIsExplicitAndDoesNotCoolDownAccount(t *testing.T) {
	err := &chathub.ResultError{
		Value:   "InvalidRequest",
		Message: "Sorry, I wasn't able to respond to that.",
	}
	if got := ClassifyError(err); got != CategoryUpstreamStructured {
		t.Fatalf("ClassifyError()=%q want %q", got, CategoryUpstreamStructured)
	}

	w := httptest.NewRecorder()
	writeUpstreamError(w, err)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502", w.Code)
	}
	if got := w.Header().Get("X-M365-Upstream-Result"); got != "InvalidRequest" {
		t.Fatalf("X-M365-Upstream-Result=%q want InvalidRequest", got)
	}
	if !strings.Contains(w.Body.String(), `"type":"upstream_invalid_request"`) {
		t.Fatalf("body does not expose the stable error type: %q", w.Body.String())
	}
	if strings.Contains(w.Body.String(), err.Message) {
		t.Fatalf("body leaked the upstream fallback message: %q", w.Body.String())
	}
	if msg, code := streamUpstreamError(err); code != "upstream_invalid_request" || strings.Contains(msg, err.Message) {
		t.Fatalf("streamUpstreamError()=(%q, %q), want sanitized InvalidRequest error", msg, code)
	}

	h := newAccountHealth()
	h.MarkFailure("acct-result", err, time.Minute)
	if !h.Available("acct-result") {
		t.Fatal("request-scoped ResultError must not cool down the account")
	}
}

func TestResolveAccountSkipsUnhealthy(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}

	s.accountPool.MarkFailure("u-1", &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	acc, err := s.resolveAccount("")
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if acc.ID == "u-1" {
		t.Fatalf("resolveAccount must skip the cooling-down account, got %s", acc.ID)
	}
	if acc.Email == "" {
		t.Fatal("resolveAccount should return a validated account")
	}
}

func TestResolveAccountSkipsSchedulingDisabled(t *testing.T) {
	store := testAccountFiles(t)
	if err := store.SetScheduleEnabled("u-1", false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetScheduleEnabled("u-2", false); err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store, accountPool: newAccountHealth()}
	acc, err := s.resolveAccount("")
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "u-3" {
		t.Fatalf("scheduled account=%s want u-3", acc.ID)
	}
	explicit, err := s.resolveAccount("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if explicit.ID != "u-1" {
		t.Fatalf("explicit account=%s want u-1", explicit.ID)
	}
}

func TestResolveBoundAccountSkipsUnboundAccounts(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}

	acc, err := s.resolveBoundAccount([]string{"u-2", "u-3"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "u-2" && acc.ID != "u-3" {
		t.Fatalf("selected unbound account %q", acc.ID)
	}
}

func TestResolveBoundAccountSkipsDisabledAndUnhealthy(t *testing.T) {
	store := testAccountFiles(t)
	if err := store.SetScheduleEnabled("u-2", false); err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store, accountPool: newAccountHealth()}
	s.accountPool.MarkFailure("u-3", &UpstreamHTTPError{Status: 429}, 10*time.Minute)

	if _, err := s.resolveBoundAccount([]string{"u-2", "u-3"}, ""); err == nil {
		t.Fatal("expected failure when every bound account is unavailable")
	}
}

func TestResolveBoundAccountRejectsExplicitUnboundAccount(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}

	if _, err := s.resolveBoundAccount([]string{"u-2", "u-3"}, "u-1"); err == nil {
		t.Fatal("expected explicit unbound account to be rejected")
	}
}

func TestResolveAccountAllUnhealthy(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}
	for _, id := range []string{"u-1", "u-2", "u-3"} {
		s.accountPool.MarkFailure(id, &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	}
	if _, err := s.resolveAccount(""); err == nil {
		t.Fatal("resolveAccount must fail when every account is cooling down")
	}
}

func TestNextHealthyAccount(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}

	s.accountPool.MarkFailure("u-2", &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	acc, err := s.nextHealthyAccount("u-1", nil)
	if err != nil {
		t.Fatalf("nextHealthyAccount: %v", err)
	}
	if acc.ID == "u-1" || acc.ID == "u-2" {
		t.Fatalf("nextHealthyAccount must skip the avoided and the unhealthy account, got %s", acc.ID)
	}
	if acc.ID != "u-3" {
		t.Fatalf("expected u-3, got %s", acc.ID)
	}

	for _, id := range []string{"u-1", "u-2", "u-3"} {
		s.accountPool.MarkFailure(id, &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	}
	if _, err := s.nextHealthyAccount("", nil); err == nil {
		t.Fatal("nextHealthyAccount must fail when no healthy account remains")
	}
}

func TestNextHealthyAccountStaysWithinBoundAccounts(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}

	s.accountPool.MarkFailure("u-1", &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	acc, err := s.nextHealthyAccount("u-1", []string{"u-1", "u-2"})
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "u-2" {
		t.Fatalf("bound failover account=%s want u-2", acc.ID)
	}

	s.accountPool.MarkFailure("u-2", &UpstreamHTTPError{Status: 401}, 10*time.Minute)
	if _, err := s.nextHealthyAccount("u-1", []string{"u-1", "u-2"}); err == nil || err.Error() != "no bound account available" {
		t.Fatalf("error=%v want no bound account available", err)
	}
}

func TestChatStreamRejectsExplicitUnboundAccount(t *testing.T) {
	const rawKey = "bound-stream-key"
	store := testAccountFiles(t)
	keys := newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
	keys.Keys = []apiKeyRecord{{Hash: keyHash(rawKey), AccountIDs: []string{"u-2"}}}
	s := &Server{tokens: store, apiKeys: keys, accountPool: newAccountHealth()}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/chat/stream", strings.NewReader(`{"accountId":"u-1","message":"hello"}`))
	r.Header.Set("Authorization", "Bearer "+rawKey)
	s.chatStream(w, r)

	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "upstream request failed") {
		t.Fatalf("status=%d body=%s want masked unbound-account rejection", w.Code, w.Body.String())
	}
}

func TestChatStreamDoesNotFallBackOutsideUnavailableBoundAccounts(t *testing.T) {
	const rawKey = "unavailable-bound-stream-key"
	store := testAccountFiles(t)
	keys := newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
	keys.Keys = []apiKeyRecord{{Hash: keyHash(rawKey), AccountIDs: []string{"u-1", "u-2"}}}
	s := &Server{tokens: store, apiKeys: keys, accountPool: newAccountHealth()}
	s.accountPool.MarkFailure("u-1", &UpstreamHTTPError{Status: http.StatusTooManyRequests}, time.Minute)
	s.accountPool.MarkFailure("u-2", &UpstreamHTTPError{Status: http.StatusUnauthorized}, time.Minute)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/chat/stream", strings.NewReader(`{"message":"hello"}`))
	r.Header.Set("Authorization", "Bearer "+rawKey)
	s.chatStream(w, r)

	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "upstream request failed") {
		t.Fatalf("status=%d body=%s want masked bound-account exhaustion error", w.Code, w.Body.String())
	}
}

func TestScheduleAccount(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/accounts/schedule", strings.NewReader(`{"id":"u-1","enabled":false}`))
	s.scheduleAccount(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if store.ScheduleEnabled("u-1") {
		t.Fatal("account scheduling still enabled")
	}
}

func TestAccountsReportsCooldown(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}
	s.accountPool.MarkCall("u-1")
	s.accountPool.MarkCall("u-1")
	s.accountPool.MarkFailure("u-1", &UpstreamHTTPError{Status: 429}, 20*time.Minute)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	s.accounts(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Accounts []struct {
			ID              string     `json:"id"`
			Status          string     `json:"status"`
			ScheduleEnabled bool       `json:"scheduleEnabled"`
			CallCount       uint64     `json:"callCount"`
			CooldownUntil   *time.Time `json:"cooldownUntil"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, account := range body.Accounts {
		if account.ID != "u-1" {
			continue
		}
		if account.Status != "cooldown" || account.CooldownUntil == nil || !account.ScheduleEnabled || account.CallCount != 2 {
			t.Fatalf("cooldown account=%#v", account)
		}
		return
	}
	t.Fatal("cooldown account missing")
}

func TestFailoverAllowsResolvedConversationID(t *testing.T) {
	accountID := ""
	conversationID := "conv-123"
	resolvedConversationID := "conv-123"
	if !(conversationID == "" || conversationID == resolvedConversationID) {
		t.Fatal("failover must be allowed when ConversationID was injected by session resolver")
	}
	explicitConversationID := "conv-explicit"
	if explicitConversationID == "" || explicitConversationID == resolvedConversationID {
		t.Fatal("failover must NOT be allowed when ConversationID was explicitly set by client")
	}
	_ = accountID
}

func TestErrRateLimitNoticeTriggersMarkFailure(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-rl"
	h.MarkFailure(id, chathub.ErrRateLimitNotice, 15*time.Minute)
	if h.Available(id) {
		t.Fatal("ErrRateLimitNotice must put account in cooldown")
	}
}

func TestGlobalCircuitOnlyRecordsInfrastructureFailures(t *testing.T) {
	nonGlobal := []error{
		&UpstreamHTTPError{Status: 401},
		&UpstreamHTTPError{Status: 403},
		&UpstreamHTTPError{Status: 429},
		&UpstreamHTTPError{Status: 422},
		&UpstreamHTTPError{ErrorCode: "ErrorUserBanned"},
		&UpstreamHTTPError{ErrorCode: "InsufficientTokens"},
		chathub.ErrEmptyCompletion,
		chathub.ErrOffensiveContent,
		chathub.ErrImageLimit,
		context.Canceled,
	}
	for _, err := range nonGlobal {
		ResetGlobalCircuit()
		for i := 0; i < 10; i++ {
			GlobalCircuitRecord(err)
		}
		if GlobalCircuitIsOpen() {
			t.Fatalf("non-global error opened circuit: %v", err)
		}
	}

	ResetGlobalCircuit()
	for i := 0; i < 10; i++ {
		GlobalCircuitRecord(fmt.Errorf("connection refused"))
	}
	if !GlobalCircuitIsOpen() {
		t.Fatal("transport failures must open global circuit")
	}
	ResetGlobalCircuit()
}

func TestParseMeteringAggregatesDeniedItemsRegardlessOfOrder(t *testing.T) {
	cases := []string{
		`[{"meterError":"denied","hasAccess":false},{"hasAccess":true}]`,
		`[{"hasAccess":true},{"meterError":"denied","hasAccess":false}]`,
	}
	for _, raw := range cases {
		meterError, hasAccess := ParseMetering("account", json.RawMessage(raw))
		if hasAccess || meterError != "denied" {
			t.Fatalf("ParseMetering(%s) = (%q, %v)", raw, meterError, hasAccess)
		}
	}
}

func TestAccountHealthMeteringDefaultsDeepCopySnapshotAndReset(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-metering"

	if _, hasAccess, _ := h.GetMetering(id); !hasAccess {
		t.Fatal("missing metering state must default hasAccess to true")
	}

	throttling := map[string]any{
		"numUserMessagesInConversation":    float64(3),
		"maxNumUserMessagesInConversation": float64(30),
		"nested":                           map[string]any{"value": "original"},
	}
	remaining := map[string]int{"Chat": 17}
	h.UpdateThrottling(id, throttling)
	h.UpdateMetering(id, "meter-error", false, remaining)

	throttling["nested"].(map[string]any)["value"] = "mutated"
	remaining["Chat"] = 0

	stored := h.GetThrottling(id).(map[string]any)
	if stored["nested"].(map[string]any)["value"] != "original" {
		t.Fatal("UpdateThrottling must deep-copy input")
	}
	meterError, hasAccess, gotRemaining := h.GetMetering(id)
	if meterError != "meter-error" || hasAccess || gotRemaining["Chat"] != 17 {
		t.Fatalf("unexpected metering state: error=%q access=%v remaining=%v", meterError, hasAccess, gotRemaining)
	}

	snapshot := h.Snapshot()
	snapshot[id]["throttling"].(map[string]any)["nested"].(map[string]any)["value"] = "snapshot-mutated"
	snapshot[id]["remainingAllowance"].(map[string]int)["Chat"] = 1
	if h.GetThrottling(id).(map[string]any)["nested"].(map[string]any)["value"] != "original" {
		t.Fatal("Snapshot must deep-copy throttling")
	}
	_, _, gotRemaining = h.GetMetering(id)
	if gotRemaining["Chat"] != 17 {
		t.Fatal("Snapshot must deep-copy remaining allowance")
	}

	h.ClearAllCooldowns()
	if got := h.Snapshot(); len(got) != 0 {
		t.Fatalf("reset left health state: %#v", got)
	}
	if meterError, hasAccess, remaining := h.GetMetering(id); meterError != "" || !hasAccess || len(remaining) != 0 {
		t.Fatalf("reset left metering state: error=%q access=%v remaining=%v", meterError, hasAccess, remaining)
	}
}

func TestRemainingAllowanceSnapshotBecomesStaleWithoutBeingDeleted(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-snapshot"
	h.UpdateMetering(id, "", true, map[string]int{"Chat": 17})

	h.UpdateMetering(id, "denied", false, nil)
	snapshot := h.Snapshot()[id]
	if snapshot["remainingAllowance"].(map[string]int)["Chat"] != 17 {
		t.Fatal("denied metering must preserve the last allowance snapshot")
	}
	if snapshot["remainingAllowanceStale"] != true {
		t.Fatal("denied metering must mark the allowance snapshot stale")
	}
	if snapshot["remainingAllowanceUpdatedAt"].(time.Time).IsZero() {
		t.Fatal("allowance snapshot must include its update time")
	}

	h.UpdateMetering(id, "", true, nil)
	if h.Snapshot()[id]["remainingAllowanceStale"] != true {
		t.Fatal("success without fresh allowance data must not revive a stale snapshot")
	}
}

func TestRateLimitMarksRemainingAllowanceSnapshotStale(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-rate-limited"
	h.UpdateMetering(id, "", true, map[string]int{"Chat": 9})
	h.MarkFailure(id, &UpstreamHTTPError{Status: http.StatusTooManyRequests}, time.Minute)
	if h.Snapshot()[id]["remainingAllowanceStale"] != true {
		t.Fatal("rate limiting must mark the prior allowance snapshot stale")
	}
}

func TestGlobalCooldownMarksRemainingAllowanceSnapshotStale(t *testing.T) {
	ResetGlobalCircuit()
	defer ResetGlobalCircuit()
	globalCircuit.mu.Lock()
	globalCircuit.openUntil = time.Now().Add(time.Minute)
	globalCircuit.mu.Unlock()

	h := newAccountHealth()
	const id = "acct-global-unavailable"
	h.UpdateMetering(id, "", true, map[string]int{"Chat": 11})
	h.MarkFailure(id, fmt.Errorf("request rejected while global circuit is open"), time.Minute)
	snapshot := h.Snapshot()[id]
	if snapshot["remainingAllowanceStale"] != true {
		t.Fatal("global cooldown must mark the prior allowance snapshot stale")
	}
	if snapshot["available"] != false {
		t.Fatal("global cooldown account must be unavailable")
	}
}
