package business

import (
	"context"

	followBusiness "github.com/akashc777/OneCamp/business/Follow"
)

// NoticeKind is the resource kind of a notice about a DM or a group (a
// mention in one of its messages, a moment of an agent's run asked for
// there): its ResourceUUID is the conversation's grouping id.
const NoticeKind = "conversation"

func init() { followBusiness.RegisterNoticeKind(NoticeKind, inboxNotices) }

// InConversation reports whether the person with userUUID is in the DM or
// group grpID now (InConversationNow): what every notice from a DM or a
// group is held to.
func InConversation(ctx context.Context, userUUID, grpID string) (bool, error) {
	in, err := InConversationNow(ctx, grpID, []string{userUUID})
	return len(in) == 1, err
}

// noticeReadable is who may open a conversation's notice: a seam for tests.
var noticeReadable = InConversation

// inboxNotices is how the Inbox names notices about DMs and groups
// (business/Follow noticeKinds.go): open while the reader is in the
// conversation now, unnamed (the Inbox says "your conversation").
// Registered here, not with the AI's notices, so both editions have it.
func inboxNotices(ctx context.Context, reader string, ids []string) (map[string]bool, map[string]string, error) {
	readable, _, err := followBusiness.ReadableEach(ctx, reader, ids, noticeReadable)
	return readable, nil, err
}
