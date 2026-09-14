package main

import (
	"strings"
	"testing"
)

func TestRenderAccountsCard(t *testing.T) {
	cards := []accountCard{
		{
			Profile:    Profile{Name: "work", ConfigDir: "/data/profiles/work"},
			State:      accountOK,
			Account:    "jairo@example.com",
			Usage:      profileUsage{FiveHour: 12, SevenDay: 40, FiveHourKnown: true, SevenDayKnown: true},
			Bots:       []string{"deployer", "watcher"},
			IsDefault:  true,
			Disclaimer: true,
		},
		{
			Profile: Profile{Name: "personal", ConfigDir: "/data/profiles/personal"},
			State:   accountNeedsLogin,
			Usage:   profileUsage{FiveHour: unknownUtilization, SevenDay: unknownUtilization},
		},
		{
			Profile:    Profile{Name: "spare"},
			State:      accountLoggedOut,
			Usage:      profileUsage{FiveHour: 0, SevenDay: 0, FiveHourKnown: true, SevenDayKnown: true},
			Disclaimer: true,
		},
	}
	body, buttons := renderAccounts(cards)

	for _, want := range []string{
		"work", "⭐", "✅ logged in", "jairo@example.com", "5h 12% · 7d 40%",
		"deployer, watcher", "personal", "⚠️ needs login", "5h ? · 7d ?",
		"spare", "❌ not logged in", "running: nothing", "bypass disclaimer not accepted",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the card is missing %q:\n%s", want, body)
		}
	}
	// The default account must not offer a "make default" button, and the
	// disclaimer warning belongs only to the profile that lacks it.
	if strings.Count(body, "bypass disclaimer not accepted") != 1 {
		t.Errorf("disclaimer warning shown for the wrong number of accounts:\n%s", body)
	}

	var relogin, makeDefault int
	for _, row := range buttons {
		for _, b := range row {
			switch {
			case strings.HasPrefix(b.CallbackData, "account:login:"):
				relogin++
			case strings.HasPrefix(b.CallbackData, "account:default:"):
				makeDefault++
			}
		}
	}
	if relogin != 3 {
		t.Errorf("%d relogin buttons, want one per account", relogin)
	}
	if makeDefault != 2 {
		t.Errorf("%d default buttons, want one per non-default account", makeDefault)
	}
}

func TestRenderAccountsWithNoProfiles(t *testing.T) {
	body, buttons := renderAccounts(nil)
	if !strings.Contains(body, "/account add") {
		t.Errorf("an empty account list should say how to add one: %q", body)
	}
	if len(buttons) != 0 {
		t.Errorf("no buttons expected with no accounts, got %d rows", len(buttons))
	}
}

// /account add validates the name before it becomes a directory under data_dir.
func TestAccountAddRejectsUnsafeNames(t *testing.T) {
	in, _, api := testInstance(t)
	for _, bad := range []string{"../escape", "with space", "", strings.Repeat("x", 40)} {
		in.handleAccountCommand(dmMessage(42, ""), "add "+bad)
	}
	if cfg := in.config(); len(cfg.Profiles) != 0 {
		t.Errorf("an unsafe name created a profile: %+v", cfg.Profiles)
	}
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "short name") && !strings.Contains(joined, "Usage") {
		t.Errorf("no rejection was reported:\n%s", joined)
	}
}

func TestAccountDefaultAndUnknownName(t *testing.T) {
	in, _, api := testInstance(t)
	in.setConfig(&Config{
		BotToken: "TESTTOKEN", ChatID: 42, GroupID: -100777, DataDir: in.dataDir,
		Profiles: map[string]*Profile{"a": {ConfigDir: "/tmp/a"}, "b": {ConfigDir: "/tmp/b"}},
	})
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	in.accountSetDefault(42, 0, "nope")
	if strings.Contains(strings.Join(api.texts(""), "\n"), "⭐ Default account") {
		t.Error("an unknown account was made default")
	}

	in.accountSetDefault(42, 0, "b")
	if got := in.config().DefaultProfile; got != "b" {
		t.Errorf("default profile = %q, want b", got)
	}
}

// An account carrying a running turn cannot be pulled out from under it.
func TestAccountRemoveRefusesWhileATurnIsRunning(t *testing.T) {
	in, _, _ := testInstance(t)
	in.setConfig(&Config{
		BotToken: "TESTTOKEN", ChatID: 42, GroupID: -100777, DataDir: in.dataDir,
		Profiles: map[string]*Profile{"a": {ConfigDir: "/tmp/a"}},
	})
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("busy", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: b.ID, Profile: "a", Source: sourceUser, Status: turnRunning}).Error; err != nil {
		t.Fatal(err)
	}

	if got := in.accountRemove("a"); !strings.Contains(got, "busy") {
		t.Errorf("removal was not refused: %q", got)
	}
	if _, still := in.config().Profiles["a"]; !still {
		t.Error("the profile was removed even though a turn was running on it")
	}

	// Once the turn ends, it can go.
	in.db.Model(&Turn{}).Where("bot_id = ?", b.ID).Update("status", turnDone)
	if got := in.accountRemove("a"); !strings.Contains(got, "Removed") {
		t.Errorf("removal failed after the turn finished: %q", got)
	}
	if _, still := in.config().Profiles["a"]; still {
		t.Error("the profile survived its removal")
	}
}

func TestBusyBotsByProfile(t *testing.T) {
	in, _, _ := testInstance(t)
	one, err := in.createBot("one", "")
	if err != nil {
		t.Fatal(err)
	}
	two, err := in.createBot("two", "")
	if err != nil {
		t.Fatal(err)
	}
	in.db.Create(&Turn{BotID: one.ID, Profile: "work", Source: sourceUser, Status: turnRunning})
	in.db.Create(&Turn{BotID: two.ID, Profile: "work", Source: sourceUser, Status: turnRunning})
	in.db.Create(&Turn{BotID: two.ID, Profile: "personal", Source: sourceUser, Status: turnDone})

	busy := in.busyBotsByProfile()
	if len(busy["work"]) != 2 {
		t.Errorf("work = %v, want both bots", busy["work"])
	}
	if len(busy["personal"]) != 0 {
		t.Errorf("a finished turn still counts as busy: %v", busy["personal"])
	}

	// The same source feeds profile selection's tie-break.
	if got := runningTurnsByProfileDB(in.db)["work"]; got != 2 {
		t.Errorf("runningTurnsByProfile[work] = %d, want 2", got)
	}
}

// /model and /setgroup are the two owner commands that rewrite the bootstrap
// configuration from Telegram, which is what makes a headless box possible.
func TestModelCommand(t *testing.T) {
	in, _, api := testInstance(t)
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	in.handleMessage(ownerMessage(0, "/model"))
	if !strings.Contains(strings.Join(api.texts(""), "\n"), "claude default") {
		t.Error("/model with no argument should show the current model")
	}

	in.handleMessage(ownerMessage(0, "/model sonnet"))
	if got := in.config().Model; got != "sonnet" {
		t.Errorf("model = %q, want sonnet", got)
	}
	reloaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Model != "sonnet" {
		t.Errorf("the model was not persisted: %q", reloaded.Model)
	}

	in.handleMessage(ownerMessage(0, "/model default"))
	if got := in.config().Model; got != "" {
		t.Errorf("/model default should clear it, got %q", got)
	}
}

func TestSetGroupCommand(t *testing.T) {
	in, _, api := testInstance(t)
	in.setConfig(&Config{BotToken: "TESTTOKEN", ChatID: 42, DataDir: in.dataDir})
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	// In a DM it is refused: there is nothing to bind to.
	in.handleMessage(dmMessage(42, "/setgroup"))
	if in.config().GroupID != 0 {
		t.Error("/setgroup in a DM must not set a group")
	}

	msg := ownerMessage(0, "/setgroup")
	msg.Chat.ID = -100999
	in.handleMessage(msg)

	if got := in.config().GroupID; got != -100999 {
		t.Errorf("group = %d, want -100999", got)
	}
	reloaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.GroupID != -100999 {
		t.Errorf("the group was not persisted: %d", reloaded.GroupID)
	}
	if !strings.Contains(strings.Join(api.texts(""), "\n"), "-100999") {
		t.Error("/setgroup did not confirm the new group")
	}
}
