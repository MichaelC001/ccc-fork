package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// usage.go is `/usage` (DESIGN §14.19): what the bots have actually spent,
// read back out of the `usage` object each `claude -p` result carries and the
// runner stores on the turn (turns.usage_json).
//
// The number worth watching is the cache hit ratio. A resumed turn re-sends the
// whole conversation, so almost every input token SHOULD come back as a cache
// read; a ratio that falls means something is changing the prefix of the
// request between turns and the conversation is being re-charged in full.

// turnUsage is the subset of a result's usage object ccc reads. The cost field
// is ccc's own (mergeUsage folds the result's total_cost_usd in), and every
// field is optional: old rows and a Claude Code that stops reporting one simply
// contribute zero.
type turnUsage struct {
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_input_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_input_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	TotalCostUSD        float64 `json:"total_cost_usd"`
}

// cost is whichever cost field this row carries.
func (u turnUsage) cost() float64 {
	if u.CostUSD > 0 {
		return u.CostUSD
	}
	return u.TotalCostUSD
}

// usageStats is one row of /usage: a bot, or the total.
type usageStats struct {
	Bot           string
	Turns         int
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
	CostUSD       float64
	TotalDuration time.Duration
	// Timed is how many turns had both timestamps, so the average is over the
	// turns it can actually be computed for.
	Timed int
}

// CacheHitRatio is cache_read / (cache_read + input): the share of the prompt
// that did not have to be re-read. Nothing measured yet returns 0.
func (u usageStats) CacheHitRatio() float64 {
	total := u.CacheRead + u.Input
	if total == 0 {
		return 0
	}
	return float64(u.CacheRead) / float64(total)
}

func (u usageStats) AvgDuration() time.Duration {
	if u.Timed == 0 {
		return 0
	}
	return u.TotalDuration / time.Duration(u.Timed)
}

func (u *usageStats) add(other usageStats) {
	u.Turns += other.Turns
	u.Input += other.Input
	u.Output += other.Output
	u.CacheRead += other.CacheRead
	u.CacheCreation += other.CacheCreation
	u.CostUSD += other.CostUSD
	u.TotalDuration += other.TotalDuration
	u.Timed += other.Timed
}

// aggregateUsage sums the turns that ran since a point in time, per bot and in
// total. Only turns that actually started count: a queued input folded into
// another turn (foldQueue) is bookkeeping, not a run, and counting it would
// make the "turns" column disagree with what was spent.
func aggregateUsage(db *gorm.DB, since time.Time) ([]usageStats, usageStats, error) {
	type row struct {
		BotID     int64
		Name      string
		UsageJSON string
		StartedAt *time.Time
		EndedAt   *time.Time
	}
	var rows []row
	err := db.Model(&Turn{}).
		Select("turns.bot_id, bots.name, turns.usage_json, turns.started_at, turns.ended_at").
		Joins("LEFT JOIN bots ON bots.id = turns.bot_id").
		Where("turns.started_at IS NOT NULL AND turns.created_at >= ?", since).
		Order("turns.id").Scan(&rows).Error
	if err != nil {
		return nil, usageStats{}, err
	}

	byBot := map[string]*usageStats{}
	var total usageStats
	for _, r := range rows {
		name := r.Name
		if name == "" {
			name = fmt.Sprintf("bot %d", r.BotID)
		}
		one := usageStats{Bot: name, Turns: 1}
		if r.UsageJSON != "" {
			var u turnUsage
			if err := json.Unmarshal([]byte(r.UsageJSON), &u); err == nil {
				one.Input = u.InputTokens
				one.Output = u.OutputTokens
				one.CacheRead = u.CacheReadTokens
				one.CacheCreation = u.CacheCreationTokens
				one.CostUSD = u.cost()
			}
		}
		if r.StartedAt != nil && r.EndedAt != nil && !r.EndedAt.Before(*r.StartedAt) {
			one.TotalDuration = r.EndedAt.Sub(*r.StartedAt)
			one.Timed = 1
		}
		if acc, ok := byBot[name]; ok {
			acc.add(one)
		} else {
			cp := one
			byBot[name] = &cp
		}
		total.add(one)
	}

	out := make([]usageStats, 0, len(byBot))
	for _, s := range byBot {
		out = append(out, *s)
	}
	// Biggest spender first; by name when two bots ran the same number of turns,
	// so the card is stable between calls.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Turns != out[j].Turns {
			return out[i].Turns > out[j].Turns
		}
		return out[i].Bot < out[j].Bot
	})
	total.Bot = "total"
	return out, total, nil
}

// renderUsage is the /usage card: today and the last seven days.
func renderUsage(db *gorm.DB, now time.Time) string {
	var sb strings.Builder
	sb.WriteString("📊 <b>Usage</b>\n")
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for _, period := range []struct {
		Title string
		Since time.Time
	}{
		{"Today", midnight},
		{"Last 7 days", midnight.AddDate(0, 0, -6)},
	} {
		perBot, total, err := aggregateUsage(db, period.Since)
		if err != nil {
			return "Could not read the usage: " + htmlEscape(err.Error())
		}
		fmt.Fprintf(&sb, "\n<b>%s</b>\n", period.Title)
		if total.Turns == 0 {
			sb.WriteString("  no turns\n")
			continue
		}
		for _, s := range perBot {
			fmt.Fprintf(&sb, "  %s\n", usageLine(s))
		}
		if len(perBot) > 1 {
			fmt.Fprintf(&sb, "  %s\n", usageLine(total))
		}
	}
	return sb.String()
}

// usageLine is one bot's numbers, short enough to read on a phone.
func usageLine(s usageStats) string {
	line := fmt.Sprintf("<b>%s</b> · %d turn%s · %s in / %s out · cache %.0f%% · avg %s",
		htmlEscape(s.Bot), s.Turns, plural(s.Turns), humanTokens(s.Input), humanTokens(s.Output),
		s.CacheHitRatio()*100, humanDuration(s.AvgDuration()))
	if s.CacheCreation > 0 {
		line += fmt.Sprintf(" · %s cached", humanTokens(s.CacheCreation))
	}
	if s.CostUSD > 0 {
		line += fmt.Sprintf(" · $%.2f", s.CostUSD)
	}
	return line
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// humanTokens keeps a token count to three or four characters.
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprint(n)
	}
}
