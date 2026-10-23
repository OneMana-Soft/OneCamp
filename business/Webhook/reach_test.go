package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	webhookModel "github.com/akashc777/OneCamp/models/postgres/Webhook"
	deliveryModel "github.com/akashc777/OneCamp/models/postgres/WebhookDelivery"
	logModel "github.com/akashc777/OneCamp/models/postgres/WebhookLog"
	"github.com/akashc777/OneCamp/models/taskread"
)

// reachWorld is who may read what (audit A9): people in channels, roles in
// projects, docs they can open, the tasks kept from them. Someone not among
// members has left.
type reachWorld struct {
	members    map[uuid.UUID]bool
	channels   map[string]map[uuid.UUID]bool
	roles      map[string]map[uuid.UUID]workAccess.Role
	docs       map[string]map[uuid.UUID]accessBusiness.Level
	trashed    map[string]bool
	hidden     map[uuid.UUID]map[string]bool
	restricted map[string]bool
	failing    error
	asked      map[string]int
}

func stubReach(t *testing.T) *reachWorld {
	t.Helper()
	w := &reachWorld{
		members: map[uuid.UUID]bool{}, channels: map[string]map[uuid.UUID]bool{}, roles: map[string]map[uuid.UUID]workAccess.Role{},
		docs: map[string]map[uuid.UUID]accessBusiness.Level{}, trashed: map[string]bool{}, hidden: map[uuid.UUID]map[string]bool{},
		restricted: map[string]bool{}, asked: map[string]int{},
	}
	sMember, sChannel, sGraph, sProject, sStanding, sDoc, sHidden, sRestricted := memberNow, readsChannel, graphUser, projectAs, standingIn, resolveDoc, hiddenTasks, restrictedTaskEvent
	t.Cleanup(func() {
		memberNow, readsChannel, graphUser, projectAs, standingIn, resolveDoc, hiddenTasks, restrictedTaskEvent = sMember, sChannel, sGraph, sProject, sStanding, sDoc, sHidden, sRestricted
	})
	memberNow = func(_ context.Context, id uuid.UUID) (bool, error) {
		w.asked["member"]++
		return w.members[id], w.failing
	}
	readsChannel = func(_ context.Context, who, channel string) (bool, error) {
		w.asked["channel"]++
		return w.channels[channel][uuid.MustParse(who)], nil
	}
	graphUser = func(_ context.Context, who string) (*dgraphStruct.DgraphUser, error) {
		return &dgraphStruct.DgraphUser{Uid: "0x" + who[:4], Uuid: who}, nil
	}
	projectAs = func(_ context.Context, project, uid string) (*dgraphStruct.DgraphProject, error) {
		return &dgraphStruct.DgraphProject{Uuid: project}, nil
	}
	standingIn = func(_ context.Context, who string, p *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
		w.asked["project"]++
		return workAccess.Standing{ProjectUUID: p.Uuid, Role: w.roles[p.Uuid][uuid.MustParse(who)]}, nil
	}
	resolveDoc = func(_ context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
		if res.Kind != accessBusiness.KindDoc || who.UserUID != "0x"+who.UserUUID[:4] {
			t.Errorf("asked about %+v for %+v", res, who)
		}
		level := w.docs[res.UUID][uuid.MustParse(who.UserUUID)]
		if w.trashed[res.UUID] {
			return accessBusiness.Decision{Deleted: true, Was: level}, nil
		}
		return accessBusiness.Decision{Level: level, Was: level}, nil
	}
	hiddenTasks = func(_ context.Context, who string) (taskread.Filter, error) {
		return taskread.Of(w.hidden[uuid.MustParse(who)]), nil
	}
	restrictedTaskEvent = func(_ context.Context, data map[string]interface{}) bool {
		return w.restricted[taskOfEvent(data)]
	}
	return w
}

// A webhook sends only what its owner may read: the channel or project it is
// set up for, and everything an event names. Nothing else decides it, being a
// workspace admin included.
func TestAWebhookIsHeldToWhatItsOwnerMayRead(t *testing.T) {
	w := stubReach(t)
	inside, outside, gone := uuid.New(), uuid.New(), uuid.New()
	w.members[inside], w.members[outside] = true, true
	private, project, doc, secretTask := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	w.channels[private] = map[uuid.UUID]bool{inside: true}
	w.roles[project] = map[uuid.UUID]workAccess.Role{inside: workAccess.RoleViewer}
	w.docs[doc] = map[uuid.UUID]accessBusiness.Level{inside: accessBusiness.LevelView}
	w.restricted[secretTask] = true
	w.hidden[outside] = map[string]bool{secretTask: true}
	ctx := context.Background()
	privateID := uuid.MustParse(private)

	cases := []struct {
		name         string
		who          uuid.UUID
		scope, event needs
		want         hooksBusiness.Reach
	}{
		{"someone who left", gone, needs{}, needs{}, hooksBusiness.OwnerGone},
		{"a workspace event, for a member", outside, needs{}, needs{}, hooksBusiness.Reaches},
		{"a private channel's webhook, for someone in it", inside, scopeNeeds(ScopeChannel, &privateID), needs{channel: private}, hooksBusiness.Reaches},
		{"a private channel's webhook, for an admin outside it", outside, scopeNeeds(ScopeChannel, &privateID), needs{channel: private}, hooksBusiness.OwnerOutOfScope},
		{"a private channel's event, to a workspace webhook of someone outside it", outside, needs{}, needs{channel: private}, hooksBusiness.OwnerOutOfEvent},
		{"a project's event, for a viewer", inside, needs{}, needs{project: project}, hooksBusiness.Reaches},
		{"a project's event, for someone with no role in it", outside, needs{}, needs{project: project}, hooksBusiness.OwnerOutOfEvent},
		{"a project's webhook, for someone with no role in it", outside, scopeNeeds(ScopeProject, ptr(uuid.MustParse(project))), needs{}, hooksBusiness.OwnerOutOfScope},
		{"a doc's event, for someone who can open it", inside, needs{}, needs{docEvent: true, doc: doc}, hooksBusiness.Reaches},
		{"a doc's event, for someone who can't", outside, needs{}, needs{docEvent: true, doc: doc}, hooksBusiness.OwnerOutOfEvent},
		{"a doc's event that names no doc", inside, needs{}, needs{docEvent: true}, hooksBusiness.OwnerOutOfEvent},
		{"a task kept from them", outside, needs{}, needs{task: secretTask}, hooksBusiness.OwnerOutOfEvent},
		{"a task kept to a few, them among them", inside, needs{}, needs{task: secretTask}, hooksBusiness.Reaches},
	}
	for _, c := range cases {
		got, err := reachOf(ctx, c.who, c.scope, c.event)
		if err != nil || got != c.want {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}

	// A doc in the trash reaches whoever could open it before, and nobody else.
	w.trashed[doc] = true
	for who, want := range map[uuid.UUID]hooksBusiness.Reach{inside: hooksBusiness.Reaches, outside: hooksBusiness.OwnerOutOfEvent} {
		if got, err := reachOf(ctx, who, needs{}, needs{docEvent: true, doc: doc}); err != nil || got != want {
			t.Errorf("a doc in the trash: %v, %v; want %v", got, err, want)
		}
	}

	// A lookup that fails decides nothing.
	w.failing = errors.New("connection refused")
	if _, err := reachOf(ctx, inside, needs{}, needs{}); err == nil {
		t.Error("a failed lookup answered")
	}
}

// What a dispatch reads off an event: its channel, its project, a doc's event's
// doc, and its task only when its project keeps it to a few people.
func TestAnEventNamesWhatItsReaderMustRead(t *testing.T) {
	w := stubReach(t)
	task := uuid.NewString()
	data := map[string]interface{}{"channel_id": " c1 ", "project_id": "p1", "doc_uuid": "d1", "task_id": task}
	if got := needsOf(context.Background(), "task.created", data); got != (needs{channel: "c1", project: "p1"}) {
		t.Errorf("an open task's event needs %+v", got)
	}
	w.restricted[task] = true
	if got := needsOf(context.Background(), "doc.shared", data); got != (needs{channel: "c1", project: "p1", docEvent: true, doc: "d1", task: task}) {
		t.Errorf("a doc's event about a restricted task needs %+v", got)
	}
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }

// dispatchTo stubs the webhooks an event of kind at (scope, entity) is offered
// and answers the names of those it is queued for, with their bodies.
func dispatchTo(t *testing.T, kind, lookup string, entity *uuid.UUID, hooks []*webhookModel.Webhook) func(data map[string]interface{}) map[string]string {
	t.Helper()
	names := map[uuid.UUID]string{}
	for _, h := range hooks {
		names[h.Id] = h.Name
	}
	saved, savedPublic := queueDeliveries, channelIsPublic
	t.Cleanup(func() { queueDeliveries, channelIsPublic = saved, savedPublic; dispatchCache.Purge() })
	channelIsPublic = func(context.Context, uuid.UUID) (bool, error) { return false, nil }
	var sent map[string]string
	queueDeliveries = func(_ context.Context, _ uuid.UUID, _ string, targets []hooksBusiness.Target) error {
		for _, tg := range targets {
			sent[names[tg.WebhookID]] = string(tg.Payload)
		}
		return nil
	}
	dispatchCache.Purge()
	dispatchCache.Set(dispatchCacheKey(kind, lookup, entity), dispatchCacheEntry{generation: atomic.LoadUint64(&dispatchGeneration), webhooks: hooks})
	return func(data map[string]interface{}) map[string]string {
		sent = map[string]string{}
		DispatchEvent(context.Background(), kind, data)
		return sent
	}
}

// Audit A9: a private channel's message reached the webhook of an admin
// outside it, whole when set up for the channel and as ids from the
// workspace. Now it reaches only webhooks whose owners can read the channel;
// one set up for a channel its owner can't read is still queued, so its try
// pauses it and says why (business/Hooks).
func TestAPrivateChannelsEventReachesOnlyWebhooksWhoseOwnersAreInIt(t *testing.T) {
	w := stubReach(t)
	inside, outside := uuid.New(), uuid.New()
	w.members[inside], w.members[outside] = true, true
	channel := uuid.New()
	w.channels[channel.String()] = map[uuid.UUID]bool{inside: true}
	send := dispatchTo(t, "post.created", ScopeChannel, &channel, []*webhookModel.Webhook{
		{Id: uuid.New(), Name: "insider's workspace", CreatedBy: inside, ScopeType: ScopeOrg},
		{Id: uuid.New(), Name: "outsider's workspace", CreatedBy: outside, ScopeType: ScopeOrg},
		{Id: uuid.New(), Name: "insider's channel", CreatedBy: inside, ScopeType: ScopeChannel, ScopeEntityId: &channel},
		{Id: uuid.New(), Name: "outsider's channel", CreatedBy: outside, ScopeType: ScopeChannel, ScopeEntityId: &channel},
	})
	sent := send(map[string]interface{}{"channel_id": channel.String(), "post_id": "p1", "text": "salaries"})
	if _, ok := sent["outsider's workspace"]; ok {
		t.Error("the workspace webhook of an admin outside the channel was sent its ids")
	}
	if body := sent["insider's workspace"]; body == "" || strings.Contains(body, "salaries") {
		t.Errorf("a member's workspace webhook: %q, want the ids", body)
	}
	if !strings.Contains(sent["insider's channel"], "salaries") {
		t.Errorf("the member's own channel webhook: %q, want it whole", sent["insider's channel"])
	}
	if _, ok := sent["outsider's channel"]; !ok {
		t.Error("the outsider's channel webhook wasn't queued, so nothing would pause it")
	}
	if w.asked["channel"] != 2 || w.asked["member"] != 2 {
		t.Errorf("asked %v, want each owner once", w.asked)
	}
}

// Audit A9: a workspace webhook was sent every project's tasks whole, its
// owner in the project or not.
func TestAProjectsEventReachesOnlyWebhooksWhoseOwnersSeeIt(t *testing.T) {
	w := stubReach(t)
	viewer, stranger := uuid.New(), uuid.New()
	w.members[viewer], w.members[stranger] = true, true
	project := uuid.New()
	w.roles[project.String()] = map[uuid.UUID]workAccess.Role{viewer: workAccess.RoleViewer}
	send := dispatchTo(t, "task.created", ScopeProject, &project, []*webhookModel.Webhook{
		{Id: uuid.New(), Name: "viewer's", CreatedBy: viewer, ScopeType: ScopeOrg},
		{Id: uuid.New(), Name: "stranger's", CreatedBy: stranger, ScopeType: ScopeOrg},
	})
	sent := send(map[string]interface{}{"project_id": project.String(), "task_id": uuid.NewString(), "name": "Launch plan"})
	if len(sent) != 1 || !strings.Contains(sent["viewer's"], "Launch plan") {
		t.Fatalf("sent %v, want the viewer's webhook alone, whole", sent)
	}
}

// A doc's event goes only to webhooks whose owners can open the doc, as it did
// through business/Follow's own check (docs programme C4), which this rule
// replaces; someone who has left is still queued, so the try pauses them.
func TestADocsEventReachesOnlyWebhooksWhoseOwnersCanOpenIt(t *testing.T) {
	w := stubReach(t)
	reader, stranger, gone := uuid.New(), uuid.New(), uuid.New()
	w.members[reader], w.members[stranger] = true, true
	doc := uuid.NewString()
	w.docs[doc] = map[uuid.UUID]accessBusiness.Level{reader: accessBusiness.LevelView}
	send := dispatchTo(t, "doc.shared", ScopeOrg, nil, []*webhookModel.Webhook{
		{Id: uuid.New(), Name: "reader's", CreatedBy: reader, ScopeType: ScopeOrg},
		{Id: uuid.New(), Name: "stranger's", CreatedBy: stranger, ScopeType: ScopeOrg},
		{Id: uuid.New(), Name: "former member's", CreatedBy: gone, ScopeType: ScopeOrg},
	})
	sent := send(map[string]interface{}{"doc_uuid": doc, "doc_private": true})
	_, toReader := sent["reader's"]
	_, toStranger := sent["stranger's"]
	_, toGone := sent["former member's"]
	if !toReader || toStranger || !toGone {
		t.Fatalf("sent %v", sent)
	}
}

// A webhook can't be set up for, or moved to, a channel or project its owner
// can't read, and says why in the words of who is asking.
func TestAWebhookCantBeSetUpForWhatItsOwnerCantRead(t *testing.T) {
	w := stubReach(t)
	savedC, savedP := channelIsOpen, projectIsOpen
	t.Cleanup(func() { channelIsOpen, projectIsOpen = savedC, savedP })
	channelIsOpen = func(context.Context, uuid.UUID) (bool, error) { return true, nil }
	projectIsOpen = func(context.Context, uuid.UUID) (bool, error) { return true, nil }
	inside, outside, gone := uuid.New(), uuid.New(), uuid.New()
	w.members[inside], w.members[outside] = true, true
	channel, project := uuid.New(), uuid.New()
	w.channels[channel.String()] = map[uuid.UUID]bool{inside: true}
	w.roles[project.String()] = map[uuid.UUID]workAccess.Role{inside: workAccess.RoleMember}
	ctx := context.Background()

	for _, c := range []struct {
		scope         string
		entity        *uuid.UUID
		owner, actor  uuid.UUID
		refused, said string
	}{
		{ScopeChannel, &channel, inside, inside, "", ""},
		{ScopeProject, &project, inside, outside, "", ""},
		{ScopeChannel, &channel, outside, outside, "refused", "You aren't in that channel"},
		{ScopeProject, &project, outside, outside, "refused", "You aren't in that project"},
		{ScopeChannel, &channel, outside, inside, "refused", "Its owner isn't in that channel"},
		{ScopeChannel, &channel, gone, inside, "refused", "Its owner is no longer a member"},
	} {
		scope, entity, err := webhookScope(ctx, "outgoing", c.scope, c.entity, c.owner, c.actor)
		var said ScopeError
		switch {
		case c.refused == "" && (err != nil || scope != c.scope || entity != c.entity):
			t.Errorf("%s for an owner who can read it: %q %v %v", c.scope, scope, entity, err)
		case c.refused != "" && (!errors.As(err, &said) || !strings.HasPrefix(said.Error(), c.said)):
			t.Errorf("%s: %v, want %q", c.scope, err, c.said)
		}
	}

	w.failing = errors.New("connection refused")
	if _, _, err := webhookScope(ctx, "outgoing", ScopeChannel, &channel, inside, inside); err == nil || errors.As(err, new(ScopeError)) {
		t.Errorf("a failed lookup: %v, want try again", err)
	}
}

// As each delivery is tried, its body is read again for what it names, and a
// nil body asks only about what the webhook is set up for.
func TestADeliveryIsAskedAboutAgainAsItIsTried(t *testing.T) {
	w := stubReach(t)
	outside := uuid.New()
	w.members[outside] = true
	channel := uuid.New()
	ctx := context.Background()
	body, _ := json.Marshal(OutgoingWebhookPayload{Event: "post.created", Data: map[string]interface{}{"channel_id": channel.String()}, EventID: "e1"})
	workspace := deliveryModel.Target{CreatedBy: outside, ScopeType: ScopeOrg}
	if r, err := deliveryReach(ctx, outside, workspace, body); err != nil || r != hooksBusiness.OwnerOutOfEvent {
		t.Errorf("a private channel's event, to a workspace webhook of someone outside it: %v %v", r, err)
	}
	if r, err := deliveryReach(ctx, outside, workspace, nil); err != nil || r != hooksBusiness.Reaches {
		t.Errorf("a workspace webhook as set up: %v %v", r, err)
	}
	channelHook := deliveryModel.Target{CreatedBy: outside, ScopeType: ScopeChannel, ScopeEntity: &channel}
	if r, err := deliveryReach(ctx, outside, channelHook, nil); err != nil || r != hooksBusiness.OwnerOutOfScope {
		t.Errorf("a channel's webhook its owner has left: %v %v", r, err)
	}
	if _, err := deliveryReach(ctx, outside, workspace, []byte("not json")); err == nil {
		t.Error("a body that doesn't read was answered")
	}
}

// Audit A9: a webhook's log showed every admin what it sent, a private
// channel's messages included. A body is now shown whole only to someone who
// may read what it names, and as its ids, with no answer, to anyone else.
func TestALogShowsABodyOnlyToWhoMayReadIt(t *testing.T) {
	w := stubReach(t)
	inside, outside := uuid.New(), uuid.New()
	w.members[inside], w.members[outside] = true, true
	channel := uuid.NewString()
	w.channels[channel] = map[uuid.UUID]bool{inside: true}
	body, _ := json.Marshal(OutgoingWebhookPayload{Event: "post.created", Timestamp: "t", EventID: "e1",
		Data: map[string]interface{}{"channel_id": channel, "post_id": "p1", "text": "salaries"}})
	logs := func() []*logModel.WebhookLog {
		b, answer, junk := string(body), "got: salaries", "not json"
		return []*logModel.WebhookLog{{RequestBody: &b, ResponseBody: &answer}, {RequestBody: &junk, ResponseBody: &answer}, {}}
	}

	mine := logs()
	shownTo(context.Background(), inside, mine)
	if *mine[0].RequestBody != string(body) || mine[0].ResponseBody == nil {
		t.Errorf("someone in the channel: %v %v", *mine[0].RequestBody, mine[0].ResponseBody)
	}

	theirs := logs()
	shownTo(context.Background(), outside, theirs)
	var cut struct {
		Event   string                 `json:"event"`
		EventID string                 `json:"event_id"`
		Data    map[string]interface{} `json:"data"`
	}
	if theirs[0].RequestBody == nil || json.Unmarshal([]byte(*theirs[0].RequestBody), &cut) != nil {
		t.Fatalf("an admin outside the channel: %v", theirs[0].RequestBody)
	}
	if cut.Event != "post.created" || cut.EventID != "e1" || cut.Data["post_id"] != "p1" || cut.Data["text"] != nil || theirs[0].ResponseBody != nil {
		t.Errorf("an admin outside the channel was shown %+v, answer %v", cut, theirs[0].ResponseBody)
	}
	if theirs[1].RequestBody != nil || theirs[1].ResponseBody != nil {
		t.Error("a body that doesn't read was shown")
	}
	if w.asked["channel"] != 2 {
		t.Errorf("asked about the channel %d times, want once a viewer", w.asked["channel"])
	}
}
