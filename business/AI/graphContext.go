package business

// GraphRAG for Ask AI (DG7): the semantic search's hits, followed one step
// along the work graph, so an answer can say "the launch plan is linked from
// Record the film, in project Launch" and cite each end.
//
// THREE STEPS, AND ONLY THE FIRST IS A SEARCH.
//  1. The seeds are the hits Ask AI already has: OpenSearch kNN, filtered
//     inside the query by the asker's permission filter (buildPermissionFilter,
//     unchanged) and checked live (KeepLive). Vectors stay in OpenSearch:
//     Dgraph's similar_to filters after it picks its top k, so a person with
//     narrow access would silently get fewer hits.
//  2. One graph read, one hop over DG5's allow-list from at most 12 seeds,
//     at most 4 targets of each edge (business/WorkGraph ExpandFor), then
//     every seed and every node through business/EntityLink ReadMany as the
//     asker, and, when an agent run acts for someone other than its sponsor,
//     as that person too. A link to anything either can't see is dropped, so
//     a private doc is never named. At most 30 links, ranked by kind, then
//     most recent change.
//  3. Every node through domain/Liveness as well: a task, project, post or
//     comment deleted in Postgres whose graph node hasn't heard yet is
//     dropped, as search results are.
//
// THE BUDGET. The read runs after the kNN answer and is waited on for at most
// graphContextWait (the same 800 ms the optional sources get); late, it is
// left out and the answer goes ahead without it. Its lines fit in
// graphContextMaxChars (two knowledge sources' share: agentKnowledge's
// 1,500 a source), each title cut to 60 characters.
//
// THE SWITCH. AI_GRAPH_CONTEXT (services/AI GraphContextEnabled), on unless a
// deployment turns it off; see the release note for the evaluation.

import (
	"context"
	"fmt"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	workGraph "github.com/akashc777/OneCamp/business/WorkGraph"
	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	// graphContextWait is the longest Ask AI waits for the graph's links.
	graphContextWait = 800 * time.Millisecond
	// graphContextMaxChars bounds the relationship lines.
	graphContextMaxChars = 3000
	// graphTitleMax is how much of a title a line quotes.
	graphTitleMax = 60
)

// Seams for tests.
var (
	expandGraph     = workGraph.ExpandFor
	graphLiveStates = func(ctx context.Context, links []workGraph.Link) map[string]map[string]bool {
		return liveness.States(ctx, liveness.Checkers, linkEnds(links), endKey)
	}
)

// graphSeeds is the work-graph things search hits are: a doc, a task, a
// project, a channel post, and for a comment the thing it is on. A DM, a
// group chat and a memory note have no place in the work graph.
func graphSeeds(hits []ai.SimilarResult) []workGraph.Ref {
	var out []workGraph.Ref
	add := func(kind, id string) {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			return
		}
		r := workGraph.Ref{Kind: kind, ID: id}
		for _, x := range out {
			if x == r {
				return
			}
		}
		out = append(out, r)
	}
	for _, h := range hits {
		switch h.ContentType {
		case "doc":
			add("doc", h.ContentUUID)
		case "task":
			add("task", h.ContentUUID)
		case "project":
			add("project", helpers.FirstNonBlank(h.ProjectUUID, h.ContentUUID))
		case "post":
			add("message", h.ContentUUID)
		case "comment":
			switch {
			case h.DocUUID != "":
				add("doc", h.DocUUID)
			case h.TaskUUID != "":
				add("task", h.TaskUUID)
			case h.PostUUID != "":
				add("message", h.PostUUID)
			}
		}
	}
	return out
}

// graphContext is the relationship lines for hits, as the asker (and the
// person an agent acts for) may see them, and the sources they cite. Empty
// when there is nothing to add, the switch is off, or the graph is slow or
// down: the answer never waits on it longer than graphContextWait.
func graphContext(ctx context.Context, userInfo *userModels.UserInfo, hits []ai.SimilarResult) (string, []adapter.SourceRef) {
	return formatGraphContext(graphLinks(ctx, userInfo, graphSeeds(hits)))
}

// graphLinks is what is one hop from seeds, as every reader may see it and
// live in the store that owns it; nil when the switch is off, there are no
// seeds, the asker can't be resolved, or the graph errs or is slow.
func graphLinks(ctx context.Context, userInfo *userModels.UserInfo, seeds []workGraph.Ref) []workGraph.Link {
	if !ai.GraphContextEnabled() || userInfo == nil || len(seeds) == 0 {
		return nil
	}
	readers, err := askReaders(ctx, userInfo)
	if err != nil {
		return nil
	}
	type answer struct {
		links []workGraph.Link
		err   error
	}
	wait, cancel := context.WithTimeout(ctx, graphContextWait)
	defer cancel()
	done := make(chan answer, 1)
	// Read once, here: the read may outlive this call (it is abandoned at the
	// wait, not stopped), so it never touches the package's seams again.
	expand, states := expandGraph, graphLiveStates
	helpers.GoSafeNamed("ai.graph-context", func() {
		links, err := expand(wait, readers, seeds, nil)
		if err == nil {
			links = keepLiveLinksBy(wait, links, states)
		}
		done <- answer{links, err}
	})
	select {
	case a := <-done:
		if a.err != nil {
			helpers.LogErrorWithContext(ctx, "AI graph context: %v", a.err)
			return nil
		}
		return a.links
	case <-wait.Done():
		helpers.LogInfoWithContext(ctx, "AI graph context slow; omitting it")
		return nil
	}
}

// UnifiedSourceConnected is the unified search's group of things one step
// along the work graph from its workspace hits.
const UnifiedSourceConnected = "connected"

// connectedHits is the unified search's Connected group: for each link from
// a workspace hit, the thing it reaches, if the app can open it from a
// search, with the relationship as its snippet.
func connectedHits(ctx context.Context, userInfo *userModels.UserInfo, workspace []UnifiedHit) []UnifiedHit {
	seedHits := make([]ai.SimilarResult, 0, len(workspace))
	for _, h := range workspace {
		seedHits = append(seedHits, ai.SimilarResult{ContentType: h.ContentType, ContentUUID: h.ContentUUID,
			ProjectUUID: h.ProjectUUID, PostUUID: h.PostUUID, TaskUUID: h.TaskUUID, DocUUID: h.DocUUID})
	}
	var out []UnifiedHit
	for _, l := range graphLinks(ctx, userInfo, graphSeeds(seedHits)) {
		st := sourceType(l.To.Kind)
		if st == "" || len(out) == unifiedMaxPerSource {
			continue
		}
		h := UnifiedHit{
			Source:      UnifiedSourceConnected,
			Title:       helpers.OneLine(l.To.Title, graphTitleMax),
			Snippet:     clipUnified(graphCite(l.From) + " " + relPhrase(l.Rel) + " " + graphCite(l.To)),
			Meta:        relPhrase(l.Rel) + " " + helpers.OneLine(l.From.Title, graphTitleMax),
			ContentType: st,
			ContentUUID: l.To.ID,
		}
		switch st {
		case "doc":
			h.DocUUID = l.To.ID
		case "task":
			h.TaskUUID = l.To.ID
		case "post":
			h.PostUUID = l.To.ID
			h.ChannelUUID, _ = l.To.Data["channel_id"].(string)
			h.ChannelName, _ = l.To.Data["channel_name"].(string)
		}
		out = append(out, h)
	}
	return out
}

// graphEnd is one end of a link, as domain/Liveness names content.
type graphEnd struct{ kind, id string }

// livenessType is the content type domain/Liveness checks a kind by: the
// kinds whose rows live in Postgres (or whose doc lives in the graph), and ""
// for a kind it doesn't check.
func livenessType(kind string) string {
	switch kind {
	case "doc", "task", "project", "comment":
		return kind
	case "message":
		return "post"
	}
	return ""
}

func endKey(e graphEnd) (string, string) { return livenessType(e.kind), e.id }

func linkEnds(links []workGraph.Link) []graphEnd {
	out := make([]graphEnd, 0, 2*len(links))
	for _, l := range links {
		out = append(out, graphEnd{l.From.Kind, l.From.ID}, graphEnd{l.To.Kind, l.To.ID})
	}
	return out
}

// keepLiveLinksBy drops a link either of whose ends is gone or deleted in the
// store that owns it (one query per checked kind); a check that can't run
// keeps its links, as search results are kept.
func keepLiveLinksBy(ctx context.Context, links []workGraph.Link, read func(context.Context, []workGraph.Link) map[string]map[string]bool) []workGraph.Link {
	if len(links) == 0 {
		return links
	}
	states := read(ctx, links)
	live := func(kind, id string) bool {
		state, checked := states[livenessType(kind)]
		if !checked {
			return true
		}
		ok, exists := state[strings.ToLower(id)]
		return exists && ok
	}
	out := links[:0:0]
	for _, l := range links {
		if live(l.From.Kind, l.From.ID) && live(l.To.Kind, l.To.ID) {
			out = append(out, l)
		}
	}
	return out
}

// graphCite is an item as a relationship line names it, by kind, title and
// uuid, so the model can cite it and a reader can open it.
func graphCite(h workGraph.Hop) string {
	t := strings.TrimSpace(strings.ReplaceAll(helpers.OneLine(h.Title, graphTitleMax), "\"", "'"))
	if t == "" {
		t = "untitled"
	}
	out := fmt.Sprintf("%s \"%s\" (id: %s)", h.Kind, t, h.ID)
	// A message is said where it was said.
	if ch, _ := h.Data["channel_name"].(string); h.Kind == "message" && strings.TrimSpace(ch) != "" {
		out += " in #" + helpers.OneLine(ch, graphTitleMax)
	}
	return out
}

// formatGraphContext is the relationship lines, within graphContextMaxChars,
// and a source for each end the app can open from an answer.
func formatGraphContext(links []workGraph.Link) (string, []adapter.SourceRef) {
	if len(links) == 0 {
		return "", nil
	}
	var b strings.Builder
	head := "## How These Connect (Work Graph)\n" +
		"Each line is one relationship between items the user can open. When the answer depends on one, " +
		"say so and name both items as they are written here.\n"
	b.WriteString(head)
	var sources []adapter.SourceRef
	cited := map[string]bool{}
	cite := func(h workGraph.Hop, why string) {
		st := sourceType(h.Kind)
		if st == "" || cited[h.Kind+h.ID] {
			return
		}
		cited[h.Kind+h.ID] = true
		ref := adapter.SourceRef{ContentType: st, ContentUUID: h.ID, Snippet: helpers.TruncateRunesWithSuffix(why, 150, "...")}
		switch st {
		case "doc":
			ref.DocUUID = h.ID
		case "task":
			ref.TaskUUID = h.ID
		case "post":
			ref.PostUUID = h.ID
			// A message opens in its channel.
			ref.ChannelUUID, _ = h.Data["channel_id"].(string)
			ref.ChannelName, _ = h.Data["channel_name"].(string)
		}
		sources = append(sources, ref)
	}
	n := 0
	for _, l := range links {
		line := fmt.Sprintf("- %s %s %s\n", graphCite(l.From), relPhrase(l.Rel), graphCite(l.To))
		if b.Len()+len(line) > graphContextMaxChars {
			break
		}
		b.WriteString(line)
		n++
		why := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		cite(l.From, why)
		cite(l.To, why)
	}
	if n == 0 {
		return "", nil
	}
	b.WriteString("\n")
	return b.String(), sources
}

// sourceType is the source kind the app opens an answer's citation as, for
// a work-graph kind it can open from there; "" for one it can't.
func sourceType(kind string) string {
	switch kind {
	case "doc", "task":
		return kind
	case "message":
		return "post"
	}
	return ""
}
