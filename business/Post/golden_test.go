package business

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	domain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The golden test of chat A2's channel posts: today's CreatePost fires its side
// effects as goroutines; the durable pipeline runs them from the outbox as
// sinks, reading the post back. For one post, both must tell the same people
// the same things: the live message, the search entry, the notices and the
// workspace event. (The AI edition's embedding is its own sink and file.)

type noticeCall struct {
	title, body, channel, post, author, replyTo string
	mentions                                    []string
	told                                        bool
	pushTo                                      []string
}

type recorded struct {
	mu      sync.Mutex
	live    []*mqttStruct.MqttPost
	liveTo  []string
	search  []*openSearchStruct.OpenSearchPost
	files   [][]string
	notices []noticeCall
	events  []map[string]interface{}
}

func (r *recorded) snapshot() recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return recorded{live: r.live, liveTo: r.liveTo, search: r.search, files: r.files, notices: r.notices, events: r.events}
}

func fileIDs(files []*dgraphStruct.DgraphAttachment) []string {
	var ids []string
	for _, f := range files {
		ids = append(ids, f.Uuid)
	}
	return ids
}

// recordSinks swaps every seam the fan-out and the sinks go out through for a
// recorder, and puts them back after the test.
func recordSinks(t *testing.T, stored *dgraphStruct.DgraphPost, storedErr error) *recorded {
	t.Helper()
	rec := &recorded{}
	prev := []any{publishPost, createSearchEntry, indexPost, notifyPost, dispatchPost, firstTime, readPost}
	publishPost = func(p *mqttStruct.MqttPost, ch string) {
		rec.mu.Lock()
		rec.live, rec.liveTo = append(rec.live, p), append(rec.liveTo, ch)
		rec.mu.Unlock()
	}
	createSearchEntry = func(p *openSearchStruct.OpenSearchPost, files []*dgraphStruct.DgraphAttachment) {
		rec.mu.Lock()
		rec.search, rec.files = append(rec.search, p), append(rec.files, fileIDs(files))
		rec.mu.Unlock()
	}
	indexPost = func(_ context.Context, p *openSearchStruct.OpenSearchPost, files []*dgraphStruct.DgraphAttachment) error {
		createSearchEntry(p, files)
		return nil
	}
	notifyPost = func(_ context.Context, title, body, _, ch string, mentions []*dgraphStruct.DgraphUser, _ *dgraphStruct.DgraphChannel, post string, author *dgraphStruct.DgraphUser, replyTo string, tell outboxBusiness.Told) {
		var ids []string
		for _, m := range mentions {
			ids = append(ids, m.Uuid)
		}
		rec.mu.Lock()
		rec.notices = append(rec.notices, noticeCall{title: title, body: body, channel: ch, post: post, author: author.Uuid, replyTo: replyTo, mentions: ids, told: tell != nil, pushTo: tell.Only("push", []string{"u-sam", "u-jonas"})})
		rec.mu.Unlock()
	}
	dispatchPost = func(_ context.Context, kind string, data interface{}) {
		rec.mu.Lock()
		rec.events = append(rec.events, data.(map[string]interface{}))
		rec.mu.Unlock()
	}
	firstTime = func(context.Context, outboxBusiness.Event, string, string) bool { return true }
	readPost = func(context.Context, string) (*dgraphStruct.DgraphPost, error) { return stored, storedErr }
	t.Cleanup(func() {
		publishPost = prev[0].(func(*mqttStruct.MqttPost, string))
		createSearchEntry = prev[1].(func(*openSearchStruct.OpenSearchPost, []*dgraphStruct.DgraphAttachment))
		indexPost = prev[2].(func(context.Context, *openSearchStruct.OpenSearchPost, []*dgraphStruct.DgraphAttachment) error)
		notifyPost = prev[3].(func(context.Context, string, string, string, string, []*dgraphStruct.DgraphUser, *dgraphStruct.DgraphChannel, string, *dgraphStruct.DgraphUser, string, outboxBusiness.Told))
		dispatchPost = prev[4].(func(context.Context, string, interface{}))
		firstTime = prev[5].(func(context.Context, outboxBusiness.Event, string, string) bool)
		readPost = prev[6].(func(context.Context, string) (*dgraphStruct.DgraphPost, error))
	})
	return rec
}

// A post as both paths see it: what the person sent, and what the graph holds.
type goldenPost struct {
	id      uuid.UUID
	at      time.Time
	in      *adapter.InputCreateOrUpdatePostInfo
	user    *userModels.UserInfo
	mention []*dgraphStruct.DgraphUser
	channel *dgraphStruct.DgraphChannel
	parent  *dgraphStruct.DgraphPost
}

func newGolden() goldenPost {
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	author := dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "u-maya", UserName: "Maya", UserFullName: "Maya Chen", ProfileKey: strp("maya.png")}
	return goldenPost{
		id: uuid.MustParse("2b0e7f1a-1c2d-4e3f-8a9b-0c1d2e3f4a5b"),
		at: at,
		in: &adapter.InputCreateOrUpdatePostInfo{
			HTMLText:    `<p>launch at 10 <span data-type="mention" data-id="0x2">@Sam</span></p>`,
			MediaObj:    []*dgraphStruct.DgraphAttachment{{Uuid: "f-1", FileName: "plan.pdf", ObjectKey: "k-1"}},
			ChannelUuid: "c-launch",
		},
		user:    &userModels.UserInfo{UserDgraphInfo: author},
		mention: []*dgraphStruct.DgraphUser{{Uid: "0x2", Uuid: "u-sam"}},
		channel: &dgraphStruct.DgraphChannel{Uid: "0xc", Uuid: "c-launch", Name: "launch"},
		parent:  &dgraphStruct.DgraphPost{Uid: "0x9", Uuid: "p-0", Text: "<p>when?</p>", PostBy: &dgraphStruct.DgraphUser{Uuid: "u-jonas", UserName: "Jonas"}},
	}
}

// stored is the post as the graph holds it once written: what the sinks read.
func (g goldenPost) stored(text string, deleted *time.Time) *dgraphStruct.DgraphPost {
	zero := time.Time{}
	if deleted == nil {
		deleted = &zero
	}
	author := g.user.UserDgraphInfo
	parent := *g.parent
	parent.DeletedAt = &zero
	return &dgraphStruct.DgraphPost{
		Uid: "0x77", Uuid: g.id.String(), Text: text, MediaObj: g.in.MediaObj,
		CreatedAt: &g.at, DeletedAt: deleted, PostBy: &author,
		Channel:     &dgraphStruct.DgraphChannel{Uid: g.channel.Uid, Uuid: g.channel.Uuid, Name: g.channel.Name},
		ReplyToPost: &parent,
		Mentions:    &dgraphStruct.DgraphMentions{Mentions: g.mention},
	}
}

func runSinks(t *testing.T, g goldenPost) {
	t.Helper()
	ev := outboxBusiness.Event{EventID: uuid.NewString(), Topic: MessageTopic, Kind: MessageCreated, Key: "channel:c-launch",
		Entities: map[string]string{"message": g.id.String(), "conversation": "c-launch", "kind": "channel"}, Attempt: 1}
	for _, run := range []func(context.Context, outboxBusiness.Event) error{realtimeSink, searchSink, notifySink, eventsSink} {
		if err := run(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheDurableSinksTellWhatTodaysCreatePostTells(t *testing.T) {
	g := newGolden()
	today := recordSinks(t, nil, nil)
	fanOutCreatedPost(context.Background(), g.id, g.in, g.user, g.mention, g.channel, g.parent, g.at)
	helpers.SettleBackground(10 * time.Second)
	was := today.snapshot()

	sinks := recordSinks(t, g.stored(g.in.HTMLText, nil), nil)
	runSinks(t, g)
	now := sinks.snapshot()

	if len(was.live) != 1 || len(now.live) != 1 {
		t.Fatalf("live messages: today %d, sinks %d", len(was.live), len(now.live))
	}
	a, b := was.live[0], now.live[0]
	if a.Type != b.Type || a.PostHtmlText != b.PostHtmlText || !a.PostCreatedAt.Equal(*b.PostCreatedAt) ||
		a.PostByUserUuid != b.PostByUserUuid || a.PostByProfileKey != b.PostByProfileKey || a.PostByUserName != b.PostByUserName ||
		a.PostByIsBot != b.PostByIsBot || a.PostChannelUuid != b.PostChannelUuid || a.PostUuid != b.PostUuid ||
		a.PostReplyTo.Uuid != b.PostReplyTo.Uuid || !reflect.DeepEqual(fileIDs(a.PostAttachments), fileIDs(b.PostAttachments)) ||
		was.liveTo[0] != now.liveTo[0] {
		t.Errorf("the live message differs:\n today %+v\n sinks %+v", a, b)
	}
	if !reflect.DeepEqual(was.search, now.search) || !reflect.DeepEqual(was.files, now.files) {
		t.Errorf("the search entry differs:\n today %+v %v\n sinks %+v %v", was.search, was.files, now.search, now.files)
	}
	if len(was.notices) != 1 || len(now.notices) != 1 {
		t.Fatalf("notices: today %d, sinks %d", len(was.notices), len(now.notices))
	}
	wn, nn := was.notices[0], now.notices[0]
	if wn.title != nn.title || wn.body != nn.body || wn.channel != nn.channel || wn.post != nn.post || wn.author != nn.author ||
		wn.replyTo != nn.replyTo || !reflect.DeepEqual(wn.mentions, nn.mentions) {
		t.Errorf("the notices differ:\n today %+v\n sinks %+v", wn, nn)
	}
	// The sinks tell each person once (FirstTime); today tells everyone each run.
	if wn.told || !nn.told {
		t.Error("the sinks should ask before telling each person, and today's path not")
	}
	if !reflect.DeepEqual(was.events, now.events) {
		t.Errorf("the workspace event differs:\n today %v\n sinks %v", was.events, now.events)
	}
}

func TestTheSinksSkipAPostDeletedOrGoneSinceAndAreDone(t *testing.T) {
	g := newGolden()
	deleted := g.at.Add(time.Minute)
	for name, read := range map[string]func() (*dgraphStruct.DgraphPost, error){
		"deleted": func() (*dgraphStruct.DgraphPost, error) { return g.stored(g.in.HTMLText, &deleted), nil },
		"gone":    func() (*dgraphStruct.DgraphPost, error) { return nil, domain.ErrPostGone },
	} {
		p, err := read()
		rec := recordSinks(t, p, err)
		runSinks(t, g) // every sink answers done (nil)
		got := rec.snapshot()
		if len(got.live)+len(got.search)+len(got.notices)+len(got.events) != 0 {
			t.Errorf("%s: a post that isn't there any more was still sent on: live %d, search %d, notices %d, events %d",
				name, len(got.live), len(got.search), len(got.notices), len(got.events))
		}
	}
}

func TestTheSinksSendAnEditedPostsWordsAsTheyAreNow(t *testing.T) {
	g := newGolden()
	rec := recordSinks(t, g.stored("<p>launch moved to 11</p>", nil), nil)
	runSinks(t, g)
	got := rec.snapshot()
	if len(got.live) != 1 || got.live[0].PostHtmlText != "<p>launch moved to 11</p>" {
		t.Fatalf("live: %+v", got.live)
	}
	if got.search[0].PostBody != "launch moved to 11" || got.notices[0].body != "launch moved to 11" || got.events[0]["text"] != "launch moved to 11" {
		t.Errorf("search %q, notice %q, event %v: each should carry the words as they are now", got.search[0].PostBody, got.notices[0].body, got.events[0]["text"])
	}
}

func TestTheNotifySinkTellsEachPersonOnceAcrossRetries(t *testing.T) {
	g := newGolden()
	rec := recordSinks(t, g.stored(g.in.HTMLText, nil), nil)
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
	ev := outboxBusiness.Event{EventID: uuid.NewString(), Entities: map[string]string{"message": g.id.String(), "kind": "channel"}}
	for try := 0; try < 2; try++ {
		if err := notifySink(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	got := rec.snapshot()
	if len(got.notices) != 2 || !reflect.DeepEqual(got.notices[0].pushTo, []string{"u-sam", "u-jonas"}) || len(got.notices[1].pushTo) != 0 {
		t.Errorf("push on the first try %v and the retry %v: the retry should tell nobody again", got.notices[0].pushTo, got.notices[1].pushTo)
	}
	if !seen[ev.EventID+"|"+sinkNotify+":push|u-sam"] {
		t.Error("the claim isn't per (event, sink, recipient)")
	}
}

func strp(s string) *string { return &s }
