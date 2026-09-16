package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 1×1 PNG. Small enough that a hub send of it stays well under the 1 MiB frame cap.
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xde,
	0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54,
	0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00, 0x00,
	0x00, 0x03, 0x00, 0x01, 0x00, 0x05, 0xfe, 0xd4, 0xef,
	0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44,
	0xae, 0x42, 0x60, 0x82,
}

func testHub(t *testing.T) (*hubClient, *instance, *fakeRunner, *fakeBotAPI) {
	t.Helper()
	in, runner, api := testInstance(t)
	return &hubClient{in: in, name: "mac", peers: map[string][32]byte{}}, in, runner, api
}

func TestHubSendImageLandsInInbox(t *testing.T) {
	h, in, runner, _ := testHub(t)
	b, err := in.createBot("chat", "")
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{
		"bot_id": b.ID,
		"text":   "look at this",
		"image": map[string]any{
			"mime": "image/png",
			"name": "shot.png",
			"data": base64.StdEncoding.EncodeToString(tinyPNG),
		},
	})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "send", Params: params})
	if !res.OK {
		t.Fatalf("send: %s", res.Error)
	}
	last, ok := runner.last()
	if !ok {
		t.Fatal("nothing enqueued")
	}
	wantDir := filepath.Join(b.Cwd, "inbox")
	if !strings.Contains(last.Text, wantDir) || !strings.Contains(last.Text, "look at this") {
		t.Errorf("enqueue text %q does not mention the inbox or caption", last.Text)
	}
	got, err := os.ReadFile(filepath.Join(wantDir, "shot.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(tinyPNG) {
		t.Error("saved file does not match the bytes the phone sent")
	}
}

func TestHubSendImageTooLarge(t *testing.T) {
	h, in, runner, _ := testHub(t)
	b, _ := in.createBot("chat", "")
	params, _ := json.Marshal(map[string]any{
		"bot_id": b.ID,
		"image": map[string]any{
			"mime": "image/jpeg",
			"name": "big.jpg",
			"data": base64.StdEncoding.EncodeToString(make([]byte, hubImageMaxBytes+1)),
		},
	})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "send", Params: params})
	if res.OK || res.Error != "image too large" {
		t.Fatalf("got ok=%v err=%q", res.OK, res.Error)
	}
	if _, ok := runner.last(); ok {
		t.Error("oversize image must not enqueue a turn")
	}
}

func TestHubArchiveHidesFromBotsList(t *testing.T) {
	h, in, _, api := testHub(t)
	live, _ := in.createBot("keep", "")
	gone, _ := in.createBot("gone", "")
	params, _ := json.Marshal(map[string]any{"bot_id": gone.ID})
	before := len(api.since("sendMessage"))
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "archive", Params: params})
	if !res.OK {
		t.Fatalf("archive: %s", res.Error)
	}
	if len(api.since("closeForumTopic")) == 0 {
		t.Error("archive must close the Telegram topic")
	}
	if extra := len(api.since("sendMessage")) - before; extra != 0 {
		t.Fatalf("archive must not post a Telegram alert, extra sendMessage=%d", extra)
	}
	listed := h.dispatch(hubRPC{Kind: "req", ID: "2", Method: "bots"})
	var liveList []hubBotInfo
	if err := json.Unmarshal(listed.Body, &liveList); err != nil {
		t.Fatal(err)
	}
	if len(liveList) != 1 || liveList[0].ID != live.ID {
		t.Fatalf("live list = %+v, want only %d", liveList, live.ID)
	}
	archived := h.dispatch(hubRPC{Kind: "req", ID: "3", Method: "archived"})
	var goneList []hubBotInfo
	if err := json.Unmarshal(archived.Body, &goneList); err != nil {
		t.Fatal(err)
	}
	if len(goneList) != 1 || goneList[0].ID != gone.ID || !goneList[0].Archived {
		t.Fatalf("archived list = %+v", goneList)
	}
	res = h.dispatch(hubRPC{Kind: "req", ID: "4", Method: "unarchive", Params: params})
	if !res.OK {
		t.Fatalf("unarchive: %s", res.Error)
	}
	listed = h.dispatch(hubRPC{Kind: "req", ID: "5", Method: "bots"})
	if err := json.Unmarshal(listed.Body, &liveList); err != nil {
		t.Fatal(err)
	}
	if len(liveList) != 2 {
		t.Fatalf("after unarchive live = %+v", liveList)
	}
}

func TestHubRename(t *testing.T) {
	h, in, _, api := testHub(t)
	b, _ := in.createBot("old-name", "")
	params, _ := json.Marshal(map[string]any{"bot_id": b.ID, "name": "new-name"})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "rename", Params: params})
	if !res.OK {
		t.Fatalf("rename: %s", res.Error)
	}
	got, err := botByID(in.db, b.ID)
	if err != nil || got.Name != "new-name" {
		t.Fatalf("stored name = %q err=%v", got.Name, err)
	}
	if len(api.since("editForumTopic")) == 0 {
		t.Error("rename must edit the forum topic title")
	}
}
