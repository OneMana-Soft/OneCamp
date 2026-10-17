package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

// ChannelMentionCounts is, for each channel in readMarks (its uuid → when the
// person last read it), how many live posts by others that mention the person
// arrived there since: the sidebar's "@2" beside a channel's name, until the
// read state counts mentions itself (A3). The rules are the unread count's
// (GetLatestPostInChannelCountByUserID): newer than the read mark, not
// deleted, not the person's own.
//
// One query for every channel, never one per channel, and it starts from the
// person's own mentions (since the oldest mark), so the graph reads those
// posts and no others: counting each channel's posts against its mark would
// read every post of every channel the person is in.
func ChannelMentionCounts(ctx context.Context, userDgraphUID string, readMarks map[string]time.Time) (map[string]uint64, error) {
	if userDgraphUID == "" || len(readMarks) == 0 {
		return map[string]uint64{}, nil
	}
	variables := map[string]string{"$me": userDgraphUID, "$since": oldestMark(readMarks).UTC().Format(time.RFC3339Nano)}
	var answer struct {
		Posts []mentionedPost `json:"posts"`
	}
	if err := dgraphInit.ReadInto(ctx, channelMentionsQuery, variables, &answer); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ChannelMentionCounts err: %+v", err)
		return nil, err
	}
	return countUnreadMentions(answer.Posts, readMarks), nil
}

// channelMentionsQuery reads the live posts by others that mention $me, made
// after $since, with each one's time and channel. A post's mention node is
// written with the post's own time, so a mention older than $since can't be
// in any count.
const channelMentionsQuery = `query ChannelMentions($me: string, $since: string) {
	var(func: uid($me)) {
		~mention_users @filter(gt(mention_created_at, $since)) {
			mentioned as mention_post
		}
	}
	posts(func: uid(mentioned)) @filter(gt(post_created_at, $since) AND not gt(post_deleted_at, "1970-01-01T00:00:00Z") AND NOT uid_in(post_by, $me)) {
		post_created_at
		post_channel {
			ch_uuid
		}
	}
}`

type mentionedPost struct {
	CreatedAt time.Time `json:"post_created_at"`
	Channel   *struct {
		Uuid string `json:"ch_uuid"`
	} `json:"post_channel"`
}

// countUnreadMentions counts, per channel in readMarks, the posts made there
// after its mark. Pure.
func countUnreadMentions(posts []mentionedPost, readMarks map[string]time.Time) map[string]uint64 {
	counts := map[string]uint64{}
	for _, p := range posts {
		if p.Channel == nil {
			continue
		}
		if mark, ok := readMarks[p.Channel.Uuid]; ok && p.CreatedAt.After(mark) {
			counts[p.Channel.Uuid]++
		}
	}
	return counts
}

// oldestMark is the earliest of the marks. readMarks isn't empty.
func oldestMark(readMarks map[string]time.Time) time.Time {
	var oldest time.Time
	first := true
	for _, at := range readMarks {
		if first || at.Before(oldest) {
			oldest, first = at, false
		}
	}
	return oldest
}
