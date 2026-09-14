package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestClaudeTurnArgsIsolatesTheBot(t *testing.T) {
	args := claudeTurnArgs("sonnet", "SYS", `{"mcpServers":{}}`, "11111111-2222-4333-8444-555555555555", false)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-p", "--output-format stream-json", "--verbose",
		"--permission-mode bypassPermissions", "--disable-slash-commands",
		"--strict-mcp-config", "--system-prompt SYS", "--model sonnet",
		"--session-id 11111111-2222-4333-8444-555555555555",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
	// --setting-sources must be present with an EMPTY value: that is the only
	// combination verified to load no CLAUDE.md at all.
	found := false
	for i, a := range args {
		if a == "--setting-sources" {
			found = true
			if i+1 >= len(args) || args[i+1] != "" {
				t.Errorf("--setting-sources must be followed by an empty value, got %q", args[i+1:])
			}
		}
	}
	if !found {
		t.Error("--setting-sources is missing; the bot would load the owner's CLAUDE.md")
	}
	for _, forbidden := range []string{"--bare", "--safe-mode", "--append-system-prompt"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("%s must never be used", forbidden)
		}
	}
}

func TestClaudeTurnArgsResumeAndDefaultModel(t *testing.T) {
	args := claudeTurnArgs("", "SYS", "{}", "sess-1", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume sess-1") {
		t.Errorf("resume flag missing: %s", joined)
	}
	if strings.Contains(joined, "--session-id") {
		t.Error("a resume must not also pass --session-id")
	}
	if strings.Contains(joined, "--model") {
		t.Error("no model configured should mean no --model flag")
	}
}

func TestNewUUIDIsAValidSessionID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u := newUUID()
		if !re.MatchString(u) {
			t.Fatalf("newUUID produced %q, which claude --session-id would reject", u)
		}
		if seen[u] {
			t.Fatalf("newUUID repeated %q", u)
		}
		seen[u] = true
	}
}

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{staleTokenMsg, errAuthStale},
		{"Error: Not logged in. Please run claude login", errAuthStale},
		{"Claude AI usage limit reached|1788000000", errRateLimited},
		{"API Error: 429 rate_limit_error", errRateLimited},
		{"No conversation found with session ID abc", errSessionLost},
		{"fetch failed: ECONNRESET", errTransient},
		{"API Error: 503 upstream overloaded", errTransient},
		{"TypeError: undefined is not a function", errFatal},
	}
	for _, c := range cases {
		if got := classifyFailure(c.text, 1); got != c.want {
			t.Errorf("classifyFailure(%q) = %q, want %q", truncate(c.text, 40), got, c.want)
		}
	}
}

func TestSummarizeToolNeverLeaksPayloads(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"Bash", `{"command":"go test ./...","description":"Run the test suite"}`, "run the test suite"},
		{"Bash", `{"command":"go build ./..."}`, "running go build ./..."},
		{"Read", `{"file_path":"/srv/app/poller.go"}`, "reading poller.go"},
		{"Edit", `{"file_path":"/srv/app/runner.go"}`, "editing runner.go"},
		{"Grep", `{"pattern":"TODO"}`, "searching for TODO"},
		{"mcp__ccc__ask_owner", `{"question":"deploy?"}`, "asking you a question"},
		{"mcp__ccc__remember", `{"key":"deploy-target","text":"secret stuff"}`, "remembering deploy-target"},
		{"WeirdTool", `{}`, "running WeirdTool"},
	}
	for _, c := range cases {
		got := summarizeTool(c.name, json.RawMessage(c.input))
		if got != c.want {
			t.Errorf("summarizeTool(%s) = %q, want %q", c.name, got, c.want)
		}
	}
	if got := summarizeTool("mcp__ccc__remember", json.RawMessage(`{"key":"k","text":"SECRET"}`)); strings.Contains(got, "SECRET") {
		t.Errorf("tool payload leaked into the progress line: %q", got)
	}
}

func TestSummarizeToolHandlesUnparseableInput(t *testing.T) {
	if got := summarizeTool("Read", json.RawMessage(`not json`)); got == "" {
		t.Error("a broken payload should still produce a label")
	}
}

func TestConsumeEventCollectsTheResult(t *testing.T) {
	r := &Runner{}
	res := &streamResult{}
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"abc"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`,
		`garbage that is not json`,
		`{"type":"result","subtype":"success","is_error":false,"result":"all good","usage":{"input_tokens":10}}`,
	}
	for _, l := range lines {
		r.consumeEvent([]byte(l), res, nil)
	}
	if res.Text != "all good" || res.Subtype != "success" || res.IsError {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(res.UsageJSON, "input_tokens") {
		t.Errorf("usage was not captured: %q", res.UsageJSON)
	}
	if !res.ok() {
		t.Error("a clean result should be ok()")
	}
}

func TestStreamResultFailureText(t *testing.T) {
	res := &streamResult{IsError: true, Text: "model refused", stderr: "boom", exitCode: 1}
	got := res.failureText()
	if !strings.Contains(got, "boom") || !strings.Contains(got, "model refused") {
		t.Errorf("failureText = %q", got)
	}
	empty := &streamResult{exitCode: 3}
	if !strings.Contains(empty.failureText(), "exited 3") {
		t.Errorf("a silent failure should still describe itself: %q", empty.failureText())
	}
}

func TestBotEnvPassthroughNeverLeaksClaudeVars(t *testing.T) {
	t.Setenv("CCC_TEST_TOKEN", "s3cret")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-pass")
	cfg := &Config{EnvPassthrough: []string{"CCC_TEST_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_SOMETHING", ""}}
	env := botEnv(cfg, implicitProfile())
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "CCC_TEST_TOKEN=s3cret") {
		t.Error("an allowed variable was not passed through")
	}
	if strings.Contains(joined, "ANTHROPIC_API_KEY") || strings.Contains(joined, "CLAUDE_CODE_SOMETHING") {
		t.Errorf("a CLAUDE*/ANTHROPIC* variable was passed through:\n%s", joined)
	}
}

func TestMCPConfigPointsAtThisBinary(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(nil, &Config{DataDir: dir}, nil)
	var spec struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(r.mcpConfigJSON(3, 9)), &spec); err != nil {
		t.Fatalf("mcp config is not valid JSON: %v", err)
	}
	srv, ok := spec.MCPServers["ccc"]
	if !ok {
		t.Fatal("no ccc server in the mcp config")
	}
	if srv.Type != "stdio" || srv.Command != cccPath {
		t.Errorf("server = %+v, want a stdio server running this binary", srv)
	}
	if strings.Join(srv.Args, " ") != "mcp --bot 3 --turn 9" {
		t.Errorf("args = %v", srv.Args)
	}
	if srv.Env["CCC_DB"] != dbPath(&Config{DataDir: dir}) {
		t.Errorf("CCC_DB = %q", srv.Env["CCC_DB"])
	}
}

// The queue folds everything waiting into the next turn, so a burst of
// messages costs one `claude -p` run, not one per message.
func TestQueuedInputsAreFoldedIntoOneTurn(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("queuer", "")
	if err != nil {
		t.Fatal(err)
	}
	for i, txt := range []string{"first", "second", "third"} {
		row := &Turn{BotID: b.ID, Source: sourceUser, Input: txt, Status: turnQueued, TriggerMessageID: int64(10 + i)}
		if err := in.db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	head, input, triggers, ok := foldQueue(in.db, b.ID)
	if !ok {
		t.Fatal("foldQueue found nothing to run")
	}
	if input != "first\n\nsecond\n\nthird" {
		t.Errorf("folded input = %q", input)
	}
	if len(triggers) != 3 {
		t.Errorf("triggers = %v, want one per message so each gets a ✅", triggers)
	}
	var merged int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND stop_reason LIKE ?", b.ID, "merged%").Count(&merged)
	if merged != 2 {
		t.Errorf("%d turns marked merged, want 2", merged)
	}
	var stillQueued int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND status = ? AND id <> ?", b.ID, turnQueued, head.ID).Count(&stillQueued)
	if stillQueued != 0 {
		t.Errorf("%d turns left queued behind the carrier", stillQueued)
	}

	if _, _, _, ok := foldQueue(in.db, b.ID); !ok {
		t.Error("the carrier turn is still queued until it runs")
	}
}

func TestDisabledBotDoesNotRun(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("paused", "")
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, nil)
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "go", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{botDisabled, botWaiting} {
		setBotStatus(in.db, b.ID, status)
		if r.runNext(b.ID) {
			t.Errorf("a %s bot must not run turns", status)
		}
	}
}

func TestStopDropsTheQueue(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("stopper", "")
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, nil)
	for i := 0; i < 3; i++ {
		if err := r.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "x", Status: turnQueued}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if r.Stop(b.ID) {
		t.Error("Stop should report false when nothing is running")
	}
	var n int64
	r.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", b.ID, turnQueued).Count(&n)
	if n != 0 {
		t.Errorf("%d turns still queued after /stop", n)
	}
}

func TestRenderProgress(t *testing.T) {
	got := renderProgress("running go test", 95*time.Second)
	if !strings.Contains(got, "running go test") || !strings.Contains(got, "1m35s") {
		t.Errorf("progress line = %q", got)
	}
	if !strings.Contains(renderProgress("", time.Second), "working") {
		t.Error("an empty activity should still render something")
	}
	if got := renderProgress("<script>", time.Second); strings.Contains(got, "<script>") {
		t.Errorf("progress line is not HTML-escaped: %q", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		5 * time.Second:    "5s",
		90 * time.Second:   "1m30s",
		3 * time.Hour:      "3h00m",
		3670 * time.Second: "1h01m",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct{ in, cmd, rest string }{
		{"/role", "/role", ""},
		{"/role ships things", "/role", "ships things"},
		{"/role@ccc_bot ships things", "/role", "ships things"},
		{"/STOP", "/stop", ""},
	}
	for _, c := range cases {
		cmd, rest := splitCommand(c.in)
		if cmd != c.cmd || rest != c.rest {
			t.Errorf("splitCommand(%q) = (%q,%q), want (%q,%q)", c.in, cmd, rest, c.cmd, c.rest)
		}
	}
}

func TestBotNameFromText(t *testing.T) {
	if got := botNameFromText("fix the login bug\nand write a test"); got != "fix the login bug" {
		t.Errorf("name = %q", got)
	}
	long := strings.Repeat("word ", 30)
	if got := botNameFromText(long); len(got) > 40 {
		t.Errorf("name is %d chars: %q", len(got), got)
	}
}
