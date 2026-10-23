package business

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Chat"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	domain "github.com/akashc777/OneCamp/domain/Chat"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The golden tests of chat A2's DMs and group messages.
//
// 1. Today's writers (CreateChat, CreateChatForGroup) fire their side effects
//    as goroutines; the durable pipeline runs them from the outbox as sinks,
//    reading the message back. For one message, both must tell the same people
//    the same things: the live message, a new conversation's access event,
//    the search entry, the notices and the workspace event. (The AI
//    edition's embedding is its own sink and file.)
// 2. Today's writers have callers of their own, which the pipeline doesn't
//    replace: each is pinned below by the shape it calls them with, and what
//    that shape tells.

type liveCall struct {
	payload *mqttStruct.MqttChat
	grp     string
}

type accessCall struct {
	kind, grp, change string
	people            []string
}

type chatNotice struct {
	title, body, sender, grp, chat, username, replyTo string
	mentions                                          []string
	told                                              bool
	pushTo                                            []string
}

type chatRecorded struct {
	mu      sync.Mutex
	live    []liveCall
	access  []accessCall
	search  []*openSearchStruct.OpenSearchChat
	files   [][]string
	notices []chatNotice
	events  []map[string]interface{}
}

type chatSnapshot struct {
	live    []liveCall
	access  []accessCall
	search  []*openSearchStruct.OpenSearchChat
	files   [][]string
	notices []chatNotice
	events  []map[string]interface{}
}

func (r *chatRecorded) snapshot() chatSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return chatSnapshot{live: r.live, access: r.access, search: r.search, files: r.files, notices: r.notices, events: r.events}
}

func chatFileIDs(files []*dgraphStruct.DgraphAttachment) []string {
	var ids []string
	for _, f := range files {
		ids = append(ids, f.Uuid)
	}
	return ids
}

// recordChatSinks swaps every seam the fan-out and the sinks go out through for
// a recorder, and puts them back after the test.
func recordChatSinks(t *testing.T, stored *dgraphStruct.DgraphChat, storedErr error) *chatRecorded {
	t.Helper()
	pLive, pAccess, pNotify, pEntry, pIndex, pEvents, pFirst, pRead := publishChat, publishAccess, notifyChat, createChatEntry, indexChat, dispatchChatEvent, firstTime, readChat
	t.Cleanup(func() {
		helpers.SettleBackground(10 * time.Second)
		publishChat, publishAccess, notifyChat, createChatEntry, indexChat, dispatchChatEvent, firstTime, readChat = pLive, pAccess, pNotify, pEntry, pIndex, pEvents, pFirst, pRead
		chatRead = helpers.NewTTLCache[*dgraphStruct.DgraphChat](30 * time.Second)
	})
	rec := &chatRecorded{}
	publishChat = func(p *mqttStruct.MqttChat, grp string) {
		rec.mu.Lock()
		rec.live = append(rec.live, liveCall{p, grp})
		rec.mu.Unlock()
	}
	publishAccess = func(kind, id, change string, people ...string) {
		rec.mu.Lock()
		rec.access = append(rec.access, accessCall{kind, id, change, append([]string(nil), people...)})
		rec.mu.Unlock()
	}
	notifyChat = func(_ context.Context, title, body, sender, grp string, mentions []*dgraphStruct.DgraphUser, chat, username string, _ *string, replyTo string, tell outboxBusiness.Told) {
		var ids []string
		for _, m := range mentions {
			ids = append(ids, m.Uuid)
		}
		rec.mu.Lock()
		rec.notices = append(rec.notices, chatNotice{title: title, body: body, sender: sender, grp: grp, chat: chat, username: username, replyTo: replyTo, mentions: ids, told: tell != nil, pushTo: tell.Only("push", []string{"u-sam", "u-jonas"})})
		rec.mu.Unlock()
	}
	createChatEntry = func(e *openSearchStruct.OpenSearchChat, files []*dgraphStruct.DgraphAttachment) {
		rec.mu.Lock()
		rec.search = append(rec.search, e)
		rec.files = append(rec.files, chatFileIDs(files))
		rec.mu.Unlock()
	}
	indexChat = func(_ context.Context, e *openSearchStruct.OpenSearchChat, files []*dgraphStruct.DgraphAttachment) error {
		createChatEntry(e, files)
		return nil
	}
	dispatchChatEvent = func(_ context.Context, name string, data interface{}) {
		event, _ := data.(map[string]interface{})
		rec.mu.Lock()
		rec.events = append(rec.events, event)
		rec.mu.Unlock()
	}
	firstTime = func(context.Context, outboxBusiness.Event, string, string) bool { return true }
	readChat = func(context.Context, string) (*dgraphStruct.DgraphChat, error) { return stored, storedErr }
	chatRead = helpers.NewTTLCache[*dgraphStruct.DgraphChat](30 * time.Second)
	return rec
}

// goldenChat is one message: Ada writes to Sam (a DM) or to Sam and Jonas (a
// group), mentioning Sam, quoting a message of Jonas's, with a file.
type goldenChat struct {
	kind     string
	id       uuid.UUID
	grp      string
	html     string
	files    []*dgraphStruct.DgraphAttachment
	ada, sam *dgraphStruct.DgraphUser
	people   []*dgraphStruct.DgraphUser
	quoted   *dgraphStruct.DgraphChat
	at       time.Time
	opened   []string
}

func person(uuid, uid, name string, bot bool) *dgraphStruct.DgraphUser {
	key := "profile/" + name + ".png"
	return &dgraphStruct.DgraphUser{Uuid: uuid, Uid: uid, UserName: name, ProfileKey: &key, IsBot: bot}
}

func newGoldenChat(kind string, opened bool) goldenChat {
	ada, sam, jonas := person("u-ada", "0xa", "ada", false), person("u-sam", "0xb", "sam", false), person("u-jonas", "0xc", "jonas", false)
	g := goldenChat{
		kind: kind, id: uuid.New(), html: `<p>launch at 10, <span data-type="mention">@sam</span></p>`,
		files: []*dgraphStruct.DgraphAttachment{{Uuid: "f-1", FileName: "plan.pdf", ObjectKey: "k/plan.pdf"}},
		ada:   ada, sam: sam,
		quoted: &dgraphStruct.DgraphChat{Uid: "0xq", Uuid: "c-quoted", Body: "<p>when?</p>", From: jonas},
		at:     time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
	}
	if kind == mqttStruct.CONVERSATION_KIND_DM {
		g.grp = helpers.GetGroupingId("u-ada", "u-sam")
		if opened {
			g.opened = []string{"u-ada", "u-sam"}
		}
	} else {
		g.grp = "grp-launch"
		g.people = []*dgraphStruct.DgraphUser{ada, sam, jonas}
		if opened {
			g.opened = []string{"u-ada", "u-sam", "u-jonas"}
		}
	}
	return g
}

// today is the message as today's writers hand it to their fan-out.
func (g goldenChat) today() createdChat {
	c := createdChat{
		kind: g.kind, id: g.id.String(), grp: g.grp, html: g.html, files: g.files, author: g.ada,
		opened: g.opened, mentions: []*dgraphStruct.DgraphUser{g.sam}, replyTo: g.quoted, at: g.at,
	}
	if g.kind == mqttStruct.CONVERSATION_KIND_DM {
		c.to = g.sam
	} else {
		c.people = g.people
	}
	return c
}

// stored is the message as the graph holds it, edited to html and deleted at
// deleted (nil for live).
func (g goldenChat) stored(html string, deleted *time.Time) *dgraphStruct.DgraphChat {
	zero := time.Time{}
	if deleted == nil {
		deleted = &zero
	}
	at := g.at
	c := &dgraphStruct.DgraphChat{
		Uuid: g.id.String(), Body: html, MediaObj: g.files, From: g.ada,
		DM:          &dgraphStruct.DgraphDm{GroupingId: g.grp, Participants: g.people},
		ReplyToChat: g.quoted, Mentions: &dgraphStruct.DgraphMentions{Mentions: []*dgraphStruct.DgraphUser{g.sam}},
		CreatedAt: &at, DeletedAt: deleted,
	}
	if g.kind == mqttStruct.CONVERSATION_KIND_DM {
		c.To = g.sam
	}
	return c
}

// event is the message's event as the outbox hands it to a sink.
func (g goldenChat) event() outboxBusiness.Event {
	entities := map[string]string{"message": g.id.String(), "conversation": g.grp, "kind": g.kind}
	if len(g.opened) > 0 {
		entities["opened"] = strings.Join(g.opened, ",")
	}
	return outboxBusiness.Event{EventID: uuid.NewString(), Topic: MessageTopic, Kind: MessageCreated, Key: g.kind + ":" + g.grp, Entities: entities, Attempt: 1}
}

func runChatSinks(t *testing.T, g goldenChat) {
	t.Helper()
	ev := g.event()
	for _, run := range []func(context.Context, outboxBusiness.Event) error{chatAccessSink, chatRealtimeSink, chatSearchSink, chatNotifySink, chatEventsSink} {
		if err := run(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheDurableChatSinksTellWhatTodaysWritersTell(t *testing.T) {
	for _, c := range []struct {
		name   string
		kind   string
		opened bool
	}{
		{"a DM", mqttStruct.CONVERSATION_KIND_DM, false},
		{"a first DM", mqttStruct.CONVERSATION_KIND_DM, true},
		{"a group message", mqttStruct.CONVERSATION_KIND_GROUP, false},
		{"a new group's first message", mqttStruct.CONVERSATION_KIND_GROUP, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newGoldenChat(c.kind, c.opened)
			today := recordChatSinks(t, nil, nil)
			fanOutCreatedChat(context.Background(), g.today())
			helpers.SettleBackground(10 * time.Second)
			was := today.snapshot()

			sinks := recordChatSinks(t, g.stored(g.html, nil), nil)
			runChatSinks(t, g)
			now := sinks.snapshot()

			if len(was.live) != 1 || len(now.live) != 1 || !reflect.DeepEqual(was.live[0], now.live[0]) {
				t.Errorf("the live message differs:\n today %+v\n sinks %+v", was.live, now.live)
			}
			if !reflect.DeepEqual(was.access, now.access) || len(was.access) != map[bool]int{true: 1, false: 0}[c.opened] {
				t.Errorf("the access event differs:\n today %+v\n sinks %+v", was.access, now.access)
			}
			if !reflect.DeepEqual(was.search, now.search) || !reflect.DeepEqual(was.files, now.files) {
				t.Errorf("the search entry differs:\n today %+v %v\n sinks %+v %v", was.search, was.files, now.search, now.files)
			}
			if len(was.notices) != 1 || len(now.notices) != 1 {
				t.Fatalf("notices: today %d, sinks %d", len(was.notices), len(now.notices))
			}
			wn, nn := was.notices[0], now.notices[0]
			if wn.told || !nn.told {
				t.Errorf("today's writers tell everyone (no claim), the sink asks FirstTime: today %v, sink %v", wn.told, nn.told)
			}
			wn.told, nn.told = false, false
			if !reflect.DeepEqual(wn, nn) {
				t.Errorf("the notices differ:\n today %+v\n sinks %+v", wn, nn)
			}
			if !reflect.DeepEqual(was.events, now.events) {
				t.Errorf("the workspace event differs:\n today %v\n sinks %v", was.events, now.events)
			}
		})
	}
}

func TestTheChatSinksSkipAMessageDeletedOrGoneSinceAndAreDone(t *testing.T) {
	g := newGoldenChat(mqttStruct.CONVERSATION_KIND_DM, false)
	deleted := g.at.Add(time.Minute)
	for name, read := range map[string]func() (*dgraphStruct.DgraphChat, error){
		"deleted": func() (*dgraphStruct.DgraphChat, error) { return g.stored(g.html, &deleted), nil },
		"gone":    func() (*dgraphStruct.DgraphChat, error) { return nil, domain.ErrChatGone },
	} {
		stored, err := read()
		rec := recordChatSinks(t, stored, err)
		runChatSinks(t, g) // every sink answers done (nil)
		got := rec.snapshot()
		if len(got.live)+len(got.search)+len(got.notices)+len(got.events) != 0 {
			t.Errorf("%s: a message that isn't there any more was still sent on: live %d, search %d, notices %d, events %d",
				name, len(got.live), len(got.search), len(got.notices), len(got.events))
		}
	}
}

func TestTheChatSinksSendAnEditedMessagesWordsAsTheyAreNow(t *testing.T) {
	g := newGoldenChat(mqttStruct.CONVERSATION_KIND_GROUP, false)
	rec := recordChatSinks(t, g.stored("<p>launch moved to 11</p>", nil), nil)
	runChatSinks(t, g)
	got := rec.snapshot()
	if len(got.live) != 1 || got.live[0].payload.ChatHtmlText != "<p>launch moved to 11</p>" {
		t.Fatalf("live: %+v", got.live)
	}
	if got.search[0].ChatBody != "launch moved to 11" || got.notices[0].body != "launch moved to 11" || got.events[0]["text"] != "launch moved to 11" {
		t.Errorf("search %q, notice %q, event %v: each should carry the words as they are now", got.search[0].ChatBody, got.notices[0].body, got.events[0]["text"])
	}
}

func TestTheChatNotifySinkTellsEachPersonOnceAcrossRetries(t *testing.T) {
	g := newGoldenChat(mqttStruct.CONVERSATION_KIND_DM, false)
	rec := recordChatSinks(t, g.stored(g.html, nil), nil)
	seen := map[string]bool{}
	var mu sync.Mutex
	firstTime = func(_ context.Context, ev outboxBusiness.Event, sink, recipient string) bool {
		mu.Lock()
		defer mu.Unlock()
		k := ev.EventID + "|" + sink + "|" + recipient
		if seen[k] {
			return false
		}
		seen[k] = true
		return true
	}
	ev := g.event()
	for try := 0; try < 2; try++ {
		if err := chatNotifySink(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	got := rec.snapshot()
	if len(got.notices) != 2 || !reflect.DeepEqual(got.notices[0].pushTo, []string{"u-sam", "u-jonas"}) || len(got.notices[1].pushTo) != 0 {
		t.Errorf("push on the first try %v and the retry %v: the retry should tell nobody again", got.notices[0].pushTo, got.notices[1].pushTo)
	}
	if !seen[ev.EventID+"|"+sinkChatNotify+":push|u-sam"] {
		t.Error("the claim isn't per (event, sink, recipient)")
	}
}

// A post's events never run the chat sinks' side effects, and a chat's never
// run the access sink unless it made its conversation.
func TestTheChatSinksPassOverEventsThatArentTheirs(t *testing.T) {
	g := newGoldenChat(mqttStruct.CONVERSATION_KIND_DM, false)
	rec := recordChatSinks(t, g.stored(g.html, nil), nil)
	post := outboxBusiness.Event{EventID: uuid.NewString(), Topic: MessageTopic, Entities: map[string]string{"message": g.id.String(), "conversation": "c1", "kind": "channel", "opened": "u-ada"}}
	for _, run := range []func(context.Context, outboxBusiness.Event) error{chatAccessSink, chatRealtimeSink, chatSearchSink, chatNotifySink, chatEventsSink} {
		if err := run(context.Background(), post); err != nil {
			t.Fatal(err)
		}
	}
	if err := chatAccessSink(context.Background(), g.event()); err != nil {
		t.Fatal(err)
	}
	got := rec.snapshot()
	if len(got.live)+len(got.access)+len(got.search)+len(got.notices)+len(got.events) != 0 {
		t.Errorf("sent on: %+v", got)
	}
}

// --------------------------------------------------------- today's callers

// writerSteps swaps the steps today's writers take before their fan-out (the
// row, the node, a new conversation's setup, the sender's read mark) for fakes.
type writerSteps struct {
	mu      sync.Mutex
	dmUID   string
	started []string
	reads   []string
}

func fakeWriterSteps(t *testing.T, dmUID string, quoted *dgraphStruct.DgraphChat) *writerSteps {
	t.Helper()
	s := &writerSteps{dmUID: dmUID}
	pRow, pDM, pGroup, pStartDM, pStartGroup, pRead := createChatRow, writeDM, writeGroup, startNewDM, startNewGroup, markRead
	t.Cleanup(func() {
		createChatRow, writeDM, writeGroup, startNewDM, startNewGroup, markRead = pRow, pDM, pGroup, pStartDM, pStartGroup, pRead
	})
	createChatRow = func(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }
	writeDM = func(_ context.Context, _ uuid.UUID, _ *adapter.ChatInfo, _ *userModels.UserInfo, _ *dgraphStruct.DgraphUser, _ string, _ []*dgraphStruct.DgraphUser, _ time.Time) (*dgraphStruct.DgraphChat, string, error) {
		return quoted, s.dmUID, nil
	}
	writeGroup = func(_ context.Context, _ uuid.UUID, _ *adapter.ChatInfo, _ *userModels.UserInfo, _ []*dgraphStruct.DgraphUser, _ []*dgraphStruct.DgraphUser, _ time.Time) (*dgraphStruct.DgraphChat, string, error) {
		return quoted, s.dmUID, nil
	}
	startNewDM = func(_ context.Context, _ *userModels.UserInfo, to *dgraphStruct.DgraphUser, _ uuid.UUID, grp string, _ time.Time) {
		s.mu.Lock()
		s.started = append(s.started, "dm "+grp+" with "+to.Uuid)
		s.mu.Unlock()
	}
	startNewGroup = func(_ context.Context, _ *userModels.UserInfo, grp string, people []string, _ time.Time) {
		s.mu.Lock()
		s.started = append(s.started, "group "+grp+" of "+strings.Join(people, ","))
		s.mu.Unlock()
	}
	markRead = func(_ context.Context, _, grp string, who *userModels.UserInfo, _ time.Time) {
		s.mu.Lock()
		s.reads = append(s.reads, who.UserDgraphInfo.Uuid+" read "+grp)
		s.mu.Unlock()
	}
	return s
}

func asUser(u *dgraphStruct.DgraphUser) *userModels.UserInfo {
	info := &userModels.UserInfo{UserDgraphInfo: *u}
	info.UserPostgresInfo.Id = uuid.MustParse(u.Uuid)
	return info
}

// toldOf is what a recorded fan-out told, in words a failure can show.
func toldOf(s chatSnapshot) string {
	var out []string
	for _, l := range s.live {
		out = append(out, "live to "+l.grp+" by "+l.payload.ChatByUserUuid+map[bool]string{true: " (bot)", false: ""}[l.payload.ChatByIsBot])
	}
	for _, a := range s.access {
		out = append(out, "access "+a.kind+" "+a.change+" for "+strings.Join(a.people, ","))
	}
	for _, e := range s.search {
		var who []string
		for _, p := range e.ChatParticipants {
			who = append(who, p.Uuid)
		}
		out = append(out, "search findable by ["+strings.Join(who, ",")+"]")
	}
	for _, n := range s.notices {
		out = append(out, "notices "+n.title+" mentioning ["+strings.Join(n.mentions, ",")+"]")
	}
	for _, e := range s.events {
		keys := make([]string, 0, len(e))
		for k := range e {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out = append(out, "event chat.created with "+strings.Join(keys, ","))
	}
	return strings.Join(out, "; ")
}

// Today's writers, called as each of their callers calls them, tell what they
// told before A2. The callers, by the shape they call with:
//
//	shape                          callers
//	a person's DM                  /dm/createChat with the switch off (business/Send), AI executors' send_dm
//	                               (business/AI aiExecutors.go:246), meeting recaps (business/AI meetingRecapAgent.go:546)
//	a first DM                     the same, to someone never written to
//	a bot's DM                     the AI coworker (AICoworker aiCoworker.go:634, agentNote.go:224, dmStream.go:278),
//	                               ambient private replies (AIAgent agentAmbientPrivate.go:146)
//	an import's DM                 the demo seed's read receipt (DemoSeed readReceipt.go:45, BulkImport context)
//	a group message with people    /groupChat/createChat with the switch off, AI executors (aiExecutors.go:314),
//	                               meeting recaps (meetingRecapAgent.go:512), the AI coworker (aiCoworker.go:609)
//	a group message without them   agents' group DMs (AICoworker agentDM.go:228)
//	a new group's first message    /groupChat/createChat with participants and the switch off
func TestTodaysChatWritersTellAsTheyDidForEachCaller(t *testing.T) {
	ada, sam, jonas := person("8d5f8a8e-0000-4000-8000-00000000000a", "0xa", "ada", false), person("8d5f8a8e-0000-4000-8000-00000000000b", "0xb", "sam", false), person("8d5f8a8e-0000-4000-8000-00000000000c", "0xc", "jonas", false)
	bot := person("8d5f8a8e-0000-4000-8000-0000000000bb", "0xbb", "onecamp-ai", true)
	dm := helpers.GetGroupingId(ada.Uuid, sam.Uuid)
	botDM := helpers.GetGroupingId(bot.Uuid, sam.Uuid)
	people := []*dgraphStruct.DgraphUser{ada, sam, jonas}
	ids := ada.Uuid + "," + sam.Uuid + "," + jonas.Uuid
	newGrp, _ := helpers.GenerateGroupID([]string{ada.Uuid, sam.Uuid, jonas.Uuid})
	for _, c := range []struct {
		name    string
		import_ bool
		dmUID   string
		send    func(ctx context.Context) error
		want    string
		started string
		read    string
	}{
		{
			name: "a person's DM", send: func(ctx context.Context) error {
				_, err := CreateChat(ctx, &adapter.ChatInfo{ToUuid: sam.Uuid, TextHtml: "<p>hi</p>"}, asUser(ada), sam, uuid.MustParse(sam.Uuid), []*dgraphStruct.DgraphUser{sam})
				return err
			},
			want: "live to " + dm + " by " + ada.Uuid + "; search findable by [" + ada.Uuid + "," + sam.Uuid + "]; notices Dm - ada mentioning [" + sam.Uuid + "]; event chat.created with group_id,mention_ids,message_id,receiver_id,sender_id,text",
			read: ada.Uuid + " read " + dm,
		},
		{
			name: "a first DM", dmUID: "0xdm", send: func(ctx context.Context) error {
				_, err := CreateChat(ctx, &adapter.ChatInfo{ToUuid: sam.Uuid, TextHtml: "<p>hi</p>"}, asUser(ada), sam, uuid.MustParse(sam.Uuid), nil)
				return err
			},
			want:    "live to " + dm + " by " + ada.Uuid + "; access dm added for " + ada.Uuid + "," + sam.Uuid + "; search findable by [" + ada.Uuid + "," + sam.Uuid + "]; notices Dm - ada mentioning []; event chat.created with group_id,mention_ids,message_id,receiver_id,sender_id,text",
			started: "dm " + dm + " with " + sam.Uuid,
			read:    ada.Uuid + " read " + dm,
		},
		{
			name: "a bot's DM", send: func(ctx context.Context) error {
				_, err := CreateChat(ctx, &adapter.ChatInfo{ToUuid: sam.Uuid, TextHtml: "<p>here is the summary</p>"}, asUser(bot), sam, uuid.MustParse(sam.Uuid), nil)
				return err
			},
			want: "live to " + botDM + " by " + bot.Uuid + " (bot); search findable by [" + bot.Uuid + "," + sam.Uuid + "]; notices Dm - onecamp-ai mentioning []; event chat.created with group_id,mention_ids,message_id,receiver_id,sender_id,text",
			read: bot.Uuid + " read " + botDM,
		},
		{
			name: "an import's DM", import_: true, send: func(ctx context.Context) error {
				_, err := CreateChat(ctx, &adapter.ChatInfo{ToUuid: sam.Uuid, TextHtml: "<p>did you read it?</p>"}, asUser(ada), sam, uuid.MustParse(sam.Uuid), nil)
				return err
			},
			want: "search findable by [" + ada.Uuid + "," + sam.Uuid + "]",
			read: ada.Uuid + " read " + dm,
		},
		{
			name: "a group message with its people", send: func(ctx context.Context) error {
				_, err := CreateChatForGroup(ctx, &adapter.ChatInfo{GrpUuid: "grp-launch", TextHtml: "<p>recap</p>"}, asUser(ada), nil, people)
				return err
			},
			want: "live to grp-launch by " + ada.Uuid + "; search findable by [" + ids + "]; notices Group - ada mentioning []; event chat.created with group_id,mention_ids,message_id,participant_ids,sender_id,text",
			read: ada.Uuid + " read grp-launch",
		},
		{
			// Indexed with nobody to find it, as before A2: filed, not changed here.
			name: "a group message without its people", send: func(ctx context.Context) error {
				_, err := CreateChatForGroup(ctx, &adapter.ChatInfo{GrpUuid: "grp-launch", TextHtml: "<p>[build] green</p>"}, asUser(bot), nil, nil)
				return err
			},
			want: "live to grp-launch by " + bot.Uuid + " (bot); search findable by []; notices Group - onecamp-ai mentioning []; event chat.created with group_id,mention_ids,message_id,participant_ids,sender_id,text",
			read: bot.Uuid + " read grp-launch",
		},
		{
			name: "a new group's first message", dmUID: "0xgrp", send: func(ctx context.Context) error {
				_, err := CreateChatForGroup(ctx, &adapter.ChatInfo{Participants: []string{ada.Uuid, sam.Uuid, jonas.Uuid}, TextHtml: "<p>welcome</p>"}, asUser(ada), nil, people)
				return err
			},
			want:    "live to " + newGrp + " by " + ada.Uuid + "; access group added for " + ids + "; search findable by [" + ids + "]; notices Group - ada mentioning []; event chat.created with group_id,mention_ids,message_id,participant_ids,sender_id,text",
			started: "group " + newGrp + " of " + ids,
			read:    ada.Uuid + " read " + newGrp,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := recordChatSinks(t, nil, nil)
			steps := fakeWriterSteps(t, c.dmUID, nil)
			ctx := context.Background()
			if c.import_ {
				ctx = context.WithValue(ctx, helpers.BulkImportContextKey, true)
			}
			if err := c.send(ctx); err != nil {
				t.Fatal(err)
			}
			helpers.SettleBackground(10 * time.Second)
			if got := toldOf(rec.snapshot()); got != c.want {
				t.Errorf("told:\n got  %s\n want %s", got, c.want)
			}
			if got := strings.Join(steps.started, "; "); got != c.started {
				t.Errorf("set up %q, want %q", got, c.started)
			}
			if got := strings.Join(steps.reads, "; "); got != c.read {
				t.Errorf("read marks %q, want %q", got, c.read)
			}
		})
	}
}
