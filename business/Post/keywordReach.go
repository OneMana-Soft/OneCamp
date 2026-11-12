package business

// Keywords (chat P3 stage 3, TN-53): a member whose keyword a channel post
// holds, as a whole word in any case (helpers.KeywordPattern), hears of it as
// of a mention: the Activity item and the push, by their level for the
// channel (a member at "mention" now hears of it; one who muted it doesn't).
// Never an email: the email goes only to the people a post names.

import (
	"context"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	"strings"

	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	personalAlertBusiness "github.com/akashc777/OneCamp/business/PersonalAlert"
	reachBusiness "github.com/akashc777/OneCamp/business/Reach"
	channelNotificationDomain "github.com/akashc777/OneCamp/domain/UserChannelNotification"
	"github.com/akashc777/OneCamp/helpers"
)

// The Notification Router's keyword input is this one (A4 H1): it reads a
// channel's keyword reach here, never a copy of it.
func init() {
	// A channel's call notice goes through the router too; business/Channel
	// can't import it (the router reads business/Reach, which reads Channel).
	channelBusiness.TellCallsWith(func(ctx context.Context, eventID, channelID, actor string, push map[string]string) []string {
		return notifyBusiness.Told(notifyBusiness.Tell(ctx, notifyBusiness.Event{ID: eventID, Cause: notifyBusiness.CauseCall, Actor: actor,
			Place: notifyBusiness.Place{Kind: notifyBusiness.PlaceChannel, ID: channelID}, Push: push}))
	})
	// So does a reply in a channel thread (business/Comment can't import it
	// either): the email goes to whom the thread's rule told, the Activity
	// items to the post's author and those mentioned.
	commentBusiness.RouteRepliesWith(func(ctx context.Context, n commentBusiness.ReplyNotice) commentBusiness.ReplyTold {
		told := notifyBusiness.Tell(ctx, replyEvent(n))
		return commentBusiness.ReplyTold{
			Thread:    notifyBusiness.Of(told, notifyBusiness.CauseMention, notifyBusiness.CauseReplyToYou, notifyBusiness.CauseThreadReply),
			RepliedTo: notifyBusiness.Of(told, notifyBusiness.CauseReplyToYou),
			Mentioned: notifyBusiness.Of(told, notifyBusiness.CauseMention),
		}
	}, func(ctx context.Context, n commentBusiness.ReplyNotice, who string) bool {
		tr, err := notifyBusiness.Notices.Explain(ctx, who, replyEvent(n))
		if err != nil || tr.Decision == nil {
			return false
		}
		return len(notifyBusiness.Of([]notifyBusiness.Decision{*tr.Decision}, notifyBusiness.CauseMention, notifyBusiness.CauseReplyToYou)) > 0
	})
	notifyBusiness.ReachKeywordsWith(func(ctx context.Context, channelID, text, writer string) []string {
		return keywordReached(ctx, channelID, text, writer, nil)
	})
}

// replyEvent is a reply in a channel thread as the router plans it. Pure.
func replyEvent(n commentBusiness.ReplyNotice) notifyBusiness.Event {
	return notifyBusiness.Event{ID: n.ID, MessageID: n.MessageID, Cause: notifyBusiness.CauseMessage, Actor: n.Actor,
		Author: n.Author, Repliers: n.Repliers, Mentions: n.Mentions, Push: n.Push,
		Place: notifyBusiness.Place{Kind: notifyBusiness.PlaceChannel, ID: n.ChannelID, ThreadRootID: n.PostID}}
}

// Seams for tests.
var (
	channelMembers = channelNotificationDomain.MemberIDs
	keywordHits    = personalAlertBusiness.KeywordHits
)

// keywordReached is the members of a channel (user ids) whose keywords text
// holds, other than the writer and those told already (by user id, any
// case). On a failed read it reaches nobody more: the post still notifies as
// it did.
func keywordReached(ctx context.Context, channelID, text, writer string, told map[string]bool) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	members, err := channelMembers(ctx, channelID, reachBusiness.MaxReach)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Post keywordReached: could not read the channel's members: %+v", err)
		return nil
	}
	skip := map[string]bool{strings.ToLower(writer): true}
	for id := range told {
		skip[strings.ToLower(id)] = true
	}
	var people []string
	for _, m := range members {
		if !skip[strings.ToLower(m)] {
			people = append(people, m)
		}
	}
	if len(people) == 0 {
		return nil
	}
	hits, err := keywordHits(ctx, text, people)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Post keywordReached: could not read keywords: %+v", err)
		return nil
	}
	var out []string
	for _, p := range people {
		if _, ok := hits[strings.ToLower(p)]; ok {
			out = append(out, p)
		}
	}
	return out
}
