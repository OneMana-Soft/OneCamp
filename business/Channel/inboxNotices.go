package business

import (
	"context"

	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
)

// NoticeKind is the resource kind of a notice about a channel (a mention in
// one of its messages, a moment of an agent's run asked for there): its
// ResourceUUID is the channel's id.
const NoticeKind = "channel"

func init() { followBusiness.RegisterNoticeKind(NoticeKind, inboxNotices) }

// Seams for tests: who may read a channel, and its name.
var (
	noticeReadable = ReadableBy
	noticeNames    = channelDomain.GetChannelNamesByUUIDs
)

// inboxNotices is how the Inbox names notices about channels (business/
// Follow noticeKinds.go): open, and named, only for a reader who may read the
// channel now (ReadableBy), so a notice from a channel they have left since
// keeps its place in their Inbox and says nothing of it. Registered here, not
// with the AI's notices, so both editions have it. Names that can't be read
// leave the open ones unnamed.
func inboxNotices(ctx context.Context, reader string, ids []string) (map[string]bool, map[string]string, error) {
	readable, open, err := followBusiness.ReadableEach(ctx, reader, ids, noticeReadable)
	if err != nil {
		return nil, nil, err
	}
	names, err := noticeNames(ctx, open)
	if err != nil {
		return readable, nil, nil
	}
	return readable, names, nil
}
