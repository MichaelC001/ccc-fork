package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHubRelayForwardsBox(t *testing.T) {
	s := newHubRelay()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ws", s.handleWS)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws"

	inst, err := generateHubIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := generateHubIdentity()
	if err != nil {
		t.Fatal(err)
	}

	ic, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ic.Close()
	if err := ic.WriteJSON(hubFrame{V: 1, T: "open", Role: "instance", PK: inst.ID()}); err != nil {
		t.Fatal(err)
	}
	var ack hubFrame
	if err := ic.ReadJSON(&ack); err != nil || ack.T != "open" {
		t.Fatalf("open ack: %+v %v", ack, err)
	}
	if err := ic.WriteJSON(hubFrame{V: 1, T: "offer", Code: "c0ffee"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	dc, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	if err := dc.WriteJSON(hubFrame{V: 1, T: "open", Role: "device", PK: dev.ID()}); err != nil {
		t.Fatal(err)
	}
	if err := dc.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}

	intro, _ := json.Marshal(hubPairIntro{Name: "pixel", PK: dev.ID()})
	n, ct, err := hubSeal(dev, inst.Public, intro)
	if err != nil {
		t.Fatal(err)
	}
	if err := dc.WriteJSON(hubFrame{V: 1, T: "pair", Code: "c0ffee", N: encodeNonce(n), B: encodeBox(ct)}); err != nil {
		t.Fatal(err)
	}

	ic.SetReadDeadline(time.Now().Add(2 * time.Second))
	var got hubFrame
	if err := ic.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	if got.T != "pair" {
		t.Fatalf("want pair, got %+v", got)
	}
	nn, err := decodeNonce(got.N)
	if err != nil {
		t.Fatal(err)
	}
	boxb, err := decodeBox(got.B)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := hubOpen(inst, dev.Public, nn, boxb)
	if err != nil {
		t.Fatal(err)
	}
	var parsed hubPairIntro
	if err := json.Unmarshal(plain, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Name != "pixel" {
		t.Fatalf("name %q", parsed.Name)
	}
}

func TestHubPingGetsPong(t *testing.T) {
	s := newHubRelay()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ws", s.handleWS)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws"

	dev, err := generateHubIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dc, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	if err := dc.WriteJSON(hubFrame{V: 1, T: "open", Role: "device", PK: dev.ID()}); err != nil {
		t.Fatal(err)
	}
	var ack hubFrame
	if err := dc.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	if err := dc.WriteJSON(hubFrame{V: 1, T: "ping"}); err != nil {
		t.Fatal(err)
	}
	dc.SetReadDeadline(time.Now().Add(2 * time.Second))
	var pong hubFrame
	if err := dc.ReadJSON(&pong); err != nil {
		t.Fatal(err)
	}
	if pong.T != "pong" {
		t.Fatalf("want pong, got %+v", pong)
	}
}
