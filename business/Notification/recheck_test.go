package notification

// The send-time re-check (recheck.go) without a store: each kind's read is
// a stub here, so what is asked of it, how often, and what the worker does
// with its answer can be seen. The kinds' own reads are proven against
// Postgres and the graph in sendRecheck_integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
	emailQueueModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailQueue"
	emailService "github.com/akashc777/OneCamp/services/Email"
)

// queuedAbout is a queue row of someone's email about a.
func queuedAbout(t *testing.T, a About) *emailQueueModels.QueueItem {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"about": a})
	if err != nil {
		t.Fatal(err)
	}
	return &emailQueueModels.QueueItem{ID: uuid.New(), UserID: uuid.New(), Metadata: meta}
}

// stubRechecker puts answer in kind's place for the test, and answers each
// call's asks, in order.
func stubRechecker(t *testing.T, kind string, answer func([]asked) ([]bool, error)) *[][]asked {
	t.Helper()
	was := recheckers[kind]
	t.Cleanup(func() { recheckers[kind] = was })
	var calls [][]asked
	recheckers[kind] = func(_ context.Context, asks []asked) ([]bool, error) {
		calls = append(calls, asks)
		return answer(asks)
	}
	return &calls
}

// An email whose re-check can't be read is neither sent nor suppressed: it
// waits, saying why, and is asked again later. Only its own kind waits;
// another kind in the same batch is still decided.
func TestAnEmailWhoseReCheckCantBeReadWaitsUnsent(t *testing.T) {
	stubRechecker(t, AboutChat, func([]asked) ([]bool, error) { return nil, errors.New("the graph is away") })
	stubRechecker(t, AboutChannel, func(asks []asked) ([]bool, error) { return make([]bool, len(asks)), nil })
	chat := queuedAbout(t, About{Kind: AboutChat, Item: "m1", Place: "Grp-1", Heard: notifylevel.Named})
	channel := queuedAbout(t, About{Kind: AboutChannel, Item: "p1", Place: "c1", Heard: notifylevel.Named})
	v := verdicts(context.Background(), []*emailQueueModels.QueueItem{chat, channel})
	if v[chat.ID] != askLater || v[channel.ID] != noLongerTold {
		t.Fatalf("the chat row's verdict is %d (want askLater %d), the channel row's %d (want noLongerTold %d)", v[chat.ID], askLater, v[channel.ID], noLongerTold)
	}

	sends, retried := 0, map[uuid.UUID]string{}
	wasSend, wasRetry := sendEmail, retryLater
	t.Cleanup(func() { sendEmail, retryLater = wasSend, wasRetry })
	sendEmail = func(context.Context, emailService.SendOptions) (emailService.SendResult, error) {
		sends++
		return emailService.SendResult{}, nil
	}
	retryLater = func(ids []uuid.UUID, why string, at time.Time) error {
		if at.Before(time.Now()) {
			t.Errorf("the rows are asked again at %v, before now", at)
		}
		for _, id := range ids {
			retried[id] = why
		}
		return nil
	}
	settle(context.Background(), []*emailQueueModels.QueueItem{chat})
	if sends != 0 || !reflect.DeepEqual(retried, map[uuid.UUID]string{chat.ID: "what it is about couldn't be read"}) {
		t.Errorf("the worker sent %d emails and left %v waiting, want none sent and the chat row waiting", sends, retried)
	}
}

// An email about several items (an Urgent digest's tasks) is asked of each,
// in the one read its kind makes for the batch, and holds only while every
// one does; an email about one item is the same with a list of one.
func TestAnEmailAboutManyItemsHoldsOnlyWhileEachDoes(t *testing.T) {
	calls := stubRechecker(t, AboutTask, func(asks []asked) ([]bool, error) {
		holds := make([]bool, len(asks))
		for i, a := range asks {
			holds[i] = a.about.Item != "t2"
		}
		return holds, nil
	})
	named := func(items ...string) About {
		return About{Kind: AboutTask, Item: items[0], Items: items, Heard: notifylevel.Named}
	}
	all, oneGone, single := queuedAbout(t, named("t1", "t3")), queuedAbout(t, named("t1", "t2", "t3")), queuedAbout(t, About{Kind: AboutTask, Item: "t3", Heard: notifylevel.Named})
	v := verdicts(context.Background(), []*emailQueueModels.QueueItem{all, oneGone, single})
	if v[all.ID] != send || v[oneGone.ID] != noLongerTold || v[single.ID] != send {
		t.Errorf("verdicts: every task holds %d, one doesn't %d, a single task %d; want %d, %d, %d", v[all.ID], v[oneGone.ID], v[single.ID], send, noLongerTold, send)
	}
	if len(*calls) != 1 {
		t.Fatalf("the task kind was asked %d times for one batch, want once", len(*calls))
	}
	var items []string
	for _, a := range (*calls)[0] {
		items = append(items, a.about.Item)
	}
	if !reflect.DeepEqual(items, []string{"t1", "t3", "t1", "t2", "t3", "t3"}) {
		t.Errorf("the batch asked of %v, want each row's every task", items)
	}
}

// A batch of doc emails asks the doc read rule once, for every doc and
// person in it, however many rows and docs there are.
func TestDocEmailsAskTheReadRuleOnceABatch(t *testing.T) {
	was := docReaders
	t.Cleanup(func() { docReaders = was })
	var calls []map[string][]string
	RegisterDocReaders(func(_ context.Context, people map[string][]string) (map[string]map[string]bool, error) {
		calls = append(calls, people)
		return map[string]map[string]bool{"d1": {"u1": true}, "d2": {"u3": true}}, nil
	})
	holds, err := docStillHolds(context.Background(), []asked{
		{user: "u1", about: About{Kind: AboutDoc, Item: "d1"}},
		{user: "u2", about: About{Kind: AboutDoc, Item: "d1"}},
		{user: "u3", about: About{Kind: AboutDoc, Item: "d2"}},
	})
	if err != nil || !reflect.DeepEqual(holds, []bool{true, false, true}) {
		t.Errorf("holds %v err %v, want [true false true]", holds, err)
	}
	if want := []map[string][]string{{"d1": {"u1", "u2"}, "d2": {"u3"}}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("the read rule was asked %v, want once: %v", calls, want)
	}
}

// A batch of project update emails reads the projects' members once, for
// every project and person in it.
func TestProjectEmailsReadTheMembersOnceABatch(t *testing.T) {
	was := membersAmongEach
	t.Cleanup(func() { membersAmongEach = was })
	var calls []map[string][]string
	membersAmongEach = func(_ context.Context, people map[string][]string) (map[string]map[string]bool, error) {
		calls = append(calls, people)
		return map[string]map[string]bool{}, nil
	}
	// Ids that aren't uuids stand nowhere, so the settings read asks nothing
	// of the store.
	if _, err := projectStillHolds(context.Background(), []asked{
		{user: "u1", about: About{Kind: AboutProject, Item: "up1", Place: "p1"}},
		{user: "u2", about: About{Kind: AboutProject, Item: "up1", Place: "p1"}},
		{user: "u3", about: About{Kind: AboutProject, Item: "up2", Place: "p2"}},
	}); err != nil {
		t.Fatal(err)
	}
	if want := []map[string][]string{{"p1": {"u1", "u2"}, "p2": {"u3"}}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("the members were read %v, want once: %v", calls, want)
	}
}
