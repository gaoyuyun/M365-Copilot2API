package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRealtimeUnsupportedIsJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	realtimeUnsupported(rr, httptest.NewRequest(http.MethodGet, "/v1/realtime", nil))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type=%q", got)
	}
}
