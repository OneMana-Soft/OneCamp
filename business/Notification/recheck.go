package notification

// An email waits in the queue (quiet hours, a retry, a busy worker), and what
// it is about goes on meanwhile. So each row carries what it is about
// (About, in its metadata), and at the send the worker asks again what the
// live path asked at the event, with one read per kind for the whole batch:
// the item is still there, the person may still read it, their setting there
// still lets it through, and they haven't read it in the app since. A row
// that no longer holds is not sent: it is suppressed, "no longer told".
// Every answer is the recipient's own (row.user_id), never the actor's.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"

	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	readStateDomain "github.com/akashc777/OneCamp/domain/ReadState"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userProjectNotificationDomain "github.com/akashc777/OneCamp/domain/UserProjectNotification"
	"github.com/akashc777/OneCamp/helpers"
	emailQueueModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailQueue"
	readStateModels "github.com/akashc777/OneCamp/models/postgres/ReadState"
)

// About is what a queued email is about: a Kind the worker asks again of
// (recheckers), the Item (a message's, a post's or a task's uuid, a project
// update's id), the Place it is in (a DM's or group's grouping id, a
// channel's id, a project's uuid; a task's is its own project, read with it)
// and what it is to the
// person (Heard), as the level rule (business/NotifyLevel Lets) reads it;
// for a reply in the Item's thread, the Reply too (a comment's uuid). An
// email about several items of one kind (an Urgent digest's tasks) names
// them all in Items, the first as Item too, and holds only while each does.
type About struct {
	Kind  string            `json:"kind"`
	Item  string            `json:"item"`
	Items []string          `json:"items,omitempty"`
	Place string            `json:"place"`
	Reply string            `json:"reply,omitempty"`
	Heard notifylevel.Heard `json:"heard"`
}

// The kinds asked again at the send: a message in a DM or a group (its Item
// the message, its Place the grouping id), a post in a channel (the post,
// the channel), a task (the task), a comment on a doc (the doc, its Reply
// the comment, a member's or a guest's), and a project's update (the update,
// the project).
const (
	AboutChat    = "chat"
	AboutChannel = "channel"
	AboutTask    = "task"
	AboutDoc     = "doc"
	AboutProject = "project"
)

// items is what a is about: its Items, or its one Item (a list of one).
func (a About) items() []string {
	if len(a.Items) > 0 {
		return a.Items
	}
	return []string{a.Item}
}

// verdict is what the send does with a row: send it, suppress it (no longer
// told), or leave it for later (what it is about couldn't be read).
type verdict int

const (
	send verdict = iota
	noLongerTold
	askLater
)

// asked is one row's question: whose email, about what.
type asked struct {
	user  string
	about About
}

// recheckers answer, for one kind, which of the asked still hold, by index,
// in one read for them all. A kind with none here is sent as it was queued.
var recheckers = map[string]func(context.Context, []asked) ([]bool, error){
	AboutChat:    chatStillHolds,
	AboutChannel: channelStillHolds,
	AboutTask:    taskStillHolds,
	AboutDoc:     docStillHolds,
	AboutProject: projectStillHolds,
}

// DocReaders answers, for each doc (by uuid, keyed as given), which of the
// people named may read it now: a doc that isn't there is read by nobody.
type DocReaders func(ctx context.Context, people map[string][]string) (map[string]map[string]bool, error)

// docReaders is the doc read rule. It is business/Doc's, which imports this
// package and so can't be imported by it: business/Doc hands it over when
// the server starts (RegisterDocReaders).
var docReaders DocReaders

// RegisterDocReaders hands the send-time re-check the doc read rule.
func RegisterDocReaders(readers DocReaders) { docReaders = readers }

// errNoDocRule is the doc read rule never handed over: who may read a doc
// can't be said, so its emails wait.
var errNoDocRule = errors.New("the doc read rule was never handed over")

// verdicts is each row's verdict by its id. A row with no About (an email of
// a kind that isn't asked again, or one queued before rows carried it) is
// sent. A row about several items is asked of each, in its kind's one read,
// and holds only while every one does.
func verdicts(ctx context.Context, items []*emailQueueModels.QueueItem) map[uuid.UUID]verdict {
	out := make(map[uuid.UUID]verdict, len(items))
	byKind := map[string][]*emailQueueModels.QueueItem{}
	asks := map[string][]asked{}
	spans := map[string][]int{} // each row's count of asks, in order
	for _, it := range items {
		var meta struct {
			About *About `json:"about"`
		}
		if json.Unmarshal(it.Metadata, &meta) != nil || meta.About == nil || recheckers[meta.About.Kind] == nil {
			continue
		}
		k := meta.About.Kind
		byKind[k] = append(byKind[k], it)
		of := meta.About.items()
		for _, item := range of {
			one := *meta.About
			one.Item, one.Items = item, nil
			asks[k] = append(asks[k], asked{user: it.UserID.String(), about: one})
		}
		spans[k] = append(spans[k], len(of))
	}
	for k, rows := range byKind {
		holds, err := recheckers[k](ctx, asks[k])
		if err != nil {
			helpers.LogErrorWithContext(ctx, "notification/verdicts couldn't ask %d %s emails again err: %+v", len(rows), k, err)
		}
		at := 0
		for i, it := range rows {
			n := spans[k][i]
			switch {
			case err != nil:
				out[it.ID] = askLater
			case slices.Contains(holds[at:at+n], false):
				out[it.ID] = noLongerTold
			}
			at += n
		}
	}
	return out
}

// standingsIn is where each of asks stands with its item in t's places now
// (domain/ReadState StandingsOf), in one read.
func standingsIn(ctx context.Context, t readStateModels.Table, asks []asked) ([]readStateDomain.Standing, error) {
	pairs := make([]readStateDomain.Asked, len(asks))
	for i, a := range asks {
		pairs[i] = readStateDomain.Asked{User: a.user, Place: a.about.Place, Item: a.about.Item, Reply: a.about.Reply}
	}
	return readStateDomain.StandingsOf(ctx, t, pairs)
}

// stillHeard is whether someone standing at s still hears a: the item is
// live and they haven't read it, they have a setting in its place (someone
// with none isn't in it, as the live path reads it), and it lets a through.
func stillHeard(s readStateDomain.Standing, a asked) bool {
	return s.Unread && s.Level != "" && notifylevel.Lets(s.Level, a.about.Heard)
}

// chatStillHolds is, for emails about messages in DMs and groups, whether
// each still holds: stillHeard, and they are in the conversation now (InNow:
// a DM's two people, a group's as the graph has them). One Postgres read
// and, when there are groups, one graph read.
func chatStillHolds(ctx context.Context, asks []asked) ([]bool, error) {
	var groups []string
	for _, a := range asks {
		if !helpers.IsDMGroupingID(a.about.Place) {
			groups = append(groups, a.about.Place)
		}
	}
	standing, err := standingsIn(ctx, readStateModels.Chat, asks)
	if err != nil {
		return nil, err
	}
	people, err := conversationDomain.PeopleOfMany(ctx, groups)
	if err != nil {
		return nil, err
	}
	holds := make([]bool, len(asks))
	for i, a := range asks {
		in := conversationDomain.InNow(a.about.Place, people[a.about.Place], []string{a.user})
		holds[i] = stillHeard(standing[i], a) && len(in) == 1
	}
	return holds, nil
}

// channelStillHolds is, for emails about posts in channels, whether each
// still holds: stillHeard, where a setting in the channel is its membership
// (business/Notify reads its members so; leaving removes it). One read.
func channelStillHolds(ctx context.Context, asks []asked) ([]bool, error) {
	standing, err := standingsIn(ctx, readStateModels.Channel, asks)
	if err != nil {
		return nil, err
	}
	holds := make([]bool, len(asks))
	for i, a := range asks {
		holds[i] = stillHeard(standing[i], a)
	}
	return holds, nil
}

// taskStillHolds is, for emails about tasks, whether each still holds: the
// task is there and they may read it (domain/Task ReadersOfMany, the task
// read rule), the reply it quotes is still there, and their setting for its
// project lets it through (notifylevel HearsAt: with none, they hear). Tasks
// keep no read marker, so seeing one in the app doesn't stop its email. One
// graph read, one of the restrictions, and one of the settings and replies.
func taskStillHolds(ctx context.Context, asks []asked) ([]bool, error) {
	people := map[string][]string{}
	on := make([]userProjectNotificationDomain.OnTask, len(asks))
	for i, a := range asks {
		people[a.about.Item] = append(people[a.about.Item], a.user)
		on[i] = userProjectNotificationDomain.OnTask{User: a.user, Task: a.about.Item, Reply: a.about.Reply}
	}
	readers, err := taskDomain.ReadersOfMany(ctx, people)
	if err != nil {
		return nil, err
	}
	standing, err := userProjectNotificationDomain.StandingsOnTasks(ctx, on)
	if err != nil {
		return nil, err
	}
	holds := make([]bool, len(asks))
	for i, a := range asks {
		s := standing[i]
		holds[i] = readers[a.about.Item][a.user] && s.ReplyLive && notifylevel.HearsAt(s.Level, s.Set, a.about.Heard)
	}
	return holds, nil
}

// docStillHolds is, for emails about comments on docs, whether each still
// holds: the doc is there and they may read it now (business/Doc's read rule,
// docReaders), and the comment is still there, a member's or a guest's. Docs
// keep no notification level and no read marker for comments, so neither is
// asked. One graph read and one of the grants that end (the rule's), and one
// of the comments.
func docStillHolds(ctx context.Context, asks []asked) ([]bool, error) {
	if docReaders == nil {
		return nil, errNoDocRule
	}
	people := map[string][]string{}
	replies := make([]string, 0, len(asks))
	for _, a := range asks {
		people[a.about.Item] = append(people[a.about.Item], a.user)
		replies = append(replies, a.about.Reply)
	}
	readers, err := docReaders(ctx, people)
	if err != nil {
		return nil, err
	}
	there, err := commentDomain.StillThere(ctx, replies)
	if err != nil {
		return nil, err
	}
	holds := make([]bool, len(asks))
	for i, a := range asks {
		holds[i] = readers[a.about.Item][a.user] && (a.about.Reply == "" || there[strings.ToLower(a.about.Reply)])
	}
	return holds, nil
}

// membersAmongEach is domain/Project MembersAmongEach, a seam: a test counts
// how often a batch reads it.
var membersAmongEach = projectDomain.MembersAmongEach

// projectStillHolds is, for emails about a project's updates, whether each
// still holds: they are in the project now (its members and admins,
// domain/Project MembersAmongEach), the project is there with the update
// still in it, and their setting for the project lets it through
// (notifylevel HearsAt: with none, they hear). Updates keep no read marker,
// so seeing one in the app doesn't stop its email. One graph read, and one of
// the settings and of what is still there.
func projectStillHolds(ctx context.Context, asks []asked) ([]bool, error) {
	people := map[string][]string{}
	in := make([]userProjectNotificationDomain.InProject, len(asks))
	for i, a := range asks {
		people[a.about.Place] = append(people[a.about.Place], a.user)
		in[i] = userProjectNotificationDomain.InProject{User: a.user, Project: a.about.Place, Update: a.about.Item}
	}
	members, err := membersAmongEach(ctx, people)
	if err != nil {
		return nil, err
	}
	standing, err := userProjectNotificationDomain.StandingsInProjects(ctx, in)
	if err != nil {
		return nil, err
	}
	holds := make([]bool, len(asks))
	for i, a := range asks {
		s := standing[i]
		holds[i] = members[a.about.Place][a.user] && s.Live && notifylevel.HearsAt(s.Level, s.Set, a.about.Heard)
	}
	return holds, nil
}
