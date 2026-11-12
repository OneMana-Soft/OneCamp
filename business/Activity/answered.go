package business

// What the person has answered of what asked them something (V-701), read
// for a page of the feed at once: at most one read of each kind, whatever
// the page holds, and none when nothing on it asks them anything.

import (
	"context"
	"time"

	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	"github.com/akashc777/OneCamp/helpers"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	approvalModels "github.com/akashc777/OneCamp/models/postgres/Approvals"
	readStateModels "github.com/akashc777/OneCamp/models/postgres/ReadState"
	"github.com/google/uuid"
)

// answers is what the person has done about the items that ask them
// something: when they last wrote in each conversation (channels and chats,
// keyed as activityScope keys them) and in each thread (keyed by the post or
// message the thread hangs off), and which approval requests are settled (by
// approval id: no longer waiting on them).
type answers struct {
	channels, chats, threads map[string]time.Time
	settled                  map[string]bool
}

// Seams for the reads' tests: where the person last wrote, where they last
// replied, and which requests still wait on them (each the one reader of
// its rule: the read state's rows, the threads' replies, and the "Waiting
// for my approval" list's).
var (
	wroteIn      = readStateModels.LastWrote
	repliedIn    = commentDomain.LastRepliesBy
	waitingAmong = approvalModels.WaitingAmong
)

// readAnswers reads what the person (user, their node me) has answered of
// the high items: they can only drop. A read that fails is logged and
// answers nothing, so those items stay as scored.
func readAnswers(ctx context.Context, user uuid.UUID, me string, items []dgraphModels.UnifiedActivityItem) answers {
	var channels, chats, posts, messages []string
	var requests []uuid.UUID
	asked := map[uuid.UUID]string{} // each request's id as its notice says it
	for i := range items {
		item := &items[i]
		if item.Priority != PriorityHigh {
			continue
		}
		if id := approvalOf(item); id != "" {
			if u, err := uuid.Parse(id); err == nil && asked[u] == "" {
				requests, asked[u] = append(requests, u), id
			}
			continue
		}
		switch kind, id := activityScope(item); kind {
		case "channel":
			if _, err := uuid.Parse(id); err == nil {
				channels = append(channels, id)
			}
		case "chat":
			chats = append(chats, id)
		}
		switch kind, id := activityThread(item); kind {
		case "post":
			posts = append(posts, id)
		case "chat":
			messages = append(messages, id)
		}
	}
	var a answers
	var err error
	if len(channels) > 0 {
		if a.channels, err = wroteIn(ctx, readStateModels.Channel, user, channels); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Activity: what %s wrote in their channels couldn't be read: %+v", user, err)
		}
	}
	if len(chats) > 0 {
		if a.chats, err = wroteIn(ctx, readStateModels.Chat, user, chats); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Activity: what %s wrote in their chats couldn't be read: %+v", user, err)
		}
	}
	if len(posts)+len(messages) > 0 {
		if a.threads, err = repliedIn(ctx, me, posts, messages); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Activity: %s's thread replies couldn't be read: %+v", user, err)
		}
	}
	if len(requests) > 0 {
		waiting, err := waitingAmong(ctx, user, requests)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Activity: the requests waiting on %s couldn't be read: %+v", user, err)
			return a
		}
		a.settled = make(map[string]bool, len(requests))
		for u, id := range asked {
			if !waiting[u] {
				a.settled[id] = true
			}
		}
	}
	return a
}
