package web

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed all:web
var webFS embed.FS

var webContent http.FileSystem

func init() {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	webContent = http.FS(sub)
}

const defaultMaxRequestBodyBytes int64 = 8 << 20

// requestBodyLimit is the last-resort guard for endpoints that do not have a
// more specific decoder limit. Individual handlers may still choose a lower
// bound for protocol-specific payloads.
func requestBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			limit := defaultMaxRequestBodyBytes
			if raw := strings.TrimSpace(os.Getenv("M365_MAX_REQUEST_BODY_BYTES")); raw != "" {
				if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed >= 1<<20 && parsed <= 64<<20 {
					limit = parsed
				}
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

var (
	sseHeartbeatTick         = 5 * time.Second
	sseHeartbeatIdle         = 15 * time.Second
	sseHeartbeatWriteTimeout = 5 * time.Second
)

type sseResponseWriter struct {
	http.ResponseWriter
	mu         sync.Mutex
	flusher    http.Flusher
	started    chan struct{}
	stop       chan struct{}
	headerOnce sync.Once
	stopOnce   sync.Once
	lastWrite  atomic.Int64
}

func newSSEResponseWriter(w http.ResponseWriter) *sseResponseWriter {
	s := &sseResponseWriter{ResponseWriter: w, started: make(chan struct{}), stop: make(chan struct{})}
	s.flusher, _ = w.(http.Flusher)
	s.lastWrite.Store(time.Now().UnixNano())
	return s
}

func (s *sseResponseWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *sseResponseWriter) startIfSSELocked() {
	if !strings.HasPrefix(strings.ToLower(s.Header().Get("Content-Type")), "text/event-stream") {
		return
	}
	s.headerOnce.Do(func() {
		h := s.Header()
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		close(s.started)
	})
}

func (s *sseResponseWriter) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startIfSSELocked()
	s.ResponseWriter.WriteHeader(code)
}

func (s *sseResponseWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startIfSSELocked()
	n, err := s.ResponseWriter.Write(p)
	if n > 0 {
		s.lastWrite.Store(time.Now().UnixNano())
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return n, err
}

func (s *sseResponseWriter) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startIfSSELocked()
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *sseResponseWriter) heartbeat(ctx context.Context, cancel context.CancelFunc) {
	select {
	case <-s.started:
	case <-s.stop:
		return
	case <-ctx.Done():
		return
	}
	ticker := time.NewTicker(sseHeartbeatTick)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if time.Since(time.Unix(0, s.lastWrite.Load())) < sseHeartbeatIdle {
				continue
			}
			if err := s.writeHeartbeat(); err != nil {
				cancel()
				return
			}
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *sseResponseWriter) writeHeartbeat() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	controller := http.NewResponseController(s.ResponseWriter)
	_ = controller.SetWriteDeadline(time.Now().Add(sseHeartbeatWriteTimeout))
	n, err := s.ResponseWriter.Write([]byte(": keepalive\n\n"))
	_ = controller.SetWriteDeadline(time.Time{})
	if n > 0 {
		s.lastWrite.Store(time.Now().UnixNano())
	}
	if err == nil && s.flusher != nil {
		s.flusher.Flush()
	}
	return err
}

func (s *sseResponseWriter) closeStop() { s.stopOnce.Do(func() { close(s.stop) }) }

func sseKeepaliveMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		sw := newSSEResponseWriter(w)
		done := make(chan struct{})
		go func() { defer close(done); sw.heartbeat(ctx, cancel) }()
		next.ServeHTTP(sw, r.WithContext(ctx))
		sw.closeStop()
		<-done
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; form-action 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; script-src 'self' 'unsafe-inline' https://unpkg.com https://cdn.jsdelivr.net")
		if r.URL.Path == "/" || r.URL.Path == "/login" || r.URL.Path == "/api/admin/login" || r.URL.Path == "/api/admin/session" || r.URL.Path == "/api/admin/change-password" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rootPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/login" && r.URL.Path != "/conversation" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	name := "index.html"
	if r.URL.Path == "/login" {
		name = "login.html"
	} else if r.URL.Path == "/conversation" {
		name = "conversation.html"
	}
	f, err := webContent.Open(name)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "web interface unavailable")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "web interface unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, name, st.ModTime(), f)
}
