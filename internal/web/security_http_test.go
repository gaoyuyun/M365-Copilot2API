package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestBodyLimitUsesConfiguredBound(t *testing.T) {
	t.Setenv("M365_MAX_REQUEST_BODY_BYTES", "1048576")
	h := requestBodyLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			t.Error("expected max request body error")
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 1048577)))
	r.ContentLength = 1048577
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", rr.Code)
	}
}
