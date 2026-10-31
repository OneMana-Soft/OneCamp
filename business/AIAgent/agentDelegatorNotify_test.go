package business

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

func TestDelegatorPushData(t *testing.T) {
	d := delegatorPushData("Release Captain", "t-1", "Write the launch announcement", "I'm blocked and need your input:\n Which channel?")
	if d[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Release Captain · Write the launch announcement" {
		t.Fatalf("title = %q", d[firebaseInit.FIREBASE_PUSH_DATA_TITLE])
	}
	if d[firebaseInit.FIREBASE_PUSH_DATA_TYPE] != "task" || d[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] != "t-1" {
		t.Fatal("the tap must open the task: type task, id in type_id")
	}
	if d[firebaseInit.FIREBASE_PUSH_DATA_BODY] != "I'm blocked and need your input: Which channel?" {
		t.Fatalf("body = %q", d[firebaseInit.FIREBASE_PUSH_DATA_BODY])
	}
	if got := delegatorPushData("Release Captain", "t-1", "", strings.Repeat("y", 500)); len([]rune(got[firebaseInit.FIREBASE_PUSH_DATA_BODY])) > taskDelegatorBodyMax ||
		got[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Release Captain" {
		t.Fatalf("long body or missing name mishandled: %+v", got)
	}
}

func TestNotifyTaskDelegatorTellsWhoHandedItOver(t *testing.T) {
	savedName, savedPush, savedReach := delegatedTaskName, pushToDelegator, reach
	t.Cleanup(func() { delegatedTaskName, pushToDelegator, reach = savedName, savedPush, savedReach })
	sent := make(chan [2]string, 5)
	delegatedTaskName = func(context.Context, *model.AiAgent, string) string { return "Book the launch retro" }
	pushToDelegator = func(_ context.Context, user, _ string, data map[string]string) {
		sent <- [2]string{user, data[firebaseInit.FIREBASE_PUSH_DATA_TITLE]}
	}
	// u-1 may read the task; u-2 was taken off its project; u-3's can't be read.
	reach.readsTask = func(_ context.Context, user, task string) (bool, error) {
		if user == "u-3" {
			return false, errors.New("lookup failed")
		}
		return user == "u-1" && task == "t-9", nil
	}
	agent := &model.AiAgent{Id: uuid.New(), Name: "Release Captain"}

	notifyTaskDelegator(agent, "t-9", "", "Done.")
	notifyTaskDelegator(agent, "t-9", "u-1", "  ")
	notifyTaskDelegator(agent, "t-9", "u-1", "Done — booked for Friday.")
	notifyTaskDelegator(agent, "t-9", "u-2", "Done — booked for Friday.")
	notifyTaskDelegator(agent, "t-9", "u-3", "Done — booked for Friday.")
	if busy := helpers.SettleBackground(5 * time.Second); len(busy) > 0 {
		t.Fatalf("still at work: %v", busy)
	}
	close(sent)
	var got [][2]string
	for s := range sent {
		got = append(got, s)
	}
	// Nobody to tell, nothing to say, someone who may no longer read the task,
	// or a check that can't be made, pushes nothing.
	if want := [][2]string{{"u-1", "Release Captain · Book the launch retro"}}; !slices.Equal(got, want) {
		t.Fatalf("pushed %v, want %v", got, want)
	}
}

// Where a run's moment is told: a channel its asker can still read, a group or
// DM they are still in; a task surface never (its delegator is told by
// notifyTaskDelegator), and a lookup that fails is its error.
func TestSeesWhereAskedIsTheChannelOrTheConversationNow(t *testing.T) {
	in := func(want string) func(context.Context, string, string) (bool, error) {
		return func(_ context.Context, user, id string) (bool, error) {
			if id == "broken" {
				return false, errors.New("lookup failed")
			}
			return user == "ana" && id == want, nil
		}
	}
	r := sponsorReach{readsChannel: in("c-1"), inConversation: in("g-1")}
	ctx := context.Background()
	for _, c := range []struct {
		name string
		who  string
		s    Surface
		want bool
		err  bool
	}{
		{"a channel she reads", "ana", Surface{Kind: SurfaceChannelPost, ChannelID: "c-1"}, true, false},
		{"a channel she can't read", "ana", Surface{Kind: SurfaceChannelPost, ChannelID: "c-2"}, false, false},
		{"someone else in her channel", "cy", Surface{Kind: SurfaceChannelPost, ChannelID: "c-1"}, false, false},
		{"her group", "ana", Surface{Kind: SurfaceGroupChat, GroupID: "g-1"}, true, false},
		{"her DM", "ana", Surface{Kind: SurfaceDM, GroupID: "g-1"}, true, false},
		{"a group she left", "ana", Surface{Kind: SurfaceGroupChat, GroupID: "g-2"}, false, false},
		{"a task", "ana", Surface{Kind: SurfaceTask}, false, false},
		{"a lookup that fails", "ana", Surface{Kind: SurfaceChannelPost, ChannelID: "broken"}, false, true},
	} {
		got, err := r.seesWhereAsked(ctx, c.who, c.s)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%s: %v, %v; want %v, error %v", c.name, got, err, c.want, c.err)
		}
	}
}
