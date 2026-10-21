package business

// Keywords (chat P3 stage 3, TN-53): a member whose keyword a channel post
// holds, as a whole word in any case (helpers.KeywordPattern), hears of it as
// of a mention: the Activity item and the push, by their level for the
// channel (a member at "mention" now hears of it; one who muted it doesn't).
// Never an email: the email goes only to the people a post names.

import (
	"context"
	"strings"

	personalAlertBusiness "github.com/akashc777/OneCamp/business/PersonalAlert"
	reachBusiness "github.com/akashc777/OneCamp/business/Reach"
	channelNotificationDomain "github.com/akashc777/OneCamp/domain/UserChannelNotification"
	"github.com/akashc777/OneCamp/helpers"
)

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
