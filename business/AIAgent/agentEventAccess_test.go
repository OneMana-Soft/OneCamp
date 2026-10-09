package business

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// fakeReach is a sponsor who can read channel "open", is in project "mine"
// and can view table "t1". Lookups of "broken" fail.
func fakeReach() sponsorReach {
	answer := func(yes string) func(context.Context, string, string) (bool, error) {
		return func(_ context.Context, user, id string) (bool, error) {
			if user != "sponsor" {
				return false, nil
			}
			if id == "broken" {
				return false, errors.New("lookup failed")
			}
			return id == yes, nil
		}
	}
	return sponsorReach{readsChannel: answer("open"), inProject: answer("mine"), viewsTable: answer("t1")}
}

func TestAnEventReachesAnAgentOnlyWhereItsSponsorCanSee(t *testing.T) {
	r := fakeReach()
	cases := []struct {
		name  string
		event string
		data  map[string]interface{}
		want  bool
	}{
		{"a message in a channel they read", "post.created", map[string]interface{}{"channel_id": "open", "text": "hi"}, true},
		{"a message in a private channel they aren't in", "post.created", map[string]interface{}{"channel_id": "secret", "text": "hi"}, false},
		{"a comment there", "post.comment.created", map[string]interface{}{"channel_id": "secret"}, false},
		{"a channel created where they can't read", "channel.created", map[string]interface{}{"channel_id": "secret"}, false},
		{"a task in their project", "task.created", map[string]interface{}{"project_id": "mine"}, true},
		{"a task in another project", "task.status_changed", map[string]interface{}{"project_id": "theirs"}, false},
		{"a row in a table they view", "table.row.updated", map[string]interface{}{"table_id": "t1"}, true},
		{"a row in another table", "table.row.created", map[string]interface{}{"table_id": "t2"}, false},
		{"a pull request on their project's repo", "github.pr.opened", map[string]interface{}{"project_id": "mine"}, true},
		{"CI on another project's repo", "github.check_run.completed", map[string]interface{}{"project_id": "theirs"}, false},
		{"an occurrence that doesn't say where", "post.created", map[string]interface{}{"text": "hi"}, false},
		{"the wrong kind of place", "task.created", map[string]interface{}{"channel_id": "open"}, false},
		{"a lookup that fails", "post.created", map[string]interface{}{"channel_id": "broken"}, false},
		{"a direct message", "chat.created", map[string]interface{}{"channel_id": "open"}, false},
		{"an internal event", EventTypeAgentMessage, map[string]interface{}{"channel_id": "open"}, false},
	}
	for _, c := range cases {
		if got := r.sees(context.Background(), "sponsor", c.event, c.data); got != c.want {
			t.Errorf("%s: sees=%v, want %v", c.name, got, c.want)
		}
	}
	if r.sees(context.Background(), "someone else", "post.created", map[string]interface{}{"channel_id": "open"}) {
		t.Error("it is the sponsor's sight that counts")
	}
}

func TestSavingAnAgentChecksWhereItPoints(t *testing.T) {
	r := fakeReach()
	cases := []struct {
		name             string
		triggerType, cfg string
		scope            string
		want             error
	}{
		{"a bindable event, no scope", model.TriggerEvent, `{"event":"post.created"}`, "", nil},
		{"a direct-message event", model.TriggerEvent, `{"event":"chat.created"}`, "", errEventNotBindable},
		{"an internal event", model.TriggerEvent, `{"event":"agent.message"}`, "", errEventNotBindable},
		{"a GitHub event", model.TriggerEvent, `{"event":"github.issue.opened"}`, "", errGitHubEvent},
		{"another GitHub event", model.TriggerEvent, `{"event":"github.check_run.completed"}`, "", errGitHubEvent},
		{"moves in their project", model.TriggerEvent, `{"event":"task.status_changed","project_id":"mine"}`, "", nil},
		{"moves in another project", model.TriggerEvent, `{"event":"task.status_changed","project_id":"theirs"}`, "", errScopeProject},
		{"scoped to a channel they read", model.TriggerMention, `{}`, `{"channel_ids":["open"]}`, nil},
		{"scoped to one they can't", model.TriggerMention, `{}`, `{"channel_ids":["open","secret"]}`, errScopeChannel},
		{"scoped to their project", model.TriggerSchedule, `{}`, `{"project_ids":["mine"]}`, nil},
		{"scoped to another project", model.TriggerSchedule, `{}`, `{"project_ids":["theirs"]}`, errScopeProject},
		{"a scope lookup that fails", model.TriggerMention, `{}`, `{"channel_ids":["broken"]}`, errScopeChannel},
	}
	for _, c := range cases {
		if got := r.checkReach(context.Background(), "sponsor", c.triggerType, c.cfg, c.scope); !errors.Is(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAnAmbientAgentConsidersOnlyChannelsItsSponsorReads(t *testing.T) {
	saved := reach
	t.Cleanup(func() { reach = saved })
	reach = fakeReach()
	a := &model.AiAgent{Id: uuid.New(), Name: "Helper", Ambient: true, CreatedBy: uuid.New()}
	question := "Does anyone know when the vendor contract renews?"
	// An unscoped agent: anywhere its sponsor reads, and nowhere else.
	reach.readsChannel = func(_ context.Context, user, id string) (bool, error) {
		return user == a.CreatedBy.String() && id == "open", nil
	}
	if !ambientConsiders(context.Background(), a, "open", "author", question, nil) {
		t.Error("a question in a channel its sponsor reads is considered")
	}
	if ambientConsiders(context.Background(), a, "secret", "author", question, nil) {
		t.Error("a private channel its sponsor isn't in is never considered")
	}
	reach.readsChannel = func(context.Context, string, string) (bool, error) { return false, errors.New("lookup failed") }
	if ambientConsiders(context.Background(), a, "open", "author", question, nil) {
		t.Error("a lookup that fails is a no")
	}
}

// TestEventDispatchAsksWhatTheSponsorCanSee is the wiring half: the rule
// above does nothing unless the event loop asks it, and that loop needs a
// running AI service to exercise. Reading the source is the honest check.
func TestEventDispatchAsksWhatTheSponsorCanSee(t *testing.T) {
	raw, err := os.ReadFile("agentTriggers.go")
	if err != nil {
		t.Fatal(err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
	start := strings.Index(src, "func handleAgentEvent(")
	if start < 0 {
		t.Fatal("could not find handleAgentEvent; this test has gone stale")
	}
	end := strings.Index(src[start:], "launchAgent(")
	if end < 0 {
		t.Fatal("could not find the event loop's launch; this test has gone stale")
	}
	if !strings.Contains(src[start:start+end], "reach.sees(") {
		t.Error("handleAgentEvent launches event agents without asking reach.sees, " +
			"so an agent hears events from places its sponsor can't see")
	}
}

// A message an agent is bound to is its author's request: their words are what
// the run acts on. Any other event is nobody's, and runs for the sponsor alone.
func TestAMessageEventIsAskedForByItsAuthor(t *testing.T) {
	cases := []struct {
		name      string
		event     string
		data      map[string]interface{}
		wantAsker string
		wantAsked bool
	}{
		{"a channel message", "post.created", map[string]interface{}{"author_id": " ravi ", "text": "hi"}, "ravi", true},
		{"a comment in a thread", "post.comment.created", map[string]interface{}{"author_id": "ravi"}, "ravi", true},
		{"a message that doesn't name its author", "post.created", map[string]interface{}{"text": "hi"}, "", true},
		{"a post an incoming webhook made", "post.created", map[string]interface{}{"requested_by": "sana", "source": "incoming_webhook"}, "", true},
		{"a task someone created", "task.created", map[string]interface{}{"author_id": "ravi"}, "", false},
		{"an issue opened on GitHub", "github.issue.opened", map[string]interface{}{"project_id": "p1", "body": "hi"}, "", true},
		{"a review on GitHub", "github.pr.review_submitted", map[string]interface{}{"reviewer": "mallory"}, "", true},
		{"CI finishing on GitHub", "github.check_run.completed", map[string]interface{}{"conclusion": "failure"}, "", true},
		{"a row that changed", "table.row.updated", map[string]interface{}{"table_id": "t1"}, "", false},
	}
	for _, c := range cases {
		asker, asked := eventAsker(context.Background(), c.event, c.data)
		if asker != c.wantAsker || asked != c.wantAsked {
			t.Errorf("%s: asker %q asked %v, want %q %v", c.name, asker, asked, c.wantAsker, c.wantAsked)
		}
	}
	// Whatever the GitHub sync does is nobody's too, a message included, and so
	// is whatever a public form files.
	for by, ctx := range map[string]context.Context{
		"the GitHub sync": helpers.WithGitHubOrigin(context.Background()),
		"a public form":   helpers.WithPublicSubmission(context.Background()),
	} {
		for _, event := range []string{"task.status_changed", "task.created", "post.created"} {
			if asker, asked := eventAsker(ctx, event, map[string]interface{}{"author_id": "ravi"}); asker != "" || !asked {
				t.Errorf("%s by %s: asker %q asked %v, want nobody identified", event, by, asker, asked)
			}
		}
	}
}
