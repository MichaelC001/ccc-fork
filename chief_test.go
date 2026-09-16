package main

import (
	"strings"
	"testing"
	"time"
)

func TestSpawnSessionIsChiefOnly(t *testing.T) {
	in, _, api := testInstance(t)
	worker, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: worker.ID}
	res, _, err := s.spawnSession(t.Context(), nil, spawnSessionIn{Prompt: "fix the deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a worker must not spawn sessions")
	}
	if n := len(api.since("createForumTopic")); n != 0 {
		t.Errorf("worker spawn created %d topics; sessions are backend-only", n)
	}

	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	s = &mcpServer{db: in.db, config: in.cfg, botID: chief.ID}
	res, _, err = s.spawnSession(t.Context(), nil, spawnSessionIn{Prompt: "fix the deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("chief spawn failed: %+v", res.Content)
	}
	if n := len(api.since("createForumTopic")); n != 0 {
		t.Fatalf("spawn_session must not create a Telegram topic, got %d", n)
	}
	spawned, err := botByName(in.db, botNameFromText("fix the deploy"))
	if err != nil {
		t.Fatalf("spawned worker: %v", err)
	}
	if spawned.TopicID >= 0 {
		t.Fatalf("spawned worker should be backend-only (TopicID < 0), got %+v", spawned)
	}
	var queued []InboxMessage
	in.db.Where("from_bot_id = ?", chief.ID).Find(&queued)
	if len(queued) != 1 || queued[0].Text != "fix the deploy" || !queued[0].Wake {
		t.Fatalf("first prompt not queued: %+v", queued)
	}
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "fix the deploy") || strings.Contains(c.Params.Get("text"), "🤝") {
			t.Errorf("spawn_session must not dump the prompt into Telegram: %q", c.Params.Get("text"))
		}
	}
}

func TestTellSessionIsChiefOnly(t *testing.T) {
	in, _, api := testInstance(t)
	alpha, _ := in.createBot("alpha", "")
	beta, _ := in.createBot("beta", "")
	s := &mcpServer{db: in.db, config: in.cfg, botID: alpha.ID}
	res, _, err := s.tellSession(t.Context(), nil, tellSessionIn{Session: "beta", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("workers must not tell each other")
	}
	var n int64
	in.db.Model(&InboxMessage{}).Where("to_bot_id = ?", beta.ID).Count(&n)
	if n != 0 {
		t.Fatalf("worker tell leaked an inbox row")
	}

	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	s = &mcpServer{db: in.db, config: in.cfg, botID: chief.ID}
	res, _, err = s.tellSession(t.Context(), nil, tellSessionIn{Session: "beta", Text: "ship it"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("chief tell failed: %+v", res.Content)
	}
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "ship it") || strings.Contains(c.Params.Get("text"), "🤝") {
			t.Errorf("tell_session must not dump the message into Telegram: %q", c.Params.Get("text"))
		}
	}
}

func TestReportToGeneralIsWorkerOnly(t *testing.T) {
	in, _, api := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: chief.ID}
	res, _, err := s.reportToGeneral(t.Context(), nil, reportToGeneralIn{Text: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("General must not report to itself")
	}

	worker, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	s = &mcpServer{db: in.db, config: in.cfg, botID: worker.ID}
	res, _, err = s.reportToGeneral(t.Context(), nil, reportToGeneralIn{Text: "build green"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("worker report failed: %+v", res.Content)
	}
	var queued []InboxMessage
	in.db.Where("to_bot_id = ?", chief.ID).Find(&queued)
	if len(queued) != 1 || queued[0].Text != "build green" {
		t.Fatalf("report inbox: %+v", queued)
	}
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "build green") || strings.Contains(c.Params.Get("text"), "🤝") {
			t.Errorf("report_to_general must not dump the report into Telegram: %q", c.Params.Get("text"))
		}
	}
}

func TestNotifyOwnerStillPostsToTelegram(t *testing.T) {
	in, _, api := testInstance(t)
	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: w.ID}
	res, _, err := s.notifyOwner(t.Context(), nil, notifyOwnerIn{Text: "deploy is down"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("notify_owner failed: %+v", res.Content)
	}
	found := false
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "deploy is down") {
			found = true
		}
	}
	if !found {
		t.Error("notify_owner must still reach the owner")
	}
}

func TestOwnerSessionStatusOneLiner(t *testing.T) {
	if got, ok := ownerSessionStatus("", false); !ok || got != "done" {
		t.Errorf("idle success = %q ok=%v, want done", got, ok)
	}
	if got, ok := ownerSessionStatus("", true); !ok || got != "waiting" {
		t.Errorf("pending question = %q ok=%v, want waiting", got, ok)
	}
	if got, ok := ownerSessionStatus(errFatal, false); !ok || got != "error" {
		t.Errorf("failure = %q ok=%v, want error", got, ok)
	}
	if _, ok := ownerSessionStatus("stopped", false); ok {
		t.Error("/stop should not ping the owner")
	}
	if _, ok := ownerSessionStatus(chiefTimeoutClass, false); ok {
		t.Error("General timeout should not ping as a session status")
	}
	if got := ownerSessionStatusLine("deployer", "done"); got != "session deployer done" {
		t.Errorf("line = %q", got)
	}
}

func TestPostOwnerSessionStatusSkipsGeneral(t *testing.T) {
	in, _, _ := testInstance(t)
	ui := &fakeUI{}
	r := newRunner(in.db, in.cfg, ui)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	r.postOwnerSessionStatus(chief, "", false)
	if len(ui.posts) != 0 {
		t.Errorf("General must not post a session status one-liner, got %v", ui.posts)
	}

	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	r.postOwnerSessionStatus(w, "", false)
	if len(ui.posts) != 1 || ui.posts[0] != "session deployer done" {
		t.Errorf("worker done = %v, want one-liner", ui.posts)
	}
	r.postOwnerSessionStatus(w, "", true)
	if ui.posts[len(ui.posts)-1] != "session deployer waiting" {
		t.Errorf("worker waiting = %v", ui.posts)
	}
	r.postOwnerSessionStatus(w, errFatal, false)
	if ui.posts[len(ui.posts)-1] != "session deployer error" {
		t.Errorf("worker error = %v", ui.posts)
	}
}

func TestChiefEnvelopeListsWorkersNotItself(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	in.db.Create(&Turn{BotID: w.ID, Source: sourceUser, Input: "go", Output: "deployed fecha to vps3", Status: turnDone, EndedAt: &now})

	got := buildEnvelope(in.db, chief, sourceUser, "what's running?", time.Now())
	if !strings.Contains(got, "active sessions:") || !strings.Contains(got, "deployer") {
		t.Errorf("chief envelope missing roster:\n%s", got)
	}
	if !strings.Contains(got, "deployed fecha") {
		t.Errorf("chief envelope missing last output:\n%s", got)
	}
	if strings.Contains(got, generalBotName+" [") {
		t.Errorf("roster listed General:\n%s", got)
	}

	workerEnv := buildEnvelope(in.db, w, sourceUser, "hi", time.Now())
	if strings.Contains(workerEnv, "active sessions:") {
		t.Errorf("worker envelope must not list the roster:\n%s", workerEnv)
	}
}

func TestArchiveAndRenameRefuseGeneral(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: chief.ID}
	res, _, err := s.archiveBot(t.Context(), nil, archiveBotIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("General must not archive itself")
	}
	res, _, err = s.setName(t.Context(), nil, setNameIn{Name: "boss"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("General must not rename")
	}
	if err := archiveBotRow(in.db, chief.ID); err == nil {
		t.Fatal("archiveBotRow should refuse General")
	}
}

func TestChiefPromptIsByteStable(t *testing.T) {
	b := promptBot{Name: "General", Cwd: "/tmp", Chief: true}
	first := renderSystemPrompt(b, "host", []otherBot{{Name: "a"}})
	second := renderSystemPrompt(b, "host", []otherBot{{Name: "b"}})
	if first != second {
		t.Errorf("chief system prompt is not byte-stable")
	}
	if strings.Contains(first, botIdle) || strings.Contains(first, "deployer") {
		t.Error("live roster leaked into the chief system prompt")
	}
	if !strings.Contains(first, "spawn_session") || !strings.Contains(first, "dispatcher") {
		t.Errorf("chief prompt missing dispatcher tools:\n%s", first)
	}
	if !strings.Contains(first, "60 second") {
		t.Errorf("chief prompt must mention the 60s cap:\n%s", first)
	}
	if !strings.Contains(first, "does not see those reports") {
		t.Errorf("chief prompt must not dump session reports to the owner:\n%s", first)
	}
}

func TestCreateBotIsBackendOnly(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if b.TopicID >= 0 {
		t.Fatalf("new session TopicID = %d, want < 0", b.TopicID)
	}
	if isGeneralBot(b) {
		t.Fatalf("new session must be a backend worker: %+v", b)
	}
	if n := len(api.since("createForumTopic")); n != 0 {
		t.Errorf("createBot created %d forum topics", n)
	}
}

func TestDestForTopic(t *testing.T) {
	cfg := &Config{BotToken: "T", ChatID: 42}
	chat, thread, ok := destForTopic(cfg, 0)
	if !ok || chat != 42 || thread != 0 {
		t.Errorf("General dest = %d/%d ok=%v, want DM 42/0", chat, thread, ok)
	}
	_, _, ok = destForTopic(cfg, 7)
	if ok {
		t.Error("a leftover positive TopicID must have no Telegram dest")
	}
	_, _, ok = destForTopic(cfg, -3)
	if ok {
		t.Error("backend worker must have no Telegram dest")
	}
	cfg.ChatID = 0
	_, _, ok = destForTopic(cfg, 0)
	if ok {
		t.Error("General without ChatID has no Telegram dest")
	}
}

func TestChiefTimeoutIsSixtySecondsForGeneralOnly(t *testing.T) {
	if d := chiefTimeoutFor(&Bot{TopicID: 0}); d != 60*time.Second {
		t.Errorf("General timeout = %s, want 60s", d)
	}
	if d := chiefTimeoutFor(&Bot{TopicID: 1}); d != 0 {
		t.Errorf("worker timeout = %s, want none", d)
	}
	if d := chiefTimeoutFor(nil); d != 0 {
		t.Errorf("nil bot timeout = %s, want none", d)
	}
}

func TestChiefTimeoutInputTellsGeneralToSpawn(t *testing.T) {
	got := chiefTimeoutInput()
	for _, want := range []string{"Error:", "too long for General", "spawn_session", "tell_session"} {
		if !strings.Contains(got, want) {
			t.Errorf("timeout input missing %q:\n%s", want, got)
		}
	}
	if !isChiefTimeoutFollowUp(got) {
		t.Error("the injected error must be recognised as a follow-up so it cannot loop")
	}
	if isChiefTimeoutFollowUp("please spawn a session for the deploy") {
		t.Error("an ordinary spawn request must not look like the timeout follow-up")
	}
}

func TestPersistChiefTimeoutEnqueuesTheInstruction(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, nil)
	row := &Turn{BotID: chief.ID, Source: sourceUser, Input: "fix fecha", Status: turnRunning}
	if err := in.db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	r.persistChiefTimeout(chief, row, "fix fecha", time.Now(), nil)

	var queued []Turn
	in.db.Where("bot_id = ? AND status = ?", chief.ID, turnQueued).Find(&queued)
	if len(queued) != 1 {
		t.Fatalf("queued %d turns, want the injected error", len(queued))
	}
	if queued[0].Source != sourceSystem {
		t.Errorf("source = %q, want system", queued[0].Source)
	}
	if queued[0].Input != chiefTimeoutInput() {
		t.Errorf("injected input = %q", queued[0].Input)
	}

	var done Turn
	in.db.First(&done, row.ID)
	if done.Status != turnFailed || done.ErrorClass != chiefTimeoutClass {
		t.Errorf("timed-out turn = %+v, want failed/%s", done, chiefTimeoutClass)
	}
	var stillQueued int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND status = ? AND id = ?", chief.ID, turnQueued, row.ID).Count(&stillQueued)
	if stillQueued != 0 {
		t.Error("the killed turn must not stay queued")
	}
}

func TestPersistChiefTimeoutDoesNotLoop(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, nil)
	row := &Turn{BotID: chief.ID, Source: sourceSystem, Input: chiefTimeoutInput(), Status: turnRunning}
	if err := in.db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	r.persistChiefTimeout(chief, row, chiefTimeoutInput(), time.Now(), nil)

	var queued int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", chief.ID, turnQueued).Count(&queued)
	if queued != 0 {
		t.Errorf("follow-up timeout enqueued %d more turns; that would loop", queued)
	}
}
