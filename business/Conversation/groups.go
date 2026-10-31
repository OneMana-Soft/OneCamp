package business

// Group conversations, done Slack's way (D-S1, D-S8, D-S9; chat A9).
//
// A group is a fixed set of people, named by them (helpers.GenerateGroupID),
// and it is never re-keyed. Adding people opens the conversation of the new
// set (or the one there is for exactly those people), and what was said before
// stays with the people who were there, unless the person adding chooses to
// share it: the last day, the last 7 days or everything, each counted exactly,
// at most shareCap messages and replies (share.go copies them). Both
// conversations say what happened, in a system line. A group can be named and
// hidden by each of its people until a newer message comes, made into a
// private channel, and left (Leave) while it keeps three people.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	lastseenBusiness "github.com/akashc777/OneCamp/business/LastSeenChat"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	chatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Share is how much of a conversation's history the people added get.
type Share string

const (
	ShareNone Share = "none"
	ShareDay  Share = "day"
	ShareWeek Share = "week"
	ShareAll  Share = "all"
)

// since is when a share starts (the zero time for everything); ok is false
// for no share. Pure.
func (s Share) since(now time.Time) (time.Time, bool) {
	switch s {
	case ShareDay:
		return now.Add(-24 * time.Hour), true
	case ShareWeek:
		return now.Add(-7 * 24 * time.Hour), true
	case ShareAll:
		return time.Time{}, true
	}
	return time.Time{}, false
}

// valid is a choice the sheet offers. Pure.
func (s Share) valid() bool {
	return s == ShareNone || s == ShareDay || s == ShareWeek || s == ShareAll
}

const (
	// maxAdded is how many people one Add people takes.
	maxAdded = 50
	// maxGroupName is the longest a group's name may be, in characters.
	maxGroupName = 80
)

// refuse is a request the person can fix, in their words.
func refuse(status int, msg string) error {
	return &sendBusiness.Rejection{Status: status, Msg: msg}
}

// Seams for tests: the graph's people and channels, the clock.
var (
	peopleOf    = conversationDomain.PeopleOf
	channelNode = func(ctx context.Context, id uuid.UUID) (*dgraphStruct.DgraphChannel, error) {
		ch, err := conversationDomain.ChannelNode(ctx, id.String())
		if errors.Is(err, conversationDomain.ErrNoConversation) {
			return nil, errShareNowhere
		}
		return ch, err
	}
	personByUUID  = userBusiness.GetDgraphUserInfoByUUID
	channelPeople = conversationDomain.ChannelPeople
	groupsNow     = time.Now
)

// groupPeopleOf is a DM's or group's people, for a share.
func groupPeopleOf(ctx context.Context, groupingID string) ([]*dgraphStruct.DgraphUser, error) {
	people, err := peopleOf(ctx, groupingID)
	if errors.Is(err, conversationDomain.ErrNoConversation) {
		return nil, errShareNowhere
	}
	return people, err
}

// chatConversation is ref, a DM or a group who may act in at level: its
// decision, or the refusal they get.
func chatConversation(ctx context.Context, who *userModels.UserInfo, ref Ref, level accessBusiness.Level) (accessBusiness.Decision, error) {
	if ref.Kind != KindDM && ref.Kind != KindGroup {
		return accessBusiness.Decision{}, ErrNotFound
	}
	d, err := Resolve(ctx, who, ref)
	if err != nil {
		return d, err
	}
	if d.Level < level {
		return d, refuse(http.StatusForbidden, "You can't change this conversation.")
	}
	return d, nil
}

// refPeople is a DM's or group's people: a DM's two by its id (and the graph's
// account of each), a group's from the graph.
func refPeople(ctx context.Context, ref Ref) ([]*dgraphStruct.DgraphUser, error) {
	if ref.Kind == KindGroup {
		return peopleOf(ctx, ref.ID)
	}
	var out []*dgraphStruct.DgraphUser
	seen := map[string]bool{}
	for _, id := range strings.Fields(ref.ID) {
		if seen[id] {
			continue
		}
		seen[id] = true
		u, err := personByUUID(ctx, id)
		if err != nil {
			return nil, err
		}
		if u != nil && u.Uid != "" {
			out = append(out, u)
		}
	}
	return out, nil
}

// PeopleIn is a conversation's people as the graph has them, whoever asks: a
// channel's members, a DM's two, a group's people. What each may do in it is
// Resolve's to say.
func PeopleIn(ctx context.Context, ref Ref) ([]*dgraphStruct.DgraphUser, error) {
	if ref.Kind == KindChannel {
		return channelPeople(ctx, ref.ID)
	}
	return refPeople(ctx, ref)
}

// ShareChoice is one row of the Add people sheet's "Share earlier messages?":
// what there is to share for that choice, and what a share copies.
type ShareChoice struct {
	Share    Share `json:"share"`
	Messages int   `json:"messages"`
	Replies  int   `json:"replies"`
	// Shared is how many messages and replies a share copies: all of them,
	// or the newest that fit within Cap, the oldest left out.
	Shared int  `json:"shared"`
	Capped bool `json:"capped"`
	Cap    int  `json:"cap"`
}

// shareCounts is the graph's counts, a seam for tests.
var shareCounts = conversationDomain.ShareCounts

// ShareChoices is what each choice shares of ref, for who to choose from.
func ShareChoices(ctx context.Context, who *userModels.UserInfo, ref Ref) ([]ShareChoice, error) {
	if _, err := chatConversation(ctx, who, ref, LevelPost); err != nil {
		return nil, err
	}
	now := groupsNow()
	shares := []Share{ShareDay, ShareWeek, ShareAll}
	since := make([]time.Time, len(shares))
	for i, s := range shares {
		since[i], _ = s.since(now)
	}
	counts, err := shareCounts(ctx, ref.ID, since)
	if err != nil {
		return nil, err
	}
	out := []ShareChoice{{Share: ShareNone, Cap: shareCap}}
	for i, s := range shares {
		c := counts[i]
		out = append(out, ShareChoice{
			Share: s, Messages: c.Messages, Replies: c.Replies,
			Shared: min(c.Total(), shareCap), Capped: c.Total() > shareCap, Cap: shareCap,
		})
	}
	return out, nil
}

// Opened is the conversation people were taken to.
type Opened struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// Existed is a conversation that was there already, for exactly those
	// people.
	Existed bool `json:"existed"`
	// Sharing is how many earlier messages and replies are being copied in.
	Sharing int `json:"sharing"`
}

// shareCutoff is the graph's cutoff, a seam for tests.
var shareCutoff = conversationDomain.ShareCutoff

// AddPeople is "Add people" in a DM or group: it opens the conversation of
// the people there now with add, or the one there is for exactly them, and
// shares the history share chooses. The DM or group people were added from is
// unchanged but for a line that says where it continues.
func AddPeople(ctx context.Context, who *userModels.UserInfo, from Ref, add []string, share Share) (Opened, error) {
	if !share.valid() {
		return Opened{}, refuse(http.StatusBadRequest, "Choose how much of the conversation to share.")
	}
	if _, err := chatConversation(ctx, who, from, LevelPost); err != nil {
		return Opened{}, err
	}
	people, err := refPeople(ctx, from)
	if err != nil {
		return Opened{}, err
	}
	added, err := peopleToAdd(ctx, people, add)
	if err != nil {
		return Opened{}, err
	}
	everyone := append(slices.Clone(people), added...)
	if len(everyone) < 3 {
		return Opened{}, refuse(http.StatusBadRequest, "A group needs at least three people.")
	}
	ids := make([]string, len(everyone))
	for i, p := range everyone {
		ids[i] = p.Uuid
	}
	group, err := helpers.GenerateGroupID(ids)
	if err != nil {
		return Opened{}, refuse(http.StatusBadRequest, "Those people can't be in a group together.")
	}
	actor := &who.UserDgraphInfo
	existing, err := peopleOf(ctx, group)
	existed := err == nil && len(existing) > 0
	if err != nil && !errors.Is(err, conversationDomain.ErrNoConversation) {
		return Opened{}, err
	}
	if !existed {
		if err := openGroup(ctx, actor, from, group, everyone, added); err != nil {
			return Opened{}, err
		}
	}
	opened := Opened{Kind: KindGroup, ID: group, Existed: existed}
	if _, err := writeGroupLine(ctx, from.ID, actor, Line{Kind: LineGroupContinued, Actor: actor.Uuid, Targets: uuidsOfPeople(added), To: group},
		actor.DisplayName()+" added "+names(added)+". The conversation continues with them in a new one.", nil); err != nil {
		// The new conversation is open; the old one only lacks its line.
		helpers.LogErrorWithContext(ctx, "business/Conversation the line about %s couldn't be written in %s: %+v", group, from.ID, err)
	}
	if since, ok := share.since(groupsNow()); ok {
		n, err := startShare(ctx, actor, from, KindGroup, group, since)
		if err != nil {
			return opened, err
		}
		opened.Sharing = n
	}
	return opened, nil
}

// peopleToAdd is the people add names who aren't in people yet, each of whom
// may be one of a group's people (MayBeInGroup).
func peopleToAdd(ctx context.Context, people []*dgraphStruct.DgraphUser, add []string) ([]*dgraphStruct.DgraphUser, error) {
	in := map[string]bool{}
	for _, p := range people {
		in[strings.ToLower(p.Uuid)] = true
	}
	var out []*dgraphStruct.DgraphUser
	for _, raw := range add {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, refuse(http.StatusBadRequest, "Choose people from the list.")
		}
		if in[id.String()] {
			continue
		}
		in[id.String()] = true
		if len(out) == maxAdded {
			return nil, refuse(http.StatusBadRequest, "Add at most 50 people at a time.")
		}
		u, err := personByUUID(ctx, id.String())
		if err != nil {
			return nil, err
		}
		if rj := sendBusiness.MayBeInGroup(u); rj != nil {
			return nil, rj
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil, refuse(http.StatusBadRequest, "Everyone you chose is here already.")
	}
	return out, nil
}

func uuidsOfPeople(ps []*dgraphStruct.DgraphUser) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Uuid)
	}
	return out
}

// openGroup makes the group of everyone, as its first line says: the graph's
// conversation and edges, the index, everyone's read marks (from now: the
// line, and any history shared in, are read) and settings (as they had them
// in from; the people added, everything), and every open app hears of it.
func openGroup(ctx context.Context, actor *dgraphStruct.DgraphUser, from Ref, group string, everyone, added []*dgraphStruct.DgraphUser) error {
	others := slices.DeleteFunc(slices.Clone(everyone), func(p *dgraphStruct.DgraphUser) bool { return p.Uuid == actor.Uuid })
	if _, err := writeGroupLine(ctx, group, actor, Line{Kind: LineGroupStarted, Actor: actor.Uuid, Targets: uuidsOfPeople(added), From: from.ID},
		actor.DisplayName()+" started this conversation with "+names(others)+".", everyone); err != nil {
		return err
	}
	ids := uuidsOfPeople(everyone)
	conversationDomain.Opened(ctx, conversationDomain.KindGroup, group, actor.Uuid, ids, nil)
	now := groupsNow()
	if err := lastseenBusiness.BulkCreateOrUpdateLastSeenChat(ctx, group, ids, now); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Conversation the new group's read marks: %+v", err)
	}
	carryLevels(ctx, from, group, everyone, now)
	mqttBusiness.PublishConversationAccess(mqttStruct.CONVERSATION_KIND_GROUP, group, mqttStruct.CONVERSATION_ADDED, ids...)
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: resourceEventBusiness.ConversationCreated, ResourceKind: KindGroup, ResourceUUID: group,
		Actor: resourceEventBusiness.Actor{UserUUID: actor.Uuid}, Subjects: ids,
		Data: map[string]string{"from": from.ID},
	})
	return nil
}

// carryLevels gives everyone a setting for the new group: the one they had in
// from (someone who muted it keeps it muted), everything for the rest.
func carryLevels(ctx context.Context, from Ref, group string, everyone []*dgraphStruct.DgraphUser, at time.Time) {
	ids := uuidsOfPeople(everyone)
	levels, err := chatNotificationBusiness.GetLevelsInConversation(ctx, from.ID, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Conversation reading the settings to carry over: %+v", err)
		levels = map[string]string{}
	}
	byLevel := map[string][]string{}
	for _, id := range ids {
		level := levels[strings.ToLower(id)]
		if level == "" {
			level = postgressStruct.NOTIFICATION_TYPE_ALL
		}
		byLevel[level] = append(byLevel[level], id)
	}
	for level, people := range byLevel {
		chatNotificationBusiness.BulkCreateChatNotification(ctx, people, group, level, at)
	}
}

// startShare queues the share of from's history since since into the group
// or channel to, as many as fit within the cap, newest first, and says how
// many it copies.
func startShare(ctx context.Context, actor *dgraphStruct.DgraphUser, from Ref, toKind, to string, since time.Time) (int, error) {
	cutoff, err := shareCutoff(ctx, from.ID, since, shareCap)
	if err != nil {
		return 0, err
	}
	if cutoff.After(since) {
		since = cutoff
	}
	counts, err := shareCounts(ctx, from.ID, []time.Time{since})
	if err != nil {
		return 0, err
	}
	total := 0
	if len(counts) == 1 {
		total = min(counts[0].Total(), shareCap)
	}
	if total == 0 {
		return 0, nil
	}
	err = queueShare(ctx, sharePayload{
		From: from.ID, ToKind: toKind, To: to, Since: since, Until: groupsNow(), By: actor.Uuid, Cap: shareCap,
	})
	return total, err
}

// refreshOpenApps has the open apps of a conversation's people read it again:
// a share's copies are in it.
func refreshOpenApps(ctx context.Context, kind, ref string) {
	if kind == KindChannel {
		channelBusiness.NotifyChannelUpdated(ref, channelUpdateHistoryShared)
		return
	}
	people, err := peopleOf(ctx, ref)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Conversation telling %s's open apps of its shared history: %+v", ref, err)
		return
	}
	mqttBusiness.PublishConversationAccess(mqttStruct.CONVERSATION_KIND_GROUP, ref, mqttStruct.CONVERSATION_ADDED, uuidsOfPeople(people)...)
}

// channelUpdateHistoryShared is the channel update a finished share sends.
const channelUpdateHistoryShared = "history_shared"

// SetHidden hides a DM or group for who until a newer message comes, or shows
// it again.
func SetHidden(ctx context.Context, who *userModels.UserInfo, ref Ref, hidden bool) error {
	if _, err := chatConversation(ctx, who, ref, LevelRead); err != nil {
		return err
	}
	return conversationDomain.Hide(ctx, ref.Kind, ref.ID, who.UserDgraphInfo.Uuid, hidden)
}

// minGroupPeople is the fewest people a group has: two are a DM, which every
// list of groups leaves out (dm_participants > 2), so a group of three isn't
// left.
const minGroupPeople = 3

// Seam for tests: where someone is taken out of a group in the graph.
var (
	leaveGroupNode    = chatDomain.LeaveGroup
	forgetChatSetting = chatNotificationBusiness.ForgetChatSetting
)

// Leave takes who out of the group ref, as Microsoft Teams lets a group chat
// be left (Slack only closes one; hiding is that, SetHidden): out of the
// conversation index and the graph together (conversationDomain.Remove, as a
// channel's leaving does), so its messages, notices and sidebar row stop
// reaching them, and their open apps are told. Its people read "Maya left the
// conversation." A group of three can't be left: hiding it, or making it a
// private channel, is the way out. The group keeps its id: whoever starts a
// group with exactly its first people again brings them back into it.
func Leave(ctx context.Context, who *userModels.UserInfo, ref Ref) error {
	if ref.Kind != KindGroup {
		return refuse(http.StatusBadRequest, "Only a group conversation can be left. Hide this one instead.")
	}
	if _, err := chatConversation(ctx, who, ref, LevelRead); err != nil {
		return err
	}
	people, err := peopleOf(ctx, ref.ID)
	if err != nil {
		return err
	}
	me := &who.UserDgraphInfo
	if !slices.ContainsFunc(people, func(p *dgraphStruct.DgraphUser) bool { return p != nil && p.Uid == me.Uid }) {
		return ErrNotFound
	}
	if len(people) <= minGroupPeople {
		return refuse(http.StatusConflict, "A group needs at least three people, so this one can't be left. Hide it instead, or make it a private channel.")
	}
	if err := conversationDomain.Remove(ctx, KindGroup, ref.ID, []string{me.Uuid}, func() error {
		return leaveGroupNode(ctx, ref.ID, me.Uid)
	}); err != nil {
		return err
	}
	// Their setting for the group goes with them: who hears of its messages is
	// read from those settings. One that can't be taken away now is logged:
	// nobody out of the group is told of it (InConversationNow), and the next
	// start's repair takes it away.
	if err := forgetChatSetting(ctx, me.Uuid, ref.ID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Conversation Leave %s: their setting: %+v", ref.ID, err)
	}
	mqttBusiness.PublishConversationAccess(mqttStruct.CONVERSATION_KIND_GROUP, ref.ID, mqttStruct.CONVERSATION_DROPPED, me.Uuid)
	// They have left either way: a line that couldn't be written is logged.
	if _, err := writeGroupLine(ctx, ref.ID, me, Line{Kind: LineGroupLeft, Actor: me.Uuid}, me.DisplayName()+" left the conversation.", nil); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Conversation Leave %s: the line: %+v", ref.ID, err)
	}
	return nil
}

// cleanGroupName is a group's name as it is kept, or a refusal. Pure.
func cleanGroupName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) > maxGroupName {
		return "", refuse(http.StatusBadRequest, "A name can be at most 80 characters.")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", refuse(http.StatusBadRequest, "A name can't hold that character.")
		}
	}
	return name, nil
}

// Rename names a group ("" takes its name away), and says so in a line.
func Rename(ctx context.Context, who *userModels.UserInfo, ref Ref, name string) error {
	if ref.Kind != KindGroup {
		return refuse(http.StatusBadRequest, "Only a group conversation has a name.")
	}
	if _, err := chatConversation(ctx, who, ref, LevelPost); err != nil {
		return err
	}
	name, err := cleanGroupName(name)
	if err != nil {
		return err
	}
	if err := chatDomain.SetGroupName(ctx, ref.ID, name); err != nil {
		return err
	}
	actor := &who.UserDgraphInfo
	text := actor.DisplayName() + " named the conversation “" + name + "”."
	if name == "" {
		text = actor.DisplayName() + " took the conversation’s name away."
	}
	_, err = writeGroupLine(ctx, ref.ID, actor, Line{Kind: LineGroupRenamed, Actor: actor.Uuid, Name: name}, text, nil)
	return err
}

// privateChannelName is the name a private channel is made with, trimmed, or
// the one name rule's refusal in its words (helpers.NameRuleMessage): it said
// "letters, numbers and dashes", which isn't the rule. Pure.
func privateChannelName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !helpers.IsValidName(name) {
		return "", refuse(http.StatusBadRequest, helpers.NameRuleMessage)
	}
	return name, nil
}

// MakePrivateChannel makes the private channel name for the people of a DM
// or group, with add: who makes it is its manager, both say so in a line, the
// group says where it continues, and with copyHistory its history is copied
// into the channel (its people could already read all of it).
func MakePrivateChannel(ctx context.Context, who *userModels.UserInfo, from Ref, add []string, name string, copyHistory bool) (Opened, error) {
	if _, err := chatConversation(ctx, who, from, LevelPost); err != nil {
		return Opened{}, err
	}
	name, err := privateChannelName(name)
	if err != nil {
		return Opened{}, err
	}
	taken, err := channelBusiness.CheckIfChannelExist(ctx, &name)
	if err != nil {
		return Opened{}, err
	}
	if taken {
		return Opened{}, refuse(http.StatusConflict, "There's a channel called that already.")
	}
	people, err := refPeople(ctx, from)
	if err != nil {
		return Opened{}, err
	}
	var added []*dgraphStruct.DgraphUser
	if len(add) > 0 {
		if added, err = peopleToAdd(ctx, people, add); err != nil {
			return Opened{}, err
		}
	}
	err, channel := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{ChannelName: name, ChannelPrivate: true}, who)
	if err != nil {
		return Opened{}, err
	}
	actor := &who.UserDgraphInfo
	var others []*dgraphStruct.DgraphUser
	for _, p := range append(slices.Clone(people), added...) {
		if p.Uuid == actor.Uuid || sendBusiness.MayBeInGroup(p) != nil {
			continue
		}
		id, err := uuid.Parse(p.Uuid)
		if err != nil {
			continue
		}
		if err := channelBusiness.AddChannelMemberEdge(ctx, channel, p, id); err != nil {
			return Opened{}, err
		}
		others = append(others, p)
	}
	ch, err := channelNode(ctx, channel)
	if err != nil {
		return Opened{}, err
	}
	if _, err := writeChannelLine(ctx, channel.String(), ch.Uid, actor, Line{Kind: LineChannelFromGroup, Actor: actor.Uuid, Targets: uuidsOfPeople(others), From: from.ID},
		actor.DisplayName()+" made this channel from the conversation with "+names(others)+"."); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Conversation the new channel's line: %+v", err)
	}
	if _, err := writeGroupLine(ctx, from.ID, actor, Line{Kind: LineGroupBecameChannel, Actor: actor.Uuid, To: channel.String(), Name: name},
		actor.DisplayName()+" continued this conversation in the private channel #"+name+".", nil); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Conversation the group's line about #%s: %+v", name, err)
	}
	if err := conversationDomain.ContinuedIn(ctx, from.Kind, from.ID, channel); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Conversation recording that %s continues in #%s: %+v", from.ID, name, err)
	}
	opened := Opened{Kind: KindChannel, ID: channel.String()}
	if copyHistory {
		n, err := startShare(ctx, actor, from, KindChannel, channel.String(), time.Time{})
		if err != nil {
			return opened, err
		}
		opened.Sharing = n
	}
	return opened, nil
}

// Member is one person in a conversation.
type Member struct {
	UserUUID string `json:"user_uuid"`
	Role     string `json:"role"`
}

// maxMembersPage is the most people one page of a conversation's people holds.
const maxMembersPage = 100

// Members is a page of the conversation's people, after the person after (""
// for the first page), and the after for the next page ("" when this is the
// last), for someone who may read it.
func Members(ctx context.Context, who *userModels.UserInfo, ref Ref, after string, limit int) ([]Member, string, error) {
	if _, err := Resolve(ctx, who, ref); err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > maxMembersPage {
		limit = maxMembersPage
	}
	row, err := models.ByRef(ctx, ref.ID)
	if err != nil {
		return nil, "", err
	}
	if row == nil {
		return nil, "", ErrNotFound
	}
	from := uuid.Nil
	if after != "" {
		if from, err = uuid.Parse(after); err != nil {
			return nil, "", refuse(http.StatusBadRequest, "That page isn't there.")
		}
	}
	rows, err := models.LiveMembers(ctx, row.ID, from, limit+1)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1].UserID.String()
	}
	out := make([]Member, len(rows))
	for i, m := range rows {
		out[i] = Member{UserUUID: m.UserID.String(), Role: m.Role}
	}
	return out, next, nil
}

// MayType is whether who may say they are typing in ref, as it is now:
// someone who may write in it (an announcement channel's members, in its
// threads, included). An archived channel takes no typing.
func MayType(ctx context.Context, who *userModels.UserInfo, ref Ref) bool {
	d, err := Resolve(ctx, who, ref)
	return err == nil && d.Level >= LevelReply
}
