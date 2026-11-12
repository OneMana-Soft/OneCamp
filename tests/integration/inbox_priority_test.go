//go:build integration
// +build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	approvalsBusiness "github.com/akashc777/OneCamp/business/Approvals"
	_ "github.com/akashc777/OneCamp/business/Follow" // hands the feed its notices
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
)

// V-701: the Inbox's Priority holds what asks the person something until they
// answer it. ben asks ana to approve her task, and asks her for something in
// two channels; once she has opened both channels all three are still
// Priority. Her reply in one mention's thread drops it, her message in the
// other's channel drops that one, and her decision drops the request. The
// feed is the one she already reads: Priority only re-orders it.
func TestPriorityHoldsWhatAsksYouUntilYouAnswer(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	const live = "0001-01-01T00:00:00Z"
	now := time.Now().UTC()
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339Nano) }

	asked, err := approvalsBusiness.AskForTask(ctx, &ben, uuid.MustParse(w.task),
		approvalsBusiness.AskInput{Approvers: []string{ana.UserPostgresInfo.Id.String()}, Ask: "The layout"})
	if err != nil {
		t.Fatalf("ben asks ana to approve her task: %v", err)
	}

	launch, design, rollback, hero := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	members := []map[string]any{{"uid": ana.UserDgraphInfo.Uid}, {"uid": ben.UserDgraphInfo.Uid}}
	var nodes []map[string]any
	for name, id := range map[string]string{"launch": launch, "design": design} {
		w.exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`, id, name+"-"+id[:8])
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "Channel", "ch_uuid": id, "ch_name": name,
			"ch_private": false, "ch_deleted_at": live, "ch_members": members})
	}
	// ask is ben's message in a channel that names ana, as CreatePost stores it.
	ask := func(key, channel, post, text string) {
		nodes = append(nodes,
			map[string]any{"uid": "_:" + key, "dgraph.type": "Post", "post_uuid": post, "post_text": text,
				"post_by": map[string]any{"uid": ben.UserDgraphInfo.Uid}, "post_channel": map[string]any{"uid": "_:" + channel},
				"post_created_at": at(30 * time.Minute), "post_deleted_at": live},
			map[string]any{"uid": "_:m-" + key, "dgraph.type": "Mention", "mention_users": []map[string]any{{"uid": ana.UserDgraphInfo.Uid}},
				"mention_post": map[string]any{"uid": "_:" + key}, "mention_created_at": at(30 * time.Minute)})
	}
	ask("rollback", "launch", rollback, "@ana can we get the rollback steps into the Launch sync notes before Thursday?")
	ask("hero", "design", hero, "Final hero is in. @ana, want a look before it ships?")
	uids := w.dg.Mutate(t, nodes)

	// priority is how ana's feed scores the request and the two asks; ""
	// is one the feed doesn't list.
	priority := func(t *testing.T) (request, rollbackAsk, heroAsk string) {
		t.Helper()
		p, err := activityBusiness.GetUnifiedActivity(ctx, activityBusiness.FeedQuery{User: ana.UserPostgresInfo.Id,
			DgraphID: ana.UserDgraphInfo.Uid, Before: time.Now().Add(time.Minute), Limit: 50, Notices: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Activities {
			switch {
			case it.Notice != nil && it.Notice.Kind == "work.approval.requested" && it.Notice.Data["approval_id"] == asked.ID.String():
				request = it.Priority
			case it.Mention != nil && it.Mention.Post != nil && it.Mention.Post.Uuid == rollback:
				rollbackAsk = it.Priority
			case it.Mention != nil && it.Mention.Post != nil && it.Mention.Post.Uuid == hero:
				heroAsk = it.Priority
			}
		}
		return
	}
	expect := func(t *testing.T, request, rollbackAsk, heroAsk string) {
		t.Helper()
		r, rb, h := priority(t)
		if r != request || rb != rollbackAsk || h != heroAsk {
			t.Errorf("the request %q, the rollback ask %q, the hero ask %q; want %q, %q, %q", r, rb, h, request, rollbackAsk, heroAsk)
		}
	}
	high, low := activityBusiness.PriorityHigh, activityBusiness.PriorityLow

	t.Run("opening the conversations answers nothing", func(t *testing.T) {
		for _, ch := range []string{launch, design} {
			w.exec(`INSERT INTO last_seen_channel (user_id, channel_id, user_last_seen) VALUES ($1, $2, NOW())`, ana.UserPostgresInfo.Id, ch)
		}
		expect(t, high, high, high)
	})
	t.Run("a reply in its thread answers the ask", func(t *testing.T) {
		w.dg.Mutate(t, []map[string]any{{"uid": uids["rollback"], "post_comments": []map[string]any{{"uid": "_:reply", "dgraph.type": "Comment",
			"comment_uuid": uuid.NewString(), "comment_text": "Added them under Rollback.", "comment_by": map[string]any{"uid": ana.UserDgraphInfo.Uid},
			"comment_post": map[string]any{"uid": uids["rollback"]}, "comment_created_at": time.Now().UTC().Format(time.RFC3339Nano),
			"comment_deleted_at": live}}}})
		expect(t, high, low, high)
	})
	t.Run("a message in its channel answers the ask", func(t *testing.T) {
		w.exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, uuid.New(), design, ana.UserPostgresInfo.Id)
		expect(t, high, low, low)
	})
	t.Run("deciding the request answers it", func(t *testing.T) {
		asAna, me := w.signedIn("ana")
		if v, err := approvalsBusiness.Decide(asAna, &me, asked.TaskID, asked.ID, model.VerdictApprove, ""); err != nil || v.State != model.Approved {
			t.Fatalf("ana approves: %+v %v", v, err)
		}
		expect(t, low, low, low)
	})
}
