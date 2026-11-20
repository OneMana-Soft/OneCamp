package business

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// fakeAccess answers from two lists: the channels the owner can read and the
// projects they're in. Anything else is a no; "broken" is a lookup that fails,
// and "gone" one that found nothing (as the graph's models answer it).
func fakeAccess(readable, projects []string) ownerAccess {
	in := func(list []string, id string) (bool, error) {
		if id == "broken" {
			return false, errors.New("graph unreachable")
		}
		if id == "gone" {
			return false, helpers.NotFound("nothing by that id in dgraph")
		}
		for _, x := range list {
			if x == id {
				return true, nil
			}
		}
		return false, nil
	}
	return ownerAccess{
		readsChannel: func(_ context.Context, _, channelID string) (bool, error) { return in(readable, channelID) },
		inProject:    func(_ context.Context, _, projectID string) (bool, error) { return in(projects, projectID) },
	}
}

func TestAWorkflowHearsOnlyWhatItsOwnerCanSee(t *testing.T) {
	a := fakeAccess([]string{"mine", "public"}, []string{"ours"})
	for _, c := range []struct {
		name string
		ev   triggerEvent
		want bool
	}{
		{"a message in a channel the owner is in", triggerEvent{channelID: "mine"}, true},
		{"a message in a public channel", triggerEvent{channelID: "public"}, true},
		{"a message in a private channel the owner isn't in", triggerEvent{channelID: "theirs"}, false},
		{"a channel that couldn't be looked up", triggerEvent{channelID: "broken"}, false},
		{"a task moving in the owner's project", triggerEvent{projectID: "ours"}, true},
		{"a task moving in a project the owner isn't in", triggerEvent{projectID: "other"}, false},
		{"a project that couldn't be looked up", triggerEvent{projectID: "broken"}, false},
		{"the owner's project, replying where they can't read", triggerEvent{channelID: "theirs", projectID: "ours"}, false},
		{"the owner's project, replying where they can", triggerEvent{channelID: "mine", projectID: "ours"}, true},
	} {
		if got := a.allows(context.Background(), uuid.NewString(), c.ev); got != c.want {
			t.Errorf("%s: allows = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAScopeTheOwnerCantSeeIsRefused(t *testing.T) {
	a := fakeAccess([]string{}, []string{})
	ctx, owner := context.Background(), uuid.NewString()
	channel := uuid.New()
	mine := fakeAccess([]string{channel.String()}, []string{"ours"})

	if err := a.checkScope(ctx, owner, &channel, workflowModel.TriggerMessagePosted, nil); err != errScopeChannel {
		t.Errorf("a channel the owner can't read: %v", err)
	}
	if err := mine.checkScope(ctx, owner, &channel, workflowModel.TriggerMessagePosted, nil); err != nil {
		t.Errorf("a channel the owner can read: %v", err)
	}
	// "Any channel" names no channel; what it hears is decided per event.
	if err := a.checkScope(ctx, owner, nil, workflowModel.TriggerMessagePosted, nil); err != nil {
		t.Errorf("any channel: %v", err)
	}

	move := func(project string) map[string]interface{} { return map[string]interface{}{"project_id": project} }
	if err := mine.checkScope(ctx, owner, nil, workflowModel.TriggerTaskStatusChanged, move("other")); err != errScopeProject {
		t.Errorf("moves in a project the owner isn't in: %v", err)
	}
	// CR-444 D1: a refusal in its words, so a workflow install out of scope
	// answers 400 with them (business/Marketplace AsRefusal), not 500.
	var refused *helpers.Refusal
	err := mine.checkScope(ctx, owner, nil, workflowModel.TriggerTaskStatusChanged, move("other"))
	if !errors.As(err, &refused) || refused.Status != http.StatusBadRequest || refused.Msg != "choose a project the workflow's owner is in" {
		t.Errorf("moves in a project the owner isn't in, as a refusal: %#v", err)
	}
	if err := mine.checkScope(ctx, owner, nil, workflowModel.TriggerTaskStatusChanged, move("ours")); err != nil {
		t.Errorf("moves in the owner's project: %v", err)
	}
	if err := mine.checkScope(ctx, owner, nil, workflowModel.TriggerTaskStatusChanged, move("")); err != nil {
		t.Errorf("moves in any project: %v", err)
	}
	// Only the task-move trigger reads a project from its config.
	if err := a.checkScope(ctx, owner, nil, workflowModel.TriggerMessagePosted, move("other")); err != nil {
		t.Errorf("a project id another trigger ignores: %v", err)
	}
}

// V-1281: a scope whose check couldn't read the graph is still refused (a
// workflow never saves on a failed read), in the same words for the form, but
// not as a helpers.Refusal: an install answers it 500 in its own words instead
// of 400 "choose ...", and the cause is logged where the check ran. A real no
// and an id that names nothing stay the refusal itself.
func TestAScopeWhoseReadFailedIsNoRefusal(t *testing.T) {
	ctx, owner := context.Background(), uuid.NewString()
	move := func(project string) map[string]interface{} { return map[string]interface{}{"project_id": project} }
	theirs := uuid.New()
	for _, c := range []struct {
		name    string
		a       ownerAccess
		channel *uuid.UUID
		trigger string
		config  map[string]interface{}
		refusal error
	}{
		{"a channel whose read failed", fakeAccess(nil, nil), &theirs, workflowModel.TriggerMessagePosted, nil, errScopeChannel},
		{"moves in a project whose read failed", fakeAccess(nil, nil), nil, workflowModel.TriggerTaskStatusChanged, move("broken"), errScopeProject},
	} {
		// The channel's id is a uuid: its fake reads it as "broken".
		if c.channel != nil {
			c.a.readsChannel = brokenFor(c.channel.String(), c.a.readsChannel)
		}
		logs := captureErrorLogs(t)
		got := c.a.checkScope(ctx, owner, c.channel, c.trigger, c.config)
		if got == nil {
			t.Errorf("%s: saved on a failed read", c.name)
			continue
		}
		var refused *helpers.Refusal
		if errors.As(got, &refused) || errors.Is(got, c.refusal) {
			t.Errorf("%s: answered as the refusal %#v, want a failure an install answers 500", c.name, got)
		}
		if got.Error() != c.refusal.Error() {
			t.Errorf("%s: the form would read %q, want %q", c.name, got.Error(), c.refusal.Error())
		}
		if line := logs.String(); !strings.Contains(line, "graph unreachable") || !strings.Contains(line, "checkScope") {
			t.Errorf("%s: the cause isn't logged at the check: %q", c.name, line)
		}
	}

	gone := fakeAccess(nil, nil)
	gone.readsChannel = goneFor(theirs.String(), gone.readsChannel)
	if err := gone.checkScope(ctx, owner, &theirs, workflowModel.TriggerMessagePosted, nil); err != errScopeChannel {
		t.Errorf("a channel that isn't there: %#v, want the refusal", err)
	}
	if err := gone.checkScope(ctx, owner, nil, workflowModel.TriggerTaskStatusChanged, move("gone")); err != errScopeProject {
		t.Errorf("moves in a project that isn't there: %#v, want the refusal", err)
	}
}

// brokenFor and goneFor read id as "broken" or "gone", and anything else as read does.
func brokenFor(id string, read func(context.Context, string, string) (bool, error)) func(context.Context, string, string) (bool, error) {
	return func(ctx context.Context, owner, asked string) (bool, error) {
		if asked == id {
			asked = "broken"
		}
		return read(ctx, owner, asked)
	}
}

func goneFor(id string, read func(context.Context, string, string) (bool, error)) func(context.Context, string, string) (bool, error) {
	return func(ctx context.Context, owner, asked string) (bool, error) {
		if asked == id {
			asked = "gone"
		}
		return read(ctx, owner, asked)
	}
}

// captureErrorLogs is what helpers logs at ERROR until the test ends.
func captureErrorLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	prev := helpers.Logger
	b := &lockedBuffer{}
	helpers.Logger = slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelError}))
	t.Cleanup(func() { helpers.Logger = prev })
	return b
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
