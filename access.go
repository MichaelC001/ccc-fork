package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Access control (DESIGN §8 "Access control", §12).
//
// Bots run with bypass permissions on the owner's machine, so the chat IS the
// trust boundary: this file is what stops anyone who finds the bot from
// getting a shell. The policy is default-deny by Telegram user id — the owner
// from config.chat_id is always allowed, everybody else must be `approved` in
// the access table before a single update from them is looked at, in a group
// or in a DM, message or callback or edit.
//
// Pairing exists so the owner can let somebody in without touching the server:
// an unknown DM gets one 6-hex code, and the OWNER turns it into access with
// `/access pair <code>`. A code is never approved because a message asked for
// it — approval only ever comes from the owner's own chat.

// Access states.
const (
	accessPending  = "pending"
	accessApproved = "approved"
	accessBlocked  = "blocked"
)

const (
	// pairCodeTTL is how long a pairing code stays usable.
	pairCodeTTL = time.Hour
	// maxPendingPairs caps the pending queue: past it, unknown users get
	// nothing at all, so the bot cannot be used to spam the owner.
	maxPendingPairs = 3
	// maxPairReplies is how many times ccc will answer the same stranger
	// before going silent for good.
	maxPairReplies = 2
)

// accessRole is what the gate decided about an inbound update.
type accessRole int

const (
	roleDenied accessRole = iota // drop the update
	roleUser                     // approved: may talk to bots
	roleOwner                    // the owner: may also run the owner commands
)

// classifyAccess is the whole policy, as a pure-ish function over the access
// table. It never writes; pairing side effects live in handleUnknownUser.
func classifyAccess(db *gorm.DB, config *Config, userID int64) accessRole {
	if userID == 0 {
		return roleDenied // a Telegram update with no sender is not something to act on
	}
	if config != nil && config.ChatID != 0 && userID == config.ChatID {
		return roleOwner
	}
	if config == nil || config.ChatID == 0 {
		// Not bootstrapped yet: nobody is the owner, so nobody is allowed.
		// `ccc config set chat_id <id>` (or `ccc setup`) is what opens the door.
		return roleDenied
	}
	var row Access
	if err := db.First(&row, "telegram_user_id = ?", userID).Error; err != nil {
		return roleDenied
	}
	if row.State == accessApproved {
		return roleUser
	}
	return roleDenied
}

// newPairCode mints a 6-hex-character pairing code.
func newPairCode() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A predictable code is still harmless: it is useless without the
		// owner typing it, and it expires in an hour.
		n := time.Now().UnixNano()
		b[0], b[1], b[2] = byte(n), byte(n>>8), byte(n>>16)
	}
	return hex.EncodeToString(b[:])
}

// pendingPairCount counts the unexpired pending rows.
func pendingPairCount(db *gorm.DB, now time.Time) int64 {
	var n int64
	db.Model(&Access{}).Where("state = ? AND (code_expires_at IS NULL OR code_expires_at > ?)",
		accessPending, now).Count(&n)
	return n
}

// pairingOutcome is what handleUnknownUser decided to do about a stranger.
type pairingOutcome struct {
	Reply string // "" = say nothing at all
	Code  string // "" = no new code was minted
}

// handleUnknownUser runs the pairing state machine for one message from a
// non-approved user in a DM. Group messages never reach it: an unknown user in
// the group is dropped in silence, because answering there would let anyone who
// finds the group make the bot talk.
func handleUnknownUser(db *gorm.DB, userID int64, display string, now time.Time) pairingOutcome {
	var row Access
	err := db.First(&row, "telegram_user_id = ?", userID).Error
	switch {
	case err == nil && row.State == accessBlocked:
		return pairingOutcome{} // blocked users get silence, forever
	case err == nil:
		// Known pending user. Refresh an expired code, but never answer more
		// than maxPairReplies times in total.
		if row.Replies >= maxPairReplies {
			return pairingOutcome{}
		}
		code := row.PairCode
		expires := now.Add(pairCodeTTL)
		if code == "" || row.CodeExpiresAt == nil || row.CodeExpiresAt.Before(now) {
			code = newPairCode()
		} else if row.CodeExpiresAt != nil {
			expires = *row.CodeExpiresAt
		}
		db.Model(&Access{}).Where("telegram_user_id = ?", userID).Updates(map[string]any{
			"display": display, "pair_code": code, "code_expires_at": expires, "replies": row.Replies + 1,
		})
		return pairingOutcome{Reply: pairingReply(code), Code: code}
	}

	// Brand new user.
	if pendingPairCount(db, now) >= maxPendingPairs {
		return pairingOutcome{} // the queue is full: silence, no row, no code
	}
	code := newPairCode()
	expires := now.Add(pairCodeTTL)
	row = Access{
		TelegramUserID: userID,
		Display:        display,
		State:          accessPending,
		PairCode:       code,
		CodeExpiresAt:  &expires,
		Replies:        1,
	}
	if err := db.Create(&row).Error; err != nil {
		return pairingOutcome{}
	}
	return pairingOutcome{Reply: pairingReply(code), Code: code}
}

func pairingReply(code string) string {
	return "This bot is private. Give its owner this code to be let in:\n<code>" +
		htmlEscape(code) + "</code>\nIt expires in an hour."
}

// approveByCode is the owner's `/access pair <code>`.
func approveByCode(db *gorm.DB, code string, now time.Time) (*Access, error) {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" {
		return nil, fmt.Errorf("give me the code: /access pair <code>")
	}
	var row Access
	err := db.Where("pair_code = ? AND state = ?", code, accessPending).First(&row).Error
	if err != nil {
		return nil, fmt.Errorf("no pending request with code %s", code)
	}
	if row.CodeExpiresAt != nil && row.CodeExpiresAt.Before(now) {
		return nil, fmt.Errorf("code %s has expired; ask them to message the bot again", code)
	}
	if err := setAccessState(db, row.TelegramUserID, row.Display, accessApproved); err != nil {
		return nil, err
	}
	row.State = accessApproved
	return &row, nil
}

// setAccessState upserts one access row.
func setAccessState(db *gorm.DB, userID int64, display, state string) error {
	updates := map[string]any{"state": state, "pair_code": "", "code_expires_at": nil, "replies": 0}
	if display != "" {
		updates["display"] = display
	}
	res := db.Model(&Access{}).Where("telegram_user_id = ?", userID).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	return db.Create(&Access{TelegramUserID: userID, Display: display, State: state}).Error
}

func revokeAccess(db *gorm.DB, userID int64) (bool, error) {
	res := db.Where("telegram_user_id = ?", userID).Delete(&Access{})
	return res.RowsAffected > 0, res.Error
}

// ---------------------------------------------------------------------------
// Telegram surface
// ---------------------------------------------------------------------------

// gate is the single entry point every inbound update goes through. It returns
// the sender's role and, for a denied DM, posts at most the pairing reply.
func (in *instance) gate(userID int64, display string, chatID int64, isDM bool) accessRole {
	cfg := in.config()
	role := classifyAccess(in.db, cfg, userID)
	if role != roleDenied {
		return role
	}
	if !isDM {
		return roleDenied // silence in the group; see handleUnknownUser
	}
	out := handleUnknownUser(in.db, userID, display, time.Now())
	if out.Reply == "" {
		return roleDenied
	}
	if cfg.BotToken != "" && chatID != 0 {
		_, _ = sendMessageHTMLGetID(cfg, chatID, 0, out.Reply) // safe-ignore: a failed pairing reply changes nothing about the deny
	}
	in.notifyOwnerOfPairing(userID, display, out.Code)
	return roleDenied
}

// notifyOwnerOfPairing tells the owner somebody is knocking, with a button that
// approves them. The button is the ONLY shortcut: the request itself can never
// approve anything.
func (in *instance) notifyOwnerOfPairing(userID int64, display, code string) {
	cfg := in.config()
	if cfg.BotToken == "" || cfg.ChatID == 0 || code == "" {
		return
	}
	who := display
	if who == "" {
		who = fmt.Sprint(userID)
	}
	body := fmt.Sprintf("🔐 <b>%s</b> (<code>%d</code>) wants access.\nCode <code>%s</code> — expires in an hour.",
		htmlEscape(who), userID, htmlEscape(code))
	buttons := [][]InlineKeyboardButton{{
		{Text: "✅ Allow", CallbackData: "access:pair:" + code},
		{Text: "🚫 Block", CallbackData: "access:block:" + fmt.Sprint(userID)},
	}}
	_, _ = sendMessageKeyboardGetID(cfg, cfg.ChatID, 0, body, buttons) // safe-ignore: the owner can still use /access pair
}

// handleAccessCommand implements `/access …` (owner only; the caller checks).
func (in *instance) handleAccessCommand(msg *TelegramMessage, rest string) {
	sub, arg := splitFirstWord(rest)
	switch strings.ToLower(sub) {
	case "", "list":
		in.reply(msg, in.renderAccessList())

	case "pair":
		row, err := approveByCode(in.db, arg, time.Now())
		if err != nil {
			in.reply(msg, "❌ "+htmlEscape(err.Error()))
			return
		}
		in.reply(msg, fmt.Sprintf("✅ Allowed <b>%s</b> (<code>%d</code>).", htmlEscape(row.Display), row.TelegramUserID))
		in.tellUser(row.TelegramUserID, "✅ You have been allowed in. Talk to the bots in the group.")

	case "add":
		id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
		if err != nil {
			in.reply(msg, "Usage: /access add &lt;telegram user id&gt;")
			return
		}
		if err := setAccessState(in.db, id, "", accessApproved); err != nil {
			in.reply(msg, "❌ "+htmlEscape(err.Error()))
			return
		}
		in.reply(msg, fmt.Sprintf("✅ Allowed <code>%d</code>.", id))

	case "remove":
		id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
		if err != nil {
			in.reply(msg, "Usage: /access remove &lt;telegram user id&gt;")
			return
		}
		removed, err := revokeAccess(in.db, id)
		if err != nil {
			in.reply(msg, "❌ "+htmlEscape(err.Error()))
			return
		}
		if !removed {
			in.reply(msg, "Nothing to remove.")
			return
		}
		in.reply(msg, fmt.Sprintf("🗑 Removed <code>%d</code>.", id))

	case "block":
		id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
		if err != nil {
			in.reply(msg, "Usage: /access block &lt;telegram user id&gt;")
			return
		}
		if err := setAccessState(in.db, id, "", accessBlocked); err != nil {
			in.reply(msg, "❌ "+htmlEscape(err.Error()))
			return
		}
		in.reply(msg, fmt.Sprintf("🚫 Blocked <code>%d</code>.", id))

	default:
		in.reply(msg, "Usage: /access [list] · /access pair &lt;code&gt; · /access add|remove|block &lt;id&gt;")
	}
}

func (in *instance) renderAccessList() string {
	var rows []Access
	if err := in.db.Order("state, telegram_user_id").Find(&rows).Error; err != nil {
		return "Could not read the access list: " + htmlEscape(err.Error())
	}
	cfg := in.config()
	var sb strings.Builder
	sb.WriteString("<b>Access</b>\n")
	fmt.Fprintf(&sb, "• owner <code>%d</code>\n", cfg.ChatID)
	if len(rows) == 0 {
		sb.WriteString("<i>Nobody else. Unknown users who DM the bot get a pairing code.</i>")
		return sb.String()
	}
	now := time.Now()
	sort.Slice(rows, func(i, j int) bool { return rows[i].State < rows[j].State })
	for _, r := range rows {
		who := r.Display
		if who == "" {
			who = "(unnamed)"
		}
		extra := ""
		if r.State == accessPending {
			extra = " code " + r.PairCode
			if r.CodeExpiresAt != nil && r.CodeExpiresAt.Before(now) {
				extra += " (expired)"
			}
		}
		fmt.Fprintf(&sb, "• %s <code>%d</code> — %s%s\n", htmlEscape(who), r.TelegramUserID, r.State, htmlEscape(extra))
	}
	return sb.String()
}

// handleAccessCallback answers the Allow/Block buttons from the owner's DM.
func (in *instance) handleAccessCallback(cb *CallbackQuery, parts []string) {
	if len(parts) < 3 {
		return
	}
	switch parts[1] {
	case "pair":
		row, err := approveByCode(in.db, parts[2], time.Now())
		if err != nil {
			in.editCallbackMessage(cb, "❌ "+htmlEscape(err.Error()))
			return
		}
		in.editCallbackMessage(cb, fmt.Sprintf("✅ Allowed <b>%s</b> (<code>%d</code>).",
			htmlEscape(row.Display), row.TelegramUserID))
		in.tellUser(row.TelegramUserID, "✅ You have been allowed in. Talk to the bots in the group.")
	case "block":
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return
		}
		if err := setAccessState(in.db, id, "", accessBlocked); err != nil {
			in.editCallbackMessage(cb, "❌ "+htmlEscape(err.Error()))
			return
		}
		in.editCallbackMessage(cb, fmt.Sprintf("🚫 Blocked <code>%d</code>.", id))
	}
}

// tellUser sends a one-off DM to a user id (used to confirm an approval).
func (in *instance) tellUser(userID int64, html string) {
	cfg := in.config()
	if cfg.BotToken == "" || userID == 0 {
		return
	}
	_, _ = sendMessageHTMLGetID(cfg, userID, 0, html) // safe-ignore: the user may never have started a DM with the bot
}

// editCallbackMessage rewrites the message a button lived on, which both shows
// the result and retires the buttons.
func (in *instance) editCallbackMessage(cb *CallbackQuery, html string) {
	cfg := in.config()
	if cb.Message == nil || cfg.BotToken == "" {
		return
	}
	_ = editMessageHTML(cfg, cb.Message.Chat.ID, int64(cb.Message.MessageID), cb.Message.MessageThreadID, html) // safe-ignore: cosmetic
}
