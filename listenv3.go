package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gorm.io/gorm"
)

// listenv3.go is the Telegram side of ccc v3 (DESIGN §8): one forum group, one
// topic per bot, plain text in a topic is a turn for that bot, plain text in
// General creates a bot. It replaces the v2 fleet-mirroring poller.

// instance is one `ccc listen` process: config + database + runner.
type instance struct {
	db      *gorm.DB
	runner  turnRunner
	dataDir string

	mu  sync.Mutex
	cfg *Config
}

func (in *instance) config() *Config {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.cfg
}

// telegramUI is the runner's view of Telegram: everything lands in the bot's
// own topic inside the configured forum group.
type telegramUI struct{ in *instance }

func (t telegramUI) Post(topicID int64, html string) (int64, error) {
	cfg := t.in.config()
	if cfg.BotToken == "" || cfg.GroupID == 0 {
		return 0, nil
	}
	return sendMessageHTMLGetID(cfg, cfg.GroupID, topicID, html)
}

func (t telegramUI) Edit(topicID, msgID int64, html string) error {
	cfg := t.in.config()
	if cfg.BotToken == "" || cfg.GroupID == 0 || msgID == 0 {
		return nil
	}
	return editMessageHTML(cfg, cfg.GroupID, msgID, topicID, html)
}

func (t telegramUI) React(messageID int64, emoji string) {
	cfg := t.in.config()
	if cfg.BotToken == "" || cfg.GroupID == 0 || messageID == 0 {
		return
	}
	if err := setMessageReaction(cfg, cfg.GroupID, messageID, emoji); err != nil {
		hookLog("reaction failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------

// listenV3 is `ccc listen`: open the database, start the runner, long-poll
// Telegram. Bootstrap config (token, group, profiles) stays in config.json;
// everything runtime lives in SQLite (DESIGN §5).
func listenV3() error {
	time.Sleep(time.Duration(os.Getpid()%500) * time.Millisecond)

	lockPath := filepath.Join(cacheDir(), "ccc.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("Another ccc listen instance is already running, exiting quietly")
		return nil
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) // safe-ignore: the lock also dies with the process

	lockFile.Truncate(0) // safe-ignore: the pid line below is a diagnostic, not state ccc reads back
	lockFile.Seek(0, 0)  // safe-ignore: same
	fmt.Fprintf(lockFile, "%d\n", os.Getpid())

	initListenLog()
	if listenLogFile != nil {
		defer listenLogFile.Close()
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not configured. Run: ccc setup <bot_token>")
	}
	if cfg.BotToken == "" {
		return fmt.Errorf("no bot token. Run: ccc setup <bot_token>")
	}

	db, err := openStore(dbPath(cfg))
	if err != nil {
		return err
	}
	in := &instance{db: db, cfg: cfg, dataDir: dataDir(cfg)}
	runner := newRunner(db, cfg, telegramUI{in})
	in.runner = runner
	defer runner.Close()

	linkSharedProjects(cfg)
	setBotCommandsV3(cfg.BotToken)
	listenLog("ccc v3 listening (group: %d, db: %s)", cfg.GroupID, dbPath(cfg))

	// Anything left running from a previous process is not running any more.
	db.Model(&Turn{}).Where("status = ?", turnRunning).
		Updates(map[string]any{"status": turnFailed, "error_class": errFatal, "stop_reason": "ccc restarted"})
	db.Model(&Bot{}).Where("status = ?", botRunning).Update("status", botIdle)
	// Re-arm queues that survived the restart.
	var bots []Bot
	db.Where("archived_at IS NULL").Find(&bots)
	for i := range bots {
		runner.kick(bots[i].ID)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		listenLog("Shutting down (signal: %v)", sig)
		runner.Close()
		os.Exit(0)
	}()

	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}
	for {
		reqURL := fmt.Sprintf("%s?offset=%d&timeout=30&allowed_updates=%s",
			telegramURL(cfg.BotToken, "getUpdates"), offset, `["message","callback_query"]`)
		resp, err := telegramClientGet(client, cfg.BotToken, reqURL)
		if err != nil {
			listenLog("Network error: %v (retrying...)", err)
			time.Sleep(5 * time.Second)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize)) // safe-ignore: a short read is handled as a parse error below
		resp.Body.Close()                                                 // safe-ignore: nothing to do if closing a read body fails

		var updates TelegramUpdate
		if err := json.Unmarshal(body, &updates); err != nil {
			listenLog("Parse error: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if !updates.OK {
			listenLog("Telegram API error: %s", updates.Description)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, u := range updates.Result {
			offset = u.UpdateID + 1
			if u.CallbackQuery != nil {
				in.handleCallback(u.CallbackQuery)
				continue
			}
			msg := u.Message
			in.handleMessage(&msg)
		}
	}
}

// linkSharedProjects makes every profile resolve transcripts from the same
// place, which is what lets a retried turn resume the SAME session UUID on a
// different account (DESIGN §4). It only ever CREATES symlinks; an existing
// projects/ directory is left exactly as it is.
func linkSharedProjects(cfg *Config) {
	shared := filepath.Join(dataDir(cfg), "projects")
	profiles := listProfiles(cfg)
	for _, p := range profiles {
		dir := profileProjectsDir(p)
		if _, err := os.Lstat(shared); err != nil {
			// The implicit ~/.claude profile already owns a real projects/ with
			// history; point the shared path at it rather than the reverse.
			if p.Implicit {
				if _, err := os.Stat(dir); err == nil {
					if err := os.MkdirAll(filepath.Dir(shared), 0o700); err == nil {
						if err := os.Symlink(dir, shared); err != nil {
							hookLog("shared projects link: %v", err)
						}
					}
					continue
				}
			}
			if err := os.MkdirAll(shared, 0o700); err != nil {
				hookLog("shared projects dir: %v", err)
				return
			}
		}
		if _, err := os.Lstat(dir); err == nil {
			continue // never touch an existing projects/
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			continue
		}
		if err := os.Symlink(shared, dir); err != nil {
			hookLog("link %s -> %s: %v", dir, shared, err)
		}
	}
}

func setBotCommandsV3(botToken string) {
	commands := []map[string]string{
		{"command": "role", "description": "Show or set this bot's role"},
		{"command": "new", "description": "Start a fresh conversation (memory kept)"},
		{"command": "stop", "description": "Stop the running turn and drop the queue"},
		{"command": "cwd", "description": "Show or set this bot's working directory"},
		{"command": "memory", "description": "List or search this bot's memories"},
		{"command": "forget", "description": "Forget a memory: /forget <scope> <key>"},
		{"command": "bots", "description": "List all bots"},
		{"command": "status", "description": "Instance health: profiles, queue, running turns"},
	}
	for _, scope := range []map[string]any{nil, {"type": "all_group_chats"}} {
		payload := map[string]any{"commands": commands}
		if scope != nil {
			payload["scope"] = scope
		}
		body, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Post(telegramURL(botToken, "setMyCommands"), "application/json", strings.NewReader(string(body)))
		if err == nil {
			resp.Body.Close() // safe-ignore: the command list is cosmetic; a failed close changes nothing
		}
	}
}

// ---------------------------------------------------------------------------
// Update handling
// ---------------------------------------------------------------------------

// handleMessage routes one inbound Telegram message (DESIGN §8 "Conversation").
func (in *instance) handleMessage(msg *TelegramMessage) {
	cfg := in.config()
	// Owner-only for Phase 2a; the pairing/allowlist system is Phase 2b.
	if cfg.ChatID != 0 && msg.From.ID != cfg.ChatID {
		return
	}
	inGroup := cfg.GroupID != 0 && msg.Chat.ID == cfg.GroupID
	topicID := msg.MessageThreadID

	// Attachments are saved into the bot's workspace before anything else, so
	// the text path below sees a normal message carrying a path.
	if inGroup && topicID > 0 {
		if handled := in.handleAttachment(msg); handled {
			return
		}
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	if strings.HasPrefix(text, "/") {
		in.handleCommand(msg, text, inGroup, topicID)
		return
	}

	if !inGroup {
		in.reply(msg, "Talk to me in the group: a topic per bot. Send text in General to create a new bot.")
		return
	}

	if topicID == 0 {
		in.createBotFromText(msg, text)
		return
	}
	b, err := botByTopic(in.db, topicID)
	if err != nil {
		in.reply(msg, "This topic has no bot behind it. Send a message in General to create one.")
		return
	}
	in.deliver(b, msg, text)
}

// deliver turns a plain message into an input for a bot: either the answer to a
// pending question or a new turn.
func (in *instance) deliver(b *Bot, msg *TelegramMessage, text string) {
	if q, ok := in.matchQuestion(b, msg); ok {
		answer := answerQuestion(in.db, q, text)
		setBotStatus(in.db, b.ID, botIdle)
		if _, err := in.runner.Enqueue(b.ID, sourceUser, answer, int64(msg.MessageID)); err != nil {
			in.reply(msg, "Could not queue that: "+err.Error())
		}
		return
	}
	if _, err := in.runner.Enqueue(b.ID, sourceUser, text, int64(msg.MessageID)); err != nil {
		in.reply(msg, "Could not queue that: "+err.Error())
	}
}

// matchQuestion decides whether a message is the answer to an ask_owner: either
// an explicit reply to the question message, or any text sent while the bot is
// parked waiting for one.
func (in *instance) matchQuestion(b *Bot, msg *TelegramMessage) (*Question, bool) {
	if msg.ReplyToMessage != nil {
		var q Question
		err := in.db.Where("bot_id = ? AND asked_message_id = ? AND answered_at IS NULL",
			b.ID, msg.ReplyToMessage.MessageID).First(&q).Error
		if err == nil {
			return &q, true
		}
	}
	if b.Status == botWaiting {
		return pendingQuestion(in.db, b.ID)
	}
	return nil, false
}

// handleCallback processes an inline button tap. Only q:<question>:<option> is
// defined in Phase 2a.
func (in *instance) handleCallback(cb *CallbackQuery) {
	cfg := in.config()
	if cfg.ChatID != 0 && cb.From.ID != cfg.ChatID {
		return
	}
	answerCallbackQuery(cfg, cb.ID)
	parts := strings.Split(cb.Data, ":")
	if len(parts) != 3 || parts[0] != "q" {
		return
	}
	qID, err1 := strconv.ParseInt(parts[1], 10, 64)
	idx, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return
	}
	var q Question
	if err := in.db.First(&q, qID).Error; err != nil {
		return
	}
	if q.AnsweredAt != nil {
		return
	}
	opts := questionOptions(&q)
	if idx < 0 || idx >= len(opts) {
		return
	}
	choice := opts[idx]
	answer := answerQuestion(in.db, &q, choice)
	if cb.Message != nil && cfg.GroupID != 0 {
		confirmed := "❓ " + htmlEscape(q.Question) + "\n✓ <b>" + htmlEscape(choice) + "</b>"
		_ = editMessageHTML(cfg, cfg.GroupID, int64(cb.Message.MessageID), q.BotID, confirmed) // safe-ignore: ticking the question message is cosmetic
	}
	setBotStatus(in.db, q.BotID, botIdle)
	if _, err := in.runner.Enqueue(q.BotID, sourceUser, answer, 0); err != nil {
		hookLog("enqueue answer: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Bot creation
// ---------------------------------------------------------------------------

// createBotFromText handles plain text in General: a new bot whose topic is
// named after the first line, with the message as its first input.
func (in *instance) createBotFromText(msg *TelegramMessage, text string) {
	b, err := in.createBot(botNameFromText(text), "")
	if err != nil {
		in.reply(msg, "Could not create the bot: "+err.Error())
		return
	}
	in.reply(msg, fmt.Sprintf("🤖 Created <b>%s</b> — continue in its topic.", htmlEscape(b.Name)))
	if _, err := in.runner.Enqueue(b.ID, sourceUser, text, 0); err != nil {
		hookLog("enqueue first message: %v", err)
	}
}

// createBot creates the forum topic, the workspace and the database row.
func (in *instance) createBot(name, role string) (*Bot, error) {
	cfg := in.config()
	name = uniqueBotName(in.db, sanitizeBotName(name))
	topicID, err := createForumTopic(cfg, name)
	if err != nil {
		return nil, err
	}
	ws := botWorkspace(cfg, name)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return nil, err
	}
	b := &Bot{Name: name, TopicID: topicID, Role: role, Cwd: ws, Status: botIdle}
	if err := in.db.Create(b).Error; err != nil {
		return nil, err
	}
	return b, nil
}

// botNameFromText derives a topic name from the first line of a message.
func botNameFromText(text string) string {
	line := text
	if i := strings.IndexAny(line, "\n\r"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if len(line) > 40 {
		if cut := strings.LastIndex(line[:40], " "); cut > 10 {
			line = line[:cut]
		} else {
			line = line[:40]
		}
	}
	return line
}

// sanitizeBotName keeps names usable as directory names and topic titles.
func sanitizeBotName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == 0:
			return '-'
		case r < 32:
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("bot-%d", time.Now().Unix())
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// ---------------------------------------------------------------------------
// Attachments
// ---------------------------------------------------------------------------

// handleAttachment saves photos/documents into the bot's workspace inbox/ and
// enqueues a turn carrying the path (DESIGN §8). Voice notes are transcribed
// when the voice build is present.
func (in *instance) handleAttachment(msg *TelegramMessage) bool {
	if msg.Voice == nil && msg.Document == nil && len(msg.Photo) == 0 {
		return false
	}
	cfg := in.config()
	b, err := botByTopic(in.db, msg.MessageThreadID)
	if err != nil {
		return false
	}
	inbox := filepath.Join(botCwd(cfg, b), "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		in.reply(msg, "Could not create inbox/: "+err.Error())
		return true
	}

	switch {
	case msg.Voice != nil:
		path := filepath.Join(inbox, fmt.Sprintf("voice_%d.ogg", time.Now().UnixNano()))
		if err := downloadTelegramFile(cfg, msg.Voice.FileID, path); err != nil {
			in.reply(msg, "Download failed: "+err.Error())
			return true
		}
		if voiceSupported {
			transcription, err := transcribeAudio(cfg, path)
			if err == nil && strings.TrimSpace(transcription) != "" {
				in.reply(msg, "📝 "+htmlEscape(transcription))
				in.deliver(b, msg, "[voice transcription, may contain errors] "+transcription)
				return true
			}
		}
		in.deliver(b, msg, fmt.Sprintf("The owner sent a voice note, saved at %s (transcribe it yourself if you need the text).", path))
		return true

	case len(msg.Photo) > 0:
		photo := msg.Photo[len(msg.Photo)-1]
		path := filepath.Join(inbox, fmt.Sprintf("photo_%d.jpg", time.Now().UnixNano()))
		if err := downloadTelegramFile(cfg, photo.FileID, path); err != nil {
			in.reply(msg, "Download failed: "+err.Error())
			return true
		}
		caption := strings.TrimSpace(msg.Caption)
		if caption == "" {
			caption = "The owner sent an image."
		}
		in.deliver(b, msg, fmt.Sprintf("%s It is saved at %s", caption, path))
		return true

	default:
		name := sanitizeFileName(msg.Document.FileName)
		path := filepath.Join(inbox, name)
		if err := downloadTelegramFile(cfg, msg.Document.FileID, path); err != nil {
			in.reply(msg, "Download failed: "+err.Error())
			return true
		}
		caption := strings.TrimSpace(msg.Caption)
		if caption == "" {
			caption = "The owner sent a file."
		}
		in.deliver(b, msg, fmt.Sprintf("%s It is saved at %s", caption, path))
		return true
	}
}

// sanitizeFileName strips path separators from a Telegram-provided file name:
// the name comes from the world, and it is used to build a path.
func sanitizeFileName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return fmt.Sprintf("file_%d", time.Now().UnixNano())
	}
	return name
}

func botCwd(cfg *Config, b *Bot) string {
	if strings.TrimSpace(b.Cwd) != "" {
		return b.Cwd
	}
	return botWorkspace(cfg, b.Name)
}

// ---------------------------------------------------------------------------
// Commands (DESIGN §8)
// ---------------------------------------------------------------------------

func (in *instance) handleCommand(msg *TelegramMessage, text string, inGroup bool, topicID int64) {
	cmd, rest := splitCommand(text)
	switch cmd {
	case "/bots":
		in.reply(msg, in.renderBots())
		return
	case "/status":
		in.reply(msg, in.renderStatus())
		return
	case "/bot":
		if !inGroup {
			in.reply(msg, "Use /bot in the group.")
			return
		}
		name, role := splitFirstWord(rest)
		if name == "" {
			in.reply(msg, "Usage: /bot &lt;name&gt; [role]")
			return
		}
		b, err := in.createBot(name, role)
		if err != nil {
			in.reply(msg, "Could not create the bot: "+err.Error())
			return
		}
		in.reply(msg, fmt.Sprintf("🤖 Created <b>%s</b>.", htmlEscape(b.Name)))
		return
	}

	if !inGroup || topicID == 0 {
		in.reply(msg, "That command only works inside a bot's topic.")
		return
	}
	b, err := botByTopic(in.db, topicID)
	if err != nil {
		in.reply(msg, "This topic has no bot behind it.")
		return
	}

	switch cmd {
	case "/role":
		if strings.TrimSpace(rest) == "" {
			role := strings.TrimSpace(b.Role)
			if role == "" {
				role = "(not set)"
			}
			in.reply(msg, "<b>Role</b>\n"+htmlEscape(role))
			return
		}
		// The system prompt is recorded per conversation, so a new role only
		// takes effect in a new one (see runner.go's flag notes).
		in.db.Model(&Bot{}).Where("id = ?", b.ID).
			Updates(map[string]any{"role": strings.TrimSpace(rest), "session_id": ""})
		in.reply(msg, "📝 Role updated; the next message starts a fresh conversation with it.")

	case "/new":
		in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "")
		in.reply(msg, "🆕 Fresh conversation. Memories are kept.")

	case "/stop":
		if in.runner.Stop(b.ID) {
			in.reply(msg, "🛑 Stopping.")
		} else {
			in.reply(msg, "Nothing was running; the queue is now empty.")
		}
		setBotStatus(in.db, b.ID, botIdle)

	case "/cwd":
		if strings.TrimSpace(rest) == "" {
			in.reply(msg, "<code>"+htmlEscape(botCwd(in.config(), b))+"</code>")
			return
		}
		path := expandPath(strings.TrimSpace(rest))
		if !filepath.IsAbs(path) {
			in.reply(msg, "Give an absolute path.")
			return
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			in.reply(msg, "That directory does not exist.")
			return
		}
		in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("cwd", path)
		in.reply(msg, "📂 Working dir set to <code>"+htmlEscape(path)+"</code>")

	case "/memory":
		mems, err := searchMemories(in.db, b.ID, strings.TrimSpace(rest), "", 20)
		if err != nil {
			in.reply(msg, "Search failed: "+err.Error())
			return
		}
		if len(mems) == 0 {
			in.reply(msg, "No memories.")
			return
		}
		var sb strings.Builder
		for _, m := range mems {
			where := m.Scope
			if m.Scope == scopeProject {
				where = "project " + filepath.Base(m.ScopeKey)
			}
			fmt.Fprintf(&sb, "• <b>%s</b> <i>[%s]</i>\n%s\n", htmlEscape(m.Key), htmlEscape(where), htmlEscape(truncate(m.Text, 300)))
		}
		in.reply(msg, sb.String())

	case "/forget":
		scope, key := splitFirstWord(rest)
		if scope == "" || key == "" {
			in.reply(msg, "Usage: /forget &lt;user|project|bot&gt; &lt;key&gt;")
			return
		}
		scopeKey := ""
		switch scope {
		case scopeUser:
		case scopeBot:
			scopeKey = fmt.Sprint(b.ID)
		case scopeProject:
			scopeKey = botCwd(in.config(), b)
		default:
			in.reply(msg, "Scope must be user, project or bot.")
			return
		}
		res := in.db.Where("scope = ? AND scope_key = ? AND key = ?", scope, scopeKey, key).Delete(&Memory{})
		if res.RowsAffected == 0 {
			in.reply(msg, "No such memory.")
			return
		}
		in.reply(msg, "🗑 Forgot <b>"+htmlEscape(key)+"</b>")

	default:
		in.reply(msg, "Unknown command.")
	}
}

func splitCommand(text string) (string, string) {
	cmd := text
	rest := ""
	if i := strings.IndexAny(text, " \n"); i >= 0 {
		cmd, rest = text[:i], strings.TrimSpace(text[i+1:])
	}
	// Telegram appends @botname to commands in groups.
	if at := strings.IndexByte(cmd, '@'); at > 0 {
		cmd = cmd[:at]
	}
	return strings.ToLower(cmd), rest
}

func splitFirstWord(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexAny(s, " \n"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

func (in *instance) renderBots() string {
	bots, err := liveBots(in.db)
	if err != nil || len(bots) == 0 {
		return "No bots yet. Send a message in General to create one."
	}
	var sb strings.Builder
	sb.WriteString("<b>Bots</b>\n")
	for i := range bots {
		b := &bots[i]
		var last Turn
		when := "never"
		if err := in.db.Where("bot_id = ?", b.ID).Order("id DESC").First(&last).Error; err == nil {
			when = humanDuration(time.Since(last.CreatedAt)) + " ago"
		}
		role := strings.TrimSpace(b.Role)
		if role == "" {
			role = "(no role)"
		}
		fmt.Fprintf(&sb, "• <b>%s</b> [%s] — %s · last %s\n",
			htmlEscape(b.Name), b.Status, htmlEscape(truncate(role, 80)), when)
	}
	return sb.String()
}

func (in *instance) renderStatus() string {
	cfg := in.config()
	var sb strings.Builder
	sb.WriteString("<b>ccc status</b>\n")
	var queued, running int64
	in.db.Model(&Turn{}).Where("status = ?", turnQueued).Count(&queued)
	in.db.Model(&Turn{}).Where("status = ?", turnRunning).Count(&running)
	var nBots int64
	in.db.Model(&Bot{}).Where("archived_at IS NULL").Count(&nBots)
	fmt.Fprintf(&sb, "bots: %d · running: %d · queued: %d\n", nBots, running, queued)
	fmt.Fprintf(&sb, "model: %s\n", firstNonEmpty(instanceModel(cfg), "claude default"))
	fmt.Fprintf(&sb, "data: <code>%s</code>\n", htmlEscape(dataDir(cfg)))
	sb.WriteString("\n<b>Accounts</b>\n")
	now := time.Now()
	for _, s := range collectProfileStats(cfg, nil, now) {
		state := "ok"
		if !s.CooledUntil.IsZero() && s.CooledUntil.After(now) {
			state = "cooldown until " + s.CooledUntil.Format("15:04")
		}
		fmt.Fprintf(&sb, "• %s — 5h %d%%, 7d %d%% (%s)\n", htmlEscape(s.Name), s.FiveHour, s.SevenDay, state)
	}
	return sb.String()
}

// reply answers in the same chat/topic the message came from.
func (in *instance) reply(msg *TelegramMessage, html string) {
	cfg := in.config()
	if cfg.BotToken == "" {
		return
	}
	if _, err := sendMessageHTMLGetID(cfg, msg.Chat.ID, msg.MessageThreadID, html); err != nil {
		hookLog("reply failed: %v", err)
	}
}
