package business

import (
	"context"
	"errors"
	"testing"

	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// fakeAccess answers from two lists: the channels the owner can read and the
// projects they're in. Anything else is a no; "broken" is a lookup that fails.
func fakeAccess(readable, projects []string) ownerAccess {
	in := func(list []string, id string) (bool, error) {
		if id == "broken" {
			return false, errors.New("graph unreachable")
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
