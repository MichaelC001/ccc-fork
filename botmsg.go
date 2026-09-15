package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gorm.io/gorm"
)

// queueBotMessage is the one path a bot-to-bot message takes: an inbox row
// plus the 🤝 mirror in both Telegram topics so the owner sees the exchange
// whether the sender used the Claude MCP tool or `ccc tell` from Grok/agy.
func queueBotMessage(db *gorm.DB, cfg *Config, from *Bot, toName, body string, wake bool) (*Bot, *InboxMessage, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, nil, fmt.Errorf("message text is empty")
	}
	if from == nil {
		return nil, nil, fmt.Errorf("unknown sender")
	}
	target, err := botByName(db, strings.TrimSpace(toName))
	if err != nil {
		return nil, nil, fmt.Errorf("no live bot named %q", toName)
	}
	if target.ID == from.ID {
		return nil, nil, fmt.Errorf("you cannot send a message to yourself")
	}
	msg := InboxMessage{ToBotID: target.ID, FromBotID: &from.ID, Text: body, Wake: wake}
	if err := db.Create(&msg).Error; err != nil {
		return nil, nil, fmt.Errorf("could not queue the message: %w", err)
	}
	postBotMirror(cfg, from, target, body)
	return target, &msg, nil
}

func postBotMirror(cfg *Config, from, to *Bot, body string) {
	if cfg == nil || cfg.BotToken == "" || cfg.GroupID == 0 || from == nil || to == nil {
		return
	}
	html := fmt.Sprintf("🤝 <b>%s</b> → <b>%s</b>: %s",
		htmlEscape(from.Name), htmlEscape(to.Name), renderTelegramHTML(truncate(body, 1500)))
	if to.TopicID != 0 {
		_, _ = sendMessageHTMLGetID(cfg, cfg.GroupID, to.TopicID, html) // safe-ignore: a failed mirror must not fail the send
	}
	if from.TopicID != 0 && from.TopicID != to.TopicID {
		_, _ = sendMessageHTMLGetID(cfg, cfg.GroupID, from.TopicID, html) // safe-ignore: same
	}
}

// runTellCommand is `ccc tell [--no-wake] <bot> [text…]` — the Grok/agy
// substitute for send_to_bot. Sender is CCC_BOT_ID (set on every turn) or the
// bot that owns the working directory.
func runTellCommand(args []string) error {
	wake := true
	var rest []string
	for _, a := range args {
		if a == "--no-wake" {
			wake = false
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: ccc tell [--no-wake] <bot> [text]")
	}
	target := rest[0]
	body := strings.TrimSpace(strings.Join(rest[1:], " "))
	if body == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		body = strings.TrimSpace(string(b))
	}
	if body == "" {
		return fmt.Errorf("usage: ccc tell [--no-wake] <bot> [text]")
	}

	config, err := loadConfig()
	if err != nil || config == nil {
		return fmt.Errorf("no config found: %w", err)
	}
	path := os.Getenv("CCC_DB")
	if path == "" {
		path = dbPath(config)
	}
	db, err := openStore(path)
	if err != nil {
		return fmt.Errorf("open the ccc database: %w", err)
	}
	defer closeStore(db)

	from, err := senderBot(db)
	if err != nil {
		return err
	}
	to, _, err := queueBotMessage(db, config, from, target, body, wake)
	if err != nil {
		return err
	}
	fmt.Printf("queued for %s\n", to.Name)
	return nil
}

func senderBot(db *gorm.DB) (*Bot, error) {
	if id := strings.TrimSpace(os.Getenv("CCC_BOT_ID")); id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err == nil && n > 0 {
			return botByID(db, n)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve the working directory: %w", err)
	}
	b, err := botByCwd(db, cwd)
	if err != nil {
		return nil, fmt.Errorf("no bot owns %s — run this from a bot's working directory (or set CCC_BOT_ID)", cwd)
	}
	return b, nil
}
