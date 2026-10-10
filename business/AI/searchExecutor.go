package business

// search_workspace tool executor — lets an agent (or the assistant) search
// across the user's workspace, Memory layer, and connected accounts in one
// call, reusing the same UnifiedSearch the Cmd+K palette uses. Read-only and
// permission-scoped: it resolves the FULL UserInfo for the requesting user and
// UnifiedSearch only ever returns content that user can already see.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// RegisterSearchExecutors wires the cross-source search tool. Called from
// RegisterToolExecutors alongside the other core tools.
func RegisterSearchExecutors() {
	ai.RegisterExecutor("search_workspace", executeSearchWorkspace)
}

// executeSearchWorkspace searches as the requesting user and renders the
// results for the model. Two sources, because each misses what the other finds:
// the global (Cmd+K) search matches NAMES (a doc called "Launch sync notes"),
// and UnifiedSearch's semantic recall matches what was SAID. Semantic recall
// alone answered "Launch sync notes" with four copies of the message that
// mentioned it and never the doc, so an agent told to add to that doc could not
// find it. Every hit now carries the ids its tools take, and repeats collapse.
func executeSearchWorkspace(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	query := strings.TrimSpace(action.Params["query"])
	if query == "" {
		return "", nil, fmt.Errorf("query is required")
	}

	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil || userInfo == nil {
		return "", nil, fmt.Errorf("could not resolve the requesting user")
	}

	resp, err := UnifiedSearch(ctx, userInfo, query)
	if err != nil {
		return "", nil, err
	}
	if resp == nil || !resp.Enabled {
		return "AI search is not enabled for this workspace.", nil, nil
	}
	named := namedSearch(ctx, userInfo, query)
	return renderUnifiedSearchForModel(withNamedHits(resp, named)), nil, nil
}

// namedSearch finds docs, tasks, projects, channels and boards by name through
// the same permission-scoped global search the search bar uses. A seam; empty
// on any failure, since semantic results still stand without it.
//
// The scope is resolved the way sign-in resolves it (channels, projects AND
// teams): the executor's lighter user record has no projects or teams, and a
// search scoped by it missed everything project-bound.
var namedSearch = func(ctx context.Context, userInfo *userModels.UserInfo, query string) []UnifiedHit {
	scope := &userInfo.UserDgraphInfo
	if full, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, scope.Uuid); err == nil && full != nil {
		scope = full
	}
	hits, ok := namedHitsAs(ctx, scope, query)
	if !ok {
		return nil
	}
	// An agent searching for someone other than its sponsor keeps only what
	// both of them find. The global index decides visibility per person (a
	// private doc by its grant lists, not by a scope list), so the asker's own
	// search is the only way to learn what they can see; an asker who cannot be
	// resolved gets no named results rather than the sponsor's.
	if requester, _, forOther := ai.RunRequester(ctx); forOther {
		asker, err := profileOf(ctx, requester)
		if err != nil {
			return nil
		}
		theirs, ok := namedHitsAs(ctx, asker, query)
		if !ok {
			return nil
		}
		hits = sharedHits(hits, theirs)
	}
	return rankNamedHits(hits, query, unifiedMaxPerSource)
}

// namedHitsAs runs the global name search as one person.
func namedHitsAs(ctx context.Context, scope *dgraphStruct.DgraphUser, query string) ([]UnifiedHit, bool) {
	page, err := globalSearchBusiness.GetUnifiedGlobalSearch(ctx, scope.Uuid, scope.EmailID, scope.Channels, scope.Projects, scope.Teams, query)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI/namedSearch global search failed err: %v", err)
		return nil, false
	}
	return namedHits(page.Page, 0), true
}

// sharedHits keeps the hits of mine that also appear in theirs, by the object
// each names. Pure.
func sharedHits(mine, theirs []UnifiedHit) []UnifiedHit {
	seen := make(map[string]bool, len(theirs))
	for _, h := range theirs {
		seen[namedHitKey(h)] = true
	}
	out := make([]UnifiedHit, 0, len(mine))
	for _, h := range mine {
		if k := namedHitKey(h); k != "" && seen[k] {
			out = append(out, h)
		}
	}
	return out
}

// namedHitKey names the object a named hit points at. Empty for a hit that
// names nothing, which therefore never counts as shared.
func namedHitKey(h UnifiedHit) string {
	for _, id := range []string{h.DocUUID, h.TaskUUID, h.ContentUUID, h.ProjectUUID, h.ChannelUUID} {
		if id != "" {
			return h.ContentType + ":" + id
		}
	}
	return ""
}

// namedHits turns global-search results into hits, keeping only named things
// (messages already come from semantic recall). Pure.
func namedHits(results []*openSearchStruct.GlobalSearchOpenSearchResp, max int) []UnifiedHit {
	var out []UnifiedHit
	for _, r := range results {
		if r == nil || (max > 0 && len(out) == max) {
			continue
		}
		h := UnifiedHit{Source: UnifiedSourceWorkspace}
		switch {
		case r.Doc != nil && r.Doc.Uuid != "":
			h.Title, h.ContentType, h.DocUUID = "Doc: "+r.Doc.DocTitle, "doc", r.Doc.Uuid
		case r.Task != nil && r.Task.Uuid != "":
			h.Title, h.ContentType, h.TaskUUID, h.ProjectUUID = "Task: "+r.Task.TaskName, "task", r.Task.Uuid, r.Task.TaskProjectUuid
			h.Meta = r.Task.TaskProjectName
		case r.Project != nil && r.Project.Uuid != "":
			h.Title, h.ContentType, h.ProjectUUID = "Project: "+r.Project.ProjectName, "project", r.Project.Uuid
		case r.Channel != nil && r.Channel.Uuid != "":
			h.Title, h.ContentType, h.ChannelUUID = "Channel: #"+r.Channel.ChannelName, "channel", r.Channel.Uuid
		case r.Board != nil && r.Board.Uuid != "":
			h.Title, h.ContentType, h.ContentUUID = "Board: "+r.Board.BoardTitle, "board", r.Board.Uuid
		default:
			continue
		}
		out = append(out, h)
	}
	return out
}

// nameMatchScore rates how well a title answers a name query: 3 for the exact
// name, 2 when it contains the whole phrase, 1 when it has every word, else 0.
// Pure.
func nameMatchScore(title, query string) int {
	t := strings.ToLower(strings.Join(strings.Fields(title), " "))
	q := strings.ToLower(strings.Join(strings.Fields(query), " "))
	if q == "" {
		return 0
	}
	switch {
	case t == q:
		return 3
	case strings.Contains(t, q):
		return 2
	}
	for _, w := range strings.Fields(q) {
		if !strings.Contains(t, w) {
			return 0
		}
	}
	return 1
}

// rankNamedHits orders named hits by how well their name matches the query
// (keeping search order among equals) and keeps the best max. The search bar's
// order put "Q4 launch" above a doc titled exactly "Launch sync notes", and
// the doc fell past the cut. Pure.
func rankNamedHits(hits []UnifiedHit, query string, max int) []UnifiedHit {
	name := func(h UnifiedHit) string {
		if i := strings.Index(h.Title, ": "); i >= 0 {
			return h.Title[i+2:]
		}
		return h.Title
	}
	sorted := append([]UnifiedHit(nil), hits...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return nameMatchScore(name(sorted[i]), query) > nameMatchScore(name(sorted[j]), query)
	})
	if max > 0 && len(sorted) > max {
		sorted = sorted[:max]
	}
	return sorted
}

// withNamedHits puts named matches first as their own group, then removes
// repeats across every group. Pure.
func withNamedHits(resp *UnifiedSearchResponse, named []UnifiedHit) *UnifiedSearchResponse {
	out := &UnifiedSearchResponse{Enabled: resp.Enabled, Query: resp.Query}
	if len(named) > 0 {
		out.Groups = append(out.Groups, UnifiedSearchGroup{Source: UnifiedSourceWorkspace, Label: "Docs, tasks, projects and channels", Hits: named})
	}
	out.Groups = append(out.Groups, resp.Groups...)
	seen := map[string]bool{}
	for i := range out.Groups {
		out.Groups[i].Hits = dedupeHits(out.Groups[i].Hits, seen)
	}
	return out
}

// dedupeHits drops hits already seen: the same item by id, or, for hits with
// no id, the same title and text. seen is shared across calls so repeats
// collapse across groups. Pure apart from seen.
func dedupeHits(hits []UnifiedHit, seen map[string]bool) []UnifiedHit {
	out := make([]UnifiedHit, 0, len(hits))
	for _, h := range hits {
		keys := hitKeys(h)
		dup := false
		for _, k := range keys {
			if seen[k] {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		for _, k := range keys {
			seen[k] = true
		}
		out = append(out, h)
	}
	return out
}

// minDedupeTextRunes keeps short replies ("ok", "thanks") from collapsing into
// one another just because they read alike.
const minDedupeTextRunes = 20

// hitKeys names a hit by its id and, when its text is substantial, by that
// text too: the same message copied into another channel (the demo's nightly
// refresh leaves one copy per archived channel) is still one thing to read.
func hitKeys(h UnifiedHit) []string {
	var keys []string
	for _, id := range []string{h.DocUUID, h.TaskUUID, h.ContentUUID, h.PostUUID, h.ProjectUUID, h.ChannelUUID} {
		if id != "" {
			keys = append(keys, h.ContentType+":"+id)
			break
		}
	}
	if h.URL != "" {
		keys = append(keys, "url:"+h.URL)
	}
	text := strings.ToLower(strings.Join(strings.Fields(h.Title+" "+h.Snippet), " "))
	if len([]rune(strings.TrimSpace(h.Snippet))) >= minDedupeTextRunes || len(keys) == 0 {
		keys = append(keys, "text:"+text)
	}
	return keys
}

// hitRef is how a hit is named to the model: the ids its tools take. Pure.
func hitRef(h UnifiedHit) string {
	switch {
	case h.DocUUID != "":
		return "doc_uuid=" + h.DocUUID
	case h.TaskUUID != "":
		if h.ProjectUUID != "" {
			return "task_uuid=" + h.TaskUUID + " project_uuid=" + h.ProjectUUID
		}
		return "task_uuid=" + h.TaskUUID
	case h.PostUUID != "" && h.ChannelUUID != "":
		return "channel_uuid=" + h.ChannelUUID + " post_uuid=" + h.PostUUID
	case h.ContentType == "project" && h.ProjectUUID != "":
		return "project_uuid=" + h.ProjectUUID
	case h.ContentType == "channel" && h.ChannelUUID != "":
		return "channel_uuid=" + h.ChannelUUID
	case h.ChatGrpID != "":
		return "chat_grp_id=" + h.ChatGrpID
	case h.ChannelUUID != "":
		return "channel_uuid=" + h.ChannelUUID
	case h.ContentUUID != "":
		return h.ContentType + "_uuid=" + h.ContentUUID
	}
	return ""
}

// renderUnifiedSearchForModel formats a unified-search response as a bounded,
// model-friendly plain-text digest. Pure (no I/O) so it can be unit tested.
func renderUnifiedSearchForModel(resp *UnifiedSearchResponse) string {
	var sb strings.Builder
	any := false
	for _, g := range resp.Groups {
		if len(g.Hits) == 0 {
			continue
		}
		any = true
		sb.WriteString(fmt.Sprintf("## %s\n", g.Label))
		for _, h := range g.Hits {
			line := "- " + strings.TrimSpace(h.Title)
			if m := strings.TrimSpace(h.Meta); m != "" {
				line += " (" + m + ")"
			}
			if u := strings.TrimSpace(h.URL); u != "" {
				line += " — " + u
			}
			if ref := hitRef(h); ref != "" {
				line += " [" + ref + "]"
			}
			sb.WriteString(line + "\n")
			if s := strings.TrimSpace(h.Snippet); s != "" {
				sb.WriteString("  " + s + "\n")
			}
		}
	}
	if !any {
		return fmt.Sprintf("No results found for %q across the workspace or connected apps.", resp.Query)
	}
	return strings.TrimRight(sb.String(), "\n")
}
