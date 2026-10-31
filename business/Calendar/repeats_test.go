package Calendar

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	checkInBusiness "github.com/akashc777/OneCamp/business/CheckIn"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	recurrenceModel "github.com/akashc777/OneCamp/models/postgres/TaskRecurrence"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// What repeats on a person's calendar (calendar head 9, gcal-1b): the
// check-ins of the channels in their session's list, named as it names them,
// and which of their open tasks repeat on a schedule, with the dates ahead.
func TestRepeats(t *testing.T) {
	standup, chEng, chOps := uuid.New(), uuid.New(), uuid.New()
	weekly, monthly, theirs := uuid.New(), uuid.New(), uuid.New()
	sam := &model.UserInfo{}
	sam.UserDgraphInfo.Uuid, sam.UserDgraphInfo.Uid = uuid.NewString(), "0x1"
	sam.UserDgraphInfo.Channels = []*dgraphStruct.DgraphChannel{
		{Uuid: chEng.String(), Name: "engineering"}, nil, {Uuid: "not-a-uuid"}, {Uuid: chOps.String(), Name: "ops"}, {Uuid: chEng.String(), Name: "engineering"},
	}
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 14)
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

	origAsks, origScheduled, origOpen := checkInAsks, scheduledRepeats, openTasksOf
	defer func() { checkInAsks, scheduledRepeats, openTasksOf = origAsks, origScheduled, origOpen }()
	var channelsRead []uuid.UUID
	checkInAsks = func(channels []uuid.UUID, f, tt time.Time, max int) ([]checkInBusiness.Asks, error) {
		channelsRead = channels
		if !f.Equal(from) || !tt.Equal(to) || max != maxRepeatTimes {
			t.Errorf("asked %v %v %d", f, tt, max)
		}
		return []checkInBusiness.Asks{{ID: standup, Channel: chEng, Question: "What did you work on today?", At: []time.Time{from.Add(17 * time.Hour)}}}, nil
	}
	var repeatsOf [][]string
	scheduledRepeats = func(tasks []string) ([]recurrenceModel.TaskRecurrence, error) {
		repeatsOf = append(repeatsOf, tasks)
		return []recurrenceModel.TaskRecurrence{
			{TaskUUID: weekly, Rule: "FREQ=WEEKLY;BYDAY=MO", Mode: recurrenceModel.ModeSchedule},
			{TaskUUID: monthly, Rule: "FREQ=MONTHLY", Mode: recurrenceModel.ModeSchedule},
		}, nil
	}
	var filters []string
	due := func(d time.Time) *time.Time { return &d }
	openTasksOf = func(_ context.Context, u *model.UserInfo, filter string) ([]*dgraphStruct.DgraphTask, error) {
		if u != sam {
			t.Errorf("read the tasks of %v", u)
		}
		filters = append(filters, filter)
		return []*dgraphStruct.DgraphTask{
			// Its next one is the 1st of December, after the stretch: none.
			{Uuid: monthly.String(), Name: "Invoice", DueDate: due(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))},
			{Uuid: weekly.String(), Name: "Plan the week", DueDate: due(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)), Project: &dgraphStruct.DgraphProject{Uuid: "p1", Name: "Ops"}},
			// Doesn't repeat: left out.
			{Uuid: theirs.String(), Name: "Not a repeat", DueDate: due(from)},
			// No due date: never asked about.
			{Uuid: uuid.NewString(), Name: "Someday"},
			nil,
		}, nil
	}

	view, err := Repeats(context.Background(), sam, from, to, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(channelsRead) != 2 || channelsRead[0] != chEng || channelsRead[1] != chOps {
		t.Fatalf("read the check-ins of %v", channelsRead)
	}
	if len(view.CheckIns) != 1 || view.CheckIns[0].ChannelName != "engineering" || view.CheckIns[0].ID != standup.String() || len(view.CheckIns[0].At) != 1 {
		t.Fatalf("check-ins %+v", view.CheckIns)
	}
	// The asker's open tasks first, then the repeats of those with a due date alone.
	if len(filters) != 1 || filters[0] != dgraphStruct.TASK_OPEN_FILTER {
		t.Fatalf("filter %v", filters)
	}
	if len(repeatsOf) != 1 || strings.Join(repeatsOf[0], ",") != monthly.String()+","+weekly.String()+","+theirs.String() {
		t.Fatalf("asked the repeats of %v", repeatsOf)
	}
	if len(view.Tasks) != 1 {
		t.Fatalf("tasks %+v", view.Tasks)
	}
	plan := view.Tasks[0]
	if plan.TaskUUID != weekly.String() || plan.Name != "Plan the week" || plan.ProjectName != "Ops" || plan.Rule != "FREQ=WEEKLY;BYDAY=MO" ||
		len(plan.Due) != 1 || !plan.Due[0].Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("plan %+v", plan)
	}

	// No open task with a due date: the repeats aren't asked.
	repeatsOf = nil
	openTasksOf = func(context.Context, *model.UserInfo, string) ([]*dgraphStruct.DgraphTask, error) {
		return []*dgraphStruct.DgraphTask{{Uuid: theirs.String()}}, nil
	}
	if view, err := Repeats(context.Background(), sam, from, to, now); err != nil || len(view.Tasks) != 0 || len(repeatsOf) != 0 {
		t.Fatalf("no dated tasks: %+v %v %v", view, err, repeatsOf)
	}

	// A failed read is an error, never "nothing repeats".
	checkInAsks = func([]uuid.UUID, time.Time, time.Time, int) ([]checkInBusiness.Asks, error) {
		return nil, errors.New("down")
	}
	if _, err := Repeats(context.Background(), sam, from, to, now); err == nil {
		t.Fatal("a failed check-in read answered")
	}
}
