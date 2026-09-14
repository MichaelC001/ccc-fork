package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testScheduler wires a scheduler onto a test instance. The runner stays fake,
// so a watch that fires records a turn instead of spawning claude.
func testScheduler(t *testing.T) (*scheduler, *instance, *fakeRunner, *fakeBotAPI) {
	t.Helper()
	in, runner, api := testInstance(t)
	s := newScheduler(in)
	in.sched = s
	return s, in, runner, api
}

// A watch wakes its bot only when the command's output changes — the first run
// is a baseline, an unchanged run is free, and a change carries a diff.
func TestWatchWakesTheBotOnlyWhenTheOutputChanges(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state.txt")
	if err := os.WriteFile(state, []byte("build: green\ntests: 12 passing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "ci", "cat "+state, 60); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	s.runDueWatches(now) // baseline
	if len(runner.enqueued) != 0 {
		t.Fatalf("the first run must not wake anybody: %+v", runner.enqueued)
	}
	var w Watch
	if err := in.db.Where("bot_id = ?", b.ID).First(&w).Error; err != nil {
		t.Fatal(err)
	}
	if w.LastHash == "" || w.LastRunAt == nil {
		t.Fatal("the baseline was not recorded")
	}

	// Unchanged output, interval elapsed: still nothing.
	s.runDueWatches(now.Add(2 * time.Minute))
	if len(runner.enqueued) != 0 {
		t.Fatalf("an unchanged watch woke the bot: %+v", runner.enqueued)
	}

	// Now it changes.
	if err := os.WriteFile(state, []byte("build: RED\ntests: 11 passing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.runDueWatches(now.Add(4 * time.Minute))
	if len(runner.enqueued) != 1 {
		t.Fatalf("a changed watch enqueued %d turns, want 1", len(runner.enqueued))
	}
	turn := runner.enqueued[0]
	if turn.BotID != b.ID || turn.Source != sourceWatch {
		t.Errorf("turn = %+v, want a watch turn for the bot", turn)
	}
	for _, want := range []string{`Watch "ci" changed`, "- build: green", "+ build: RED"} {
		if !strings.Contains(turn.Text, want) {
			t.Errorf("the watch input is missing %q:\n%s", want, turn.Text)
		}
	}
	// Only the diff travels, never the whole before/after.
	if strings.Contains(turn.Text, "tests: 12 passing") && strings.Contains(turn.Text, "- tests: 12 passing") == false {
		t.Errorf("unchanged context leaked into the input:\n%s", turn.Text)
	}
}

// A watch respects its interval rather than running every tick.
func TestWatchRespectsItsInterval(t *testing.T) {
	s, in, _, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "clock", "date +%s%N", 600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.runDueWatches(now)
	var first Watch
	in.db.Where("bot_id = ?", b.ID).First(&first)

	s.runDueWatches(now.Add(time.Minute)) // well inside the 10-minute interval
	var second Watch
	in.db.Where("bot_id = ?", b.ID).First(&second)
	if !first.LastRunAt.Equal(*second.LastRunAt) {
		t.Error("the watch ran again before its interval had elapsed")
	}
}

func TestWatchIntervalHasAFloor(t *testing.T) {
	_, in, _, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := upsertWatch(in.db, b.ID, "hammer", "true", 1)
	if err != nil {
		t.Fatal(err)
	}
	if w.IntervalS != watchMinInterval {
		t.Errorf("interval = %d, want it clamped to %d", w.IntervalS, watchMinInterval)
	}
}

// Changing a watch's command resets its baseline, so the next run does not
// report the difference between two unrelated commands as a change.
func TestChangingAWatchCommandResetsTheBaseline(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "x", "echo one", 60); err != nil {
		t.Fatal(err)
	}
	s.runDueWatches(time.Now())
	if _, err := upsertWatch(in.db, b.ID, "x", "echo two", 60); err != nil {
		t.Fatal(err)
	}
	s.runDueWatches(time.Now())
	if len(runner.enqueued) != 0 {
		t.Errorf("switching commands reported a spurious change: %+v", runner.enqueued)
	}
}

func TestWatchOfAnArchivedBotIsDisabled(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("goner", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "x", "echo hi", 60); err != nil {
		t.Fatal(err)
	}
	if err := archiveBotRow(in.db, b.ID); err != nil {
		t.Fatal(err)
	}
	s.runDueWatches(time.Now())
	var w Watch
	in.db.Where("bot_id = ?", b.ID).First(&w)
	if w.Enabled {
		t.Error("a watch on an archived bot must stop running")
	}
	if len(runner.enqueued) != 0 {
		t.Error("an archived bot was woken")
	}
}

// A schedule fires once at its time; a cron schedule rolls forward instead.
func TestSchedulesFire(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("sleeper", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	once := Schedule{BotID: b.ID, FireAt: now.Add(-time.Minute), Note: "check the deploy"}
	later := Schedule{BotID: b.ID, FireAt: now.Add(time.Hour), Note: "not yet"}
	daily := Schedule{BotID: b.ID, FireAt: now.Add(-time.Minute), Note: "morning report", RecurringCron: "0 9 * * *"}
	for _, sc := range []*Schedule{&once, &later, &daily} {
		if err := in.db.Create(sc).Error; err != nil {
			t.Fatal(err)
		}
	}

	s.fireDueSchedules(now)

	if len(runner.enqueued) != 2 {
		t.Fatalf("%d turns enqueued, want the two due schedules: %+v", len(runner.enqueued), runner.enqueued)
	}
	var notes []string
	for _, e := range runner.enqueued {
		if e.Source != sourceSchedule {
			t.Errorf("source = %q, want schedule", e.Source)
		}
		notes = append(notes, e.Text)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "check the deploy") || !strings.Contains(joined, "morning report") {
		t.Errorf("the notes did not reach the bot:\n%s", joined)
	}
	if strings.Contains(joined, "not yet") {
		t.Error("a future schedule fired early")
	}

	var firedOnce Schedule
	in.db.First(&firedOnce, once.ID)
	if firedOnce.FiredAt == nil {
		t.Error("the one-shot schedule was not retired")
	}
	var recurring Schedule
	in.db.First(&recurring, daily.ID)
	if recurring.FiredAt != nil {
		t.Error("a recurring schedule must not be retired")
	}
	if !recurring.FireAt.After(now) {
		t.Errorf("the recurring schedule did not roll forward: %s", recurring.FireAt)
	}

	// Firing again changes nothing: the due ones are gone or moved.
	before := len(runner.enqueued)
	s.fireDueSchedules(now)
	if len(runner.enqueued) != before {
		t.Error("a schedule fired twice for the same due time")
	}
}

func TestParseCron(t *testing.T) {
	for _, good := range []string{"0 9 * * *", "*/15 * * * *", "@daily", "@every 1h"} {
		if _, err := parseCron(good); err != nil {
			t.Errorf("parseCron(%q) failed: %v", good, err)
		}
	}
	for _, bad := range []string{"", "not a cron", "99 99 99 99 99"} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("parseCron(%q) should have failed", bad)
		}
	}
}

func TestLineDiffIsTruncated(t *testing.T) {
	before := ""
	var after strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&after, "line %d\n", i)
	}
	diff := lineDiff(before, after.String(), 512)
	if len(diff) > 700 {
		t.Errorf("diff is %d bytes, want it capped near 512", len(diff))
	}
	if !strings.Contains(diff, "truncated") {
		t.Errorf("a truncated diff should say so: %q", diff)
	}
}

func TestLineDiffReportsBothSides(t *testing.T) {
	diff := lineDiff("a\nb\nc\n", "a\nc\nd\n", 4096)
	if !strings.Contains(diff, "- b") {
		t.Errorf("a removed line is missing: %q", diff)
	}
	if !strings.Contains(diff, "+ d") {
		t.Errorf("an added line is missing: %q", diff)
	}
	if strings.Contains(diff, "a") && strings.Contains(diff, "- a") {
		t.Errorf("an unchanged line was reported: %q", diff)
	}
}

// spawn_bot creates a real topic and a real bot, and the child's send_to_bot
// comes back to the parent as a turn.
func TestSpawnBotAndChildReportsBack(t *testing.T) {
	in, _, api := testInstance(t)
	parent, err := in.createBot("lead", "coordinates")
	if err != nil {
		t.Fatal(err)
	}
	topicsBefore := len(api.since("createForumTopic"))

	s := &mcpServer{db: in.db, config: in.cfg, botID: parent.ID}
	res, _, err := s.spawnBot(t.Context(), nil, spawnBotIn{
		Name: "helper", Role: "runs the tests", FirstMessage: "run go test and tell me the result",
	})
	if err != nil {
		t.Fatalf("spawnBot: %v", err)
	}
	if res.IsError {
		t.Fatalf("spawnBot reported an error: %+v", res.Content)
	}

	if got := len(api.since("createForumTopic")) - topicsBefore; got != 1 {
		t.Fatalf("%d topics created, want 1", got)
	}
	child, err := botByName(in.db, "helper")
	if err != nil {
		t.Fatalf("the child bot was not created: %v", err)
	}
	if child.ParentBotID == nil || *child.ParentBotID != parent.ID {
		t.Errorf("the child is not linked to its parent: %+v", child.ParentBotID)
	}
	if child.TopicID == 0 {
		t.Error("the child has no topic")
	}

	// The first message is queued as an inbox row from the parent, which the
	// runner turns into the child's first turn once the parent's turn ends.
	runner := newRunner(in.db, in.cfg, noopTestUI{})
	runner.Close() // no turn loop may actually run: this test never spawns claude
	// The bots are disabled for the same reason — deliverInbox only writes.
	in.db.Model(&Bot{}).Update("status", botDisabled)
	runner.deliverInbox(parent.ID)

	var firstTurn Turn
	if err := in.db.Where("bot_id = ? AND status = ?", child.ID, turnQueued).First(&firstTurn).Error; err != nil {
		t.Fatalf("the child never got its first message: %v", err)
	}
	if !strings.Contains(firstTurn.Input, "run go test") || !strings.Contains(firstTurn.Input, "lead") {
		t.Errorf("the first turn does not carry the message and its sender: %q", firstTurn.Input)
	}

	// Now the child reports back.
	childServer := &mcpServer{db: in.db, config: in.cfg, botID: child.ID}
	res, _, err = childServer.sendToBot(t.Context(), nil, sendToBotIn{Bot: "lead", Text: "all 42 tests pass"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the child could not message its parent: %+v", res.Content)
	}
	runner.deliverInbox(child.ID)

	var reply Turn
	if err := in.db.Where("bot_id = ? AND status = ?", parent.ID, turnQueued).First(&reply).Error; err != nil {
		t.Fatalf("the parent never received the report: %v", err)
	}
	if !strings.Contains(reply.Input, "all 42 tests pass") || !strings.Contains(reply.Input, "helper") {
		t.Errorf("the report is wrong: %q", reply.Input)
	}
	if reply.Source != sourceBot {
		t.Errorf("source = %q, want bot", reply.Source)
	}

	// Both topics saw the exchange.
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "spawned") || !strings.Contains(joined, "all 42 tests pass") {
		t.Errorf("the group did not see the collaboration:\n%s", joined)
	}
}

func TestArchiveBotClosesTheTopicAndStopsAutomation(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("done", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "x", "echo hi", 60); err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Schedule{BotID: b.ID, FireAt: time.Now().Add(time.Hour), Note: "later"}).Error; err != nil {
		t.Fatal(err)
	}

	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	res, _, err := s.archiveBot(t.Context(), nil, archiveBotIn{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("archiveBot failed: %+v", res.Content)
	}

	if _, err := botByName(in.db, "done"); err == nil {
		t.Error("the bot is still live after being archived")
	}
	if len(api.since("closeForumTopic")) != 1 {
		t.Error("the topic was not closed")
	}
	var w Watch
	in.db.Where("bot_id = ?", b.ID).First(&w)
	if w.Enabled {
		t.Error("the watch survived the archive")
	}
	var pending int64
	in.db.Model(&Schedule{}).Where("bot_id = ? AND fired_at IS NULL", b.ID).Count(&pending)
	if pending != 0 {
		t.Error("a pending schedule survived the archive")
	}
}

func TestScheduleWakeupToolValidatesItsInput(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("sleeper", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}

	if res, _, _ := s.scheduleWakeup(t.Context(), nil, scheduleIn{Note: "nothing"}); !res.IsError {
		t.Error("a wakeup with no time should be refused")
	}
	if res, _, _ := s.scheduleWakeup(t.Context(), nil, scheduleIn{At: "tomorrow", Note: "x"}); !res.IsError {
		t.Error("a non-RFC3339 time should be refused")
	}
	if res, _, _ := s.scheduleWakeup(t.Context(), nil, scheduleIn{Cron: "every day", Note: "x"}); !res.IsError {
		t.Error("an unparseable cron should be refused")
	}
	if res, _, _ := s.scheduleWakeup(t.Context(), nil, scheduleIn{InSeconds: 100 * 365 * 24 * 3600, Note: "x"}); !res.IsError {
		t.Error("a wakeup a century away should be refused")
	}

	res, _, err := s.scheduleWakeup(t.Context(), nil, scheduleIn{InSeconds: 3600, Note: "check the build"})
	if err != nil || res.IsError {
		t.Fatalf("a valid wakeup was refused: %+v", res)
	}
	rows, err := listSchedules(in.db, b.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%d schedules stored, want 1 (%v)", len(rows), err)
	}

	// A bot can only cancel its own wakeups.
	other, err := in.createBot("other", "")
	if err != nil {
		t.Fatal(err)
	}
	otherServer := &mcpServer{db: in.db, config: in.cfg, botID: other.ID}
	if res, _, _ := otherServer.cancelSchedule(t.Context(), nil, cancelScheduleIn{ID: rows[0].ID}); res.IsError {
		t.Error("cancelling someone else's schedule should report 'not yours', not error")
	}
	if left, _ := listSchedules(in.db, b.ID); len(left) != 1 {
		t.Error("another bot cancelled this bot's schedule")
	}
}

func TestProjectRegistryRoundTrip(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("coder", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}

	res, _, err := s.getProject(t.Context(), nil, getProjectIn{Path: "/srv/app"})
	if err != nil || res.IsError {
		t.Fatalf("get_project on an unknown path should not error: %+v", res)
	}

	if _, _, err := s.setProject(t.Context(), nil, setProjectIn{
		Path: "/srv/app", Name: "app", Stack: "Go + SQLite", DeployNotes: "systemd on vps3",
	}); err != nil {
		t.Fatal(err)
	}
	// A partial update must not blank the other fields.
	if _, _, err := s.setProject(t.Context(), nil, setProjectIn{Path: "/srv/app", Description: "the thing"}); err != nil {
		t.Fatal(err)
	}

	res, _, err = s.getProject(t.Context(), nil, getProjectIn{Path: "/srv/app"})
	if err != nil {
		t.Fatal(err)
	}
	body := toolText(res)
	for _, want := range []string{"Go + SQLite", "systemd on vps3", "the thing"} {
		if !strings.Contains(body, want) {
			t.Errorf("get_project lost %q:\n%s", want, body)
		}
	}
}

// noopTestUI is a botUI that does nothing, for tests that build a real Runner
// only to exercise its database side.
type noopTestUI struct{}

func (noopTestUI) Post(int64, string) (int64, error) { return 0, nil }
func (noopTestUI) Edit(int64, int64, string) error   { return nil }
func (noopTestUI) React(int64, string)               {}
