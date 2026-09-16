package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// General is the persistent dispatcher session that lives in the forum group's
// root topic (Telegram thread id 0). It sees the roster, can spawn and tell
// sessions, and is the only place sessions may report back to.
const generalBotName = "General"

func isGeneralBot(b *Bot) bool {
	return b != nil && b.TopicID == 0
}

// chiefTurnTimeout caps one General turn. Workers have no such cap. Tests may
// shorten it so they do not wait 30s.
var chiefTurnTimeout = 30 * time.Second

const (
	chiefTimeoutClass  = "chief_timeout"
	idleRemindInterval = 10 * time.Minute
)

func chiefTimeoutFor(b *Bot) time.Duration {
	if !isGeneralBot(b) {
		return 0
	}
	return chiefTurnTimeout
}

// chiefTimeoutInput is injected as the next General turn when the 30s cap
// fires. It is an error the dispatcher must act on, not a silent kill.
func chiefTimeoutInput() string {
	return "Error: this work is too long for General (30s cap). " +
		"You MUST pass it to a session with spawn_session (new) or tell_session (existing). " +
		"Do not continue the work yourself."
}

func isChiefTimeoutFollowUp(input string) bool {
	return strings.Contains(input, "too long for General")
}

func idleRemindText(name string) string {
	return fmt.Sprintf("⏳ «%s» sigue esperando que hagas algo. Responde en su tema, dime qué hacer, o archívala.", name)
}

func generalBot(db *gorm.DB) (*Bot, error) {
	var b Bot
	err := db.Where("topic_id = ? AND archived_at IS NULL", int64(0)).Order("id").First(&b).Error
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ensureGeneralBot returns the dispatcher row, creating it if needed. It does
// not create a forum topic: General already exists as the group root.
func (in *instance) ensureGeneralBot() (*Bot, error) {
	return ensureGeneralBotRow(in.db, in.config())
}

func ensureGeneralBotRow(db *gorm.DB, cfg *Config) (*Bot, error) {
	if b, err := generalBot(db); err == nil {
		return b, nil
	}
	cwd := botWorkspace(cfg, generalBotName)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, err
	}
	name := uniqueBotName(db, generalBotName)
	b := &Bot{
		Name:    name,
		TopicID: 0,
		Cwd:     cwd,
		Engine:  defaultEngine(cfg),
		Status:  botIdle,
	}
	if err := db.Create(b).Error; err != nil {
		if existing, err2 := generalBot(db); err2 == nil {
			return existing, nil
		}
		return nil, err
	}
	return b, nil
}

// sessionLine is one live worker for the chief's envelope / list_sessions.
type sessionLine struct {
	Name   string
	Status string
	Engine string
	Age    string
	Last   string
}

func sessionRoster(db *gorm.DB, exceptID int64) []sessionLine {
	bots, err := liveBots(db)
	if err != nil {
		return nil
	}
	out := make([]sessionLine, 0, len(bots))
	for i := range bots {
		b := &bots[i]
		if b.ID == exceptID || isGeneralBot(b) {
			continue
		}
		line := sessionLine{Name: b.Name, Status: b.Status, Engine: botEngine(b)}
		var last Turn
		q := db.Where("bot_id = ? AND status = ?", b.ID, turnDone).Order("id DESC")
		if q.First(&last).Error == nil {
			line.Last = truncate(collapseWhitespace(last.Output), 120)
			if last.EndedAt != nil {
				line.Age = humanDuration(time.Since(*last.EndedAt))
			} else {
				line.Age = humanDuration(time.Since(last.CreatedAt))
			}
		}
		out = append(out, line)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func formatSessionRoster(lines []sessionLine) string {
	if len(lines) == 0 {
		return "no live sessions"
	}
	var sb strings.Builder
	for _, l := range lines {
		engine := l.Engine
		if engine == engineClaude {
			engine = ""
		}
		fmt.Fprintf(&sb, "%s [%s]", l.Name, l.Status)
		if engine != "" {
			fmt.Fprintf(&sb, " %s", engine)
		}
		if l.Age != "" {
			fmt.Fprintf(&sb, " · last %s ago", l.Age)
		}
		if l.Last != "" {
			fmt.Fprintf(&sb, " — %s", l.Last)
		}
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}
