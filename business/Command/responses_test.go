package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	cardsBusiness "github.com/akashc777/OneCamp/business/Cards"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// An app answers a command at its response URL up to five times in 30
// minutes: privately to whoever typed it, or as itself in the channel it was
// typed in, never in a direct message.

type responseFakes struct {
	saved     map[string]invocation
	delivered []*commandAdapter.CommandResponse
	posted    []string
}

func fakeResponses(t *testing.T) *responseFakes {
	t.Helper()
	t.Setenv("JWT_SECRET", "responses-test-secret")
	f := &responseFakes{saved: map[string]invocation{}}
	s1, s2, s3, s4, s5 := saveInvocation, takeUse, deliverEphemeral, postInChannel, responseBase
	t.Cleanup(func() { saveInvocation, takeUse, deliverEphemeral, postInChannel, responseBase = s1, s2, s3, s4, s5 })
	saveInvocation = func(_ context.Context, token string, inv invocation) error { f.saved[token] = inv; return nil }
	takeUse = func(_ context.Context, token string) (invocation, bool, error) {
		inv, ok := f.saved[token]
		if !ok || inv.Uses <= 0 || time.Now().Unix() > inv.Expires {
			return invocation{}, false, nil
		}
		inv.Uses--
		f.saved[token] = inv
		return inv, true, nil
	}
	deliverEphemeral = func(_ context.Context, _ string, resp *commandAdapter.CommandResponse) {
		f.delivered = append(f.delivered, resp)
	}
	postInChannel = func(_ context.Context, _ invocation, text string) error {
		f.posted = append(f.posted, text)
		return nil
	}
	responseBase = func() string { return "https://api.example.test/" }
	return f
}

func invoke(t *testing.T, channel bool) (string, CommandContext) {
	t.Helper()
	var u userModels.UserInfo
	u.UserDgraphInfo.Uuid = "sam"
	cc := CommandContext{User: u, Command: "deploy", TriggerID: "trig-1"}
	if channel {
		ch := uuid.New()
		cc.ChannelID = &ch
	} else {
		g := "sam ana"
		cc.DmGroupID = &g
	}
	url := newResponseURL(context.Background(), uuid.New(), "Deploy bot", "", cc)
	if !strings.HasPrefix(url, "https://api.example.test/command-responses/") {
		t.Fatalf("response URL %q", url)
	}
	return url[strings.LastIndex(url, "/")+1:], cc
}

func TestAnAppAnswersFiveTimesAtItsResponseURL(t *testing.T) {
	f := fakeResponses(t)
	token, _ := invoke(t, true)
	for i := 0; i < ResponseUses; i++ {
		if err := Respond(context.Background(), token, RespondBody{Text: "Deploying…"}); err != nil {
			t.Fatalf("answer %d: %v", i+1, err)
		}
	}
	if err := Respond(context.Background(), token, RespondBody{Text: "once more"}); !errors.Is(err, ErrResponseGone) {
		t.Fatalf("a sixth answer: %v", err)
	}
	if len(f.delivered) != ResponseUses || f.delivered[0].TriggerID != "trig-1" || !f.delivered[0].Ephemeral {
		t.Fatalf("delivered %d: %+v", len(f.delivered), f.delivered[0])
	}
	if err := Respond(context.Background(), strings.Repeat("0", 48), RespondBody{Text: "x"}); !errors.Is(err, ErrResponseGone) {
		t.Fatalf("a token never made: %v", err)
	}
}

func TestAnAnswerPostsAsTheAppInAChannelButNeverInADirectMessage(t *testing.T) {
	f := fakeResponses(t)
	token, _ := invoke(t, true)
	if err := Respond(context.Background(), token, RespondBody{Text: "Deployed web 214", ResponseType: "in_channel"}); err != nil || len(f.posted) != 1 || f.posted[0] != "Deployed web 214" {
		t.Fatalf("in a channel: %v, posted %v", err, f.posted)
	}
	dm, _ := invoke(t, false)
	if err := Respond(context.Background(), dm, RespondBody{Text: "Deployed", ResponseType: "in_channel"}); !errors.Is(err, ErrNoPlace) {
		t.Fatalf("in a direct message: %v", err)
	}
	if len(f.posted) != 1 {
		t.Fatalf("posted %v", f.posted)
	}
}

func TestAnAnswersButtonsAreSealedForThePersonItShowsTo(t *testing.T) {
	f := fakeResponses(t)
	token, _ := invoke(t, true)
	blocks := json.RawMessage(`[{"type": "section", "text": {"type": "plain_text", "text": "Roll back?"}}, {"type": "actions", "elements": [{"type": "button", "text": "Roll back", "action_id": "rollback"}]}, {"type": "input"}]`)
	if err := Respond(context.Background(), token, RespondBody{Blocks: blocks}); err != nil {
		t.Fatal(err)
	}
	got := f.delivered[0]
	if len(got.Blocks) != 2 || got.CardToken == "" {
		t.Fatalf("the answer's card: %d blocks, token %q", len(got.Blocks), got.CardToken)
	}
	// The card answers its own person's press, and no one else's.
	cardsBusiness.RegisterOwner("test-app-press", func(context.Context, cardsBusiness.Action) (*cardsBusiness.Reply, error) {
		return &cardsBusiness.Reply{}, nil
	})
	if _, err := cardsBusiness.Act(context.Background(), cardsBusiness.Who{UserUUID: "ana"}, got.CardToken, "rollback", ""); !errors.Is(err, cardsBusiness.ErrNotYours) {
		t.Fatalf("someone else pressing: %v", err)
	}
}

func TestAnAnswerNeedsSomethingToSay(t *testing.T) {
	fakeResponses(t)
	token, _ := invoke(t, true)
	if err := Respond(context.Background(), token, RespondBody{}); err == nil {
		t.Fatal("an empty answer was taken")
	}
}

// An answer posted in the channel is drawn by the one card renderer, the
// one an incoming webhook's blocks get: its words and links, as a post.
func TestAnAnswerInTheChannelIsDrawnAsTheCard(t *testing.T) {
	f := fakeResponses(t)
	token, _ := invoke(t, true)
	blocks := json.RawMessage(`[{"type": "header", "text": "Deployed web 214"}, {"type": "section", "text": {"type": "mrkdwn", "text": "*3 min*, <https://ci.example.com/214|the log>"}}, {"type": "actions", "elements": [{"type": "button", "text": "Roll back", "action_id": "rollback"}]}]`)
	if err := Respond(context.Background(), token, RespondBody{Text: "Deployed", Blocks: blocks, ResponseType: "in_channel"}); err != nil {
		t.Fatal(err)
	}
	got := f.posted[0]
	for _, want := range []string{"<h3>Deployed web 214</h3>", "<strong>3 min</strong>", `href="https://ci.example.com/214"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "Roll back") {
		t.Errorf("a button nobody can press was posted: %s", got)
	}
}
