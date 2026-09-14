package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// usageTurn seeds one finished turn with a usage blob.
func usageTurn(t *testing.T, in *instance, botID int64, at time.Time, dur time.Duration, usage string) {
	t.Helper()
	started := at
	ended := at.Add(dur)
	row := &Turn{
		BotID: botID, Source: sourceUser, Status: turnDone, CreatedAt: at,
		StartedAt: &started, EndedAt: &ended, UsageJSON: usage,
	}
	if err := in.db.Create(row).Error; err != nil {
		t.Fatalf("seed turn: %v", err)
	}
}

func TestAggregateUsageSumsPerBotAndTotal(t *testing.T) {
	in, _, _ := testInstance(t)
	now := time.Now()
	a, err := in.createBot("alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("beta", "")
	if err != nil {
		t.Fatal(err)
	}

	usageTurn(t, in, a.ID, now.Add(-time.Hour), 20*time.Second,
		`{"input_tokens":1000,"output_tokens":200,"cache_read_input_tokens":9000,"cache_creation_input_tokens":500,"cost_usd":0.25}`)
	usageTurn(t, in, a.ID, now.Add(-30*time.Minute), 40*time.Second,
		`{"input_tokens":1000,"output_tokens":300,"cache_read_input_tokens":11000,"cost_usd":0.35}`)
	usageTurn(t, in, b.ID, now.Add(-10*time.Minute), 10*time.Second,
		`{"input_tokens":500,"output_tokens":100}`)
	// A turn from before the window, and one that never ran (a folded input):
	// neither may contribute.
	usageTurn(t, in, b.ID, now.AddDate(0, 0, -10), time.Second, `{"input_tokens":999999}`)
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Status: turnDone, CreatedAt: now,
		StopReason: "merged into turn 1"}).Error; err != nil {
		t.Fatal(err)
	}

	perBot, total, err := aggregateUsage(in.db, now.AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(perBot) != 2 {
		t.Fatalf("per-bot rows = %+v, want two", perBot)
	}
	if perBot[0].Bot != "alpha" || perBot[0].Turns != 2 {
		t.Errorf("busiest bot first: %+v", perBot[0])
	}
	if perBot[0].Input != 2000 || perBot[0].Output != 500 || perBot[0].CacheRead != 20000 || perBot[0].CacheCreation != 500 {
		t.Errorf("alpha's tokens = %+v", perBot[0])
	}
	if got := perBot[0].CostUSD; got < 0.599 || got > 0.601 {
		t.Errorf("alpha's cost = %v, want 0.60", got)
	}
	if got := perBot[0].AvgDuration(); got != 30*time.Second {
		t.Errorf("alpha's average duration = %v, want 30s", got)
	}
	// 20000 cache reads against 2000 fresh input tokens.
	if got := perBot[0].CacheHitRatio(); got < 0.908 || got > 0.910 {
		t.Errorf("alpha's cache hit ratio = %v, want ~0.909", got)
	}

	if total.Turns != 3 || total.Input != 2500 || total.CacheRead != 20000 {
		t.Errorf("total = %+v, want the three turns inside the window", total)
	}
	if total.Bot != "total" {
		t.Errorf("total row is labelled %q", total.Bot)
	}
}

func TestAggregateUsageToleratesMissingAndBrokenUsage(t *testing.T) {
	in, b := testDB(t)
	now := time.Now()
	usageTurn(t, in, b.ID, now, time.Second, "")
	usageTurn(t, in, b.ID, now, time.Second, "not json at all")
	usageTurn(t, in, b.ID, now, time.Second, `{"input_tokens":10,"total_cost_usd":0.1}`)

	perBot, total, err := aggregateUsage(in.db, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(perBot) != 1 || total.Turns != 3 {
		t.Fatalf("turns without usage still count as turns: %+v", total)
	}
	if total.Input != 10 {
		t.Errorf("input = %d, want only the one readable row", total.Input)
	}
	// total_cost_usd is what older rows carry, before mergeUsage folded the
	// cost into the usage object under cost_usd.
	if got := total.CostUSD; got < 0.099 || got > 0.101 {
		t.Errorf("cost = %v, want the legacy total_cost_usd field to be read", got)
	}
	if total.CacheHitRatio() != 0 {
		t.Error("a ratio with no cache reads must be 0, not NaN")
	}
}

func TestRenderUsageCoversTodayAndTheWeek(t *testing.T) {
	in, b := testDB(t)
	now := time.Date(2026, 9, 14, 15, 0, 0, 0, time.Local)
	usageTurn(t, in, b.ID, now.Add(-time.Hour), 5*time.Second,
		`{"input_tokens":1000,"cache_read_input_tokens":3000,"cost_usd":0.5}`)
	usageTurn(t, in, b.ID, now.AddDate(0, 0, -3), 5*time.Second, `{"input_tokens":2000}`)

	card := renderUsage(in.db, now)
	for _, want := range []string{"Today", "Last 7 days", "tester", "cache 75%", "$0.50"} {
		if !strings.Contains(card, want) {
			t.Errorf("/usage is missing %q:\n%s", want, card)
		}
	}
}

func TestRenderUsageWithNoTurns(t *testing.T) {
	in, _ := testDB(t)
	if !strings.Contains(renderUsage(in.db, time.Now()), "no turns") {
		t.Error("an idle instance should say so rather than print an empty table")
	}
}

// The runner stores the result's cost beside its usage, so /usage can read one
// column per turn.
func TestMergeUsageFoldsTheCostIn(t *testing.T) {
	got := mergeUsage(json.RawMessage(`{"input_tokens":5}`), 0.125)
	var fields map[string]any
	if err := json.Unmarshal([]byte(got), &fields); err != nil {
		t.Fatalf("merged usage does not parse: %v (%s)", err, got)
	}
	if fields["cost_usd"] != 0.125 || fields["input_tokens"].(float64) != 5 {
		t.Errorf("merged usage = %s", got)
	}
	if mergeUsage(nil, 0) != "" {
		t.Error("nothing to store means an empty column")
	}
	if got := mergeUsage(json.RawMessage(`{"input_tokens":5}`), 0); got != `{"input_tokens":5}` {
		t.Errorf("a run with no cost must keep the usage verbatim: %s", got)
	}
	if got := mergeUsage(json.RawMessage(`not json`), 0.5); got != "not json" {
		t.Errorf("an unreadable usage blob is stored as-is: %s", got)
	}
}

func TestHumanTokens(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1500: "1.5k", 2_500_000: "2.5M"}
	for in, want := range cases {
		if got := humanTokens(in); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", in, got, want)
		}
	}
}
