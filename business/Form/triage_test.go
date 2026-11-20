package business

import (
	"context"
	"testing"
	"time"

	formula "github.com/akashc777/OneCamp/business/Formula"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A sent form's task goes to triage when its project has triage on (package 9
// R5a, through triage Arrive), with the address the visitor gave; a task
// triage doesn't hold (off, full, or an item that didn't stick, which Arrive
// logs) leaves the form sent, since sending again would make it twice.
func TestASentFormsTaskWaitsInTriage(t *testing.T) {
	l, _ := visitorForm(t)
	l.fields = []Field{
		{Id: "what", Label: "What do you need?", Type: ShortText, Required: true},
		{Id: "email", Label: "Your email", Type: Email},
	}
	made := uuid.New()
	type held struct {
		project, task uuid.UUID
		src           triageBusiness.Source
		from          triageBusiness.Requester
	}
	var got []held
	saved := struct {
		make   func(context.Context, *userModels.UserInfo, uuid.UUID, taskWriter.NewTask) (uuid.UUID, error)
		arrive func(context.Context, string, uuid.UUID, uuid.UUID, triageBusiness.Source, triageBusiness.Requester) bool
		record func(context.Context, uuid.UUID, *uuid.UUID, *uuid.UUID, []byte) (uuid.UUID, error)
		count  func(context.Context, uuid.UUID, time.Time) (int, error)
		tell   func(context.Context, map[string]interface{})
	}{makeTask, arriveInTriage, recordSubmission, countSince, tellRules}
	t.Cleanup(func() {
		makeTask, arriveInTriage, recordSubmission, countSince, tellRules = saved.make, saved.arrive, saved.record, saved.count, saved.tell
	})
	countSince = func(context.Context, uuid.UUID, time.Time) (int, error) { return 0, nil }
	makeTask = func(context.Context, *userModels.UserInfo, uuid.UUID, taskWriter.NewTask) (uuid.UUID, error) {
		return made, nil
	}
	arriveInTriage = func(_ context.Context, _ string, project, task uuid.UUID, src triageBusiness.Source, from triageBusiness.Requester) bool {
		got = append(got, held{project, task, src, from})
		return len(got) == 1
	}
	recordSubmission = func(context.Context, uuid.UUID, *uuid.UUID, *uuid.UUID, []byte) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	told := make(chan struct{}, 2)
	tellRules = func(context.Context, map[string]interface{}) { told <- struct{}{} }
	send := func(answers map[string]any) error {
		err := Submit(context.Background(), l.form.Token, answers, formula.NumberSigns{}, "", "", time.Now())
		if err == nil {
			// The rules hear of it in the background: wait, so Cleanup doesn't race them.
			select {
			case <-told:
			case <-time.After(5 * time.Second):
				t.Fatal("the rules never heard of the form")
			}
		}
		return err
	}

	if err := send(map[string]any{"what": "New logo", "email": "Sam <sam@example.com>"}); err != nil {
		t.Fatal(err)
	}
	want := held{l.form.ProjectUUID, made, triageBusiness.FromForm, triageBusiness.Requester{Email: "sam@example.com"}}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("held %+v, want %+v", got, want)
	}

	// Not held (triage off, full, or the item didn't stick): the form is sent all the same.
	if err := send(map[string]any{"what": "New logo"}); err != nil {
		t.Errorf("a task triage didn't hold failed the form: %v", err)
	}
	if len(got) != 2 || got[1].from != (triageBusiness.Requester{}) {
		t.Errorf("a form sent with no address: %+v", got)
	}
}
