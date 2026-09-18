package main

import (
	"html"
	"regexp"
)

var tagRe = regexp.MustCompile(`<[^>]+>`)

func stripTags(s string) string {
	return tagRe.ReplaceAllString(s, "")
}

// muxUI fans runner progress out to Telegram and to paired phones. The hub
// path is best-effort: a missing client never blocks a turn.

type muxUI struct {
	tg  telegramUI
	hub *hubClient
}

func (m muxUI) Post(topicID int64, htmlBody string) (int64, error) {
	id, err := m.tg.Post(topicID, htmlBody)
	m.emit("post", topicID, htmlBody)
	return id, err
}

func (m muxUI) PostSilent(topicID int64, htmlBody string) (int64, error) {
	id, err := m.tg.PostSilent(topicID, htmlBody)
	m.emit("progress", topicID, htmlBody)
	return id, err
}

func (m muxUI) Edit(topicID, msgID int64, htmlBody string) error {
	err := m.tg.Edit(topicID, msgID, htmlBody)
	m.emit("progress", topicID, htmlBody)
	return err
}

func (m muxUI) React(messageID int64, emoji string) {
	m.tg.React(messageID, emoji)
}

func (m muxUI) Delete(topicID, msgID int64) error {
	if d, ok := any(m.tg).(messageDeleter); ok {
		return d.Delete(topicID, msgID)
	}
	return nil
}

func (m muxUI) emit(kind string, topicID int64, htmlBody string) {
	if m.hub == nil {
		return
	}
	b, err := botByTopic(m.hub.in.db, topicID)
	if err != nil {
		return
	}
	m.hub.pushEvent(kind, map[string]any{
		"bot_id":  b.ID,
		"bot":     b.Name,
		"general": isGeneralBot(b),
		"text":    html.UnescapeString(stripTags(htmlBody)),
	})
}
