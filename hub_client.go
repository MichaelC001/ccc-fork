package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// hubClient is the instance side of the public hub: one outbound websocket
// the phone can reach even when this machine is behind NAT.

type hubClient struct {
	in   *instance
	id   hubIdentity
	url  string
	name string

	mu    sync.Mutex
	conn  *websocket.Conn
	peers map[string][32]byte // device pk hex -> pubkey
}

func instanceDisplayName(cfg *Config) string {
	if cfg != nil && strings.TrimSpace(cfg.InstanceName) != "" {
		return strings.TrimSpace(cfg.InstanceName)
	}
	h, _ := os.Hostname()
	if h == "" {
		return "ccc"
	}
	return h
}

func startHubClient(in *instance) *hubClient {
	cfg := in.config()
	if !hubEnabled(cfg) {
		return nil
	}
	id, err := loadOrCreateHubIdentity()
	if err != nil {
		listenLog("hub: identity: %v", err)
		return nil
	}
	hc := &hubClient{
		in:    in,
		id:    id,
		url:   hubURLFromConfig(cfg),
		name:  instanceDisplayName(cfg),
		peers: map[string][32]byte{},
	}
	var devices []HubDevice
	in.db.Find(&devices)
	for _, d := range devices {
		if pk, err := parseHubPublic(d.PubKey); err == nil {
			hc.peers[d.PubKey] = pk
		}
	}
	go hc.loop()
	listenLog("hub: identity %s via %s", id.ID()[:12], hc.url)
	return hc
}

func (h *hubClient) loop() {
	for {
		if err := h.connect(); err != nil {
			listenLog("hub: %v (retry in 5s)", err)
			time.Sleep(5 * time.Second)
			continue
		}
	}
}

func (h *hubClient) connect() error {
	u, err := url.Parse(h.url)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
	default:
		return fmt.Errorf("hub_url must be ws(s)://, got %s", u.Scheme)
	}
	u.Path = "/v1/ws"
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 15 * time.Second}
	c, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.conn = c
	h.mu.Unlock()
	defer func() {
		c.Close()
		h.mu.Lock()
		if h.conn == c {
			h.conn = nil
		}
		h.mu.Unlock()
	}()
	if err := c.WriteJSON(hubFrame{V: 1, T: "open", Role: "instance", PK: h.id.ID()}); err != nil {
		return err
	}
	h.offerPending()
	c.SetReadLimit(1 << 20)
	for {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Minute))
		_, raw, err := c.ReadMessage()
		if err != nil {
			return err
		}
		var f hubFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		switch f.T {
		case "open":
			h.offerPending()
		case "pair":
			h.handlePair(f)
		case "fwd":
			h.handleFwd(f)
		}
	}
}

func (h *hubClient) offerPending() {
	var codes []HubPairCode
	now := time.Now()
	h.in.db.Where("expires_at > ?", now).Find(&codes)
	for _, c := range codes {
		h.send(hubFrame{V: 1, T: "offer", Code: c.Code})
	}
}

func (h *hubClient) send(f hubFrame) {
	h.mu.Lock()
	c := h.conn
	h.mu.Unlock()
	if c == nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = c.WriteJSON(f)
}

func (h *hubClient) handlePair(f hubFrame) {
	from, err := parseHubPublic(f.PK)
	if err != nil {
		return
	}
	nonce, err := decodeNonce(f.N)
	if err != nil {
		return
	}
	ct, err := decodeBox(f.B)
	if err != nil {
		return
	}
	plain, err := hubOpen(h.id, from, nonce, ct)
	if err != nil {
		listenLog("hub: pair box rejected")
		return
	}
	var intro hubPairIntro
	if err := json.Unmarshal(plain, &intro); err != nil || intro.PK != f.PK {
		return
	}
	now := time.Now()
	_ = h.in.db.Where("code = ? AND expires_at > ?", strings.ToLower(f.Code), now).Delete(&HubPairCode{})
	name := strings.TrimSpace(intro.Name)
	if name == "" {
		name = "phone"
	}
	var existing HubDevice
	if err := h.in.db.Where("pub_key = ?", intro.PK).First(&existing).Error; err == nil {
		existing.Name = name
		existing.LastSeen = &now
		h.in.db.Save(&existing)
	} else {
		h.in.db.Create(&HubDevice{PubKey: intro.PK, Name: name, PairedAt: now, LastSeen: &now})
	}
	h.mu.Lock()
	h.peers[intro.PK] = from
	h.mu.Unlock()
	hello, _ := json.Marshal(hubHello{Name: h.name, Hostname: h.name, Bots: h.botCount()})
	rpc, _ := json.Marshal(hubRPC{Kind: "pair", OK: true, Body: hello})
	h.sendBox(intro.PK, from, rpc)
	listenLog("hub: paired device %s (%s)", intro.PK[:12], name)
}

func (h *hubClient) botCount() int {
	var n int64
	h.in.db.Model(&Bot{}).Where("archived_at IS NULL").Count(&n)
	return int(n)
}

func (h *hubClient) handleFwd(f hubFrame) {
	from, err := parseHubPublic(f.PK)
	if err != nil {
		return
	}
	h.mu.Lock()
	_, allowed := h.peers[f.PK]
	h.mu.Unlock()
	if !allowed {
		return
	}
	nonce, err := decodeNonce(f.N)
	if err != nil {
		return
	}
	ct, err := decodeBox(f.B)
	if err != nil {
		return
	}
	plain, err := hubOpen(h.id, from, nonce, ct)
	if err != nil {
		return
	}
	var rpc hubRPC
	if err := json.Unmarshal(plain, &rpc); err != nil || rpc.Kind != "req" {
		return
	}
	now := time.Now()
	h.in.db.Model(&HubDevice{}).Where("pub_key = ?", f.PK).Update("last_seen", now)
	res := h.dispatch(rpc)
	raw, _ := json.Marshal(res)
	h.sendBox(f.PK, from, raw)
}

func (h *hubClient) sendBox(toHex string, toPub [32]byte, plain []byte) {
	n, ct, err := hubSeal(h.id, toPub, plain)
	if err != nil {
		return
	}
	h.send(hubFrame{V: 1, T: "fwd", To: toHex, N: encodeNonce(n), B: encodeBox(ct)})
}

func (h *hubClient) pushEvent(kind string, body any) {
	raw, _ := json.Marshal(body)
	rpc, _ := json.Marshal(hubRPC{Kind: "event", Method: kind, Body: raw})
	h.mu.Lock()
	peers := make(map[string][32]byte, len(h.peers))
	for k, v := range h.peers {
		peers[k] = v
	}
	h.mu.Unlock()
	for id, pk := range peers {
		h.sendBox(id, pk, rpc)
	}
}

func (h *hubClient) dispatch(rpc hubRPC) hubRPC {
	out := hubRPC{Kind: "res", ID: rpc.ID, OK: true}
	switch rpc.Method {
	case "hello":
		body, _ := json.Marshal(hubHello{Name: h.name, Hostname: h.name, Bots: h.botCount()})
		out.Body = body
	case "bots":
		bots, _ := liveBots(h.in.db)
		list := make([]hubBotInfo, 0, len(bots))
		for i := range bots {
			b := &bots[i]
			info := hubBotInfo{ID: b.ID, Name: b.Name, Role: b.Role, Status: b.Status, Engine: botEngine(b)}
			var last Turn
			if err := h.in.db.Where("bot_id = ?", b.ID).Order("id DESC").First(&last).Error; err == nil {
				info.Last = last.CreatedAt.UTC().Format(time.RFC3339)
			}
			list = append(list, info)
		}
		body, _ := json.Marshal(list)
		out.Body = body
	case "history":
		var p struct {
			BotID int64 `json:"bot_id"`
			Limit int   `json:"limit"`
		}
		_ = json.Unmarshal(rpc.Params, &p)
		if p.Limit <= 0 || p.Limit > 100 {
			p.Limit = 40
		}
		var turns []Turn
		h.in.db.Where("bot_id = ?", p.BotID).Order("id DESC").Limit(p.Limit).Find(&turns)
		list := make([]hubTurnInfo, 0, len(turns))
		for i := len(turns) - 1; i >= 0; i-- {
			t := turns[i]
			list = append(list, hubTurnInfo{
				ID: t.ID, Source: t.Source, Input: t.Input, Output: t.Output,
				Status: t.Status, At: t.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
		body, _ := json.Marshal(list)
		out.Body = body
	case "send":
		var p struct {
			BotID int64  `json:"bot_id"`
			Text  string `json:"text"`
		}
		_ = json.Unmarshal(rpc.Params, &p)
		p.Text = strings.TrimSpace(p.Text)
		if p.Text == "" {
			out.OK, out.Error = false, "empty message"
			return out
		}
		b, err := botByID(h.in.db, p.BotID)
		if err != nil {
			out.OK, out.Error = false, "unknown bot"
			return out
		}
		if _, err := h.in.runner.Enqueue(b.ID, sourceUser, p.Text, 0); err != nil {
			out.OK, out.Error = false, err.Error()
			return out
		}
		body, _ := json.Marshal(map[string]any{"queued": true, "bot": b.Name})
		out.Body = body
	default:
		out.OK, out.Error = false, "unknown method"
	}
	return out
}

// HubPairCode is a one-time pairing offer `ccc pair` writes; listen advertises
// it to the hub until it expires or is consumed.
type HubPairCode struct {
	Code      string    `gorm:"primaryKey"`
	ExpiresAt time.Time `gorm:"index"`
}

