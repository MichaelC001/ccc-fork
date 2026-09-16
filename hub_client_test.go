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

func TestHubSendFileLandsInInbox(t *testing.T) {
	h, in, runner, _ := testHub(t)
	b, err := in.createBot("chat", "")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("hello-apk-bytes")
	params, _ := json.Marshal(map[string]any{
		"bot_id": b.ID,
		"text":   "install this",
		"file": map[string]any{
			"mime": "application/vnd.android.package-archive",
			"name": "ccc.apk",
			"data": base64.StdEncoding.EncodeToString(payload),
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
	if !strings.Contains(last.Text, "ccc.apk") || !strings.Contains(last.Text, "install this") {
		t.Errorf("enqueue text %q", last.Text)
	}
	got, err := os.ReadFile(filepath.Join(b.Cwd, "inbox", "ccc.apk"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Error("saved file does not match")
	}
	var files []HubFile
	in.db.Find(&files)
	if len(files) != 1 || files[0].Name != "ccc.apk" || files[0].Direction != "in" {
		t.Fatalf("hub file row = %+v", files)
	}
	hist := h.dispatch(hubRPC{Kind: "req", ID: "2", Method: "history", Params: mustJSON(map[string]any{"bot_id": b.ID})})
	var turns []hubTurnInfo
	if err := json.Unmarshal(hist.Body, &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || len(turns[0].Files) != 1 || turns[0].Files[0].Name != "ccc.apk" {
		t.Fatalf("history files = %+v", turns)
	}
}

func TestHubChunkedUploadAndDownload(t *testing.T) {
	h, in, runner, _ := testHub(t)
	b, _ := in.createBot("chat", "")
	payload := make([]byte, hubChunkBytes+7)
	for i := range payload {
		payload[i] = byte(i)
	}
	begin := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "put_begin", Params: mustJSON(map[string]any{
		"bot_id": b.ID, "name": "blob.bin", "size": len(payload),
	})})
	if !begin.OK {
		t.Fatalf("begin: %s", begin.Error)
	}
	var started struct {
		UploadID string `json:"upload_id"`
		N        int    `json:"n"`
	}
	json.Unmarshal(begin.Body, &started)
	if started.N != 2 || started.UploadID == "" {
		t.Fatalf("begin body %+v", started)
	}
	for i := 0; i < started.N; i++ {
		start := i * hubChunkBytes
		end := start + hubChunkBytes
		if end > len(payload) {
			end = len(payload)
		}
		res := h.dispatch(hubRPC{Kind: "req", ID: "c", Method: "put_chunk", Params: mustJSON(map[string]any{
			"upload_id": started.UploadID,
			"i":         i,
			"data":      base64.StdEncoding.EncodeToString(payload[start:end]),
		})})
		if !res.OK {
			t.Fatalf("chunk %d: %s", i, res.Error)
		}
	}
	commit := h.dispatch(hubRPC{Kind: "req", ID: "2", Method: "put_commit", Params: mustJSON(map[string]any{
		"upload_id": started.UploadID, "text": "here",
	})})
	if !commit.OK {
		t.Fatalf("commit: %s", commit.Error)
	}
	if _, ok := runner.last(); !ok {
		t.Fatal("commit must enqueue")
	}
	got, err := os.ReadFile(filepath.Join(b.Cwd, "inbox", "blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("roundtrip mismatch")
	}
	var row HubFile
	if err := in.db.Where("name = ?", "blob.bin").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	var rebuilt []byte
	n := chunkCount(row.Size)
	for i := 0; i < n; i++ {
		res := h.dispatch(hubRPC{Kind: "req", ID: "g", Method: "get_chunk", Params: mustJSON(map[string]any{
			"file_id": row.ID, "i": i,
		})})
		if !res.OK {
			t.Fatalf("get %d: %s", i, res.Error)
		}
		var chunk struct {
			Data string `json:"data"`
		}
		json.Unmarshal(res.Body, &chunk)
		part, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			t.Fatal(err)
		}
		rebuilt = append(rebuilt, part...)
	}
	if string(rebuilt) != string(payload) {
		t.Fatal("download mismatch")
	}
}

func TestRecordOutgoingFileIsOfferedToPhones(t *testing.T) {
	h, in, _, _ := testHub(t)
	b, _ := in.createBot("chat", "")
	path := filepath.Join(t.TempDir(), "out.apk")
	if err := os.WriteFile(path, []byte("apk"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := recordOutgoingFile(in.db, b.ID, 0, path, "out.apk", 3)
	if err != nil {
		t.Fatal(err)
	}
	h.flushOutgoingFiles()
	var fresh HubFile
	in.db.First(&fresh, f.ID)
	if fresh.PushedAt == nil {
		t.Fatal("flush must mark the file pushed")
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
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
	if len(api.since("closeForumTopic")) != 0 {
		t.Error("backend sessions have no Telegram topic to close")
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

func TestHubHistoryIncludesLiveProgress(t *testing.T) {
	h, in, _, _ := testHub(t)
	b, err := in.createBot("chat", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "hi", Status: turnRunning}).Error; err != nil {
		t.Fatal(err)
	}
	h.pushEvent("progress", map[string]any{"bot_id": b.ID, "bot": b.Name, "text": "reading main.dart · 8s"})

	params, _ := json.Marshal(map[string]any{"bot_id": b.ID})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "history", Params: params})
	if !res.OK {
		t.Fatalf("history: %s", res.Error)
	}
	var turns []hubTurnInfo
	if err := json.Unmarshal(res.Body, &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Progress != "reading main.dart · 8s" {
		t.Fatalf("history progress = %+v", turns)
	}

	listed := h.dispatch(hubRPC{Kind: "req", ID: "2", Method: "bots"})
	var bots []hubBotInfo
	if err := json.Unmarshal(listed.Body, &bots); err != nil {
		t.Fatal(err)
	}
	if len(bots) != 1 || bots[0].Progress != "reading main.dart · 8s" {
		t.Fatalf("bots progress = %+v", bots)
	}

	h.pushEvent("post", map[string]any{"bot_id": b.ID, "bot": b.Name, "text": "done"})
	in.db.Model(&Turn{}).Where("bot_id = ?", b.ID).Update("status", turnDone)
	listed = h.dispatch(hubRPC{Kind: "req", ID: "3", Method: "bots"})
	bots = nil
	if err := json.Unmarshal(listed.Body, &bots); err != nil {
		t.Fatal(err)
	}
	if len(bots) != 1 || bots[0].Progress != "" {
		t.Fatalf("progress after post = %+v", bots)
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
	if len(api.since("editForumTopic")) != 0 {
		t.Error("backend sessions have no forum topic to rename")
	}
}
