package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// The hub is an untrusted relay. TLS authenticates the hub as a server; every
// payload that is not a routing header is a NaCl box the hub cannot open.
//
// Default is the public free hub. Override with `ccc config set hub_url …`
// or CCC_HUB_URL. Set hub_url to "-" to disable.

const defaultHubURL = "wss://hub.mentasystems.com"

func hubURLFromConfig(cfg *Config) string {
	if env := strings.TrimSpace(os.Getenv("CCC_HUB_URL")); env != "" {
		return env
	}
	if cfg != nil && strings.TrimSpace(cfg.HubURL) != "" {
		return strings.TrimSpace(cfg.HubURL)
	}
	return defaultHubURL
}

func hubEnabled(cfg *Config) bool {
	u := hubURLFromConfig(cfg)
	return u != "" && u != "-"
}

// hubFrame is one websocket JSON message. Open/pair-route are visible to the
// hub; rpc/event bodies travel only as Box.
type hubFrame struct {
	V    int    `json:"v"`
	T    string `json:"t"`              // open|pair|fwd|err|ping|pong
	Role string `json:"role,omitempty"` // instance|device (open)
	PK   string `json:"pk,omitempty"`
	To   string `json:"to,omitempty"`
	Code string `json:"code,omitempty"`
	ID   string `json:"id,omitempty"`
	N    string `json:"n,omitempty"` // nonce, raw-url base64
	B    string `json:"b,omitempty"` // ciphertext, raw-url base64
	Err  string `json:"err,omitempty"`
	Name string `json:"name,omitempty"`
}

func encodeNonce(n [24]byte) string { return base64.RawURLEncoding.EncodeToString(n[:]) }

func decodeNonce(s string) ([24]byte, error) {
	var n [24]byte
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 24 {
		return n, fmt.Errorf("bad nonce")
	}
	copy(n[:], b)
	return n, nil
}

func encodeBox(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decodeBox(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// hubRPC is the plaintext inside a fwd box.
type hubRPC struct {
	Kind   string          `json:"kind"` // req|res|event|pair
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type hubBotInfo struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Status   string `json:"status"`
	Engine   string `json:"engine"`
	Last     string `json:"last,omitempty"`
	LastText string `json:"last_text,omitempty"`
	Archived bool   `json:"archived,omitempty"`
}

// hubImageMaxBytes is the decoded image cap on a hub `send`. The public hub
// websocket is 1 MiB; a 512 KiB JPEG plus JSON/NaCl/base64 still fits.
const hubImageMaxBytes = 512 * 1024

type hubTurnInfo struct {
	ID     int64  `json:"id"`
	Source string `json:"source"`
	Input  string `json:"input"`
	Output string `json:"output"`
	Status string `json:"status"`
	At     string `json:"at"`
}

type hubHello struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Bots     int    `json:"bots"`
}

type hubPairIntro struct {
	Name string `json:"name"`
	PK   string `json:"pk"`
}

const pairCodeTTLHub = 10 * time.Minute
