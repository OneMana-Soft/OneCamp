package businness

// Making a change to who may do what with docs, and everything that follows
// from it: the indexes that carry a doc's sharing are written again whole
// (I13), the change is told to the Resource Events bus (I14), and the live
// sockets of the people whose level moved are closed so they reconnect and are
// authorised afresh (I10).

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	collab "github.com/akashc777/OneCamp/services/collab"
)

// ErrNoSuchPrincipal is a person or a team a change names that isn't there.
var ErrNoSuchPrincipal = domain.ErrNoSuchPrincipal

// writeOf is a change as the graph writes it.
func writeOf(c SharingChange) domain.AccessWrite {
	w := domain.AccessWrite{ClearPeople: c.Replace, ClearTeams: c.Replace}
	if len(c.People) > 0 {
		w.People = map[string]string{}
		for id, l := range c.People {
			w.People[id] = grantName(l)
		}
	}
	if len(c.Teams) > 0 {
		w.Teams = map[string]string{}
		for id, l := range c.Teams {
			w.Teams[id] = grantName(min(l, level.Edit))
		}
	}
	if g := c.General; g != nil {
		priv, comment, edit := flagsOf(*g)
		w.Private, w.PublicComment, w.PublicEdit = &priv, &comment, &edit
	}
	return w
}

// grantName is a level as a grant list names it; none is no list.
func grantName(l level.Level) string {
	if l == level.None {
		return ""
	}
	return l.String()
}

// ApplySharing makes one change to docs (by uuid), all of them in one
// transaction: a page and every page under it, or one page.
func ApplySharing(ctx context.Context, docUUIDs []string, c SharingChange) error {
	return domain.WriteDocAccess(ctx, docUUIDs, writeOf(c))
}

// HandOverSharing makes a person a doc's owner, with a change to its sharing
// in the same transaction (the new owner off every list, the old one given
// full access).
func HandOverSharing(ctx context.Context, docUUID, toUserUUID string, c SharingChange) error {
	w := writeOf(c)
	w.OwnerUUID = toUserUUID
	return domain.WriteDocAccess(ctx, []string{docUUID}, w)
}

// SetLock sets a doc's lock (level.LockNone lifts it), by the person named.
func SetLock(ctx context.Context, docUUID, lock, byUserUUID string, at time.Time) error {
	return domain.WriteDocAccess(ctx, []string{docUUID}, domain.AccessWrite{Lock: &lock, LockedByUUID: byUserUUID, At: at})
}

// Actor is who changed access, from server context (I11).
type Actor = resourceEventBusiness.Actor

// AccessChanged does what follows a change to docs' sharing: their search and
// AI entries are written again from the graph, the change is told to the
// events bus (accessEvents), and the sockets of the people whose level moved
// are closed so that they reconnect and are authorised afresh (everyone's when
// the workspace's level moved). When it took the docs from anyone, their
// topics are taken off every client whose person can't read them now, at the
// broker, which asks about a subscription only when it is made (I10).
// Nothing here fails the change.
func AccessChanged(ctx context.Context, docUUIDs []string, by Actor, kind resourceEventBusiness.Kind, ex Explained, data map[string]string) {
	var moved []string
	for _, s := range ex.People {
		moved = append(moved, s.UserUUID)
	}
	everyone := ex.Everyone != nil
	now := time.Now()
	for _, id := range docUUIDs {
		RepairAccessIndexes(ctx, id)
		for _, e := range accessEvents(id, by, kind, ex, data, now) {
			resourceEventBusiness.Bus.Emit(ctx, e)
		}
	}
	if everyone || len(moved) > 0 {
		var who []string
		if !everyone {
			who = moved
		}
		ReauthLater(docUUIDs, who)
	}
	if tookFromAnyone(ex) {
		topics := make([]string, 0, len(docUUIDs))
		for _, id := range docUUIDs {
			topics = append(topics, helpers.GetMqttTopicForDoc(id))
		}
		mqttBusiness.RecheckTopics(topics, readsDocTopic)
	}
}

// tookFromAnyone is whether a change took a doc from anyone who could read
// it: a person's level, or the workspace's, fell below view. Everyone's shift
// with no levels marks a change whose people aren't counted, which may have.
// Pure.
func tookFromAnyone(ex Explained) bool {
	if ex.Everyone != nil && ex.Everyone.To < level.View {
		return true
	}
	for _, s := range ex.People {
		if s.From >= level.View && s.To < level.View {
			return true
		}
	}
	return false
}

// readsDocTopic is ReadsDoc for a doc's topic, as the broker's recheck asks
// it; a topic that isn't a doc's is no one's.
func readsDocTopic(ctx context.Context, userUUID, topic string) (bool, error) {
	kind, id, ok := helpers.ParseMqttTopic(topic)
	if !ok || kind != helpers.MqttKindDoc {
		return false, nil
	}
	return ReadsDoc(ctx, userUUID, id)
}

// accessEvents are what a change to one doc's sharing tells the bus: the
// change itself (kind, with data), then doc.shared for the people who can
// open the doc now and couldn't before, one event for each level they were
// given, highest first, its data the change's with that level added ("You can
// edit it", in A6's notice and its audit row). data is never written to. Pure.
func accessEvents(docUUID string, by Actor, kind resourceEventBusiness.Kind, ex Explained, data map[string]string, at time.Time) []resourceEventBusiness.Event {
	out := []resourceEventBusiness.Event{{Kind: kind, ResourceKind: "doc", ResourceUUID: docUUID, Actor: by, At: at, Data: data}}
	gained := map[level.Level][]string{}
	for _, s := range ex.People {
		if s.From < level.View && s.To >= level.View {
			gained[s.To] = append(gained[s.To], s.UserUUID)
		}
	}
	for _, l := range []level.Level{level.Full, level.Edit, level.Comment, level.View} {
		if len(gained[l]) == 0 {
			continue
		}
		shared := make(map[string]string, len(data)+1)
		for k, v := range data {
			shared[k] = v
		}
		shared["level"] = l.String()
		out = append(out, resourceEventBusiness.Event{
			Kind: resourceEventBusiness.DocShared, ResourceKind: "doc", ResourceUUID: docUUID, Actor: by, At: at,
			Subjects: gained[l], Data: shared,
		})
	}
	return out
}

// reauthTimeout bounds closing one doc's sockets: the change is made, and a
// socket the service can't be reached to close is asked again whenever it
// reconnects anyway.
const reauthTimeout = 5 * time.Second

// ReauthLater closes the docs' live sockets of the people named (everyone's
// when none are) in the background, one doc after another, so a change never
// waits on the collaboration service.
func ReauthLater(docUUIDs []string, userUUIDs []string) {
	ids := append([]string(nil), docUUIDs...)
	helpers.GoSafeNamed("doc.access-reauth", func() {
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), reauthTimeout)
			Reauth(ctx, id, userUUIDs)
			cancel()
		}
	})
}

// Reauth closes the live sockets on a doc of the people named (everyone's when
// none are), so they reconnect and are authorised afresh (I10). Until the Live
// Doc Writer runs (A4), there is nothing to close: a socket is authorised again
// whenever it reconnects anyway.
func Reauth(ctx context.Context, docUUID string, userUUIDs []string) {
	err := collab.Live.Reauth(ctx, docUUID, userUUIDs)
	switch {
	case err == nil:
	case errors.Is(err, collab.ErrUnavailable):
		helpers.LogInfoWithContext(ctx, "business/Doc Reauth %s: live sockets can't be closed yet (no live writer)", docUUID)
	default:
		helpers.LogErrorWithContext(ctx, "business/Doc Reauth %s err: %+v", docUUID, err)
	}
}

// RepairAccessIndexes writes a doc's derived entries again from the graph,
// whole (I13): its search entry with its comments' and files', and in the AI
// edition its AI entries' sharing (docAccessAI.go). In the background; a
// failure is logged, and the next change or the nightly repair writes them
// again.
func RepairAccessIndexes(ctx context.Context, docUUID string) {
	syncDocSearch(docUUID, true)
	if aiAccessEntries == nil {
		return
	}
	helpers.GoSafeNamed("doc.access-ai-entry", func() {
		bg := context.Background()
		s, err := ReadSharing(bg, docUUID)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Doc RepairAccessIndexes %s: %+v", docUUID, err)
			return
		}
		if err := aiAccessEntries(bg, s); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Doc RepairAccessIndexes %s (AI entries): %+v", docUUID, err)
		}
	})
}

// aiAccessEntries writes a doc's AI entries' sharing from its sharing: set by
// docAccessAI.go in the AI edition, nil without it.
var aiAccessEntries func(ctx context.Context, s *Sharing) error
