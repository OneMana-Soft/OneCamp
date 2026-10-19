package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	domain "github.com/akashc777/OneCamp/domain/Post"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The durable channel post's steps (chat A2, durable.go): the rows and events,
// then the node, then the events released and delivered; a node that fails
// takes the rows back; an event that can't be released is left to its
// resolver; a send whose client id has a message already answers that message.

// steps records what CreatePostDurably asked of its seams, and answers as told.
type steps struct {
	rowsErr, nodeErr error
	// unreleased is the event whose release fails.
	unreleased int64
	did        []string
	tookBack   []string
	asked      string
}

func recordSteps(t *testing.T, s *steps) {
	t.Helper()
	pRows, pNode, pRelease, pDeliver, pTakeBack, pAnswered := writeRows, writeNode, release, deliver, takeBack, answered
	t.Cleanup(func() {
		writeRows, writeNode, release, deliver, takeBack, answered = pRows, pNode, pRelease, pDeliver, pTakeBack, pAnswered
	})
	writeRows = func(_ context.Context, _, _, _ uuid.UUID, clientID string, _ time.Time, _ string) ([]int64, error) {
		s.did = append(s.did, "rows "+clientID)
		if s.rowsErr != nil {
			return nil, s.rowsErr
		}
		return []int64{7, 8}, nil
	}
	writeNode = func(context.Context, uuid.UUID, *adapter.InputCreateOrUpdatePostInfo, *userModels.UserInfo, []*dgraphStruct.DgraphUser, *dgraphStruct.DgraphChannel, time.Time) (*dgraphStruct.DgraphPost, error) {
		s.did = append(s.did, "node")
		return nil, s.nodeErr
	}
	release = func(_ context.Context, id int64) error {
		s.did = append(s.did, fmt.Sprintf("release %d", id))
		if id == s.unreleased {
			return errors.New("postgres away")
		}
		return nil
	}
	deliver = func(id int64) { s.did = append(s.did, fmt.Sprintf("deliver %d", id)) }
	takeBack = func(_ context.Context, post uuid.UUID, events []int64) error {
		s.tookBack = append(s.tookBack, fmt.Sprintf("%s %v", post, events))
		return nil
	}
	answered = func(_ context.Context, _, _ uuid.UUID, clientID string) (*adapter.OutputCreatePostForPost, error) {
		s.asked = clientID
		return &adapter.OutputCreatePostForPost{Uuid: "the-first-post"}, nil
	}
}

func durableSend(t *testing.T) (*adapter.OutputCreatePostForPost, error) {
	t.Helper()
	who := &userModels.UserInfo{}
	who.UserPostgresInfo.Id = uuid.New()
	who.UserDgraphInfo.Uuid = who.UserPostgresInfo.Id.String()
	return CreatePostDurably(context.Background(), &adapter.InputCreateOrUpdatePostInfo{ChannelUUID: uuid.New(), HTMLText: "<p>hi</p>"}, who, nil, &dgraphStruct.DgraphChannel{}, "local-1")
}

func TestADurablePostIsReleasedAndDeliveredOnceItsNodeIsWritten(t *testing.T) {
	s := &steps{}
	recordSteps(t, s)
	out, err := durableSend(t)
	if err != nil || out == nil || out.Uuid == "" {
		t.Fatalf("%+v %v", out, err)
	}
	if got := strings.Join(s.did, ", "); got != "rows local-1, node, release 7, deliver 7, release 8, deliver 8" {
		t.Errorf("steps: %s", got)
	}
	if len(s.tookBack) != 0 {
		t.Error("a written post was taken back")
	}
}

func TestADurablePostWhoseNodeFailsIsTakenBack(t *testing.T) {
	s := &steps{nodeErr: errors.New("graph down")}
	recordSteps(t, s)
	out, err := durableSend(t)
	if err == nil || out != nil {
		t.Fatalf("a post with no node answered %+v", out)
	}
	if got := strings.Join(s.did, ", "); got != "rows local-1, node" {
		t.Errorf("steps: %s: nothing is released for a post with no node", got)
	}
	if len(s.tookBack) != 1 || !strings.HasSuffix(s.tookBack[0], " [7 8]") {
		t.Errorf("took back %v: its rows go, and both its events", s.tookBack)
	}
}

func TestAnEventItsWriterCouldntReleaseIsLeftToItsResolver(t *testing.T) {
	s := &steps{unreleased: 7}
	recordSteps(t, s)
	out, err := durableSend(t)
	if err != nil || out == nil {
		t.Fatalf("the post is written, so it answers: %+v %v", out, err)
	}
	if got := strings.Join(s.did, ", "); got != "rows local-1, node, release 7, release 8, deliver 8" {
		t.Errorf("steps: %s: a held event isn't delivered, and the other goes on", got)
	}
	if len(s.tookBack) != 0 {
		t.Error("a written post was taken back")
	}
}

// firstPostIs makes the first try's post there (its node written) or not.
func firstPostIs(t *testing.T, there bool) {
	t.Helper()
	prev := readPost
	t.Cleanup(func() { readPost = prev })
	readPost = func(_ context.Context, id string) (*dgraphStruct.DgraphPost, error) {
		if id != "the-first-post" {
			t.Errorf("read %s, not the first try's post", id)
		}
		if !there {
			return nil, domain.ErrPostGone
		}
		return &dgraphStruct.DgraphPost{Uuid: id}, nil
	}
}

func TestASecondSendWithItsClientIDAnswersTheFirst(t *testing.T) {
	s := &steps{rowsErr: errSentAlready}
	recordSteps(t, s)
	firstPostIs(t, true)
	out, err := durableSend(t)
	if err != nil || out == nil || out.Uuid != "the-first-post" {
		t.Fatalf("%+v %v: the first post's answer", out, err)
	}
	if s.asked != "local-1" || strings.Join(s.did, ", ") != "rows local-1" {
		t.Errorf("asked %q after %v: no second node, no events", s.asked, s.did)
	}
}

// A retry while the first try's post has no node (still being written, or its
// writer died and the resolver will take it back) is told to try again, never
// answered with a post that may never be there.
func TestARetryWhileTheFirstTryHasNoNodeIsStillBeingSent(t *testing.T) {
	s := &steps{rowsErr: errSentAlready}
	recordSteps(t, s)
	firstPostIs(t, false)
	out, err := durableSend(t)
	if !errors.Is(err, ErrStillBeingSent) || out != nil {
		t.Fatalf("%+v %v: still being sent", out, err)
	}
}

// A post's live delivery is in its channel's order, apart from its other side
// effects, which are in its own: one post's slow notices never hold back the
// next post reaching open apps.
func TestAPostsLiveEventIsInItsChannelsOrderAndTheRestInItsOwn(t *testing.T) {
	post, channel := uuid.New(), uuid.New()
	events := postEvents(MessageCreated, post, channel, "u-ada", false)
	if len(events) != 2 {
		t.Fatalf("%d events", len(events))
	}
	live, rest := events[0], events[1]
	if live.Key != "channel:"+channel.String() || strings.Join(live.Sinks, ",") != sinkRealtime {
		t.Errorf("live: %s %v", live.Key, live.Sinks)
	}
	if rest.Key != "post:"+post.String() || strings.Join(rest.Sinks, ",") != "post.search,post.embed,post.notify,post.events" {
		t.Errorf("the rest: %s %v", rest.Key, rest.Sinks)
	}
	for _, e := range events {
		if e.Topic != MessageTopic || e.Kind != MessageCreated || e.Actor != "u-ada" ||
			e.Entities["message"] != post.String() || e.Entities["conversation"] != channel.String() || e.Entities["kind"] != "channel" {
			t.Errorf("event %+v", e)
		}
	}
	// Every sink a post has is in one of them, once.
	seen := map[string]int{}
	for _, e := range events {
		for _, s := range e.Sinks {
			seen[s]++
		}
	}
	for _, s := range []string{sinkRealtime, sinkSearch, sinkEmbed, sinkNotify, sinkEvents} {
		if seen[s] != 1 {
			t.Errorf("%s is in %d of the events", s, seen[s])
		}
	}
}

// An imported post is indexed and nothing else, as today's CreatePost does
// for an import: no app gets history live, and nobody is told of it.
func TestAnImportedPostIsOnlyIndexed(t *testing.T) {
	post := uuid.New()
	events := postEvents(MessageCreated, post, uuid.New(), "u-ada", true)
	if len(events) != 1 || events[0].Key != "post:"+post.String() || strings.Join(events[0].Sinks, ",") != sinkSearch {
		t.Errorf("an imported post's events: %+v", events)
	}
}

func TestAClientIDOnceDoesntTakeIsntKept(t *testing.T) {
	for id, want := range map[string]any{
		"local-1":                 "local-1",
		"":                        nil,
		strings.Repeat("a", 64):   strings.Repeat("a", 64),
		strings.Repeat("a", 65):   nil,
		"has space":               nil,
		"<script>":                nil,
		"0b8f-4c1e_9a2d-77e1b3c4": "0b8f-4c1e_9a2d-77e1b3c4",
	} {
		if got := ledgerClientID(id); got != want {
			t.Errorf("ledgerClientID(%q) = %v, want %v", id, got, want)
		}
	}
}
