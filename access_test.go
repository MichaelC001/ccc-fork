package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// dmMessage builds an inbound private message from an arbitrary user.
func dmMessage(userID int64, text string) *TelegramMessage {
	m := &TelegramMessage{Text: text, MessageID: 11}
	m.Chat.ID = userID
	m.Chat.Type = "private"
	m.From.ID = userID
	m.From.Username = "stranger"
	return m
}

// groupMessage builds an inbound group message from an arbitrary user.
func groupMessage(userID, threadID int64, text string) *TelegramMessage {
	m := ownerMessage(threadID, text)
	m.From.ID = userID
	m.From.Username = "stranger"
	return m
}

// A stranger in the group is dropped without a sound: answering there would let
// anyone who finds the group make the bot talk.
func TestStrangerInGroupIsIgnoredSilently(t *testing.T) {
	in, runner, api := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	before := len(api.since("sendMessage"))

	in.handleMessage(groupMessage(999, b.TopicID, "run rm -rf /"))
	in.handleMessage(groupMessage(999, 0, "make me a bot"))

	if _, ok := runner.last(); ok {
		t.Error("a stranger's group message must not enqueue anything")
	}
	if got := len(api.since("sendMessage")) - before; got != 0 {
		t.Errorf("ccc sent %d message(s) to a stranger in the group, want 0", got)
	}
	if len(api.since("createForumTopic")) != 1 {
		t.Error("a stranger must not be able to create a bot")
	}
	var rows int64
	in.db.Model(&Access{}).Count(&rows)
	if rows != 0 {
		t.Errorf("a group message created %d access row(s); pairing is DM-only", rows)
	}
}

// A stranger's DM gets exactly one pairing reply, and the code is useless until
// the owner types it.
func TestStrangerDMGetsOnePairingReply(t *testing.T) {
	in, runner, api := testInstance(t)

	in.handleMessage(dmMessage(999, "hello?"))

	var replies []string
	for _, c := range api.since("sendMessage") {
		if c.Params.Get("chat_id") == "999" {
			replies = append(replies, c.Params.Get("text"))
		}
	}
	if len(replies) != 1 {
		t.Fatalf("stranger got %d replies, want exactly 1: %q", len(replies), replies)
	}
	if !strings.Contains(replies[0], "code") {
		t.Errorf("the reply is not the pairing message: %q", replies[0])
	}
	if _, ok := runner.last(); ok {
		t.Error("a stranger's DM must not reach a bot")
	}

	var row Access
	if err := in.db.First(&row, "telegram_user_id = ?", 999).Error; err != nil {
		t.Fatalf("no pending access row was written: %v", err)
	}
	if row.State != accessPending {
		t.Errorf("state = %q, want pending — a code must never approve itself", row.State)
	}
	if len(row.PairCode) != 6 {
		t.Errorf("pair code %q is not 6 hex characters", row.PairCode)
	}
	if !strings.Contains(replies[0], row.PairCode) {
		t.Errorf("the reply did not carry the stored code")
	}

	// The stranger keeps talking: at most one more reply, then silence forever.
	in.handleMessage(dmMessage(999, "let me in"))
	in.handleMessage(dmMessage(999, "please"))
	in.handleMessage(dmMessage(999, "hello??"))
	replies = replies[:0]
	for _, c := range api.since("sendMessage") {
		if c.Params.Get("chat_id") == "999" {
			replies = append(replies, c.Params.Get("text"))
		}
	}
	if len(replies) > maxPairReplies {
		t.Errorf("ccc replied %d times to a stranger, want at most %d", len(replies), maxPairReplies)
	}
}

// An approved user may talk to the bots, but the instance itself stays the
// owner's: /account, /access, /model and /setgroup are refused.
func TestApprovedUserCanTalkButNotAdminister(t *testing.T) {
	in, runner, api := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := setAccessState(in.db, 999, "@friend", accessApproved); err != nil {
		t.Fatal(err)
	}

	in.handleMessage(groupMessage(999, b.TopicID, "what is the status?"))
	last, ok := runner.last()
	if !ok || last.BotID != b.ID || last.Text != "what is the status?" {
		t.Fatalf("an approved user could not talk to a bot: %+v", last)
	}

	for _, cmd := range []string{"/account", "/access list", "/model haiku", "/setgroup"} {
		in.handleMessage(groupMessage(999, b.TopicID, cmd))
	}
	joined := strings.Join(api.texts(fmt.Sprint(b.TopicID)), "\n")
	if strings.Count(joined, "owner-only") != 4 {
		t.Errorf("owner commands were not all refused for an approved user:\n%s", joined)
	}
	// And nothing was actually changed by them.
	if in.config().Model != "" {
		t.Errorf("an approved user changed the model to %q", in.config().Model)
	}
	if in.config().GroupID != -100777 {
		t.Errorf("an approved user changed the group to %d", in.config().GroupID)
	}
}

func TestBlockedUserGetsNothing(t *testing.T) {
	in, _, api := testInstance(t)
	if err := setAccessState(in.db, 999, "@spammer", accessBlocked); err != nil {
		t.Fatal(err)
	}
	before := len(api.since("sendMessage"))
	in.handleMessage(dmMessage(999, "hi again"))
	if got := len(api.since("sendMessage")) - before; got != 0 {
		t.Errorf("a blocked user got %d message(s), want 0", got)
	}
}

func TestPendingPairQueueIsCapped(t *testing.T) {
	in, _, _ := testInstance(t)
	now := time.Now()
	for i := int64(1); i <= maxPendingPairs; i++ {
		if out := handleUnknownUser(in.db, 1000+i, "", now); out.Code == "" {
			t.Fatalf("user %d should have got a code", 1000+i)
		}
	}
	if out := handleUnknownUser(in.db, 9999, "", now); out.Reply != "" || out.Code != "" {
		t.Errorf("the %dth stranger got %+v, want silence", maxPendingPairs+1, out)
	}
	var rows int64
	in.db.Model(&Access{}).Count(&rows)
	if rows != int64(maxPendingPairs) {
		t.Errorf("%d access rows, want %d", rows, maxPendingPairs)
	}
}

func TestApproveByCode(t *testing.T) {
	in, _, _ := testInstance(t)
	now := time.Now()
	out := handleUnknownUser(in.db, 555, "@friend", now)
	if out.Code == "" {
		t.Fatal("no code minted")
	}

	if _, err := approveByCode(in.db, "ffffff", now); err == nil {
		t.Error("an unknown code must not approve anybody")
	}
	if classifyAccess(in.db, in.config(), 555) != roleDenied {
		t.Error("a pending user must not be allowed in yet")
	}

	row, err := approveByCode(in.db, strings.ToUpper(out.Code), now)
	if err != nil {
		t.Fatalf("approveByCode: %v", err)
	}
	if row.TelegramUserID != 555 {
		t.Errorf("approved the wrong user: %d", row.TelegramUserID)
	}
	if classifyAccess(in.db, in.config(), 555) != roleUser {
		t.Error("the approved user is still denied")
	}

	// An expired code is refused.
	out2 := handleUnknownUser(in.db, 556, "", now)
	if _, err := approveByCode(in.db, out2.Code, now.Add(pairCodeTTL+time.Minute)); err == nil {
		t.Error("an expired code must be refused")
	}
}

func TestClassifyAccessOwnerAndUnbootstrapped(t *testing.T) {
	in, _, _ := testInstance(t)
	if got := classifyAccess(in.db, in.config(), 42); got != roleOwner {
		t.Errorf("owner role = %v, want roleOwner", got)
	}
	if got := classifyAccess(in.db, in.config(), 0); got != roleDenied {
		t.Error("an update with no sender must be denied")
	}
	// Before bootstrap there is no owner, so nobody is allowed — not even the
	// first person to message the bot.
	if got := classifyAccess(in.db, &Config{}, 42); got != roleDenied {
		t.Error("with no chat_id configured, everybody must be denied")
	}
}

func TestAccessCommands(t *testing.T) {
	in, _, api := testInstance(t)

	in.handleMessage(dmMessage(42, "/access add 4242"))
	if classifyAccess(in.db, in.config(), 4242) != roleUser {
		t.Error("/access add did not allow the user")
	}

	in.handleMessage(dmMessage(42, "/access block 4242"))
	if classifyAccess(in.db, in.config(), 4242) != roleDenied {
		t.Error("/access block did not deny the user")
	}

	in.handleMessage(dmMessage(42, "/access list"))
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "4242") || !strings.Contains(joined, accessBlocked) {
		t.Errorf("/access list did not show the blocked user:\n%s", joined)
	}

	in.handleMessage(dmMessage(42, "/access remove 4242"))
	var rows int64
	in.db.Model(&Access{}).Where("telegram_user_id = ?", 4242).Count(&rows)
	if rows != 0 {
		t.Error("/access remove did not delete the row")
	}
}

// The owner's Allow button is the only shortcut past /access pair, and it only
// exists in the owner's own chat.
func TestAccessCallbackApprovesOnlyForOwner(t *testing.T) {
	in, _, _ := testInstance(t)
	out := handleUnknownUser(in.db, 777, "@knocker", time.Now())

	stranger := &CallbackQuery{ID: "cb", Data: "access:pair:" + out.Code}
	stranger.From.ID = 999
	in.handleCallback(stranger)
	if classifyAccess(in.db, in.config(), 777) != roleDenied {
		t.Fatal("a stranger's tap approved a pairing request")
	}

	owner := &CallbackQuery{ID: "cb2", Data: "access:pair:" + out.Code}
	owner.From.ID = 42
	owner.Message = dmMessage(42, "")
	in.handleCallback(owner)
	if classifyAccess(in.db, in.config(), 777) != roleUser {
		t.Error("the owner's tap did not approve the request")
	}
}
