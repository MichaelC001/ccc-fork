package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"
)

// `ccc mcp --bot <id> --turn <id>` is the stdio MCP server Claude Code spawns
// for each turn from the inline --mcp-config (DESIGN §6). It is the same
// binary as the listener and opens the same SQLite file; the bot identity
// comes from the flags, never from the model, so a bot cannot act as another.
//
// Everything a tool receives is data written by the model. It is validated and
// stored; it is never treated as an instruction to ccc.

// mcpServer is the per-turn server state.
type mcpServer struct {
	db     *gorm.DB
	config *Config
	botID  int64
	turnID int64
}

// runMCPServer is the entry point for the `mcp` subcommand.
func runMCPServer(args []string) error {
	var botID, turnID int64
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--bot":
			if i+1 < len(args) {
				botID, _ = strconv.ParseInt(args[i+1], 10, 64) // safe-ignore: a bad id stays 0 and is rejected below
				i++
			}
		case "--turn":
			if i+1 < len(args) {
				turnID, _ = strconv.ParseInt(args[i+1], 10, 64) // safe-ignore: an unknown turn id only loses the question<->turn link
				i++
			}
		}
	}
	if botID == 0 {
		return fmt.Errorf("ccc mcp requires --bot <id>")
	}

	// The listener passes the database and config paths through the MCP env
	// block; falling back to the defaults keeps manual invocation working.
	config, err := loadConfig()
	if err != nil {
		config = &Config{}
	}
	path := os.Getenv("CCC_DB")
	if path == "" {
		path = dbPath(config)
	}
	db, err := openStore(path)
	if err != nil {
		return err
	}
	s := &mcpServer{db: db, config: config, botID: botID, turnID: turnID}

	server := mcp.NewServer(&mcp.Implementation{Name: "ccc", Version: version}, nil)
	s.register(server)
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

// text is the standard single-text-block tool result.
func text(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func toolErr(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// ---------------------------------------------------------------------------
// Tool inputs
// ---------------------------------------------------------------------------

type rememberIn struct {
	Scope       string `json:"scope" jsonschema:"where the memory belongs: user (about the owner, shared by all bots), project (about one code base) or bot (private to you)"`
	Key         string `json:"key" jsonschema:"short stable identifier, e.g. deploy-target or prefers-spanish"`
	Text        string `json:"text" jsonschema:"the fact to remember, one or two sentences"`
	ProjectPath string `json:"project_path,omitempty" jsonschema:"absolute path of the project, required for scope=project"`
}

type recallIn struct {
	Query string `json:"query" jsonschema:"words to search for; empty lists the most recent memories"`
	Scope string `json:"scope,omitempty" jsonschema:"restrict to user, project or bot"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results (default 10)"`
}

type forgetIn struct {
	Scope       string `json:"scope" jsonschema:"user, project or bot"`
	Key         string `json:"key" jsonschema:"the key to delete"`
	ProjectPath string `json:"project_path,omitempty" jsonschema:"absolute path, required for scope=project"`
}

type emptyIn struct{}

type sendToBotIn struct {
	Bot  string `json:"bot" jsonschema:"name of the target bot (see list_bots)"`
	Text string `json:"text" jsonschema:"the message"`
	Wake *bool  `json:"wake,omitempty" jsonschema:"run the target bot now instead of waiting for its next turn (default true)"`
}

type notifyOwnerIn struct {
	Text    string `json:"text" jsonschema:"what to tell the owner"`
	Urgency string `json:"urgency,omitempty" jsonschema:"normal (default) or urgent; urgent also sends a direct message"`
}

type askOwnerIn struct {
	Question string   `json:"question" jsonschema:"the question, one sentence"`
	Options  []string `json:"options,omitempty" jsonschema:"up to 4 answers to offer as buttons; omit for a free-text answer"`
}

type updateInstructionsIn struct {
	Role string `json:"role" jsonschema:"your new role description, replacing the current one"`
}

type sendFileIn struct {
	Path    string `json:"path" jsonschema:"absolute path of the file to send"`
	Caption string `json:"caption,omitempty" jsonschema:"optional caption"`
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func (s *mcpServer) register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "remember",
		Description: "Store a durable fact so you and the other bots still know it in future conversations.",
	}, s.remember)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "recall",
		Description: "Search the memories you can see: all user memories, all project memories and your own.",
	}, s.recall)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "forget",
		Description: "Delete one memory by scope and key.",
	}, s.forget)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_bots",
		Description: "List the other bots in the team with their roles and status.",
	}, s.listBots)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_to_bot",
		Description: "Send a message to another bot. It is mirrored into both Telegram topics so the owner sees it.",
	}, s.sendToBot)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "notify_owner",
		Description: "Tell the owner something in your Telegram topic. Use urgency=urgent only when it is worth an interruption.",
	}, s.notifyOwner)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ask_owner",
		Description: "Ask the owner a question and END YOUR TURN. The answer arrives as your next message.",
	}, s.askOwner)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_instructions",
		Description: "Replace your own role description. This starts a fresh conversation on your next message.",
	}, s.updateInstructions)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_file",
		Description: "Send a file from this machine into your Telegram topic (max 50 MB).",
	}, s.sendFile)
}

// ---------------------------------------------------------------------------
// Tool implementations
// ---------------------------------------------------------------------------

func (s *mcpServer) bot() (*Bot, error) { return botByID(s.db, s.botID) }

func (s *mcpServer) remember(_ context.Context, _ *mcp.CallToolRequest, in rememberIn) (*mcp.CallToolResult, any, error) {
	key := strings.TrimSpace(in.Key)
	body := strings.TrimSpace(in.Text)
	if key == "" || body == "" {
		return toolErr("remember needs a non-empty key and text"), nil, nil
	}
	if len(key) > 120 {
		return toolErr("key is too long (max 120 characters)"), nil, nil
	}
	if len(body) > 4000 {
		body = body[:4000]
	}
	scope, scopeKey, err := memoryScopeKey(strings.TrimSpace(in.Scope), s.botID, in.ProjectPath)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	if err := upsertMemory(s.db, scope, scopeKey, key, body, s.botID); err != nil {
		return toolErr("could not store the memory: %v", err), nil, nil
	}
	return text("remembered %q in scope %s", key, scope), nil, nil
}

func (s *mcpServer) recall(_ context.Context, _ *mcp.CallToolRequest, in recallIn) (*mcp.CallToolResult, any, error) {
	scope := strings.TrimSpace(in.Scope)
	if scope != "" && scope != scopeUser && scope != scopeProject && scope != scopeBot {
		return toolErr("unknown scope %q (use user, project or bot)", scope), nil, nil
	}
	mems, err := searchMemories(s.db, s.botID, in.Query, scope, in.Limit)
	if err != nil {
		return toolErr("search failed: %v", err), nil, nil
	}
	if len(mems) == 0 {
		return text("no memories matched"), nil, nil
	}
	var sb strings.Builder
	for _, m := range mems {
		where := m.Scope
		if m.Scope == scopeProject {
			where = "project " + m.ScopeKey
		}
		fmt.Fprintf(&sb, "[%s] %s: %s\n", where, m.Key, m.Text)
	}
	return text("%s", strings.TrimRight(sb.String(), "\n")), nil, nil
}

func (s *mcpServer) forget(_ context.Context, _ *mcp.CallToolRequest, in forgetIn) (*mcp.CallToolResult, any, error) {
	scope, scopeKey, err := memoryScopeKey(strings.TrimSpace(in.Scope), s.botID, in.ProjectPath)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	key := strings.TrimSpace(in.Key)
	res := s.db.Where("scope = ? AND scope_key = ? AND key = ?", scope, scopeKey, key).Delete(&Memory{})
	if res.Error != nil {
		return toolErr("could not delete: %v", res.Error), nil, nil
	}
	if res.RowsAffected == 0 {
		return text("no memory %q in scope %s", key, scope), nil, nil
	}
	return text("forgot %q", key), nil, nil
}

func (s *mcpServer) listBots(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	bots, err := liveBots(s.db)
	if err != nil {
		return toolErr("could not list bots: %v", err), nil, nil
	}
	var sb strings.Builder
	for i := range bots {
		b := &bots[i]
		marker := ""
		if b.ID == s.botID {
			marker = " (you)"
		}
		role := strings.TrimSpace(b.Role)
		if role == "" {
			role = "(no role set)"
		}
		fmt.Fprintf(&sb, "%s%s [%s] — %s\n", b.Name, marker, b.Status, role)
	}
	if sb.Len() == 0 {
		return text("no bots"), nil, nil
	}
	return text("%s", strings.TrimRight(sb.String(), "\n")), nil, nil
}

func (s *mcpServer) sendToBot(_ context.Context, _ *mcp.CallToolRequest, in sendToBotIn) (*mcp.CallToolResult, any, error) {
	body := strings.TrimSpace(in.Text)
	if body == "" {
		return toolErr("send_to_bot needs text"), nil, nil
	}
	target, err := botByName(s.db, strings.TrimSpace(in.Bot))
	if err != nil {
		return toolErr("no live bot named %q (use list_bots)", in.Bot), nil, nil
	}
	if target.ID == s.botID {
		return toolErr("you cannot send a message to yourself"), nil, nil
	}
	wake := true
	if in.Wake != nil {
		wake = *in.Wake
	}
	from := s.botID
	msg := InboxMessage{ToBotID: target.ID, FromBotID: &from, Text: body, Wake: wake}
	if err := s.db.Create(&msg).Error; err != nil {
		return toolErr("could not queue the message: %v", err), nil, nil
	}
	self, err := s.bot()
	fromName := "a bot"
	if err == nil {
		fromName = self.Name
	}
	mirror := fmt.Sprintf("🤝 <b>%s</b> → <b>%s</b>: %s",
		htmlEscape(fromName), htmlEscape(target.Name), htmlEscape(truncate(body, 1500)))
	s.post(target.TopicID, mirror)
	if err == nil && self.TopicID != target.TopicID {
		s.post(self.TopicID, mirror)
	}
	return text("message queued for %s", target.Name), nil, nil
}

func (s *mcpServer) notifyOwner(_ context.Context, _ *mcp.CallToolRequest, in notifyOwnerIn) (*mcp.CallToolResult, any, error) {
	body := strings.TrimSpace(in.Text)
	if body == "" {
		return toolErr("notify_owner needs text"), nil, nil
	}
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	prefix := "🔔"
	if strings.EqualFold(in.Urgency, "urgent") {
		prefix = "🚨"
	}
	msg := fmt.Sprintf("%s %s", prefix, htmlEscape(truncate(body, 3000)))
	s.post(b.TopicID, msg)
	if strings.EqualFold(in.Urgency, "urgent") && s.config.ChatID != 0 && s.config.BotToken != "" {
		_, _ = sendMessageHTMLGetID(s.config, s.config.ChatID, 0,
			fmt.Sprintf("%s <b>%s</b>: %s", prefix, htmlEscape(b.Name), htmlEscape(truncate(body, 3000)))) // safe-ignore: the topic message already went out
	}
	return text("owner notified"), nil, nil
}

// maxQuestionOptions is Telegram-friendly and matches DESIGN §6 (≤4 options).
const maxQuestionOptions = 4

func (s *mcpServer) askOwner(_ context.Context, _ *mcp.CallToolRequest, in askOwnerIn) (*mcp.CallToolResult, any, error) {
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return toolErr("ask_owner needs a question"), nil, nil
	}
	if len(in.Options) > maxQuestionOptions {
		return toolErr("at most %d options are allowed", maxQuestionOptions), nil, nil
	}
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	opts := make([]string, 0, len(in.Options))
	for _, o := range in.Options {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		opts = append(opts, truncate(o, 60))
	}
	optionsJSON, err := json.Marshal(opts)
	if err != nil {
		return toolErr("invalid options"), nil, nil
	}
	row := Question{BotID: b.ID, Question: truncate(q, 2000), OptionsJSON: string(optionsJSON)}
	if s.turnID != 0 {
		row.TurnID = &s.turnID
	}
	if err := s.db.Create(&row).Error; err != nil {
		return toolErr("could not record the question: %v", err), nil, nil
	}
	msgID := s.postQuestion(b.TopicID, row.ID, q, opts)
	if msgID != 0 {
		s.db.Model(&Question{}).Where("id = ?", row.ID).Update("asked_message_id", msgID)
	}
	return text(`{"status":"asked","question_id":%d} — end your turn now; the answer arrives as your next message`, row.ID), nil, nil
}

func (s *mcpServer) updateInstructions(_ context.Context, _ *mcp.CallToolRequest, in updateInstructionsIn) (*mcp.CallToolResult, any, error) {
	role := strings.TrimSpace(in.Role)
	if role == "" {
		return toolErr("update_instructions needs a role"), nil, nil
	}
	if len(role) > 4000 {
		role = role[:4000]
	}
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	if err := s.db.Model(&Bot{}).Where("id = ?", b.ID).Update("role", role).Error; err != nil {
		return toolErr("could not update the role: %v", err), nil, nil
	}
	s.post(b.TopicID, "📝 <b>New role</b>\n"+htmlEscape(truncate(role, 3000)))
	return text("role updated; your next message starts a fresh conversation with it"), nil, nil
}

// sendFileMaxBytes is Telegram's bot upload limit.
const sendFileMaxBytes = 50 * 1024 * 1024

func (s *mcpServer) sendFile(_ context.Context, _ *mcp.CallToolRequest, in sendFileIn) (*mcp.CallToolResult, any, error) {
	path := expandPath(strings.TrimSpace(in.Path))
	if path == "" {
		return toolErr("send_file needs a path"), nil, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return toolErr("bad path: %v", err), nil, nil
	}
	if reason, ok := sendFileForbidden(s.config, abs); ok {
		return toolErr("refusing to send that file: %s", reason), nil, nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		return toolErr("cannot read %s: %v", abs, err), nil, nil
	}
	if info.IsDir() {
		return toolErr("%s is a directory", abs), nil, nil
	}
	if info.Size() > sendFileMaxBytes {
		return toolErr("%s is %d bytes, over the 50 MB Telegram limit", abs, info.Size()), nil, nil
	}
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	if s.config.BotToken == "" || s.config.GroupID == 0 {
		return text("(no Telegram configured; %s not sent)", abs), nil, nil
	}
	if err := sendFile(s.config, s.config.GroupID, b.TopicID, abs, in.Caption); err != nil {
		return toolErr("send failed: %v", err), nil, nil
	}
	return text("sent %s", filepath.Base(abs)), nil, nil
}

// sendFileForbidden implements the DESIGN §12 rule: credentials never leave the
// machine through send_file. Anything inside a Claude config dir, inside
// <data_dir>/profiles, or with a credential-ish name is refused.
func sendFileForbidden(config *Config, abs string) (string, bool) {
	lower := strings.ToLower(filepath.Base(abs))
	for _, bad := range []string{".credentials.json", "credentials.json", ".claude.json", "id_rsa", "id_ed25519", ".env"} {
		if lower == bad {
			return "it looks like a credentials file", true
		}
	}
	guarded := []string{filepath.Join(dataDir(config), "profiles")}
	for _, p := range listProfiles(config) {
		guarded = append(guarded, claudeHome(p))
	}
	home, err := os.UserHomeDir()
	if err == nil {
		guarded = append(guarded, filepath.Join(home, ".ssh"), filepath.Join(home, ".aws"), configDir())
	}
	for _, g := range guarded {
		if g == "" {
			continue
		}
		if abs == g || strings.HasPrefix(abs, strings.TrimRight(g, string(filepath.Separator))+string(filepath.Separator)) {
			return "it is inside a credentials/config directory", true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Telegram side of the MCP process
// ---------------------------------------------------------------------------

// post writes into a topic, or does nothing when this instance has no Telegram
// configured (which is how the runner is exercised in tests).
func (s *mcpServer) post(topicID int64, html string) {
	if s.config == nil || s.config.BotToken == "" || s.config.GroupID == 0 || topicID == 0 {
		return
	}
	_, _ = sendMessageHTMLGetID(s.config, s.config.GroupID, topicID, html) // safe-ignore: a failed mirror must not fail the tool call
}

// postQuestion renders an ask_owner question with one button per option and
// returns the message id so a tap can be matched back to the question.
func (s *mcpServer) postQuestion(topicID, questionID int64, question string, options []string) int64 {
	if s.config == nil || s.config.BotToken == "" || s.config.GroupID == 0 || topicID == 0 {
		return 0
	}
	body := "❓ " + htmlEscape(question)
	if len(options) == 0 {
		body += "\n<i>Reply to this message with your answer.</i>"
		id, _ := sendMessageHTMLGetID(s.config, s.config.GroupID, topicID, body) // safe-ignore: a question with no message id can still be answered by reply
		return id
	}
	var rows [][]InlineKeyboardButton
	for i, o := range options {
		rows = append(rows, []InlineKeyboardButton{{
			Text:         o,
			CallbackData: fmt.Sprintf("q:%d:%d", questionID, i),
		}})
	}
	id, err := sendMessageKeyboardGetID(s.config, s.config.GroupID, topicID, body, rows)
	if err != nil {
		return 0
	}
	return id
}

// pendingQuestion returns the bot's oldest unanswered question, if any.
func pendingQuestion(db *gorm.DB, botID int64) (*Question, bool) {
	var q Question
	if err := db.Where("bot_id = ? AND answered_at IS NULL", botID).Order("id").First(&q).Error; err != nil {
		return nil, false
	}
	return &q, true
}

// answerQuestion records an answer and returns the text to feed back into the
// bot as its next input (DESIGN §6 ask_owner).
func answerQuestion(db *gorm.DB, q *Question, answer string) string {
	now := time.Now()
	db.Model(&Question{}).Where("id = ?", q.ID).
		Updates(map[string]any{"answer": answer, "answered_at": now})
	return fmt.Sprintf("Answer to %q: %s", q.Question, answer)
}

// questionOptions decodes the stored options list.
func questionOptions(q *Question) []string {
	var opts []string
	if q.OptionsJSON == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(q.OptionsJSON), &opts); err != nil {
		return nil
	}
	return opts
}
