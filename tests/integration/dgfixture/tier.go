// Package dgfixture builds a workspace in Dgraph at a known size, the same way
// every time, through the real schema: people (bots and external accounts
// among them), teams, projects, channels with their members, posts with
// comments, reactions and mentions, DMs and groups with their messages, docs
// in deep trees with their shares, mentions and links, whiteboards, tasks
// with subtasks and dependency chains, and workspace memory. It also builds
// the super-nodes a real workspace grows: #general with every person in it,
// one person with 20,000 posts, one doc with 2,000 backlinks, one project
// board with thousands of cards and one long dependency chain.
//
// WHY. Every speed claim about the graph (DG4's N+1 fixes, DG5's work graph,
// DG7's GraphRAG, DG9, DG10) needs a graph the size of a real workspace to be
// measured on, and the integration tests seed a handful of nodes each. The
// budget tests in tests/integration (dgraph_budget_test.go) build this once
// and time the hot reads on it.
//
// Liveness is production's: a live node carries the zero time
// (0001-01-01T00:00:00Z) in its *_deleted_at, a deleted one a real time, so
// a read that forgets its liveness filter shows deleted work here too.
//
// The generator is pure (Generate writes RDF to a Sink), so its shape is unit
// tested without a server; Write sends it to a live Dgraph. The package
// imports no application package (helpers/integrationHarnessGuard_test.go):
// what it writes is the schema's predicates, by name.
package dgfixture

import (
	"fmt"
	"os"
)

// Tier is how big a fixture is.
type Tier struct {
	Name string

	People        int // everyone, the bots, external accounts and deleted people among them
	Bots          int
	External      int
	DeletedPeople int

	Teams    int
	Projects int

	Channels          int // #general among them
	ViewerChannels    int // channels the viewer is in, #general among them
	ChannelMembersMin int // members of a channel other than #general
	ChannelMembersMax int

	Posts            int // every post, the super poster's among them
	SuperPosterPosts int // the one person with a great many posts

	DMs            int // one-to-one conversations
	ViewerDMs      int // the viewer's among them
	Groups         int // group conversations
	ViewerGroups   int
	Chats          int // every message in a DM or group, the busiest DM's among them
	BusiestDMChats int // the viewer's busiest DM

	Docs              int
	DocDepth          int // the deepest tree
	DocParentChildren int // children of the one wide page
	Backlinks         int // docs mentioning the backlinked doc

	Boards int // whiteboards

	Tasks       int
	BoardTasks  int // cards on the one big project board
	ChainLength int // tasks in the long dependency chain

	MemoryItems int

	DocPosts int // posts that share a doc (post_doc), the backlinked doc's among them

	RefPosts int // posts whose body links a doc and a task (refers_to), the backlinked doc's among them
}

// Small is the default: every shape and every super-node, at a size a busy
// laptop builds in a minute or two and a few hundred MB of Dgraph.
var Small = Tier{
	Name:   "small",
	People: 240, Bots: 4, External: 10, DeletedPeople: 6,
	Teams: 8, Projects: 16,
	Channels: 40, ViewerChannels: 24, ChannelMembersMin: 8, ChannelMembersMax: 60,
	Posts: 36000, SuperPosterPosts: 20000,
	DMs: 400, ViewerDMs: 80, Groups: 40, ViewerGroups: 12,
	Chats: 12000, BusiestDMChats: 3000,
	Docs: 3000, DocDepth: 8, DocParentChildren: 300, Backlinks: 2000,
	Boards:      60,
	Tasks:       6000,
	BoardTasks:  1500,
	ChainLength: 40,
	MemoryItems: 400,
	DocPosts:    600,
	RefPosts:    600,
}

// Scale is the report's workspace (design/dgraph/REPORT-2026-10-18.md §4.6):
// 2,000 people, 300 channels, 1M posts, 50,000 docs and 100,000 tasks. It
// takes most of an hour to write and several GB of Dgraph, so only
// DGRAPH_FIXTURE_TIER=scale builds it.
var Scale = Tier{
	Name:   "scale",
	People: 2000, Bots: 10, External: 100, DeletedPeople: 40,
	Teams: 40, Projects: 200,
	Channels: 300, ViewerChannels: 120, ChannelMembersMin: 20, ChannelMembersMax: 400,
	Posts: 1000000, SuperPosterPosts: 20000,
	DMs: 4000, ViewerDMs: 400, Groups: 400, ViewerGroups: 60,
	Chats: 200000, BusiestDMChats: 20000,
	Docs: 50000, DocDepth: 12, DocParentChildren: 2000, Backlinks: 2000,
	Boards:      500,
	Tasks:       100000,
	BoardTasks:  10000,
	ChainLength: 200,
	MemoryItems: 5000,
	DocPosts:    20000,
	RefPosts:    20000,
}

// TierEnv names the tier a run builds: small (the default) or scale.
const TierEnv = "DGRAPH_FIXTURE_TIER"

// TierFromEnv is the tier TierEnv names.
func TierFromEnv() (Tier, error) {
	switch v := os.Getenv(TierEnv); v {
	case "", Small.Name:
		return Small, nil
	case Scale.Name:
		return Scale, nil
	default:
		return Tier{}, fmt.Errorf("%s=%q: the tiers are %q and %q", TierEnv, v, Small.Name, Scale.Name)
	}
}

// check refuses a tier whose parts don't fit together.
func (t Tier) check() error {
	switch {
	case t.People < 20:
		return fmt.Errorf("tier %s: at least 20 people", t.Name)
	case t.Bots+t.External+t.DeletedPeople+2 >= t.People:
		return fmt.Errorf("tier %s: bots, external and deleted people leave nobody", t.Name)
	case t.ViewerChannels > t.Channels || t.Channels < 3:
		return fmt.Errorf("tier %s: %d viewer channels of %d", t.Name, t.ViewerChannels, t.Channels)
	case t.SuperPosterPosts > t.Posts:
		return fmt.Errorf("tier %s: the super poster's posts are more than all posts", t.Name)
	case t.ViewerDMs > t.DMs || t.ViewerGroups > t.Groups:
		return fmt.Errorf("tier %s: more viewer conversations than conversations", t.Name)
	case t.ViewerDMs < 1 || t.BusiestDMChats > t.Chats:
		return fmt.Errorf("tier %s: the busiest DM needs a viewer DM and fewer chats than all", t.Name)
	case t.Backlinks >= t.Docs || t.DocParentChildren >= t.Docs || t.DocDepth < 2:
		return fmt.Errorf("tier %s: docs too few for the backlinks, the wide page or the depth", t.Name)
	case t.Projects < 5 || t.Teams < 2:
		return fmt.Errorf("tier %s: at least 5 projects and 2 teams", t.Name)
	case 2+t.DocDepth+t.DocParentChildren+t.Backlinks > t.Docs:
		return fmt.Errorf("tier %s: docs too few for the chain, the wide page and the backlinks together", t.Name)
	case t.BoardTasks+t.ChainLength*3 > t.Tasks:
		return fmt.Errorf("tier %s: board and chain tasks are more than all tasks", t.Name)
	}
	return nil
}
