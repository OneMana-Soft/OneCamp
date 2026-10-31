package business

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// A due request reminds the approvers who haven't answered a day before,
// and again when it passes, when the person who asked hears it too; a task
// whose name nobody may read (restricted) is "A task".
func TestADueRequestRemindsWhoHasntAnswered(t *testing.T) {
	ann, bea, asker := uuid.NewString(), uuid.NewString(), uuid.New()
	soonTask, lateTask := uuid.NewString(), uuid.NewString()
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	req := func(task string, unanswered ...string) *model.DueRequest {
		return &model.DueRequest{Approval: &model.Approval{ID: uuid.New(), ResourceKind: model.KindTask, ResourceID: task, RequestedBy: asker,
			Approvers: []string{ann, bea}}, Unanswered: unanswered}
	}
	oldC, oldN, oldT := claimDue, taskNames, tell
	t.Cleanup(func() { claimDue, taskNames, tell = oldC, oldN, oldT })
	readable(t, nil)
	claimDue = func(_ context.Context, now time.Time, limit int) ([]*model.DueRequest, []*model.DueRequest, error) {
		if !now.Equal(at) || limit != remindBatch {
			t.Errorf("claimed at %v, %d", now, limit)
		}
		doc := req(uuid.NewString(), ann)
		doc.ResourceKind = model.KindDoc
		return []*model.DueRequest{req(soonTask, bea), doc}, []*model.DueRequest{req(lateTask, ann, bea)}, nil
	}
	taskNames = func(_ context.Context, ids []string) (map[string]string, error) {
		if len(ids) != 2 {
			t.Errorf("names asked for %v", ids)
		}
		return map[string]string{soonTask: "Ship it"}, nil
	}
	type told struct {
		title string
		to    []string
	}
	var got []told
	tell = func(_ context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string {
		ids := []string{}
		for _, r := range to {
			ids = append(ids, r.UserUUID)
		}
		got = append(got, told{n.Title, ids})
		return nil
	}
	if err := remind(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	want := []told{
		{"Reminder: Ship it waits for your approval, due within a day", []string{bea}},
		{"A task is past its due time and still waits for your approval", []string{ann, bea}},
		{"A task hasn't been approved by its due time", []string{asker.String()}},
	}
	if len(got) != len(want) {
		t.Fatalf("told %+v", got)
	}
	for i := range want {
		if got[i].title != want[i].title || len(got[i].to) != len(want[i].to) || got[i].to[0] != want[i].to[0] {
			t.Errorf("notice %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

// readable makes everyone in the workspace, and able to read every task but
// as unreadable says (seeing's seams); a nil readersErr reads fine.
func readable(t *testing.T, unreadable map[string]bool, readersErr ...error) {
	t.Helper()
	oldR, oldA := readersOf, actorOf
	t.Cleanup(func() { readersOf, actorOf = oldR, oldA })
	readersOf = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		if len(readersErr) > 0 {
			return nil, readersErr[0]
		}
		out := map[string]bool{}
		for _, id := range ids {
			out[id] = !unreadable[id]
		}
		return out, nil
	}
	actorOf = func(_ context.Context, id uuid.UUID) (*userModels.UserInfo, error) {
		return &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id}}, nil
	}
}

// A reminder comes long after the ask: an approver or the person who asked,
// taken off the task's project since, isn't reminded of it, and when who may
// read it can't be read, nobody is.
func TestADueRequestRemindsOnlyWhoMayStillReadTheTask(t *testing.T) {
	ann, bea, asker := uuid.NewString(), uuid.NewString(), uuid.New()
	task := uuid.NewString()
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	oldC, oldN, oldT := claimDue, taskNames, tell
	t.Cleanup(func() { claimDue, taskNames, tell = oldC, oldN, oldT })
	claimDue = func(context.Context, time.Time, int) ([]*model.DueRequest, []*model.DueRequest, error) {
		return nil, []*model.DueRequest{{Approval: &model.Approval{ID: uuid.New(), ResourceKind: model.KindTask, ResourceID: task,
			RequestedBy: asker, Approvers: []string{ann, bea}}, Unanswered: []string{ann, bea}}}, nil
	}
	taskNames = func(context.Context, []string) (map[string]string, error) {
		return map[string]string{task: "Ship it"}, nil
	}
	var told [][]string
	tell = func(_ context.Context, _ notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string {
		ids := []string{}
		for _, r := range to {
			ids = append(ids, r.UserUUID)
		}
		told = append(told, ids)
		return nil
	}

	readable(t, map[string]bool{bea: true, asker.String(): true})
	if err := remind(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	if len(told) != 1 || !slices.Equal(told[0], []string{ann}) {
		t.Errorf("told %v, want ann alone: bea and the person who asked may no longer read the task", told)
	}

	told = nil
	readable(t, nil, errors.New("the graph didn't answer"))
	if err := remind(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	if len(told) != 0 {
		t.Errorf("told %v when who may read the task couldn't be read, want nobody", told)
	}
}
