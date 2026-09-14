package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// progress is the single per-turn Telegram message that is edited in place
// while the turn runs (DESIGN §3.2): current activity plus elapsed time, at
// most one edit every progressInterval, then replaced by the final text.

const progressInterval = 3 * time.Second

// telegramTextLimit is Telegram's hard per-message cap.
const telegramTextLimit = 4096

type progress struct {
	ui      botUI
	topicID int64
	started time.Time

	mu       sync.Mutex
	msgID    int64
	activity string
	lastEdit time.Time
	shown    string
	created  bool
	finished bool
}

func newProgress(ui botUI, topicID int64, started time.Time) *progress {
	return &progress{ui: ui, topicID: topicID, started: started}
}

// set records the current activity and flushes it if the rate limit allows.
func (p *progress) set(activity string) {
	if p == nil || p.ui == nil {
		return
	}
	p.mu.Lock()
	p.activity = activity
	p.mu.Unlock()
	p.flush(false)
}

func (p *progress) flush(force bool) {
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	if !force && p.created && now.Sub(p.lastEdit) < progressInterval {
		p.mu.Unlock()
		return
	}
	text := renderProgress(p.activity, now.Sub(p.started))
	if text == p.shown {
		p.mu.Unlock()
		return
	}
	p.shown = text
	p.lastEdit = now
	msgID := p.msgID
	created := p.created
	p.mu.Unlock()

	if !created {
		id, err := p.ui.Post(p.topicID, text)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.msgID = id
		p.created = true
		p.mu.Unlock()
		return
	}
	_ = p.ui.Edit(p.topicID, msgID, text) // safe-ignore: a failed progress edit is cosmetic
}

// finish replaces the progress message with the turn's final text, sending any
// overflow beyond one Telegram message as follow-ups.
func (p *progress) finish(final string) {
	if p == nil || p.ui == nil {
		return
	}
	final = strings.TrimSpace(final)
	if final == "" {
		final = "(no reply)"
	}
	chunks := splitMessage(final, telegramTextLimit-64)

	p.mu.Lock()
	p.finished = true
	msgID := p.msgID
	created := p.created
	p.mu.Unlock()

	if created {
		if err := p.ui.Edit(p.topicID, msgID, chunks[0]); err != nil {
			_, _ = p.ui.Post(p.topicID, chunks[0]) // safe-ignore: falling back to a new message; the edit already failed
		}
	} else {
		_, _ = p.ui.Post(p.topicID, chunks[0]) // safe-ignore: nothing to do if the topic itself is gone
	}
	for _, c := range chunks[1:] {
		_, _ = p.ui.Post(p.topicID, c) // safe-ignore: same
	}
}

func renderProgress(activity string, elapsed time.Duration) string {
	if activity == "" {
		activity = "working"
	}
	return fmt.Sprintf("⏳ <i>%s</i> · %s", htmlEscape(activity), humanDuration(elapsed))
}

func humanDuration(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
}
