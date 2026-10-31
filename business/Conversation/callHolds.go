package business

// A call's chat held (calls meet-12 and zoom-14, agreed with CHAT-LEAD-7, 23
// Oct): while the people running a channel's call hold chat for everyone
// else, the thread of the call's post takes their replies only. Anyone else
// who could reply there reads it, wherever they write from: the call's own
// chat is that thread, so the channel's thread view would otherwise be a way
// round the hold. A channel's managers moderate it, and run its calls, so
// they keep writing; nothing changes in a DM or a group, whose calls have no
// hosts, or once the call ends.

import (
	"context"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// ReasonCallChatHeld is a thread's reply taken while its call holds chat.
const ReasonCallChatHeld = "call_chat_held"

// chatHeldOver is the hold's one read: a seam for tests.
var chatHeldOver = callStateModel.ChatHeldOver

// chatHolds caps at Read the threads in out whose live call holds chat for
// who, among those they would reply in. Asked after every reader, since the
// hold is the call's live state and no index keeps it. An agent acting for a
// member, or an API token acting as one, is held as that member.
func chatHolds(ctx context.Context, who accessBusiness.Subject, out map[string]accessBusiness.Decision) error {
	ids := map[uuid.UUID]string{}
	var posts []uuid.UUID
	for id, d := range out {
		if d.Level != LevelReply {
			continue
		}
		if post, err := uuid.Parse(id); err == nil {
			ids[post] = id
			posts = append(posts, post)
		}
	}
	if len(posts) == 0 {
		return nil
	}
	var member *uuid.UUID
	if id, err := uuid.Parse(who.UserUUID); err == nil {
		member = &id
	}
	held, err := chatHeldOver(ctx, posts, member, "")
	if err != nil {
		return err
	}
	for post := range held {
		id := ids[post]
		d := capAt(out[id], LevelRead)
		d.Reasons = append(d.Reasons, ReasonCallChatHeld)
		out[id] = d
	}
	return nil
}
