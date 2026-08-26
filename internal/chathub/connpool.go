package chathub

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type connectionIdentity struct {
	requestID      string
	sessionID      string
	conversationID string
}

type connectionOptions struct {
	licenseType   string
	scenario      string
	disableMemory bool
}

func optionsForRequest(req Request) connectionOptions {
	licenseType := req.LicenseType
	if licenseType == "" {
		licenseType = "Starter"
	}
	scenario := req.Scenario
	if scenario == "" {
		scenario = "OfficeWebIncludedCopilot"
	}
	return connectionOptions{
		licenseType:   licenseType,
		scenario:      scenario,
		disableMemory: req.DisableMemory,
	}
}

type pooledConn struct {
	conn      *websocket.Conn
	created   time.Time
	handshook bool
	identity  connectionIdentity
	options   connectionOptions
	taken     atomic.Bool
	writeMu   sync.Mutex
	frames    chan []byte
	errs      chan error
}

const (
	maxPoolPerKey = 2
	poolConnTTL   = 300 * time.Second
)

type ConnPool struct {
	mu      sync.Mutex
	conns   map[string][]*pooledConn // key = oid|tid
	warming map[string]int
	dialer  *websocket.Dialer
	header  http.Header
	stop    chan struct{}
	closed  bool
}

func NewConnPool(dialer *websocket.Dialer, header http.Header) *ConnPool {
	p := &ConnPool{
		conns:   make(map[string][]*pooledConn),
		warming: make(map[string]int),
		dialer:  dialer,
		header:  header,
		stop:    make(chan struct{}),
	}
	go p.gcLoop()
	return p
}

func (p *ConnPool) key(oid, tid string) string { return oid + "|" + tid }

// Take returns only a fully handshaken, unused connection whose URL options
// match the new request. The returned identity is inseparable from the socket:
// ChatHub binds these values during the WebSocket upgrade.
func (p *ConnPool) Take(oid, tid string, options connectionOptions) (*pooledConn, bool) {
	p.mu.Lock()
	key := p.key(oid, tid)
	conns := p.conns[key]
	var picked *pooledConn
	var stale []*pooledConn
	kept := conns[:0]
	for i := len(conns) - 1; i >= 0; i-- {
		pc := conns[i]
		if time.Since(pc.created) >= poolConnTTL {
			stale = append(stale, pc)
			continue
		}
		if picked == nil && pc.handshook && pc.options == options {
			picked = pc
			pc.taken.Store(true)
			continue
		}
		kept = append(kept, pc)
	}
	if len(kept) == 0 {
		delete(p.conns, key)
	} else {
		p.conns[key] = kept
	}
	p.mu.Unlock()

	for _, pc := range stale {
		pc.taken.Store(true)
		_ = pc.conn.Close()
	}
	if picked == nil {
		return nil, false
	}
	log.Printf("[connpool] hit oid=%s age_ms=%d request_id=%s", oid, time.Since(picked.created).Milliseconds(), picked.identity.requestID)
	return picked, true
}

// Warm creates a one-shot connection and records the exact identity used in
// its upgrade URL. A later request must adopt this identity before sending its
// payload; callers cannot supply a detached URL.
func (p *ConnPool) Warm(ctx context.Context, acc Account, options connectionOptions) {
	if acc.AccessToken == "" || acc.OID == "" || acc.TID == "" {
		return
	}
	identity := connectionIdentity{
		requestID:      uuid.NewString(),
		sessionID:      uuid.NewString(),
		conversationID: uuid.NewString(),
	}
	wsURL, err := BuildWSURLWithOptions(acc, identity.sessionID, identity.conversationID, identity.requestID, options.licenseType, options.scenario, options.disableMemory)
	if err != nil {
		log.Printf("[connpool] warm URL failed oid=%s err=%v", acc.OID, err)
		return
	}
	p.warmURL(ctx, acc.OID, acc.TID, wsURL, identity, options)
}

// warmURL parks a connection dialed at wsURL under the supplied identity. Warm
// is the only production entry point: it derives wsURL from the identity it
// generates, so the two can never drift apart. This split exists so tests can
// park a connection against a local endpoint.
func (p *ConnPool) warmURL(ctx context.Context, oid, tid, wsURL string, identity connectionIdentity, options connectionOptions) {
	key := p.key(oid, tid)
	if !p.reserveWarm(key) {
		return
	}
	defer p.releaseWarm(key)

	conn, resp, err := p.dialer.DialContext(ctx, wsURL, p.header.Clone())
	if err != nil {
		if resp != nil {
			log.Printf("[connpool] warm dial failed oid=%s status=%d err=%v", oid, resp.StatusCode, err)
		} else {
			log.Printf("[connpool] warm dial failed oid=%s err=%v", oid, err)
		}
		return
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"protocol":"json","version":1}`+rs)); err != nil {
		log.Printf("[connpool] warm handshake send failed oid=%s err=%v", oid, err)
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, handshake, err := conn.ReadMessage()
	if err != nil {
		log.Printf("[connpool] warm handshake recv failed oid=%s err=%v", oid, err)
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if strings.TrimSpace(strings.TrimSuffix(string(handshake), rs)) != "{}" {
		log.Printf("[connpool] warm handshake rejected oid=%s", oid)
		_ = conn.Close()
		return
	}

	pc := &pooledConn{
		conn:      conn,
		created:   time.Now(),
		handshook: true,
		identity:  identity,
		options:   options,
		frames:    make(chan []byte, 64),
		errs:      make(chan error, 1),
	}
	p.mu.Lock()
	if p.closed || len(p.conns[key]) >= maxPoolPerKey {
		p.mu.Unlock()
		_ = conn.Close()
		return
	}
	p.conns[key] = append(p.conns[key], pc)
	p.startReader(key, pc)
	p.mu.Unlock()

	log.Printf("[connpool] warmed connection oid=%s tid=%s request_id=%s", oid, tid, identity.requestID)
}

func (p *ConnPool) reserveWarm(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || len(p.conns[key])+p.warming[key] >= maxPoolPerKey {
		return false
	}
	p.warming[key]++
	return true
}

func (p *ConnPool) releaseWarm(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.warming[key] <= 1 {
		delete(p.warming, key)
		return
	}
	p.warming[key]--
}

// startReader is the connection's permanent single reader. While parked it
// answers SignalR pings; after Take it forwards every frame to the chat loop.
// Gorilla WebSocket connections cannot safely transfer read ownership after a
// timeout, so this goroutine remains the only reader for the socket's lifetime.
func (p *ConnPool) startReader(key string, pc *pooledConn) {
	go func() {
		defer close(pc.frames)
		for {
			_, msg, err := pc.conn.ReadMessage()
			if err != nil {
				if pc.taken.Load() {
					select {
					case pc.errs <- err:
					default:
					}
				} else {
					p.evict(key, pc)
				}
				return
			}
			if strings.HasPrefix(string(msg), `{"type":6}`) && !pc.taken.Load() {
				pc.writeMu.Lock()
				_ = pc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err = pc.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":6}`+rs))
				pc.writeMu.Unlock()
				if err != nil {
					p.evict(key, pc)
					return
				}
				continue
			}
			if !pc.taken.Load() {
				continue
			}
			select {
			case pc.frames <- msg:
			case <-p.stop:
				_ = pc.conn.Close()
				return
			case <-time.After(30 * time.Second):
				select {
				case pc.errs <- fmt.Errorf("pooled connection consumer stalled"):
				default:
				}
				_ = pc.conn.Close()
				return
			}
		}
	}()
}

func (p *ConnPool) evict(key string, target *pooledConn) {
	p.mu.Lock()
	conns := p.conns[key]
	for i, pc := range conns {
		if pc == target {
			p.conns[key] = append(conns[:i], conns[i+1:]...)
			break
		}
	}
	if len(p.conns[key]) == 0 {
		delete(p.conns, key)
	}
	p.mu.Unlock()
	_ = target.conn.Close()
}

func (p *ConnPool) Return(oid, tid string, conn *websocket.Conn) {
	if conn != nil {
		_ = conn.Close()
	}
}

func (p *ConnPool) Discard(oid, tid string, conn *websocket.Conn) {
	if conn != nil {
		_ = conn.Close()
	}
}

func (p *ConnPool) GC() {
	p.mu.Lock()
	now := time.Now()
	var stale []*pooledConn
	for key, conns := range p.conns {
		kept := conns[:0]
		for _, pc := range conns {
			if now.Sub(pc.created) >= poolConnTTL {
				pc.taken.Store(true)
				stale = append(stale, pc)
			} else {
				kept = append(kept, pc)
			}
		}
		if len(kept) == 0 {
			delete(p.conns, key)
		} else {
			p.conns[key] = kept
		}
	}
	p.mu.Unlock()
	for _, pc := range stale {
		_ = pc.conn.Close()
	}
}

func (p *ConnPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	var conns []*pooledConn
	for key, entries := range p.conns {
		for _, pc := range entries {
			pc.taken.Store(true)
			conns = append(conns, pc)
		}
		delete(p.conns, key)
	}
	p.mu.Unlock()
	for _, pc := range conns {
		_ = pc.conn.Close()
	}
}

func (p *ConnPool) Stats() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	details := make([]map[string]any, 0)
	for key, conns := range p.conns {
		for _, pc := range conns {
			total++
			details = append(details, map[string]any{
				"key":        key,
				"age_ms":     time.Since(pc.created).Milliseconds(),
				"handshook":  pc.handshook,
				"request_id": pc.identity.requestID,
			})
		}
	}
	return map[string]any{"mode": "identity_aware_connpool", "pooled_connections": total, "warming_connections": p.totalWarming(), "details": details}
}

func (p *ConnPool) totalWarming() int {
	total := 0
	for _, count := range p.warming {
		total += count
	}
	return total
}

func (p *ConnPool) gcLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.GC()
		}
	}
}
