//go:build integration
// +build integration

package integration_test

// The reads dgraph_budgets_test.go times, one runner a line of its table.
// Each calls the code a request runs (the domain or business function, never
// a copy of its DQL) on the fixture, and says how many things it read, so a
// read that finds nothing (a fixture that lost its shape, a filter that drops
// everything) fails rather than looking fast.

import (
	"context"
	"fmt"
	"net/url"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	treeBusiness "github.com/akashc777/OneCamp/business/Tree"
	whoKnowsBusiness "github.com/akashc777/OneCamp/business/WhoKnows"
	workGraphBusiness "github.com/akashc777/OneCamp/business/WorkGraph"
	activityDomain "github.com/akashc777/OneCamp/domain/Activity"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	whoKnowsDomain "github.com/akashc777/OneCamp/domain/WhoKnows"
	workGraphDomain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration/dgfixture"
)

// budgetRunner runs one read on the fixture and says how many things it read.
type budgetRunner func(ctx context.Context, f *dgfixture.Fixture) (int, error)

// nonEmpty is n, or an error when the read found nothing.
func nonEmpty(what string, n int, err error) (int, error) {
	if err == nil && n == 0 {
		err = fmt.Errorf("%s read nothing", what)
	}
	return n, err
}

func viewerSubject(f *dgfixture.Fixture) accessBusiness.Subject {
	return accessBusiness.Subject{UserUID: f.ViewerUID(), UserUUID: f.Viewer.UUID}
}

func treeLevel(ctx context.Context, f *dgfixture.Fixture, q treeBusiness.LevelQuery) (int, error) {
	lister, ok := treeBusiness.Hierarchy.(treeBusiness.Lister)
	if !ok {
		return 0, fmt.Errorf("the tree's hierarchy lists no levels")
	}
	page, err := lister.Level(ctx, viewerSubject(f), "doc", q)
	return nonEmpty("the level", len(page.Nodes), err)
}

func docAccess(n int) budgetRunner {
	return func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		if len(f.ViewerDocs) < n {
			return 0, fmt.Errorf("the fixture names %d docs to decide, fewer than %d", len(f.ViewerDocs), n)
		}
		got, err := docBusiness.DecideDocs(ctx, docBusiness.Viewer{UID: f.ViewerUID(), UUID: f.Viewer.UUID}, f.ViewerDocs[:n])
		return nonEmpty("the decisions", len(got), err)
	}
}

// budgetRunners are the reads, by the name their table line gives them.
var budgetRunners = map[string]budgetRunner{
	// §1.5 1: every signed-in request reads this on a Redis miss (the test has
	// no Redis, so every call is a miss).
	"profile.viewer": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		u, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, f.Viewer.UUID)
		if err != nil || u == nil {
			return 0, fmt.Errorf("no profile: %v", err)
		}
		return nonEmpty("the profile", len(u.Channels)+len(u.Projects)+len(u.Teams)+len(u.DMs), nil)
	},
	// GetDgraphUserInfoByUUID: no cache, 92 call sites (report §1.5). Measured
	// to decide whether it needs one (DG4).
	"user.byuuid": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		u, err := userDomain.GetDgraphUserInfoByUUID(ctx, f.Viewer.UUID)
		if err != nil || u == nil {
			return 0, fmt.Errorf("no person: %v", err)
		}
		return nonEmpty("the person", len(u.Uid), nil)
	},
	// The same read for the person with 20,000 posts. It names user_posts,
	// but with no block under it, so Dgraph returns none of them (measured
	// 18 Oct 2026: Posts is empty): the super-node costs the read nothing,
	// and this line keeps it that way.
	"profile.superposter": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		u, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, f.SuperPoster.UUID)
		if err != nil || u == nil {
			return 0, fmt.Errorf("no profile: %v", err)
		}
		if len(u.Posts) > 0 {
			return 0, fmt.Errorf("the profile now reads %d of the person's posts: every request would carry them", len(u.Posts))
		}
		return nonEmpty("the profile", len(u.Channels)+len(u.Projects)+len(u.Teams)+len(u.DMs), nil)
	},
	// §1.5 2: #general's newest page, and a page from the middle of its history.
	"channel.latest": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		posts, err := postDomain.GetDgraphLatestPostFromDgraph(ctx, f.General)
		return nonEmpty("the page", len(posts), err)
	},
	"channel.older": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		posts, err := postDomain.GetDgraphOldPostFromDgraph(ctx, f.General, budgetMidHistory)
		return nonEmpty("the page", len(posts), err)
	},
	// §1.5 3: the viewer's busiest DM, and a group.
	"dm.latest": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		chats, err := chatDomain.GetDgraphLatestChatFromDgraph(ctx, f.BusiestDM)
		return nonEmpty("the page", len(chats), err)
	},
	"group.latest": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		chats, err := chatDomain.GetDgraphLatestChatFromDgraph(ctx, f.ViewerGroup)
		return nonEmpty("the page", len(chats), err)
	},
	// §1.5 4: the sidebar's first page of the viewer's channels, each with its
	// newest post (#general's among them).
	"sidebar.channels": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		chs, _, err := channelDomain.GetUserActiveChannelListWithLatestPost(ctx, f.ViewerUID(), nil, 0, 50)
		return nonEmpty("the list", len(chs), err)
	},
	// §1.5 5: every DM and group of the viewer's, each with its newest message.
	"dm.list": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		u, err := chatDomain.GetUserChatListWithLatestChat(ctx, f.Viewer.UUID)
		if err != nil || u == nil {
			return 0, fmt.Errorf("no list: %v", err)
		}
		return nonEmpty("the list", len(u.DMs), nil)
	},
	// DG4: the DM list's first page, 50 DMs newest first.
	"dm.list.page": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		page, _, err := chatDomain.DMPage(ctx, f.Viewer.UUID, nil, 51, nil)
		return nonEmpty("the page", len(page), err)
	},
	// §1.5 6: the inbox's first page: mentions, comments and reactions.
	"inbox": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		m, c, r, err := activityDomain.GetUnifiedActivity(ctx, f.ViewerUID(), time.Now(), 30)
		return nonEmpty("the inbox", len(m)+len(c)+len(r), err)
	},
	// §1.5 7: the doc tree as the sidebar reads it, through the tree's own
	// service with the access decisions: the workspace's top level, the
	// viewer's private pages, a project space, and the wide page's children.
	"docs.top.workspace": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		return treeLevel(ctx, f, treeBusiness.LevelQuery{Space: "workspace"})
	},
	"docs.top.private": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		return treeLevel(ctx, f, treeBusiness.LevelQuery{Space: "private"})
	},
	"docs.top.space": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		return treeLevel(ctx, f, treeBusiness.LevelQuery{Space: "project:" + f.SpaceProject})
	},
	"docs.children": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		return treeLevel(ctx, f, treeBusiness.LevelQuery{Parent: f.DocParent})
	},
	// Docs F1: a folder opened in place, read as the tree reads any level:
	// from the folder down its children edge, never from the kind index
	// (helpers/docNodeKindGuard_test.go), its first page holding every kind
	// and its count exact, the deleted items left out. The files' facts are
	// one Postgres read after it (controllers/Doc's tree), not a graph trip.
	"docs.folder": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		lister, ok := treeBusiness.Hierarchy.(treeBusiness.Lister)
		if !ok {
			return 0, fmt.Errorf("the tree's hierarchy lists no levels")
		}
		page, err := lister.Level(ctx, viewerSubject(f), "doc", treeBusiness.LevelQuery{Parent: f.Folder})
		if err != nil {
			return 0, err
		}
		if page.Count != f.Tier.FolderItems || page.Hidden != 0 {
			return 0, fmt.Errorf("the folder counts %d items and hides %d, want %d and none", page.Count, page.Hidden, f.Tier.FolderItems)
		}
		kinds := map[string]bool{}
		for _, n := range page.Nodes {
			if d, ok := n.Data.(*dgraphStruct.DgraphDoc); ok {
				kinds[d.NodeKind] = true
			}
		}
		if len(kinds) != 4 {
			return 0, fmt.Errorf("the folder's first page has the kinds %v, want a folder, pages, files and links", kinds)
		}
		return nonEmpty("the folder", len(page.Nodes), nil)
	},
	// §1.5 8: the big project board, with the web's default column sizes.
	"board.kanban": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		none := url.Values{}
		p, err := projectBusiness.GetDgraphProjectTaskListForKanban(ctx, f.BoardProject, f.ViewerUID(), "", helpers.OpenLimit(none), helpers.ClosedLimit(none))
		if err != nil || p == nil {
			return 0, fmt.Errorf("no board: %v", err)
		}
		n := 0
		for _, c := range p.ColumnCounts {
			n += c
		}
		return nonEmpty("the board", n, nil)
	},
	// DG4b: the board as the web asks for it now, its first 60 cards of each
	// open column with every column's count, then a column's next page.
	"board.kanban.first": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		none := url.Values{}
		p, err := projectBusiness.GetDgraphProjectTaskListForKanban(ctx, f.BoardProject, f.ViewerUID(), "", 60, helpers.ClosedLimit(none))
		if err != nil || p == nil {
			return 0, fmt.Errorf("no board: %v", err)
		}
		return nonEmpty("the board", len(p.TasksTodo)+len(p.TasksInProgresss)+len(p.TasksBacklog)+len(p.TasksInReview), nil)
	},
	"board.column": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		first, err := projectBusiness.GetProjectBoardColumnPage(ctx, f.BoardProject, f.ViewerUID(), "", dgraphStruct.TASK_STATUS_TODO, "", 60)
		if err != nil || len(first.Cards) == 0 {
			return 0, fmt.Errorf("no first page: %v", err)
		}
		page, err := projectBusiness.GetProjectBoardColumnPage(ctx, f.BoardProject, f.ViewerUID(), "", dgraphStruct.TASK_STATUS_TODO, first.Cards[len(first.Cards)-1].Uuid, 60)
		return nonEmpty("the column page", len(page.Cards), err)
	},
	// §1.6: access for many docs is one query, whether 10 or 500.
	"doc.access.10":  docAccess(10),
	"doc.access.500": docAccess(500),
	// The doc with 2,000 backlinks: its backlinks panel.
	"doc.backlinks": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		docs, err := docDomain.GetDocMentioners(ctx, f.BacklinkedDoc, 50)
		return nonEmpty("the backlinks", len(docs), err)
	},
	// What moving the head of the long chain reads of the tasks waiting on it.
	"deps.downstream": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		tasks, err := taskDomain.DownstreamSchedule(ctx, f.ChainHead)
		return nonEmpty("the schedule", len(tasks), err)
	},
	// The work graph (DG5), as the viewer sees it: the graph read, then every
	// node decided in one ReadMany (one read per kind, however many nodes).
	"graph.doc.1":     graphConnected("doc", func(f *dgfixture.Fixture) string { return f.BacklinkedDoc }, 1),
	"graph.doc.2":     graphConnected("doc", func(f *dgfixture.Fixture) string { return f.BacklinkedDoc }, 2),
	"graph.person.2":  graphConnected("person", func(f *dgfixture.Fixture) string { return f.SuperPoster.UUID }, 2),
	"graph.channel.2": graphConnected("channel", func(f *dgfixture.Fixture) string { return f.General }, 2),
	"graph.project.2": graphConnected("project", func(f *dgfixture.Fixture) string { return f.BoardProject }, 2),
	// The backlinked doc to #general: it is shared there, so a path exists.
	"graph.path.near": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		got, err := workGraphBusiness.PathsBetween(ctx, viewerSubject(f), workGraphBusiness.Ref{Kind: "doc", ID: f.BacklinkedDoc}, workGraphBusiness.Ref{Kind: "channel", ID: f.General})
		if err != nil {
			return 0, err
		}
		return nonEmpty("the paths", len(got.Paths), nil)
	},
	// The backlinked doc to the big board's project: the search's worst case,
	// four hops through every capped edge; no path is a fair answer, so it
	// counts the two ends decided as well.
	"graph.path.far": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		got, err := workGraphBusiness.PathsBetween(ctx, viewerSubject(f), workGraphBusiness.Ref{Kind: "doc", ID: f.BacklinkedDoc}, workGraphBusiness.Ref{Kind: "project", ID: f.BoardProject})
		if err != nil {
			return 0, err
		}
		return 2 + len(got.Paths), nil
	},
	// Ask AI's graph context (DG7): one hop out from twelve search hits at
	// once, among them the doc 2,000 docs mention and the long chain's head,
	// then every seed and node decided in one ReadMany. It must fit the 800 ms
	// Ask AI waits for it.
	"graph.expand.12": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		seeds := []workGraphBusiness.Ref{{Kind: "doc", ID: f.BacklinkedDoc}, {Kind: "doc", ID: f.DocParent}, {Kind: "doc", ID: f.DeepDoc},
			{Kind: "task", ID: f.ChainHead}, {Kind: "project", ID: f.SpaceProject}, {Kind: "project", ID: f.BoardProject}}
		for _, d := range f.ViewerDocs {
			if len(seeds) == 12 {
				break
			}
			seeds = append(seeds, workGraphBusiness.Ref{Kind: "doc", ID: d})
		}
		links, err := workGraphBusiness.ExpandFor(ctx, []accessBusiness.Subject{viewerSubject(f)}, seeds, nil)
		return nonEmpty("the links", len(links), err)
	},
	// Who knows (DG9) after its seeds: the one count read over 200 items, and
	// the whole answer as the viewer gets it (each kind decided once, the
	// count, then the people named).
	"whoknows.count.200": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		got, err := whoKnowsDomain.Count(ctx, whoKnowsDomain.Items{Docs: f.WhoKnows.Docs, Tasks: f.WhoKnows.Tasks, Messages: f.WhoKnows.Posts})
		if err != nil {
			return 0, err
		}
		return nonEmpty("the people", len(got.People), nil)
	},
	"whoknows.rank.200": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		var seeds []whoKnowsDomain.Seed
		for _, l := range []struct {
			kind string
			ids  []string
		}{{"doc", f.WhoKnows.Docs}, {"task", f.WhoKnows.Tasks}, {"message", f.WhoKnows.Posts}} {
			for _, s := range l.ids {
				seeds = append(seeds, whoKnowsDomain.Seed{Kind: l.kind, ID: s})
			}
		}
		got, err := whoKnowsBusiness.Rank(ctx, whoKnowsBusiness.Asker{Subject: viewerSubject(f)}, "budget", seeds, whoKnowsBusiness.MaxLimit)
		if err != nil {
			return 0, err
		}
		return nonEmpty("the people", len(got.People), nil)
	},
}

// refsRunners are the work graph's references (DG6): the sink's read of one
// source, the sink itself on a source already in step (a read and no
// write), and the nightly reconciler's page reads.
var refsRunners = map[string]budgetRunner{
	"refs.source": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		s, err := workGraphDomain.ReadSource(ctx, workGraphDomain.KindMessage, f.RefPost)
		if err != nil || s == nil {
			return 0, fmt.Errorf("the ref post: %v", err)
		}
		return nonEmpty("its edges", len(s.Have), nil)
	},
	"refs.reconcile.one": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		fix, err := refsBusiness.Reconcile(ctx, refsBusiness.Message, f.RefPost)
		if err == nil && !fix.Empty() {
			err = fmt.Errorf("the fixture's ref post drifted: %+v", fix)
		}
		return 1, err
	},
	"refs.page.message": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		page, err := workGraphDomain.ReadSources(ctx, workGraphDomain.KindMessage, "", refsBusiness.PageSize)
		return nonEmpty("the page", len(page), err)
	},
	"refs.page.task": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		page, err := workGraphDomain.ReadSources(ctx, workGraphDomain.KindTask, "", refsBusiness.PageSize)
		return nonEmpty("the page", len(page), err)
	},
	"refs.page.doc": func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		page, err := workGraphDomain.ReadSources(ctx, workGraphDomain.KindDoc, "", refsBusiness.DocPageSize)
		return nonEmpty("the page", len(page), err)
	},
}

func init() {
	for k, v := range refsRunners {
		budgetRunners[k] = v
	}
}

// graphConnected is a Connected panel's read of a fixture handle.
func graphConnected(kind string, handle func(*dgfixture.Fixture) string, depth int) budgetRunner {
	return func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		got, err := workGraphBusiness.NeighboursOf(ctx, viewerSubject(f), workGraphBusiness.Ref{Kind: kind, ID: handle(f)}, depth, nil)
		if err != nil {
			return 0, fmt.Errorf("%s %s: %w", kind, handle(f), err)
		}
		n := 0
		for _, g := range got.Groups {
			n += len(g.Items)
		}
		return nonEmpty("the connected things", n, nil)
	}
}

// budgetMidHistory is a moment in the middle of the fixture's history, for a
// page of older posts.
var budgetMidHistory = time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

// DG10's reads (dg10_budget_test.go): kept apart from the map above, so
// packages adding rows there merge clean.
func init() {
	budgetRunners["deps.critical"] = dg10Critical
	budgetRunners["deps.forecast"] = dg10Forecast
	budgetRunners["deps.move"] = dg10Move
}
