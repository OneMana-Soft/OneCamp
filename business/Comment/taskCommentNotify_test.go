package business

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// taskCommentSends is what one task comment sent: whom the audience was asked
// about, the push (to whom, what, as which notice) and the email.
type taskCommentSends struct {
	askedThread, askedForThem []string
	pushedTo                  []string
	pushData                  map[string]string
	noticeID                  string
	emailedTo                 []string
}

// stubTaskComment points the seams at a task whose earlier comments are by
// thread's people, whose project's settings let audience hear of the comment,
// and which readers may read; a non-nil error fails that read.
func stubTaskComment(t *testing.T, thread []string, threadErr error, audience []string, audienceErr error, readers map[string]bool, readersErr error) *taskCommentSends {
	t.Helper()
	sent := &taskCommentSends{}
	thread0, audience0, readers0, deliver0, email0, icon0 := taskCommentThread, taskCommentAudience, taskCommentReaders, taskCommentDeliver, taskCommentEmail, taskCommentIcon
	t.Cleanup(func() {
		taskCommentThread, taskCommentAudience, taskCommentReaders, taskCommentDeliver, taskCommentEmail, taskCommentIcon = thread0, audience0, readers0, deliver0, email0, icon0
	})
	taskCommentThread = func(ctx context.Context, taskUUID string) (*dgraphStruct.DgraphTask, error) {
		if threadErr != nil {
			return nil, threadErr
		}
		task := &dgraphStruct.DgraphTask{Uuid: taskUUID, Comments: []*dgraphStruct.DgraphComment{{}}} // one whose writer isn't known
		for _, id := range thread {
			task.Comments = append(task.Comments, commentBy(id))
		}
		return task, nil
	}
	taskCommentAudience = func(ctx context.Context, projectID, writerID string, thread, forThem []string) ([]string, error) {
		if projectID != "launch" || writerID != "maya" {
			t.Errorf("the audience was asked of project %q for writer %q, want launch and maya", projectID, writerID)
		}
		sent.askedThread, sent.askedForThem = thread, forThem
		return audience, audienceErr
	}
	taskCommentReaders = func(ctx context.Context, taskUUID string, ids []string) (map[string]bool, error) {
		if taskUUID != "kit" {
			t.Errorf("who may read task %q was asked, want kit", taskUUID)
		}
		return readers, readersErr
	}
	taskCommentDeliver = func(ctx context.Context, recipients []string, data map[string]string, noticeID string) {
		sent.pushedTo, sent.pushData, sent.noticeID = recipients, data, noticeID
	}
	taskCommentEmail = func(actorUUID, actorName, actorAvatar, taskUUID, taskName, body, commentUUID string, recipients []string) {
		sent.emailedTo = recipients
	}
	taskCommentIcon = func(context.Context, *string) string { return "icon" }
	return sent
}

func pressKit(assignee string) *dgraphStruct.DgraphTask {
	task := &dgraphStruct.DgraphTask{Uuid: "kit", Name: "Press kit", Project: &dgraphStruct.DgraphProject{Uuid: "launch"}}
	if assignee != "" {
		task.Assignee = person(assignee)
	}
	return task
}

// A comment on a task is pushed as every push goes (Deliver), so the desktop
// app hears of it, to whom the project's settings say hears of it, if they
// may read the task now: a setting outlives someone's place in the project.
func TestATaskCommentIsPushedToItsReadersBySetting(t *testing.T) {
	sent := stubTaskComment(t, []string{"maya", "ben", "tom"}, nil,
		[]string{"ben", "sam", "lee", "olu"}, nil, map[string]bool{"ben": true, "sam": true, "olu": true}, nil)
	sendNewTaskCommentNotification("Comment - Maya", "Draft is in the drive", "c1",
		[]*dgraphStruct.DgraphUser{person("sam"), nil, person("mo")}, pressKit("olu"), person("maya"))

	if want := []string{"ben", "tom"}; !slices.Equal(sent.askedThread, want) {
		t.Errorf("the thread asked about was %v, want %v: never the writer, nor a comment whose writer isn't known", sent.askedThread, want)
	}
	if want := []string{"sam", "mo", "olu"}; !slices.Equal(sent.askedForThem, want) {
		t.Errorf("whom it's for was asked as %v, want %v: whom it mentions and whom the task is given to", sent.askedForThem, want)
	}
	if want := []string{"ben", "sam", "olu"}; !slices.Equal(sent.pushedTo, want) {
		t.Errorf("pushed to %v, want %v: lee hears of it by a setting left behind, and may not read the task", sent.pushedTo, want)
	}
	if want := "task_comment:kit:c1"; sent.noticeID != want {
		t.Errorf("the notice was %q, want %q, as its email's", sent.noticeID, want)
	}
	for k, want := range map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:      firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK_COMMENT,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:   "launch",
		firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID: "kit",
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:     "Comment - Maya",
		firebaseInit.FIREBASE_PUSH_DATA_BODY:      "Draft is in the drive",
		firebaseInit.FIREBASE_PUSH_DATA_ICON:      "icon",
	} {
		if got := sent.pushData[k]; got != want {
			t.Errorf("the push's %s was %q, want %q", k, got, want)
		}
	}
	if want := []string{"ben", "tom", "sam", "mo", "olu"}; !slices.Equal(sent.emailedTo, want) {
		t.Errorf("emailed %v, want %v: the dispatcher keeps it to the task's readers", sent.emailedTo, want)
	}
}

func TestATaskCommentByItsAssigneeIsNotForThem(t *testing.T) {
	sent := stubTaskComment(t, nil, nil, []string{"ben"}, nil, map[string]bool{"ben": true}, nil)
	sendNewTaskCommentNotification("Comment - Maya", "Done", "c2", nil, pressKit("maya"), person("maya"))
	if len(sent.askedForThem) != 0 {
		t.Errorf("whom it's for was %v, want nobody: Maya wrote it on her own task", sent.askedForThem)
	}
	if !slices.Equal(sent.pushedTo, []string{"ben"}) {
		t.Errorf("pushed to %v, want [ben]", sent.pushedTo)
	}
}

// Without an answer of who may hear of it, or who may read the task, nobody
// is pushed; the email still goes, as it asks who may read the task itself.
func TestATaskCommentPushesNobodyItCantCheck(t *testing.T) {
	for _, c := range []struct {
		name                    string
		audienceErr, readersErr error
	}{
		{"who hears of it by setting can't be read", errors.New("postgres is down"), nil},
		{"who may read the task can't be read", nil, errors.New("dgraph is down")},
	} {
		t.Run(c.name, func(t *testing.T) {
			sent := stubTaskComment(t, []string{"ben"}, nil, []string{"ben"}, c.audienceErr, map[string]bool{"ben": true}, c.readersErr)
			sendNewTaskCommentNotification("Comment - Maya", "Draft", "c3", nil, pressKit("olu"), person("maya"))
			if sent.pushedTo != nil {
				t.Errorf("pushed to %v, want nobody", sent.pushedTo)
			}
			if want := []string{"ben", "olu"}; !slices.Equal(sent.emailedTo, want) {
				t.Errorf("emailed %v, want %v", sent.emailedTo, want)
			}
		})
	}

	t.Run("its thread can't be read", func(t *testing.T) {
		sent := stubTaskComment(t, nil, errors.New("dgraph is down"), []string{"ben"}, nil, map[string]bool{"ben": true}, nil)
		sendNewTaskCommentNotification("Comment - Maya", "Draft", "c4", nil, pressKit("olu"), person("maya"))
		if sent.pushedTo != nil || sent.emailedTo != nil {
			t.Errorf("pushed %v and emailed %v, want nothing sent", sent.pushedTo, sent.emailedTo)
		}
	})
}
