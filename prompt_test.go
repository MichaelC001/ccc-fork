package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRenderSystemPromptCarriesIdentityAndRoster(t *testing.T) {
	got := renderSystemPrompt(
		promptBot{Name: "deployer", Role: "ships fecha to prod", Cwd: "/srv/fecha"},
		"jairo.local",
		[]otherBot{{Name: "watcher", Role: "watches CI", Status: botIdle}},
		[]string{"🚀", "📝"},
	)
	for _, want := range []string{"deployer", "ships fecha to prod", "jairo.local", "/srv/fecha", "watcher", "watches CI",
		"set_name", "🚀"} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt is missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "ask_owner") || !strings.Contains(got, "remember") {
		t.Error("system prompt does not describe the ccc tools")
	}
}

func TestRenderSystemPromptWithoutRole(t *testing.T) {
	got := renderSystemPrompt(promptBot{Name: "fresh", Cwd: "/tmp"}, "host", nil, nil)
	if !strings.Contains(got, "/role") {
		t.Errorf("a role-less bot should be told how a role gets set:\n%s", got)
	}
	if strings.Contains(got, "Other bots:") {
		t.Error("no roster should be rendered when there are no other bots")
	}
}

func TestRenderEnvelopeShape(t *testing.T) {
	now := time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
	got := renderEnvelope(envelopeInput{
		Source:      "bot:watcher",
		Message:     "the deploy failed",
		Now:         now,
		UserMems:    []Memory{{Key: "tz", Text: "Europe/Madrid"}},
		ProjectMems: []Memory{{Key: "deploy", Text: "systemd on vps3"}},
		BotMems:     []Memory{{Key: "last-run", Text: "green"}},
		InboxFrom:   map[string]int{"watcher": 2},
	})
	for _, want := range []string{
		"<context>", "</context>", "2026-09-14",
		"user memories:", "tz: Europe/Madrid",
		"project memories:", "deploy: systemd on vps3",
		"your memories:", "last-run: green",
		"pending inbox: 2 message(s) (2 from watcher)",
		`<message source="bot:watcher">`, "the deploy failed", "</message>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderEnvelopeDefaultsSourceToUser(t *testing.T) {
	got := renderEnvelope(envelopeInput{Message: "hi", Now: time.Now()})
	if !strings.Contains(got, `<message source="user">`) {
		t.Errorf("missing default source:\n%s", got)
	}
}

func TestRenderEnvelopeRespectsTheContextBudget(t *testing.T) {
	mems := make([]Memory, 200)
	for i := range mems {
		mems[i] = Memory{Key: fmt.Sprintf("key-%03d", i), Text: strings.Repeat("x", 200)}
	}
	message := "what is the status?"
	got := renderEnvelope(envelopeInput{
		Message: message, Now: time.Now(),
		UserMems: mems, ProjectMems: mems, BotMems: mems,
	})

	start := strings.Index(got, "<context>\n")
	end := strings.Index(got, "</context>")
	if start < 0 || end < 0 {
		t.Fatalf("no context block:\n%s", got)
	}
	ctx := got[start+len("<context>\n") : end]
	if len(ctx) > envelopeBudget {
		t.Errorf("context block is %d bytes, over the %d budget", len(ctx), envelopeBudget)
	}
	// The message itself is never sacrificed to the budget.
	if !strings.Contains(got, message) {
		t.Error("the message was dropped by the budget")
	}
	if !strings.Contains(ctx, "key-000") {
		t.Error("the budget dropped everything instead of filling up to the cap")
	}
}

func TestBuildEnvelopePullsLiveContext(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("ctx", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertMemory(in.db, scopeUser, "", "tz", "Europe/Madrid", b.ID); err != nil {
		t.Fatal(err)
	}
	if err := upsertMemory(in.db, scopeBot, fmt.Sprint(b.ID), "mood", "calm", b.ID); err != nil {
		t.Fatal(err)
	}
	other, err := in.createBot("other", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&InboxMessage{ToBotID: b.ID, FromBotID: &other.ID, Text: "ping"}).Error; err != nil {
		t.Fatal(err)
	}

	got := buildEnvelope(in.db, b, sourceUser, "hello", time.Now())
	for _, want := range []string{"tz: Europe/Madrid", "mood: calm", "1 from other", "hello"} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope missing %q:\n%s", want, got)
		}
	}
}

func TestBuildEnvelopeKeepsOtherBotsMemoriesOut(t *testing.T) {
	in, _, _ := testInstance(t)
	mine, _ := in.createBot("mine", "")
	theirs, _ := in.createBot("theirs", "")
	if err := upsertMemory(in.db, scopeBot, fmt.Sprint(theirs.ID), "secret", "not for you", theirs.ID); err != nil {
		t.Fatal(err)
	}
	got := buildEnvelope(in.db, mine, sourceUser, "hi", time.Now())
	if strings.Contains(got, "not for you") {
		t.Error("a bot's private memory leaked into another bot's envelope")
	}
}

// A bot with no role is onboarded through the envelope, so the instruction can
// disappear the moment update_instructions runs (the system prompt could not).
func TestEnvelopeOnboardsARoleLessBot(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("nameless", "")
	if err != nil {
		t.Fatal(err)
	}

	got := buildEnvelope(in.db, b, sourceUser, "hello", time.Now())
	for _, want := range []string{"no role yet", "update_instructions", "set_name"} {
		if !strings.Contains(got, want) {
			t.Errorf("a role-less bot was not onboarded (missing %q):\n%s", want, got)
		}
	}

	if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("role", "ships things").Error; err != nil {
		t.Fatal(err)
	}
	withRole, err := botByID(in.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := buildEnvelope(in.db, withRole, sourceUser, "hello", time.Now()); strings.Contains(got, "no role yet") {
		t.Errorf("the onboarding instruction survived the role being set:\n%s", got)
	}
}
