package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const hubIdle = 2 * time.Minute
const hubPingEvery = 30 * time.Second

// armHubConn resets the idle deadline on websocket ping/pong so a quiet
// phone still receives events. JSON `{t:ping}` is handled in the read loop.
func armHubConn(c *websocket.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(hubIdle))
	c.SetPongHandler(func(string) error {
		return c.SetReadDeadline(time.Now().Add(hubIdle))
	})
	c.SetPingHandler(func(appData string) error {
		_ = c.SetReadDeadline(time.Now().Add(hubIdle))
		return c.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
	})
}

// runHubServer is `ccc hub [addr]`. It is a dumb encrypted pipe: it learns
// public keys and pairing codes, never plaintext. Anyone can run one; the
// public default is wss://hub.mentasystems.com.

func runHubServer(addr string) error {
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	log.Printf("ccc hub listening on %s (ws /v1/ws)", addr)
	return http.ListenAndServe(addr, newHubHandler())
}

func newHubHandler() http.Handler {
	s := newHubRelay()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/privacy", serveHubPrivacy)
	mux.HandleFunc("/privacy/", serveHubPrivacy)
	mux.HandleFunc("/v1/ws", s.handleWS)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "ccc hub — encrypted relay. Clients connect at /v1/ws\nprivacy: /privacy\n")
	})
	return mux
}

type hubRelay struct {
	upgrader websocket.Upgrader
	mu       sync.Mutex
	conns    map[string]*hubConn
	codes    map[string]hubOffer
}

type hubOffer struct {
	Instance string
	Expires  time.Time
}

type hubConn struct {
	pk   string
	role string
	c    *websocket.Conn
	wmu  sync.Mutex
}

func newHubRelay() *hubRelay {
	s := &hubRelay{
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(*http.Request) bool { return true },
			ReadBufferSize:  64 << 10,
			WriteBufferSize: 64 << 10,
		},
		conns: map[string]*hubConn{},
		codes: map[string]hubOffer{},
	}
	go s.gc()
	return s
}

func (s *hubRelay) gc() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		s.mu.Lock()
		for code, o := range s.codes {
			if now.After(o.Expires) {
				delete(s.codes, code)
			}
		}
		s.mu.Unlock()
	}
}

func (s *hubRelay) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	c.SetReadLimit(1 << 20)
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		return
	}
	var open hubFrame
	if err := json.Unmarshal(raw, &open); err != nil || open.T != "open" || open.PK == "" {
		_ = c.WriteJSON(hubFrame{V: 1, T: "err", Err: "first message must be open"})
		return
	}
	if _, err := parseHubPublic(open.PK); err != nil {
		_ = c.WriteJSON(hubFrame{V: 1, T: "err", Err: "bad public key"})
		return
	}
	if open.Role != "instance" && open.Role != "device" {
		_ = c.WriteJSON(hubFrame{V: 1, T: "err", Err: "role must be instance or device"})
		return
	}
	hc := &hubConn{pk: open.PK, role: open.Role, c: c}
	s.mu.Lock()
	if old, ok := s.conns[open.PK]; ok {
		old.c.Close()
	}
	s.conns[open.PK] = hc
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.conns[open.PK] == hc {
			delete(s.conns, open.PK)
		}
		s.mu.Unlock()
	}()
	_ = hc.write(hubFrame{V: 1, T: "open", PK: open.PK, Role: open.Role})
	armHubConn(c)

	for {
		_ = c.SetReadDeadline(time.Now().Add(hubIdle))
		_, raw, err := c.ReadMessage()
		if err != nil {
			return
		}
		var f hubFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		f.V = 1
		switch f.T {
		case "ping":
			_ = hc.write(hubFrame{V: 1, T: "pong"})
		case "offer":
			if hc.role != "instance" || f.Code == "" {
				continue
			}
			s.mu.Lock()
			s.codes[strings.ToLower(f.Code)] = hubOffer{Instance: hc.pk, Expires: time.Now().Add(pairCodeTTLHub)}
			s.mu.Unlock()
		case "pair":
			if hc.role != "device" || f.Code == "" {
				_ = hc.write(hubFrame{V: 1, T: "err", Err: "pair requires a code"})
				continue
			}
			s.mu.Lock()
			o, ok := s.codes[strings.ToLower(f.Code)]
			if ok && time.Now().After(o.Expires) {
				delete(s.codes, strings.ToLower(f.Code))
				ok = false
			}
			s.mu.Unlock()
			if !ok {
				_ = hc.write(hubFrame{V: 1, T: "err", Err: "unknown or expired pairing code"})
				continue
			}
			f.To = o.Instance
			f.PK = hc.pk
			if !s.forward(o.Instance, f) {
				_ = hc.write(hubFrame{V: 1, T: "err", Err: "instance is offline"})
			}
		case "fwd":
			if f.To == "" {
				continue
			}
			f.PK = hc.pk
			if !s.forward(f.To, f) {
				_ = hc.write(hubFrame{V: 1, T: "err", ID: f.ID, Err: "peer offline"})
			}
		}
	}
}

func (s *hubRelay) forward(to string, f hubFrame) bool {
	s.mu.Lock()
	peer := s.conns[to]
	s.mu.Unlock()
	if peer == nil {
		return false
	}
	return peer.write(f) == nil
}

func (h *hubConn) write(f hubFrame) error {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	_ = h.c.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return h.c.WriteJSON(f)
}
