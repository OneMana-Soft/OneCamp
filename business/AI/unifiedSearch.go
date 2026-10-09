package business

// Unified AI search — one query, every source the user can reach.
//
// UnifiedSearch fans a single query out across the user's WORKSPACE content
// (the permission-scoped semantic index, the same recall AskAI uses) AND their
// connected external accounts (Gmail, GitHub), then returns the results grouped
// by source. This is the Glean/Notion-AI "search across all your tools" surface
// a chat-only product can't do, built on infrastructure OneCamp already owns.
//
// Design:
//   - Generic fan-out: each source is a small searcher run in PARALLEL with a
//     hard per-source timeout, so one slow external API never blocks the rest.
//     Adding a future source (Drive, Jira, …) is one entry in the searcher
//     list — no change to the orchestration.
//   - Permission-correct: workspace recall uses the caller's accessible
//     channels/projects/DMs (never widens access); connector calls resolve
//     strictly from the caller's own linked account.
//   - Degrades gracefully: an unconnected or erroring source contributes an
//     empty group (with a short note) rather than failing the whole search.
//   - Provider-agnostic: gated on the AI service being enabled; the embedding
//     recall runs through the same service chokepoint as the rest of AI.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	unifiedMaxPerSource  = 6
	unifiedSourceTimeout = 5 * time.Second
	unifiedSnippetLen    = 200
)

// Source ids (stable; also the group ordering key).
const (
	UnifiedSourceWorkspace = "workspace"
	UnifiedSourceMemory    = "memory"
	UnifiedSourceGmail     = "gmail"
	UnifiedSourceGitHub    = "github"
)

// UnifiedHit is one result. Workspace hits carry deep-link routing fields (the
// same shape as a briefing highlight, so the FE reuses its navigation);
// external hits carry an absolute URL the FE opens in a new tab.
type UnifiedHit struct {
	Source  string `json:"source"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	Meta    string `json:"meta,omitempty"` // e.g. "from alice", "#product", "owner/repo #12"
	URL     string `json:"url,omitempty"`  // external deep-link (gmail/github)
	Kind    string `json:"kind,omitempty"` // memory items: decision | commitment | question

	// Workspace deep-link routing (empty for external hits).
	ContentType  string `json:"content_type,omitempty"`
	ContentUUID  string `json:"content_uuid,omitempty"`
	ChannelUUID  string `json:"channel_uuid,omitempty"`
	ChannelName  string `json:"channel_name,omitempty"`
	ProjectUUID  string `json:"project_uuid,omitempty"`
	ChatGrpID    string `json:"chat_grp_id,omitempty"`
	ChatByUserID string `json:"chat_by_user_id,omitempty"`
	ChatToUserID string `json:"chat_to_user_id,omitempty"`
	PostUUID     string `json:"post_uuid,omitempty"`
	TaskUUID     string `json:"task_uuid,omitempty"`
	DocUUID      string `json:"doc_uuid,omitempty"`
}

// UnifiedSearchGroup is one source's results.
type UnifiedSearchGroup struct {
	Source    string       `json:"source"`
	Label     string       `json:"label"`
	Connected bool         `json:"connected"`
	Hits      []UnifiedHit `json:"hits"`
	Note      string       `json:"note,omitempty"`
}

// UnifiedSearchResponse is the full grouped result. Enabled=false → AI is off,
// the caller hides the surface.
type UnifiedSearchResponse struct {
	Enabled bool                 `json:"enabled"`
	Query   string               `json:"query"`
	Groups  []UnifiedSearchGroup `json:"groups"`
}

// unifiedSearcher is one pluggable source. run returns its hits, whether the
// source is connected/available, and an optional human note (e.g. on error).
type unifiedSearcher struct {
	source string
	label  string
	run    func(ctx context.Context) (hits []UnifiedHit, connected bool, note string)
}

// UnifiedSearch runs the query across all sources in parallel and returns the
// grouped results in stable source order. Best-effort per source.
func UnifiedSearch(ctx context.Context, userInfo *userModels.UserInfo, query string) (*UnifiedSearchResponse, error) {
	resp := &UnifiedSearchResponse{Enabled: false, Groups: []UnifiedSearchGroup{}}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return resp, nil // AI off → caller hides the surface
	}
	resp.Enabled = true

	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("a search query is required")
	}
	if len([]rune(query)) > 256 {
		query = string([]rune(query)[:256])
	}
	resp.Query = query

	// Short-window cache: a debounced search box would otherwise hit the live
	// Gmail/GitHub APIs on every keystroke. Keyed per (user, normalized query),
	// and per asker when an agent searches for someone other than its sponsor:
	// the same question asked of the same agent by two people has two answers,
	// and keyed by the sponsor alone the first answer would serve both.
	cacheUser := userInfo.UserDgraphInfo.Uuid
	requester, _, forOther := ai.RunRequester(ctx)
	if forOther {
		cacheUser += ":for:" + requester
	}
	cacheArgs := unifiedSearchCacheArgs(cacheUser, query)
	var cached UnifiedSearchResponse
	if found, _ := redisStore.GetJSON(ctx, registry.AIUnifiedSearch, cacheArgs, &cached); found {
		return &cached, nil
	}

	searchers := buildUnifiedSearchers(userInfo, query)
	if forOther {
		searchers = withoutPersonalSources(searchers)
	}

	// Fan out: each searcher gets its own bounded context so one slow source
	// can't hold the others. Results collected by index to preserve order.
	type res struct {
		idx       int
		hits      []UnifiedHit
		connected bool
		note      string
	}
	ch := make(chan res, len(searchers))
	for i, s := range searchers {
		go func(idx int, s unifiedSearcher) {
			sctx, cancel := context.WithTimeout(ctx, unifiedSourceTimeout)
			defer cancel()
			defer func() {
				// A panic in one source must never take down the request.
				if r := recover(); r != nil {
					helpers.LogErrorWithContext(sctx, "unified search source %q panicked: %v", s.source, r)
					ch <- res{idx: idx, note: "This source couldn't be searched."}
				}
			}()
			hits, connected, note := s.run(sctx)
			ch <- res{idx: idx, hits: hits, connected: connected, note: note}
		}(i, s)
	}

	collected := make([]res, len(searchers))
	for i := 0; i < len(searchers); i++ {
		r := <-ch
		collected[r.idx] = r
	}

	for i, s := range searchers {
		c := collected[i]
		// Always emit a non-nil Hits slice: a nil slice marshals to JSON `null`,
		// and an unconnected/errored source (Gmail/GitHub when not linked — the
		// default) returns nil hits. A `null` here forces every FE consumer to
		// null-guard before .length/.map; guaranteeing `[]` keeps the API
		// contract honest (Hits is always an array).
		hits := c.hits
		if hits == nil {
			hits = []UnifiedHit{}
		}
		resp.Groups = append(resp.Groups, UnifiedSearchGroup{
			Source:    s.source,
			Label:     s.label,
			Connected: c.connected,
			Hits:      hits,
			Note:      c.note,
		})
	}

	// Cache the assembled response (best-effort; nil Redis is a no-op).
	_ = redisStore.SetJSON(ctx, registry.AIUnifiedSearch, cacheArgs, resp)
	return resp, nil
}

// personalSources are the searchers that read one person's own connected
// accounts rather than the workspace.
var personalSources = map[string]bool{UnifiedSourceGmail: true, UnifiedSourceGitHub: true}

// withoutPersonalSources replaces the connected-account searchers with ones
// that report they were not searched, for a search an agent makes for someone
// other than its sponsor. The sponsor's mail and repositories are theirs; the
// workspace sources stay, already narrowed by the run's requester.
func withoutPersonalSources(searchers []unifiedSearcher) []unifiedSearcher {
	out := make([]unifiedSearcher, 0, len(searchers))
	for _, s := range searchers {
		if personalSources[s.source] {
			label := s.label
			s.run = func(context.Context) ([]UnifiedHit, bool, string) {
				return nil, false, label + " belongs to the person who set up this agent, so it was not searched."
			}
		}
		out = append(out, s)
	}
	return out
}

// unifiedSearchCacheArgs builds a deterministic cache key for a (user, query)
// pair: the user UUID plus a sha256 of the normalized query (so distinct
// queries never collide and the key stays bounded). Pure.
func unifiedSearchCacheArgs(userUUID, query string) []string {
	norm := strings.ToLower(strings.TrimSpace(query))
	sum := sha256.Sum256([]byte(norm))
	return []string{userUUID, hex.EncodeToString(sum[:])}
}

// buildUnifiedSearchers assembles the source list. Workspace is always present;
// connector sources self-report connected=false (with a connect nudge) when the
// caller hasn't linked them.
func buildUnifiedSearchers(userInfo *userModels.UserInfo, query string) []unifiedSearcher {
	userUUID := userInfo.UserDgraphInfo.Uuid
	channels, projects := getAccessibleResourceUUIDs(userInfo)
	grpIDs := accessibleGroupingIDs(userInfo)

	uid, uidErr := uuid.Parse(userUUID)

	return []unifiedSearcher{
		{
			source: UnifiedSourceWorkspace,
			label:  "Workspace",
			run: func(ctx context.Context) ([]UnifiedHit, bool, string) {
				results, err := searchSimilar(ctx, userInfo, query, channels, projects, grpIDs, unifiedMaxPerSource)
				if err != nil {
					return nil, true, "Couldn't search the workspace right now."
				}
				return workspaceHits(results), true, ""
			},
		},
		{
			source: UnifiedSourceMemory,
			label:  "Memory",
			run: func(ctx context.Context) ([]UnifiedHit, bool, string) {
				// Reuse the permission-scoped memory list (owner + accessible
				// channels/projects/DMs), then keyword-match against the query.
				// Memory items are NOT in the semantic index, so this surfaces
				// distilled decisions/commitments/questions the workspace search
				// above can't return.
				resp, err := ListWorkspaceMemory(ctx, userInfo, nil, []string{"open"}, 100, "")
				if err != nil || resp == nil {
					return nil, true, ""
				}
				return memoryHits(resp.Items, query, unifiedMaxPerSource), true, ""
			},
		},
		{
			source: UnifiedSourceGmail,
			label:  "Gmail",
			run: func(ctx context.Context) ([]UnifiedHit, bool, string) {
				if uidErr != nil || !connectorBusiness.IsConnected(ctx, uid, connectorBusiness.ProviderGmail) {
					return nil, false, "Connect Gmail under Settings → Connectors to search it here."
				}
				emails, err := connectorBusiness.GmailSearch(ctx, uid, query, unifiedMaxPerSource)
				if err != nil {
					return nil, true, "Couldn't reach Gmail right now."
				}
				return gmailHits(emails), true, ""
			},
		},
		{
			source: UnifiedSourceGitHub,
			label:  "GitHub",
			run: func(ctx context.Context) ([]UnifiedHit, bool, string) {
				if uidErr != nil || !connectorBusiness.IsConnected(ctx, uid, connectorBusiness.ProviderGitHub) {
					return nil, false, "Connect GitHub under Settings → Connectors to search it here."
				}
				return githubSearch(ctx, uid, query), true, ""
			},
		},
	}
}

// workspaceHits maps semantic recall results into unified hits, normalizing
// rich-text/HTML content to a clean snippet and preserving deep-link routing.
func workspaceHits(results []ai.SimilarResult) []UnifiedHit {
	out := make([]UnifiedHit, 0, len(results))
	for _, r := range results {
		title := workspaceTitle(r)
		out = append(out, UnifiedHit{
			Source:       UnifiedSourceWorkspace,
			Title:        title,
			Snippet:      clipUnified(helpers.HTMLToPlainText(r.ContentText)),
			Meta:         workspaceMeta(r),
			ContentType:  r.ContentType,
			ContentUUID:  r.ContentUUID,
			ChannelUUID:  r.ChannelUUID,
			ChannelName:  r.ChannelName,
			ChatGrpID:    r.ChatGrpID,
			ChatByUserID: r.ChatByUserID,
			ChatToUserID: r.ChatToUserID,
			PostUUID:     r.PostUUID,
			TaskUUID:     r.TaskUUID,
			DocUUID:      r.DocUUID,
		})
	}
	return out
}

// workspaceTitle derives a human title for a recall hit (content-type label +
// channel/author context), since indexed content has no explicit title.
func workspaceTitle(r ai.SimilarResult) string {
	t := strings.TrimSpace(r.ContentType)
	if t == "" {
		t = "Result"
	}
	// Capitalize the first letter for display (post → Post).
	label := strings.ToUpper(t[:1]) + t[1:]
	if name := strings.TrimSpace(r.AuthorName); name != "" {
		return label + " by " + name
	}
	return label
}

// workspaceMeta builds a compact context string (channel name when present).
func workspaceMeta(r ai.SimilarResult) string {
	if name := strings.TrimSpace(r.ChannelName); name != "" {
		return "#" + name
	}
	return ""
}

// memoryHits keyword-matches the user's open memory items against the query and
// maps the matches into unified hits (the fact text as title, kind + scope +
// due as meta). Pure + unit tested.
func memoryHits(items []adapter.MemoryItemView, query string, max int) []UnifiedHit {
	toks := queryTokens(query)
	out := make([]UnifiedHit, 0, max)
	for _, it := range items {
		content := strings.TrimSpace(it.Content)
		if content == "" {
			continue
		}
		if !matchesAllTokens(content, toks) {
			continue
		}
		out = append(out, UnifiedHit{
			Source:      UnifiedSourceMemory,
			Title:       clipUnified(content),
			Kind:        strings.TrimSpace(it.Kind),
			Meta:        memoryMeta(it),
			ChannelUUID: strings.TrimSpace(it.ChannelUUID),
			ProjectUUID: strings.TrimSpace(it.ProjectUUID),
			ChatGrpID:   strings.TrimSpace(it.ChatGrpID),
		})
		if len(out) >= max {
			break
		}
	}
	return out
}

// memoryMeta renders a compact provenance/due string for a memory hit.
func memoryMeta(it adapter.MemoryItemView) string {
	parts := make([]string, 0, 2)
	if s := strings.TrimSpace(it.ScopeLabel); s != "" {
		switch it.ScopeType {
		case "channel":
			parts = append(parts, "#"+s)
		default:
			parts = append(parts, s)
		}
	}
	if d := strings.TrimSpace(it.DueAt); d != "" {
		parts = append(parts, "due "+d)
	}
	return strings.Join(parts, " · ")
}

// queryTokens lowercases + splits a query into distinct non-trivial tokens.
func queryTokens(query string) []string {
	seen := map[string]bool{}
	var toks []string
	for _, f := range strings.Fields(strings.ToLower(query)) {
		f = strings.TrimFunc(f, func(r rune) bool { return !('a' <= r && r <= 'z') && !('0' <= r && r <= '9') })
		if len(f) < 2 || seen[f] {
			continue
		}
		seen[f] = true
		toks = append(toks, f)
	}
	return toks
}

// matchesAllTokens reports whether text contains every query token (AND match,
// case-insensitive). Empty token set matches nothing (avoids dumping all items).
func matchesAllTokens(text string, toks []string) bool {
	if len(toks) == 0 {
		return false
	}
	hay := strings.ToLower(text)
	for _, t := range toks {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// gmailHits maps Gmail summaries into unified hits with a deep-link to the
// message in the Gmail web UI.
func gmailHits(emails []connectorBusiness.EmailSummary) []UnifiedHit {
	out := make([]UnifiedHit, 0, len(emails))
	for _, e := range emails {
		title := strings.TrimSpace(e.Subject)
		if title == "" {
			title = "(no subject)"
		}
		hit := UnifiedHit{
			Source:  UnifiedSourceGmail,
			Title:   title,
			Snippet: clipUnified(e.Snippet),
			Meta:    gmailMeta(e.From),
		}
		if id := strings.TrimSpace(e.ID); id != "" {
			hit.URL = "https://mail.google.com/mail/u/0/#all/" + id
		}
		out = append(out, hit)
	}
	return out
}

// gmailMeta renders the sender compactly ("from alice@x.com").
func gmailMeta(from string) string {
	from = strings.TrimSpace(from)
	if from == "" {
		return ""
	}
	return "from " + from
}

// githubSearch runs a real GitHub issue/PR query scoped to the user's
// involvement (via the connector), mapping the matches into unified hits.
func githubSearch(ctx context.Context, uid uuid.UUID, query string) []UnifiedHit {
	items, err := connectorBusiness.GitHubSearch(ctx, uid, query, unifiedMaxPerSource)
	if err != nil {
		return nil
	}
	return githubItemsToHits(items, unifiedMaxPerSource)
}

// githubItemsToHits maps GitHub items into unified hits, de-duplicating by URL
// and capping the count. Pure + unit tested.
func githubItemsToHits(items []connectorBusiness.GitHubItem, max int) []UnifiedHit {
	out := make([]UnifiedHit, 0, max)
	seen := map[string]bool{}
	for _, it := range items {
		key := strings.TrimSpace(it.URL)
		if key == "" {
			key = fmt.Sprintf("%s#%d", it.Repo, it.Number)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		meta := strings.TrimSpace(it.Repo)
		if it.Number > 0 {
			meta = fmt.Sprintf("%s #%d", meta, it.Number)
		}
		if s := strings.TrimSpace(it.State); s != "" {
			meta = strings.TrimSpace(meta + " · " + s)
		}
		out = append(out, UnifiedHit{
			Source:  UnifiedSourceGitHub,
			Title:   strings.TrimSpace(it.Title),
			Snippet: "",
			Meta:    meta,
			URL:     strings.TrimSpace(it.URL),
		})
		if len(out) >= max {
			break
		}
	}
	return out
}

// clipUnified normalizes whitespace and caps a snippet to unifiedSnippetLen
// runes with an ellipsis. Pure.
func clipUnified(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	r := []rune(s)
	if len(r) <= unifiedSnippetLen {
		return s
	}
	return string(r[:unifiedSnippetLen]) + "…"
}
