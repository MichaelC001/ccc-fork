package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
)

type hubUpload struct {
	id      string
	botID   int64
	name    string
	mime    string
	size    int64
	path    string
	got     map[int]struct{}
	at      time.Time
	caption string
}

func (h *hubClient) ensureUploads() map[string]*hubUpload {
	if h.uploads == nil {
		h.uploads = map[string]*hubUpload{}
	}
	return h.uploads
}

func newUploadID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func mimeForName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".apk":
		return "application/vnd.android.package-archive"
	case ".ipa":
		return "application/octet-stream"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

func chunkCount(size int64) int {
	if size <= 0 {
		return 1
	}
	n := int((size + hubChunkBytes - 1) / hubChunkBytes)
	if n < 1 {
		return 1
	}
	return n
}

func hubFilesDir(cfg *Config) string {
	return filepath.Join(dataDir(cfg), hubFileDir)
}

func copyToHubFiles(cfg *Config, src string) (string, error) {
	if err := os.MkdirAll(hubFilesDir(cfg), 0o700); err != nil {
		return "", err
	}
	name := sanitizeFileName(filepath.Base(src))
	dst := filepath.Join(hubFilesDir(cfg), fmt.Sprintf("%d_%s", time.Now().UnixNano(), name))
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(dst)
		return "", copyErr
	}
	if closeErr != nil {
		os.Remove(dst)
		return "", closeErr
	}
	return dst, nil
}

func recordOutgoingFile(db *gorm.DB, botID, turnID int64, path, origName string, size int64) (*HubFile, error) {
	f := &HubFile{
		BotID:     botID,
		TurnID:    turnID,
		Name:      sanitizeFileName(origName),
		MIME:      mimeForName(origName),
		Size:      size,
		Path:      path,
		Direction: "out",
	}
	if err := db.Create(f).Error; err != nil {
		return nil, err
	}
	return f, nil
}

func (h *hubClient) fileLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		h.flushOutgoingFiles()
		h.flushQuestions()
		h.flushRoster()
		h.gcUploads()
	}
}

func (h *hubClient) gcUploads() {
	h.upMu.Lock()
	defer h.upMu.Unlock()
	cutoff := time.Now().Add(-15 * time.Minute)
	for id, u := range h.uploads {
		if u.at.Before(cutoff) {
			os.Remove(u.path)
			delete(h.uploads, id)
		}
	}
}

func (h *hubClient) flushOutgoingFiles() {
	if h.in == nil || h.in.db == nil {
		return
	}
	var rows []HubFile
	if err := h.in.db.Where("direction = ? AND pushed_at IS NULL", "out").Limit(20).Find(&rows).Error; err != nil {
		return
	}
	now := time.Now()
	for i := range rows {
		f := rows[i]
		b, err := botByID(h.in.db, f.BotID)
		name := ""
		if err == nil {
			name = b.Name
		}
		h.pushEvent("file", map[string]any{
			"bot_id":  f.BotID,
			"bot":     name,
			"file_id": f.ID,
			"name":    f.Name,
			"mime":    f.MIME,
			"size":    f.Size,
			"text":    f.Name,
		})
		h.in.db.Model(&HubFile{}).Where("id = ?", f.ID).Update("pushed_at", now)
	}
}

func (h *hubClient) filesForTurns(botID int64, turnIDs []int64) map[int64][]hubFileInfo {
	out := map[int64][]hubFileInfo{}
	if len(turnIDs) == 0 {
		return out
	}
	var rows []HubFile
	h.in.db.Where("bot_id = ? AND turn_id IN ?", botID, turnIDs).Order("id").Find(&rows)
	for _, f := range rows {
		out[f.TurnID] = append(out[f.TurnID], hubFileInfo{ID: f.ID, Name: f.Name, MIME: f.MIME, Size: f.Size})
	}
	return out
}

func (h *hubClient) rpcPutBegin(params json.RawMessage, out *hubRPC) {
	var p struct {
		BotID int64  `json:"bot_id"`
		Name  string `json:"name"`
		MIME  string `json:"mime"`
		Size  int64  `json:"size"`
		Text  string `json:"text"`
	}
	_ = json.Unmarshal(params, &p)
	b, err := botByID(h.in.db, p.BotID)
	if err != nil || b.ArchivedAt != nil {
		out.OK, out.Error = false, "unknown bot"
		return
	}
	if p.Size <= 0 || p.Size > hubFileMaxBytes {
		out.OK, out.Error = false, fmt.Sprintf("file must be 1..%d bytes", hubFileMaxBytes)
		return
	}
	name := sanitizeFileName(p.Name)
	mimeType := strings.TrimSpace(p.MIME)
	if mimeType == "" {
		mimeType = mimeForName(name)
	}
	dir := filepath.Join(dataDir(h.in.config()), hubFileDirUploads)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	id := newUploadID()
	path := filepath.Join(dir, id)
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	err = fd.Truncate(p.Size)
	fd.Close()
	if err != nil {
		os.Remove(path)
		out.OK, out.Error = false, err.Error()
		return
	}
	h.upMu.Lock()
	h.ensureUploads()[id] = &hubUpload{
		id:      id,
		botID:   p.BotID,
		name:    name,
		mime:    mimeType,
		size:    p.Size,
		path:    path,
		got:     map[int]struct{}{},
		at:      time.Now(),
		caption: strings.TrimSpace(p.Text),
	}
	h.upMu.Unlock()
	out.Body, _ = json.Marshal(map[string]any{"upload_id": id, "chunk": hubChunkBytes, "n": chunkCount(p.Size)})
}

func (h *hubClient) rpcPutChunk(params json.RawMessage, out *hubRPC) {
	var p struct {
		UploadID string `json:"upload_id"`
		I        int    `json:"i"`
		Data     string `json:"data"`
	}
	_ = json.Unmarshal(params, &p)
	h.upMu.Lock()
	u := h.uploads[p.UploadID]
	h.upMu.Unlock()
	if u == nil {
		out.OK, out.Error = false, "unknown upload"
		return
	}
	n := chunkCount(u.size)
	if p.I < 0 || p.I >= n {
		out.OK, out.Error = false, "bad chunk"
		return
	}
	raw, err := decodeHubBytes(p.Data)
	if err != nil || len(raw) == 0 {
		out.OK, out.Error = false, "bad chunk"
		return
	}
	want := hubChunkBytes
	if p.I == n-1 {
		want = int(u.size - int64(p.I)*hubChunkBytes)
	}
	if len(raw) != want {
		out.OK, out.Error = false, "bad chunk size"
		return
	}
	fd, err := os.OpenFile(u.path, os.O_RDWR, 0o600)
	if err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	_, err = fd.WriteAt(raw, int64(p.I)*hubChunkBytes)
	fd.Close()
	if err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	h.upMu.Lock()
	u.got[p.I] = struct{}{}
	u.at = time.Now()
	got := len(u.got)
	h.upMu.Unlock()
	out.Body, _ = json.Marshal(map[string]any{"i": p.I, "got": got, "n": n})
}

func (h *hubClient) rpcPutCommit(params json.RawMessage, out *hubRPC) {
	var p struct {
		UploadID string `json:"upload_id"`
		Text     string `json:"text"`
	}
	_ = json.Unmarshal(params, &p)
	h.upMu.Lock()
	u := h.uploads[p.UploadID]
	if u != nil {
		delete(h.uploads, p.UploadID)
	}
	h.upMu.Unlock()
	if u == nil {
		out.OK, out.Error = false, "unknown upload"
		return
	}
	n := chunkCount(u.size)
	if len(u.got) != n {
		os.Remove(u.path)
		out.OK, out.Error = false, "incomplete upload"
		return
	}
	b, err := botByID(h.in.db, u.botID)
	if err != nil || b.ArchivedAt != nil {
		os.Remove(u.path)
		out.OK, out.Error = false, "unknown bot"
		return
	}
	data, err := os.ReadFile(u.path)
	os.Remove(u.path)
	if err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	if int64(len(data)) != u.size {
		out.OK, out.Error = false, "size mismatch"
		return
	}
	caption := strings.TrimSpace(p.Text)
	if caption == "" {
		caption = u.caption
	}
	if err := h.in.ingestOwnerBytes(b, data, u.name, caption); err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	out.Body, _ = json.Marshal(map[string]any{"queued": true, "bot": b.Name, "file": u.name, "size": u.size})
}

func (h *hubClient) rpcGetChunk(params json.RawMessage, out *hubRPC) {
	var p struct {
		FileID int64 `json:"file_id"`
		I      int   `json:"i"`
	}
	_ = json.Unmarshal(params, &p)
	var f HubFile
	if err := h.in.db.First(&f, p.FileID).Error; err != nil {
		out.OK, out.Error = false, "unknown file"
		return
	}
	n := chunkCount(f.Size)
	if p.I < 0 || p.I >= n {
		out.OK, out.Error = false, "bad chunk"
		return
	}
	fd, err := os.Open(f.Path)
	if err != nil {
		out.OK, out.Error = false, "file gone"
		return
	}
	defer fd.Close()
	if _, err := fd.Seek(int64(p.I)*hubChunkBytes, io.SeekStart); err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	buf := make([]byte, hubChunkBytes)
	nr, err := io.ReadFull(fd, buf)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		buf = buf[:nr]
	} else if err != nil {
		out.OK, out.Error = false, err.Error()
		return
	} else {
		buf = buf[:nr]
	}
	out.Body, _ = json.Marshal(map[string]any{
		"file_id": f.ID,
		"i":       p.I,
		"n":       n,
		"name":    f.Name,
		"mime":    f.MIME,
		"size":    f.Size,
		"data":    base64.StdEncoding.EncodeToString(buf),
	})
}
