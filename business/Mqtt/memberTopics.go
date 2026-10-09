package business

// The message and typing topics a person may read: those of every channel,
// conversation and project they're in. GetMqttConfig hands them out, and the
// broker's question about a message or typing topic (business/MqttAccess) is
// answered from them.

import (
	"context"
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

type memberTopics struct {
	since  time.Time // when the read began
	list   []string  // in the order a client is handed them
	topics map[string]struct{}
}

var (
	memberTopicsCache = helpers.NewTTLCache[*memberTopics](memberTopicsFor)
	// memberTopicsMu orders replacements, so a read that began earlier never
	// replaces one that began later (and saw a channel joined in between).
	memberTopicsMu   sync.Mutex
	memberTopicReads singleflight.Group
)

// readMemberTopics reads a person's member topics from the graph and keeps
// them.
func readMemberTopics(ctx context.Context, userUUID string) (*memberTopics, error) {
	since := time.Now()
	user, err := userDomain.GetDgraphUserInfoByUUIDForMQTTConfig(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	mt := &memberTopics{since: since, topics: map[string]struct{}{}}
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
		}
		for _, dm := range user.DMs {
			msg, typing := helpers.GetMqttTopicForDm(dm.GroupingId)
			add(dm.GroupingId, msg, typing)
		}
		// Task comments, reactions and GitHub sync messages are published to
		// a project's topic.
		for _, p := range user.Projects {
			add(p.Uuid, helpers.GetMqttTopicForProjectMessage(p.Uuid))
		}
	}

	memberTopicsMu.Lock()
	if cur, ok := memberTopicsCache.Get(userUUID); !ok || !cur.since.After(since) {
		memberTopicsCache.Set(userUUID, mt)
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
