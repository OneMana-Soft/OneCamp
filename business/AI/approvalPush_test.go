package business

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	"github.com/google/uuid"
)

func TestApprovalPushData(t *testing.T) {
	a := &pendingModels.PendingAction{Id: uuid.New(), Description: "Delete   the Q3\nplanning doc"}
	d := approvalPushData(a, "Release Captain")
	if d[firebaseInit.FIREBASE_PUSH_DATA_TYPE] != "approval" {
		t.Fatalf("type = %q", d[firebaseInit.FIREBASE_PUSH_DATA_TYPE])
	}
	if d[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Release Captain needs your approval" {
		t.Fatalf("title = %q", d[firebaseInit.FIREBASE_PUSH_DATA_TITLE])
	}
	if d[firebaseInit.FIREBASE_PUSH_DATA_BODY] != "Delete the Q3 planning doc" {
		t.Fatalf("body = %q", d[firebaseInit.FIREBASE_PUSH_DATA_BODY])
	}
	if d[firebaseInit.FIREBASE_PUSH_DATA_TAG] != "approval_"+a.Id.String() {
		t.Fatal("each approval needs its own tag, or a second one replaces the first on the lock screen")
	}

	d = approvalPushData(&pendingModels.PendingAction{Id: uuid.New(), Description: strings.Repeat("x", 400)}, "")
	if !strings.HasPrefix(d[firebaseInit.FIREBASE_PUSH_DATA_TITLE], "OneCamp AI ") {
		t.Fatalf("no agent should read as OneCamp AI, got %q", d[firebaseInit.FIREBASE_PUSH_DATA_TITLE])
	}
	if n := len([]rune(d[firebaseInit.FIREBASE_PUSH_DATA_BODY])); n > approvalPushBodyMax {
		t.Fatalf("body is %d runes, max %d", n, approvalPushBodyMax)
	}
	if approvalPushData(&pendingModels.PendingAction{Id: uuid.New()}, "")[firebaseInit.FIREBASE_PUSH_DATA_BODY] == "" {
		t.Fatal("an empty description still needs a body that says what to do")
	}
}

// The click opens the request itself (For you's row), not a page the person
// then has to search, and the toast wears the agent square, not initials.
func TestPushCarriesTheActionID(t *testing.T) {
	a := &pendingModels.PendingAction{Id: uuid.New(), Description: "Post the release notes in #launch"}
	for _, name := range []string{"Release Captain", ""} {
		d := approvalPushData(a, name)
		if d[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] != a.Id.String() {
			t.Fatalf("type_id = %q, want the approval's id %s", d[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID], a.Id)
		}
		if d[firebaseInit.FIREBASE_PUSH_DATA_SENDER_KIND] != firebaseInit.FIREBASE_PUSH_SENDER_AGENT {
			t.Fatalf("sender_kind = %q for %q, want agent", d[firebaseInit.FIREBASE_PUSH_DATA_SENDER_KIND], name)
		}
	}
}

func TestNotifyApprovalNeededPushesToThePersonWhoMustDecide(t *testing.T) {
	savedSend := sendApprovalPush
	t.Cleanup(func() { sendApprovalPush = savedSend })

	type sent struct {
		user string
		data map[string]string
	}
	got := make(chan sent, 1)
	sendApprovalPush = func(_ context.Context, user string, data map[string]string) error {
		got <- sent{user, data}
		return nil
	}

	owner := uuid.New()
	// The approval is named when it is made (nameAgents, one read), so the push
	// says the name the card says and reads nothing of its own.
	notifyApprovalNeeded(&pendingModels.PendingAction{Id: uuid.New(), RequestedBy: owner, Description: "Archive #old-launch", AgentName: "Priya's ChatGPT"})
	select {
	case s := <-got:
		if s.user != owner.String() {
			t.Fatalf("pushed to %s, want the requester %s", s.user, owner)
		}
		if s.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Priya's ChatGPT needs your approval" {
			t.Fatalf("title = %q", s.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no push was sent")
	}
}
