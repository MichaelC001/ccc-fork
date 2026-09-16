package main

import "testing"

func TestStripTags(t *testing.T) {
	if got := stripTags("<b>hello</b> &amp; world"); got != "hello &amp; world" {
		t.Fatalf("got %q", got)
	}
}

func TestMuxUIPostDoesNotPanicWithoutHub(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("chat", "")
	if err != nil {
		t.Fatal(err)
	}
	ui := muxUI{tg: telegramUI{in}}
	if _, err := ui.Post(b.TopicID, "<b>hi</b>"); err != nil {
		t.Fatal(err)
	}
}

func TestMuxUIPostPushesToPairedPeer(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("chat", "")
	if err != nil {
		t.Fatal(err)
	}
	h := &hubClient{in: in, peers: map[string][32]byte{}}
	ui := muxUI{tg: telegramUI{in}, hub: h}
	if _, err := ui.Post(b.TopicID, "<b>new reply</b>"); err != nil {
		t.Fatal(err)
	}
}
