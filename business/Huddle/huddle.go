package business

// A conversation's call writes its post when it starts (calls CALLS-3b,
// agreed with CHAT-LEAD-2, 22 Oct): one post a call, in its conversation, as
// whoever started it, of kind huddle, carrying the call's facts. The call's
// thread is that post's: HU13 admits its invitees there, and the call's
// thread surface serves people outside the conversation.
//
// The post is written first, through Send's durable write with the client id
// call:<session>, so an instance that raced, or a retry, gets the same post
// back; then the session takes it (Call State's AttachPost, only where it has
// none). Both instances attach the same id, and nothing is ever deleted.
//
// A meeting has no conversation, so no post. A starter who may not post where
// the call is (an announcement channel's member) gets none either: the call
// goes on without a post, and its thread surface says there's none.
//
// The work graph: the post is an ordinary message, so it reaches the graph
// through the post and chat writes' existing path. Nothing new.

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	chatDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Chat"
	postDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Post"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "calls.huddle", Wants: wants, Run: run})
	// The sink runs once: a write that failed is the sweep's (CHAT-LEAD-2's
	// review).
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "calls.huddle.posts", Every: time.Minute, Run: repair})
}

// The repair's bounds: how long the sink has before the sweep looks, how many
// times a failing write is tried, and how many calls one pass takes.
const (
	repairAfter = 30 * time.Second
	repairTries = 5
	repairAPass = 50
)

// Facts are the call's, as its card reads them (post_system, chat_system).
type Facts struct {
	Session string     `json:"session"`
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended,omitempty"`
	Seconds int        `json:"seconds,omitempty"`
}

// The reads and writes, seams for tests.
var seams = struct {
	session      func(context.Context, string) (*callStateBusiness.Session, error)
	actor        func(context.Context, uuid.UUID) (*userModels.UserInfo, error)
	writeChannel func(context.Context, *userModels.UserInfo, *postAdapter.InputCreateOrUpdatePostInfo, string) (string, error)
	writeDM      func(context.Context, *userModels.UserInfo, *chatAdapter.ChatInfo, string) (string, error)
	writeGroup   func(context.Context, *userModels.UserInfo, *chatAdapter.ChatInfo, string) (string, error)
	attach       func(context.Context, *callStateBusiness.Session, uuid.UUID) (bool, error)
	publish      func(context.Context, string)
	setPostFacts func(context.Context, string, string) error
	setChatFacts func(context.Context, string, string) error
	postless     func(context.Context, time.Time, int, int) ([]string, error)
	noteTry      func(context.Context, string, bool) error
	now          func() time.Time
}{
	session:      callStateBusiness.Get,
	actor:        userDomain.ActorByUUID,
	writeChannel: writeChannelPost,
	writeDM:      writeDirectMessage,
	writeGroup:   writeGroupMessage,
	attach:       callStateBusiness.AttachPost,
	publish:      callStateBusiness.Publish,
	setPostFacts: postDgraphModels.SetCallFactsByUUID,
	setChatFacts: chatDgraphModels.SetCallFactsByUUID,
	postless:     postlessHuddles,
	noteTry:      notePostTry,
	now:          time.Now,
}

func postlessHuddles(ctx context.Context, before time.Time, tries, limit int) ([]string, error) {
	ids, err := callStateModel.PostlessHuddles(ctx, before, tries, limit)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out, err
}

func notePostTry(ctx context.Context, session string, refused bool) error {
	id, err := uuid.Parse(session)
	if err != nil {
		return err
	}
	return callStateModel.NotePostTry(ctx, id, refused)
}

// wants is a conversation's call starting or ending. Pure.
func wants(e resourceEventBusiness.Event) bool {
	return (e.Kind == resourceEventBusiness.CallStarted || e.Kind == resourceEventBusiness.CallEnded) && e.Data["kind"] == "huddle"
}

func run(ctx context.Context, e resourceEventBusiness.Event) {
	s, err := seams.session(ctx, e.Data["session"])
	if err != nil || s == nil {
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Huddle: reading call %s: %+v", e.Data["session"], err)
		}
		return
	}
	if e.Kind == resourceEventBusiness.CallEnded {
		finish(ctx, s)
		if err := report(ctx, s); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Huddle: call %s's report (the sweep tries again): %+v", s.ID, err)
		}
		return
	}
	if err := post(ctx, e, s); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Huddle: call %s's post (the sweep tries again): %+v", s.ID, err)
	}
}

// repair writes the post of each live call still without one, a while after
// it started, as its starter: the sink's write failed, or never ran. A refusal
// (the starter may not post there) is recorded and passed over from then on;
// any other failure is one more try, up to repairTries. Writing twice is
// harmless: the client id answers the first post, and AttachPost only fills
// an empty one.
func repair(ctx context.Context) error {
	ids, err := seams.postless(ctx, seams.now().Add(-repairAfter), repairTries, repairAPass)
	if err != nil {
		return err
	}
	for _, id := range ids {
		s, err := seams.session(ctx, id)
		if err != nil || s == nil || !s.Live || s.PostUUID != "" {
			continue
		}
		e := resourceEventBusiness.Event{Kind: resourceEventBusiness.CallStarted, Actor: resourceEventBusiness.Actor{UserUUID: s.StartedBy}, Data: map[string]string{"session": s.ID, "kind": "huddle"}}
		if err := post(ctx, e, s); err != nil {
			r, ok := sendBusiness.AsRejection(err)
			refused := ok && r.Status != http.StatusConflict
			if nerr := seams.noteTry(ctx, s.ID, refused); nerr != nil {
				helpers.LogErrorWithContext(ctx, "business/Huddle: noting call %s's try: %+v", s.ID, nerr)
			}
		}
	}
	return nil
}

// finish gives the call's post its end and length: a write of the facts
// alone, so the card is never edited. Open apps see it on their next read of
// the conversation; chat P10 dresses the card.
func finish(ctx context.Context, s *callStateBusiness.Session) {
	if s.PostUUID == "" || s.EndedAt == nil {
		return
	}
	facts, _ := json.Marshal(Facts{Session: s.ID, Started: s.StartedAt, Ended: s.EndedAt, Seconds: int(s.EndedAt.Sub(s.StartedAt).Seconds())})
	var err error
	switch s.Resource.Kind {
	case "channel":
		err = seams.setPostFacts(ctx, s.PostUUID, string(facts))
	case "dm", "group":
		err = seams.setChatFacts(ctx, s.PostUUID, string(facts))
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Huddle: call %s's end on its post %s: %+v", s.ID, s.PostUUID, err)
	}
}

// errNoStarter is a call whose starter can't be read as a person: it can't
// have a post written as them.
var errNoStarter = &sendBusiness.Rejection{Status: http.StatusNotFound, Msg: "the call's starter isn't a person here"}

// post writes the call's post as its starter and gives it to the call, once,
// then tells the call's people's apps (Call State's publish), whose read of the
// call now has its post, and so its chat. It answers why it couldn't: a
// refusal (sendBusiness.Rejection) is for good.
func post(ctx context.Context, e resourceEventBusiness.Event, s *callStateBusiness.Session) error {
	if s.PostUUID != "" || s.Kind != "huddle" {
		return nil
	}
	starterID, err := uuid.Parse(e.Actor.UserUUID)
	if err != nil {
		return errNoStarter
	}
	starter, err := seams.actor(ctx, starterID)
	if err != nil {
		return err
	}
	if starter == nil {
		return errNoStarter
	}
	facts, _ := json.Marshal(Facts{Session: s.ID, Started: s.StartedAt})
	text := fmt.Sprintf("<p>%s started a call</p>", html.EscapeString(starter.UserDgraphInfo.DisplayName()))
	client := "call:" + s.ID
	var id string
	switch s.Resource.Kind {
	case "channel":
		ch, perr := uuid.Parse(s.Resource.ID)
		if perr != nil {
			return nil
		}
		id, err = seams.writeChannel(ctx, starter, &postAdapter.InputCreateOrUpdatePostInfo{
			ChannelUuid: s.Resource.ID, ChannelUUID: ch, HTMLText: text, Kind: helpers.KindHuddle, System: string(facts),
		}, client)
	case "dm":
		other := ""
		for _, p := range strings.Fields(s.Resource.ID) {
			if !strings.EqualFold(p, starterID.String()) {
				other = p
			}
		}
		if other == "" {
			return nil
		}
		id, err = seams.writeDM(ctx, starter, &chatAdapter.ChatInfo{ToUuid: other, TextHtml: text, Kind: helpers.KindHuddle, System: string(facts)}, client)
	case "group":
		id, err = seams.writeGroup(ctx, starter, &chatAdapter.ChatInfo{GrpUuid: s.Resource.ID, TextHtml: text, Kind: helpers.KindHuddle, System: string(facts)}, client)
	default:
		return nil // a meeting: no conversation
	}
	if err != nil {
		return err
	}
	postID, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	won, err := seams.attach(ctx, s, postID)
	if won {
		seams.publish(ctx, s.Room)
	}
	return err
}

// The three writes, through Send: the starter's rules for posting there, then
// the durable write with the call's client id.
func writeChannelPost(ctx context.Context, who *userModels.UserInfo, in *postAdapter.InputCreateOrUpdatePostInfo, client string) (string, error) {
	p, err := sendBusiness.PrepareChannelPost(ctx, who, in)
	if err != nil {
		return "", err
	}
	out, err := p.CommitDurably(ctx, client)
	if err != nil {
		return "", err
	}
	return out.Uuid, nil
}

func writeDirectMessage(ctx context.Context, who *userModels.UserInfo, in *chatAdapter.ChatInfo, client string) (string, error) {
	m, err := sendBusiness.PrepareDirectMessage(ctx, who, in)
	if err != nil {
		return "", err
	}
	out, err := m.CommitDurably(ctx, client)
	if err != nil {
		return "", err
	}
	return out.Uuid, nil
}

func writeGroupMessage(ctx context.Context, who *userModels.UserInfo, in *chatAdapter.ChatInfo, client string) (string, error) {
	m, err := sendBusiness.PrepareGroupMessage(ctx, who, in)
	if err != nil {
		return "", err
	}
	out, err := m.CommitDurably(ctx, client)
	if err != nil {
		return "", err
	}
	return out.Uuid, nil
}
