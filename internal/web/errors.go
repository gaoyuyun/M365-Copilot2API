package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

var ErrOffensiveContent = errors.New("upstream content policy flagged as offensive")

func logOAuthError(stage string, err error) {
	var oauthErr *auth.OAuthError
	if errors.As(err, &oauthErr) {
		log.Printf("oauth_error stage=%s error=%q aadsts=%q http_status=%d correlation_id=%q trace_id=%q", stage, oauthErr.Code, oauthErr.AADSTS, oauthErr.HTTPStatus, oauthErr.CorrelationID, oauthErr.TraceID)
		return
	}
	log.Printf("oauth_error stage=%s error=%q", stage, "request_failed")
}

// upstreamError keeps transport details, including URLs and credentials, out
// of client-visible responses while retaining a server-side diagnostic.
func upstreamError(err error) string {
	if err == nil {
		return "upstream request failed"
	}
	log.Printf("upstream request failed: %v", err)
	return "upstream request failed"
}

// upstreamStatus maps a failed upstream call to the client-visible HTTP status:
// rate limits stay 429 (with Retry-After when known), auth failures become 401,
// everything else is 502. Unknown upstream failures must never leak internals.
func upstreamStatus(err error) int {
	if errors.Is(err, chathub.ErrOffensiveContent) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, chathub.ErrImageLimit) {
		return http.StatusTooManyRequests
	}
	if IsRateLimited(err) {
		return http.StatusTooManyRequests
	}
	if IsAuthFailure(err) {
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

func applyM365Headers(w http.ResponseWriter, err error, accountID string) {
	cat := ClassifyError(err)
	if accountID != "" {
		w.Header().Set("X-M365-Account-Id", accountID)
	} else {
		w.Header().Set("X-M365-Account-Id", "")
	}
	w.Header().Set("X-M365-Proxy-Error", string(cat))
	if chathub.IsInvalidRequestResult(err) {
		w.Header().Set("X-M365-Upstream-Result", "InvalidRequest")
	}
	if GlobalCircuitIsOpen() {
		remaining := int(time.Until(GlobalCircuitOpenUntil()).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		w.Header().Set("X-M365-Global-Circuit", fmt.Sprintf("open; retry-after=%d", remaining))
	} else {
		w.Header().Set("X-M365-Global-Circuit", "closed")
	}
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
		w.Header().Set("X-M365-Retry-After", fmt.Sprintf("%d", retry))
		w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(time.Duration(retry)*time.Second).Unix()))
	} else {
		switch cat {
		case CategoryQuota429:
			w.Header().Set("X-M365-Retry-After", "30")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(30*time.Second).Unix()))
		case CategoryOverload503:
			w.Header().Set("X-M365-Retry-After", "15")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(15*time.Second).Unix()))
		case CategoryAuthExpired401:
			w.Header().Set("X-M365-Retry-After", "120")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(2*time.Minute).Unix()))
		case CategoryForbidden403:
			w.Header().Set("X-M365-Retry-After", "86400")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(24*time.Hour).Unix()))
		}
	}
	if IsRateLimited(err) {
		w.Header().Set("X-M365-RateLimit-Remaining", "0")
	} else {
		w.Header().Set("X-M365-RateLimit-Remaining", "1")
	}
}

func writeResultError(w http.ResponseWriter, err error) bool {
	var resultErr *chathub.ResultError
	if !errors.As(err, &resultErr) {
		return false
	}
	if chathub.IsInvalidRequestResult(err) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_invalid_request", "M365 rejected the conversation request; the request was not completed")
		return true
	}
	writeOpenAIError(w, http.StatusBadGateway, "upstream_result_error", "M365 returned an unsuccessful completion")
	return true
}

func streamUpstreamError(err error) (message, code string) {
	var resultErr *chathub.ResultError
	if errors.As(err, &resultErr) {
		if chathub.IsInvalidRequestResult(err) {
			return "M365 rejected the conversation request; the request was not completed", "upstream_invalid_request"
		}
		return "M365 returned an unsuccessful completion", "upstream_result_error"
	}
	switch {
	case errors.Is(err, chathub.ErrImageLimit):
		return "image generation daily limit reached; try again tomorrow", "image_limit_error"
	case IsRateLimited(err):
		return "upstream is rate limiting; try again shortly", "rate_limit_error"
	case errors.Is(err, chathub.ErrOffensiveContent):
		return "M365 content policy blocked this request; try again or switch account", "upstream_content_blocked"
	case IsEmptyCompletion(err):
		return "upstream returned empty completion; the requested model may be unavailable for this tenant", "upstream_error"
	default:
		return upstreamError(err), "upstream_error"
	}
}

func writeUpstreamErrorWithAccount(w http.ResponseWriter, err error, accountID string) {
	applyM365Headers(w, err, accountID)
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	}
	status := upstreamStatus(err)
	if status == http.StatusTooManyRequests {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "30")
		}
		if w.Header().Get("X-M365-Retry-After") == "" {
			w.Header().Set("X-M365-Retry-After", w.Header().Get("Retry-After"))
		}
		if errors.Is(err, chathub.ErrImageLimit) {
			writeOpenAIError(w, status, "image_limit_error", "image generation daily limit reached; try again tomorrow")
			return
		}
		writeOpenAIError(w, status, "rate_limit_error", "upstream is rate limiting; try again shortly")
		return
	}
	if IsEmptyCompletion(err) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned empty completion; the requested model may be unavailable for this tenant")
		return
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_content_blocked", "M365 content policy blocked this request; try again or switch account")
		return
	}
	if writeResultError(w, err) {
		return
	}
	writeOpenAIError(w, status, "upstream_error", upstreamError(err))
}

// writeUpstreamError renders a failed upstream call as an HTTP response,
// surfacing the Retry-After hint for rate limits so clients can back off.
func writeUpstreamError(w http.ResponseWriter, err error) {
	applyM365Headers(w, err, "")
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	}
	status := upstreamStatus(err)
	if status == http.StatusTooManyRequests {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "30")
		}
		if w.Header().Get("X-M365-Retry-After") == "" {
			w.Header().Set("X-M365-Retry-After", w.Header().Get("Retry-After"))
		}
		if errors.Is(err, chathub.ErrImageLimit) {
			writeOpenAIError(w, status, "image_limit_error", "image generation daily limit reached; try again tomorrow")
			return
		}
		writeOpenAIError(w, status, "rate_limit_error", "upstream is rate limiting; try again shortly")
		return
	}
	if IsEmptyCompletion(err) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned empty completion; the requested model may be unavailable for this tenant")
		return
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_content_blocked", "M365 content policy blocked this request; try again or switch account")
		return
	}
	if writeResultError(w, err) {
		return
	}
	writeOpenAIError(w, status, "upstream_error", upstreamError(err))
}
