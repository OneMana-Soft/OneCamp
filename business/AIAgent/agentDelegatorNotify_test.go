package business

import (
	"context"
	"strings"
	"testing"
	"time"

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
	savedName, savedPush := delegatedTaskName, pushToDelegator
	t.Cleanup(func() { delegatedTaskName, pushToDelegator = savedName, savedPush })
	sent := make(chan [2]string, 2)
	delegatedTaskName = func(context.Context, *model.AiAgent, string) string { return "Book the launch retro" }
	pushToDelegator = func(_ context.Context, user string, data map[string]string) error {
		sent <- [2]string{user, data[firebaseInit.FIREBASE_PUSH_DATA_TITLE]}
		return nil
	}
	agent := &model.AiAgent{Id: uuid.New(), Name: "Release Captain"}

	notifyTaskDelegator(agent, "t-9", "", "Done.")
	notifyTaskDelegator(agent, "t-9", "u-1", "  ")
	select {
	case s := <-sent:
		t.Fatalf("pushed with nobody to tell or nothing to say: %v", s)
	case <-time.After(150 * time.Millisecond):
	}

	notifyTaskDelegator(agent, "t-9", "u-1", "Done — booked for Friday.")
	select {
	case s := <-sent:
		if s[0] != "u-1" || s[1] != "Release Captain · Book the launch retro" {
			t.Fatalf("pushed %v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no push reached the person who delegated the task")
	}
}
