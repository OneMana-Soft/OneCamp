package business

// The message and typing topics a person may read: those of every channel,
// conversation and project they're in. GetMqttConfig hands them out, and the
// broker's question about a message or typing topic (business/MqttAccess) is
// answered from them.

import (
	"context"
	"strings"
	"sync"
	"time"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"golang.org/x/sync/singleflight"
)

const (
	// memberTopicsFor is how long a person's topics are used before they're
	// read again. Long enough to answer every subscription of a client's
	// connect (it subscribes to all its topics at once) from one read; short,
	// because a channel just left stays subscribable for this long.
	memberTopicsFor = 10 * time.Second
	// memberTopicsReread is how old a person's topics must be before a topic
	// missing from them is worth reading them again for. Joining a channel or
	// starting a conversation fetches the client's settings, which reads them
	// anyway; this is the fallback, and what bounds the cost of a client asking
	// for topics it was never handed.
	memberTopicsReread = 2 * time.Second
)

// readPersonTopics reads a person's channels, conversations and projects
// from the graph.
var readPersonTopics = userDomain.GetDgraphUserInfoByUUIDForMQTTConfig

type memberTopics struct {
	since  time.Time // when the read began
	list   []string  // in the order a client is handed them
	topics map[string]struct{}
	// byConversation is each channel's, DM's and group's topics (its message
	// topic, then its typing topic), by the key the web app names it by
	// ("channel:<uuid>", "dm:<grouping id>", "group:<grouping id>"): a topic
	// is encrypted, so the app can't work out which conversation one is.
	byConversation map[string][]string
}

// conversationKey is the key a conversation's topics are named by in the
// client's settings, as the web app's keyOf names it. Pure.
func conversationKey(kind, id string) string {
	return kind + ":" + id
}

var (
	memberTopicsCache = helpers.NewTTLCache[*memberTopics](memberTopicsFor)
	// memberTopicsForgotten is when each person's topics were last forgotten
	// (ForgetMemberTopics). A read that began before then may have seen a
	// conversation they've been taken out of since, so it's never kept.
	memberTopicsForgotten = helpers.NewTTLCache[time.Time](time.Minute)
	// memberTopicsMu orders replacements, so a read that began earlier never
	// replaces one that began later (and saw a channel joined in between).
	memberTopicsMu   sync.Mutex
	memberTopicReads singleflight.Group
)

// ForgetMemberTopics drops what's kept of a person's topics, so the broker's
// next question about one is answered from a fresh read. Without it, someone
// just removed from a channel could subscribe to it again for as long as the
// kept topics last (memberTopicsFor).
func ForgetMemberTopics(userUUID string) {
	memberTopicsMu.Lock()
	memberTopicsCache.Delete(userUUID)
	memberTopicsForgotten.Set(userUUID, time.Now())
	memberTopicsMu.Unlock()
	// A read under way may have begun before the change; a question asked
	// from now on doesn't wait for its answer.
	memberTopicReads.Forget(userUUID)
}

// readMemberTopics reads a person's member topics from the graph and keeps
// them.
func readMemberTopics(ctx context.Context, userUUID string) (*memberTopics, error) {
	since := time.Now()
	user, err := readPersonTopics(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	mt := &memberTopics{since: since, topics: map[string]struct{}{}, byConversation: map[string][]string{}}
	add := func(id string, topics ...string) {
		if id == "" {
			return
		}
		for _, t := range topics {
			if _, dup := mt.topics[t]; !dup {
				mt.topics[t] = struct{}{}
				mt.list = append(mt.list, t)
			}
		}
	}
	if user != nil {
		for _, ch := range user.Channels {
			msg, typing := helpers.GetMqttTopicForChannel(ch.Uuid)
			add(ch.Uuid, msg, typing)
			if ch.Uuid != "" {
				mt.byConversation[conversationKey("channel", ch.Uuid)] = []string{msg, typing}
			}
		}
		for _, dm := range user.DMs {
			msg, typing := helpers.GetMqttTopicForDm(dm.GroupingId)
			add(dm.GroupingId, msg, typing)
			if dm.GroupingId != "" {
				kind := "group"
				if strings.Contains(dm.GroupingId, " ") {
					kind = "dm"
				}
				mt.byConversation[conversationKey(kind, dm.GroupingId)] = []string{msg, typing}
			}
		}
		// Task comments, reactions and GitHub sync messages are published to
		// a project's topic.
		for _, p := range user.Projects {
			add(p.Uuid, helpers.GetMqttTopicForProjectMessage(p.Uuid))
		}
	}

	memberTopicsMu.Lock()
	if forgot, ok := memberTopicsForgotten.Get(userUUID); !ok || since.After(forgot) {
		if cur, ok := memberTopicsCache.Get(userUUID); !ok || !cur.since.After(since) {
			memberTopicsCache.Set(userUUID, mt)
		}
	}
	memberTopicsMu.Unlock()
	return mt, nil
}

// IsMemberTopic answers whether topic is a message or typing topic of a
// channel, conversation or project the person is in.
func IsMemberTopic(ctx context.Context, userUUID, topic string) (bool, error) {
	if mt, ok := memberTopicsCache.Get(userUUID); ok {
		if _, in := mt.topics[topic]; in {
			return true, nil
		}
		if time.Since(mt.since) < memberTopicsReread {
			return false, nil
		}
	}
	// One read however many of a client's subscriptions ask at once: a client
	// subscribes to all its topics together when it connects. The read isn't
	// tied to the request that happened to start it, and ends inside the
	// broker's five-second wait for an answer (past it the broker refuses on
	// its own, and a read still going helps nobody).
	v, err, _ := memberTopicReads.Do(userUUID, func() (interface{}, error) {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
		defer cancel()
		return readMemberTopics(readCtx, userUUID)
	})
	if err != nil {
		return false, err
	}
	_, in := v.(*memberTopics).topics[topic]
	return in, nil
}
