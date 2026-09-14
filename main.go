package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const version = "3.0.0"

// Config is the bootstrap configuration (<config_dir>/config.json). Everything
// that changes at runtime lives in SQLite instead (DESIGN §5); this file only
// holds what ccc needs before the database exists.
type Config struct {
	BotToken          string              `json:"bot_token"`
	ChatID            int64               `json:"chat_id"`                      // the owner's Telegram user id — also their DM chat
	GroupID           int64               `json:"group_id,omitempty"`           // the forum group the bots live in
	TranscriptionLang string              `json:"transcription_lang,omitempty"` // language code for whisper (e.g. "es")
	RelayURL          string              `json:"relay_url,omitempty"`          // relay server for files over 50 MB
	Profiles          map[string]*Profile `json:"profiles,omitempty"`           // profile name -> Claude account (CLAUDE_CONFIG_DIR)
	DefaultProfile    string              `json:"default_profile,omitempty"`    // profile used when selection has no better answer
	DataDir           string              `json:"data_dir,omitempty"`           // runtime root (default ~/.local/share/ccc)
	Model             string              `json:"model,omitempty"`              // model every bot runs on (default: claude's own)
	EnvPassthrough    []string            `json:"env_passthrough,omitempty"`    // extra env var names bots inherit (DESIGN §3.1)
}

// TelegramMessage represents a Telegram message.
type TelegramMessage struct {
	MessageID       int   `json:"message_id"`
	MessageThreadID int64 `json:"message_thread_id,omitempty"` // topic id
	Chat            struct {
		ID   int64  `json:"id"`
		Type string `json:"type"` // "private", "group", "supergroup"
	} `json:"chat"`
	From struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	} `json:"from"`
	Text           string            `json:"text"`
	ReplyToMessage *TelegramMessage  `json:"reply_to_message,omitempty"`
	Voice          *TelegramVoice    `json:"voice,omitempty"`
	Photo          []TelegramPhoto   `json:"photo,omitempty"`
	Document       *TelegramDocument `json:"document,omitempty"`
	Caption        string            `json:"caption,omitempty"`
}

type TelegramVoice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

type TelegramPhoto struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int    `json:"file_size"`
}

type TelegramDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	FileSize int    `json:"file_size"`
}

// CallbackQuery represents an inline-button tap.
type CallbackQuery struct {
	ID   string `json:"id"`
	From struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	} `json:"from"`
	Message *TelegramMessage `json:"message"`
	Data    string           `json:"data"`
}

// telegramUpdateEntry is one element of a getUpdates result.
type telegramUpdateEntry struct {
	UpdateID      int              `json:"update_id"`
	Message       TelegramMessage  `json:"message"`
	EditedMessage *TelegramMessage `json:"edited_message"`
	CallbackQuery *CallbackQuery   `json:"callback_query"`
}

// TelegramUpdate represents a getUpdates response.
type TelegramUpdate struct {
	OK          bool                  `json:"ok"`
	Description string                `json:"description"`
	Result      []telegramUpdateEntry `json:"result"`
}

// TelegramResponse represents a generic Bot API response.
type TelegramResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// TopicResult is the result of creating a forum topic.
type TopicResult struct {
	MessageThreadID int64  `json:"message_thread_id"`
	Name            string `json:"name"`
}

// InlineKeyboardButton represents a Telegram inline keyboard button.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

func init() {
	initProfiles()
	initPaths()
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		return
	}

	switch os.Args[1] {
	case "-h", "--help", "help":
		printHelp()

	case "-v", "--version", "version":
		fmt.Printf("ccc version %s\n", version)

	case "setup":
		if len(os.Args) < 3 {
			fail("Usage: ccc setup <bot_token>")
		}
		must(setup(os.Args[2]))

	case "doctor":
		doctor()

	case "profile":
		must(profileCommand(os.Args[2:]))

	case "config":
		must(configCommand(os.Args[2:]))

	case "setgroup":
		config, err := loadConfig()
		must(err)
		must(setGroup(config))

	case "mcp":
		// Stdio MCP server for one turn; spawned by Claude Code, never by hand.
		must(runMCPServer(os.Args[2:]))

	case "listen":
		must(listenV3())

	case "install":
		must(installService())

	case "send":
		if len(os.Args) < 3 {
			fail("Usage: ccc send <file>")
		}
		must(handleSendFile(os.Args[2]))

	case "relay":
		port := "8080"
		if len(os.Args) >= 3 {
			port = os.Args[2]
		}
		runRelayServer(port)

	default:
		fail("Unknown command %q. Run `ccc --help`.", os.Args[1])
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// `ccc config`
// ---------------------------------------------------------------------------

// configCommand is the non-interactive bootstrap path: on a headless VM,
// `ccc config set bot_token …`, `chat_id …` and `group_id …` are enough to
// bring an instance up without ever attaching a terminal to Telegram.
func configCommand(args []string) error {
	config, err := loadConfig()
	if err != nil || config == nil {
		config = &Config{} // not configured yet: `config set` is how it starts existing
	}
	if len(args) == 0 {
		printConfig(config)
		return nil
	}
	switch args[0] {
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: ccc config get <key>")
		}
		value, err := configGet(config, args[1])
		if err != nil {
			return err
		}
		fmt.Println(value)
		return nil
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: ccc config set <key> <value>")
		}
		if err := configSet(config, args[1], strings.Join(args[2:], " ")); err != nil {
			return err
		}
		if err := saveConfig(config); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		value, err := configGet(config, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("✅ %s = %s\n", args[1], value)
		return nil
	default:
		return fmt.Errorf("usage: ccc config [get <key> | set <key> <value>]")
	}
}

// configKeys are the keys `ccc config` understands. Secrets are never printed
// back (DESIGN §12): the bot token reads as "configured".
var configKeys = []string{"bot_token", "chat_id", "group_id", "model", "data_dir", "env_passthrough", "relay_url", "transcription_lang", "default_profile"}

func configGet(config *Config, key string) (string, error) {
	switch key {
	case "bot_token":
		if config.BotToken == "" {
			return "not set", nil
		}
		return "configured", nil
	case "chat_id":
		return fmt.Sprint(config.ChatID), nil
	case "group_id":
		return fmt.Sprint(config.GroupID), nil
	case "model":
		return firstNonEmpty(config.Model, "(claude default)"), nil
	case "data_dir":
		return dataDir(config), nil
	case "env_passthrough":
		return strings.Join(config.EnvPassthrough, ","), nil
	case "relay_url":
		return firstNonEmpty(config.RelayURL, defaultRelayURL), nil
	case "transcription_lang":
		return firstNonEmpty(config.TranscriptionLang, "(auto-detect)"), nil
	case "default_profile":
		return firstNonEmpty(config.DefaultProfile, "(first by name)"), nil
	}
	return "", fmt.Errorf("unknown config key %q (known: %s)", key, strings.Join(configKeys, ", "))
}

func configSet(config *Config, key, value string) error {
	value = strings.TrimSpace(value)
	parseID := func() (int64, error) {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a number, got %q", key, value)
		}
		return n, nil
	}
	switch key {
	case "bot_token":
		config.BotToken = value
	case "chat_id":
		n, err := parseID()
		if err != nil {
			return err
		}
		config.ChatID = n
	case "group_id":
		n, err := parseID()
		if err != nil {
			return err
		}
		config.GroupID = n
	case "model":
		config.Model = value
	case "data_dir":
		config.DataDir = value
	case "env_passthrough":
		config.EnvPassthrough = splitList(value)
	case "relay_url":
		config.RelayURL = value
	case "transcription_lang":
		config.TranscriptionLang = value
	case "default_profile":
		config.DefaultProfile = value
	default:
		return fmt.Errorf("unknown config key %q (known: %s)", key, strings.Join(configKeys, ", "))
	}
	return nil
}

// splitList parses a comma- or space-separated list, dropping empties.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func printConfig(config *Config) {
	fmt.Printf("config file: %s\n\n", getConfigPath())
	for _, key := range configKeys {
		value, err := configGet(config, key)
		if err != nil {
			continue
		}
		fmt.Printf("%-19s %s\n", key+":", value)
	}
	fmt.Println("\nUsage: ccc config set <key> <value>")
}
