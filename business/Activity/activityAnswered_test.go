package business

// V-701: Priority promises "Mentions that ask something of you, and requests
// waiting on you, land here", and on the demo it was empty: the approval
// request (a notice) always scored normal, and the asking mentions dropped
// out as soon as the visitor opened their channel. An item that asks the
// person something stays in Priority until they answer it: they write in
// its conversation or its thread after it, or the approval is decided.
// Opening the conversation isn't an answer. The shapes are the demo's own.

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

var askedAt = time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)

// approvalAsk is "Maya Chen asks you to approve Q4-5 ...": the notice an
// approver gets (business/Approvals tellApprovers).
func approvalAsk(kind, approval string) dgraphModels.UnifiedActivityItem {
	item := dgraphModels.UnifiedActivityItem{ActivityType: "NOTICE", Time: askedAt.Format(time.RFC3339Nano),
		Notice: &dgraphModels.NoticeItem{ID: "n-" + approval, Kind: kind, ResourceKind: "task", ResourceUUID: "task-q4-5",
			Reason: "approver", Data: map[string]string{"approval_id": approval, "to_status": "done"}, CanOpen: true}}
	item.Priority = ScorePriority(item)
	return item
}

// rollbackAsk is the visitor's mention in #engineering that asks for the
// rollback steps (business/DemoSeed Curated).
func rollbackAsk() dgraphModels.UnifiedActivityItem {
	item := dgraphModels.UnifiedActivityItem{ActivityType: "MENTION", Time: askedAt.Format(time.RFC3339),
		Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Uuid: "post-rollback",
			Text:    `<p><span data-type="mention" data-id="visitor">@You</span> can we get the rollback steps into the Launch sync notes before Thursday? Legal asked for them in writing.</p>`,
			Channel: &dgraphStruct.DgraphChannel{Uuid: "ch-engineering"}}}}
	item.Priority = ScorePriority(item)
	return item
}

func TestAnApprovalWaitingOnYouIsPriorityUntilItIsDecided(t *testing.T) {
	if got := approvalAsk("work.approval.requested", "ap-1").Priority; got != PriorityHigh {
		t.Fatalf("an approval request addressed to the person: %q, want high", got)
	}
	items := []dgraphModels.UnifiedActivityItem{approvalAsk("work.approval.requested", "ap-1"), approvalAsk("work.approval.requested", "ap-2")}
	demoteAnswered(items, answers{settled: map[string]bool{"ap-1": true}})
	if items[0].Priority != PriorityLow {
		t.Errorf("a decided approval request: %q, want low", items[0].Priority)
	}
	if items[1].Priority != PriorityHigh {
		t.Errorf("a request still waiting: %q, want high", items[1].Priority)
	}
	// Telling the requester what was decided asks nothing of them.
	if got := approvalAsk("work.approval.decided", "ap-1").Priority; got != PriorityNormal {
		t.Errorf("the decision told to the requester: %q, want normal", got)
	}
}

func TestAMentionThatAsksStaysPriorityUntilTheyAnswer(t *testing.T) {
	if got := rollbackAsk().Priority; got != PriorityHigh {
		t.Fatalf("the rollback ask: %q, want high", got)
	}
	cases := []struct {
		name string
		a    answers
		want string
	}{
		{"nothing written", answers{}, PriorityHigh},
		{"wrote in the channel before it", answers{channels: map[string]time.Time{"ch-engineering": askedAt.Add(-time.Hour)}}, PriorityHigh},
		{"wrote in another channel after it", answers{channels: map[string]time.Time{"ch-design": askedAt.Add(time.Hour)}}, PriorityHigh},
		{"wrote in the channel after it", answers{channels: map[string]time.Time{"ch-engineering": askedAt.Add(time.Minute)}}, PriorityLow},
		{"replied in its thread after it", answers{threads: map[string]time.Time{"post-rollback": askedAt.Add(time.Minute)}}, PriorityLow},
	}
	for _, c := range cases {
		items := []dgraphModels.UnifiedActivityItem{rollbackAsk()}
		demoteAnswered(items, c.a)
		if items[0].Priority != c.want {
			t.Errorf("%s: %q, want %q", c.name, items[0].Priority, c.want)
		}
	}
}
