package business

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workGraph "github.com/akashc777/OneCamp/business/WorkGraph"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	gcDoc   = "11111111-0000-4000-8000-000000000001"
	gcTask  = "22222222-0000-4000-8000-000000000001"
	gcGone  = "22222222-0000-4000-8000-000000000002"
	gcProj  = "33333333-0000-4000-8000-000000000001"
	gcPost  = "44444444-0000-4000-8000-000000000001"
	gcChan  = "55555555-0000-4000-8000-000000000001"
	gcAsker = "66666666-0000-4000-8000-000000000001"
)

func gcUser() *userModels.UserInfo {
	return &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x9", Uuid: gcAsker}}
}

// withGraph sets what the work graph answers and which ends Liveness says are
// gone; it records the seeds and readers asked with.
func withGraph(t *testing.T, links []workGraph.Link, gone ...string) (seeds *[]workGraph.Ref, readers *[]accessBusiness.Subject, calls *int) {
	t.Helper()
	savedE, savedL := expandGraph, graphLiveStates
	t.Cleanup(func() { expandGraph, graphLiveStates = savedE, savedL })
	seeds, readers, calls = &[]workGraph.Ref{}, &[]accessBusiness.Subject{}, new(int)
	expandGraph = func(_ context.Context, who []accessBusiness.Subject, s []workGraph.Ref, _ []domain.Kind) ([]workGraph.Link, error) {
		*calls++
		*seeds, *readers = s, who
		return links, nil
	}
	graphLiveStates = func(context.Context, []workGraph.Link) map[string]map[string]bool {
		state := map[string]bool{}
		for _, l := range links {
			for _, h := range []workGraph.Hop{l.From, l.To} {
				state[strings.ToLower(h.ID)] = true
			}
		}
		for _, g := range gone {
			delete(state, g)
		}
		return map[string]map[string]bool{"task": state, "doc": state, "post": state}
	}
	return
}

func hopG(kind, id, title string) workGraph.Hop {
	return workGraph.Hop{Kind: kind, ID: id, Title: title}
}

var gcLinks = []workGraph.Link{
	{From: hopG("doc", gcDoc, "Launch plan"), Rel: "linked_from", To: hopG("task", gcTask, "Record the film")},
	{From: hopG("task", gcTask, "Record the film"), Rel: "in_project", To: hopG("project", gcProj, "Launch")},
	{From: hopG("doc", gcDoc, "Launch plan"), Rel: "linked_from", To: hopG("task", gcGone, "Old task")},
	{From: hopG("doc", gcDoc, "Launch plan"), Rel: "shared_in", To: workGraph.Hop{Kind: "message", ID: gcPost, Title: "The plan is up",
		Data: map[string]any{"channel_id": gcChan, "channel_name": "launch"}}},
}

var gcHits = []ai.SimilarResult{
	{ContentType: "doc", ContentUUID: gcDoc},
	{ContentType: "comment", ContentUUID: "77777777-0000-4000-8000-000000000001", TaskUUID: gcTask},
	{ContentType: "chat", ContentUUID: "88888888-0000-4000-8000-000000000001"},
	{ContentType: "memory", ContentUUID: "99999999-0000-4000-8000-000000000001"},
}

// TestGraphContextIsOnUnlessSwitchedOff: on by default since its evaluation;
// a deployment that sets AI_GRAPH_CONTEXT off reads no graph for Ask AI.
func TestGraphContextIsOnUnlessSwitchedOff(t *testing.T) {
	t.Setenv("AI_GRAPH_CONTEXT", "")
	_, _, calls := withGraph(t, gcLinks)
	if block, _ := graphContext(context.Background(), gcUser(), gcHits); block == "" || *calls != 1 {
		t.Errorf("on by default, but read the graph %d times: %q", *calls, block)
	}
	for _, off := range []string{"off", "false", "0", "no"} {
		t.Setenv("AI_GRAPH_CONTEXT", off)
		*calls = 0
		if block, src := graphContext(context.Background(), gcUser(), gcHits); block != "" || src != nil || *calls != 0 {
			t.Errorf("switched %s: read the graph %d times: %q", off, *calls, block)
		}
	}
}

// TestGraphContextCitesEachHop: the seeds are the hits the work graph holds
// (a doc, the task a comment is on; never a DM or a memory note); each line
// names both ends by kind, title and id; a link to something gone in its own
// store is dropped; the doc, task and message it names are sources, a
// message with its channel.
func TestGraphContextCitesEachHop(t *testing.T) {
	t.Setenv("AI_GRAPH_CONTEXT", "true")
	seeds, readers, _ := withGraph(t, gcLinks, gcGone)
	block, sources := graphContext(context.Background(), gcUser(), gcHits)
	if got := fmt.Sprint(*seeds); got != fmt.Sprint([]workGraph.Ref{{Kind: "doc", ID: gcDoc}, {Kind: "task", ID: gcTask}}) {
		t.Errorf("seeds %s", got)
	}
	if len(*readers) != 1 || (*readers)[0].UserUUID != gcAsker {
		t.Errorf("readers %+v", *readers)
	}
	for _, want := range []string{
		`doc "Launch plan" (id: ` + gcDoc + `) linked from task "Record the film" (id: ` + gcTask + `)`,
		`task "Record the film" (id: ` + gcTask + `) in the project project "Launch" (id: ` + gcProj + `)`,
		`shared in message "The plan is up" (id: ` + gcPost + `)`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the block lacks %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, gcGone) {
		t.Errorf("names a task gone from Postgres:\n%s", block)
	}
	kinds := map[string]bool{}
	for _, s := range sources {
		kinds[s.ContentType+" "+s.ContentUUID] = true
		if s.ContentType == "post" && (s.PostUUID != gcPost || s.ChannelUUID != gcChan) {
			t.Errorf("the message's source can't be opened: %+v", s)
		}
	}
	for _, want := range []string{"doc " + gcDoc, "task " + gcTask, "post " + gcPost} {
		if !kinds[want] {
			t.Errorf("no source %s in %v", want, kinds)
		}
	}
	if len(sources) != 3 {
		t.Errorf("%d sources, want 3 (a project isn't opened from an answer): %+v", len(sources), sources)
	}
}

// TestGraphContextFitsItsBudget: however many links, the lines stay within
// graphContextMaxChars, every line whole.
func TestGraphContextFitsItsBudget(t *testing.T) {
	t.Setenv("AI_GRAPH_CONTEXT", "true")
	var many []workGraph.Link
	for i := 0; i < 60; i++ {
		many = append(many, workGraph.Link{From: hopG("doc", gcDoc, strings.Repeat("Long title ", 20)), Rel: "linked_from",
			To: hopG("task", fmt.Sprintf("22222222-0000-4000-8000-%012d", i), strings.Repeat("Another long title ", 10))})
	}
	withGraph(t, many)
	block, _ := graphContext(context.Background(), gcUser(), gcHits)
	if block == "" || len(block) > graphContextMaxChars+1 {
		t.Errorf("%d chars, budget %d", len(block), graphContextMaxChars)
	}
	for _, line := range strings.Split(strings.TrimSpace(block), "\n")[2:] {
		if !strings.HasPrefix(line, "- doc") || !strings.HasSuffix(line, ")") {
			t.Errorf("a cut line: %q", line)
		}
	}
}

// TestGraphContextNeverHoldsTheAnswer: a graph slower than the wait is left
// out, and the answer goes on.
func TestGraphContextNeverHoldsTheAnswer(t *testing.T) {
	t.Setenv("AI_GRAPH_CONTEXT", "true")
	withGraph(t, gcLinks)
	expandGraph = func(ctx context.Context, _ []accessBusiness.Subject, _ []workGraph.Ref, _ []domain.Kind) ([]workGraph.Link, error) {
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
		return gcLinks, nil
	}
	start := time.Now()
	block, _ := graphContext(context.Background(), gcUser(), gcHits)
	if took := time.Since(start); block != "" || took > graphContextWait+300*time.Millisecond {
		t.Errorf("waited %v for %q", took, block)
	}
}

// TestGraphContextForSomeoneElse: an agent run asked by someone who can't be
// identified adds nothing.
func TestGraphContextForSomeoneElse(t *testing.T) {
	t.Setenv("AI_GRAPH_CONTEXT", "true")
	_, _, calls := withGraph(t, gcLinks)
	ctx := ai.WithRunRequester(context.Background(), "", "aaaaaaaa-0000-4000-8000-000000000001")
	if block, _ := graphContext(ctx, gcUser(), gcHits); block != "" || *calls != 0 {
		t.Errorf("an unknown asker: %d reads, %q", *calls, block)
	}
}
