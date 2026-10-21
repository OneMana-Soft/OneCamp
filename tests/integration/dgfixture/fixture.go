package dgfixture

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
)

// DefaultSeed is the seed the budget tests build with. A different seed is a
// different workspace of the same shape and size.
const DefaultSeed = 20261018

// namespace makes every uuid a function of the seed and the node's name, so a
// fixture's ids are the same on every build.
var namespace = uuid.MustParse("6f1c2c4e-8a51-4f3e-9d3a-0d6a1b7c9e10")

// base is when the workspace's history starts; every time in it is after.
var base = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

// span is how long its history is.
const span = 75 * 24 * time.Hour

// Person is someone in the workspace.
type Person struct {
	Key                    string // the node's name in the fixture, "u<n>"
	UUID                   string
	Name                   string
	Email                  string
	Bot, External, Deleted bool
}

// Fixture is a generated workspace and the handles a test reads it by: who
// the viewer is, which channel is #general, which doc has the backlinks and
// so on. After Write, UID names any person, team, project, channel, DM, doc,
// task or board by key.
type Fixture struct {
	Tier Tier
	Seed uint64

	People      []Person
	Viewer      Person // in ViewerChannels channels, ViewerDMs DMs, many projects and teams; mentioned, commented on, reacted to
	SuperPoster Person // SuperPosterPosts posts

	General       string   // #general's ch_uuid: every person is a member
	GeneralPosts  int      // its live and deleted posts
	ViewerChannel string   // a busy channel of the viewer's other than #general
	BusiestDM     string   // dm_grouping_id of the viewer's busiest DM
	ViewerGroup   string   // dm_grouping_id of a group of the viewer's
	DocParent     string   // doc_uuid of the wide page, DocParentChildren children
	DeepDoc       string   // doc_uuid of the deepest page, DocDepth below the top
	BacklinkedDoc string   // doc_uuid of the doc Backlinks docs mention
	RefPost       string   // post_uuid of a post whose body links the backlinked doc and a task (refers_to)
	SpaceProject  string   // project_uuid whose space holds docs, the viewer a member
	BoardProject  string   // project_uuid of the big board, the viewer a member
	ChainHead     string   // task_uuid at the head of the long dependency chain
	CustomStatus  []string // the board project's own statuses (Postgres ids)
	ViewerDocs    []string // doc_uuids of docs the viewer can open and can't, for access reads, in a fixed order
	// WhoKnows are Who knows' seeds (DG9): 200 items, as a search for a common
	// word finds them: docs (the viewer's and not), tasks and posts.
	WhoKnows struct{ Docs, Tasks, Posts []string }

	// Counts of what was written, by kind (people, posts, comments, ...).
	Counts map[string]int
	// Quads is how many N-Quads went to Dgraph, Batches in how many mutations.
	Quads, Batches int

	uids map[string]string
}

// UID is the graph uid of a node by key ("u0", "ch3", "doc12"), once written.
func (f *Fixture) UID(key string) string { return f.uids[key] }

// PersonUID is a person's graph uid.
func (f *Fixture) PersonUID(p Person) string { return f.uids[p.Key] }

// ViewerUID is the viewer's graph uid.
func (f *Fixture) ViewerUID() string { return f.uids[f.Viewer.Key] }

func id(seed uint64, key string) string {
	return uuid.NewSHA1(namespace, []byte(strconv.FormatUint(seed, 10)+"/"+key)).String()
}

// gen is one generation's state.
type gen struct {
	f *Fixture
	w *writer
	r *rand.Rand
	t Tier

	live      []int // indices of live, non-bot, internal people: who posts, comments and gets tasks
	channels  []chanInfo
	projects  []projInfo
	teams     []teamInfo
	viewerDMs []string // keys of the viewer's DMs
	dms       []dmInfo
	docs      []docInfo
}

type chanInfo struct {
	key, uuid string
	members   []int
	deleted   bool
}

type projInfo struct {
	key, uuid string
	members   []int
	deleted   bool
	team      int
}

type teamInfo struct {
	key, uuid string
	members   []int
	deleted   bool
}

type dmInfo struct {
	key, grouping string
	people        []int
}

type docInfo struct {
	key, uuid string
	depth     int
	space     string // "", "project:<i>", "team:<i>", "private:<person>"
	private   bool
	deleted   bool
	owner     int
}

// Generate writes the tier's workspace to sink, the same every time for one
// seed, and is the fixture with its handles (uids filled from sink's answers).
func Generate(ctx context.Context, t Tier, seed uint64, sink Sink) (*Fixture, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	f := &Fixture{Tier: t, Seed: seed, Counts: map[string]int{}}
	g := &gen{f: f, w: newWriter(ctx, sink), r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), t: t}
	steps := []func(){g.people, g.teamsAndProjects, g.channels_, g.posts, g.conversations, g.chats, g.docs_, g.boards, g.tasks, g.memory, g.docPosts, g.refPosts, g.whoKnows}
	for _, step := range steps {
		step()
		g.w.flush()
		if g.w.err != nil {
			return nil, g.w.err
		}
	}
	f.uids = g.w.uids
	f.Quads, f.Batches = g.w.sent, g.w.batches
	return f, nil
}

// when is the i-th of n moments spread over the workspace's history, a little
// jittered, so orders by time are not orders by key.
func (g *gen) when(i, n int) time.Time {
	step := span / time.Duration(max(n, 1))
	return base.Add(step*time.Duration(i) + time.Duration(g.r.Int64N(int64(max(step, time.Second)))))
}

// pick is k distinct members of from, in from's order.
func (g *gen) pick(from []int, k int) []int {
	if k >= len(from) {
		return append([]int(nil), from...)
	}
	idx := g.r.Perm(len(from))[:k]
	sort.Ints(idx)
	out := make([]int, k)
	for i, j := range idx {
		out[i] = from[j]
	}
	return out
}

func (g *gen) one(from []int) int { return from[g.r.IntN(len(from))] }

func (g *gen) pct(p int) bool { return g.r.IntN(100) < p }

func (g *gen) person(i int) string { return g.w.ref(g.f.People[i].Key) }

func (g *gen) count(kind string) { g.f.Counts[kind]++ }

// firstBot is the first bot's index.
func (g *gen) firstBot() int { return g.t.People - g.t.DeletedPeople - g.t.External - g.t.Bots }

// people: index 0 is the viewer, 1 the super poster; then the bots, the
// external accounts and the deleted people, at the end.
func (g *gen) people() {
	t := g.t
	for i := 0; i < t.People; i++ {
		key := "u" + strconv.Itoa(i)
		p := Person{Key: key, UUID: id(g.f.Seed, key), Name: fmt.Sprintf("person%d", i)}
		p.Email = p.Name + "@fixture.test"
		switch {
		case i == 0:
			p.Name, p.Email = "viewer", "viewer@fixture.test"
		case i == 1:
			p.Name, p.Email = "superposter", "superposter@fixture.test"
		case i >= t.People-t.DeletedPeople:
			p.Deleted = true
		case i >= t.People-t.DeletedPeople-t.External:
			p.External = true
		case i >= t.People-t.DeletedPeople-t.External-t.Bots:
			p.Bot, p.External = true, true // bots are external by class (memory: onecamp-bots-are-external)
		}
		g.f.People = append(g.f.People, p)
		if !p.Bot && !p.External && !p.Deleted {
			g.live = append(g.live, i)
		}
		s := g.w.node(key, "User")
		w := g.w
		w.lit(s, "user_uuid", p.UUID)
		w.lit(s, "user_email_id", p.Email)
		w.lit(s, "user_name", p.Name)
		w.lit(s, "user_full_name", fmt.Sprintf("Person %d", i))
		w.lit(s, "user_job_title", "Engineer")
		w.lit(s, "user_status", []string{"online", "offline", "away"}[i%3])
		w.num(s, "user_device_connected", i%3)
		created := g.when(i, t.People*4)
		w.at(s, "user_created_at", created)
		w.at(s, "user_updated_at", created)
		w.deletedAt(s, "user_deleted_at", p.Deleted, created.Add(30*24*time.Hour))
		w.boolean(s, "is_bot", p.Bot)
		w.boolean(s, "is_external", p.External)
		g.count("people")
		w.boundary()
	}
	g.f.Viewer, g.f.SuperPoster = g.f.People[0], g.f.People[1]
}

// teamsAndProjects: the viewer is in every other team and in two thirds of
// the projects, the board project, the space project and an archived one
// among them.
func (g *gen) teamsAndProjects() {
	t, w := g.t, g.w
	for i := 0; i < t.Teams; i++ {
		key := "team" + strconv.Itoa(i)
		ti := teamInfo{key: key, uuid: id(g.f.Seed, key), deleted: i == t.Teams-1}
		ti.members = g.pick(g.live[1:], max(3, len(g.live)/t.Teams*2))
		if i%2 == 0 {
			ti.members = append([]int{0}, ti.members...)
		}
		s := w.node(key, "Team")
		w.lit(s, "team_uuid", ti.uuid)
		w.lit(s, "team_name", fmt.Sprintf("Team %d", i))
		created := g.when(i, t.Teams)
		w.at(s, "team_created_at", created)
		w.at(s, "team_updated_at", created)
		w.deletedAt(s, "team_deleted_at", ti.deleted, created.Add(span/2))
		w.edge(s, "team_created_by", g.person(ti.members[0]))
		for j, m := range ti.members {
			w.edge(s, "team_members", g.person(m))
			w.edge(g.person(m), "user_teams", s)
			if j < 2 {
				w.edge(s, "team_admins", g.person(m))
			}
		}
		g.teams = append(g.teams, ti)
		g.count("teams")
		w.boundary()
	}
	for i := 0; i < t.Projects; i++ {
		key := "proj" + strconv.Itoa(i)
		pi := projInfo{key: key, uuid: id(g.f.Seed, key), team: i % (t.Teams - 1), deleted: i == t.Projects-1}
		pi.members = g.pick(g.live[1:], max(4, len(g.live)/4))
		if i%3 != 2 || i == t.Projects-1 {
			pi.members = append([]int{0}, pi.members...)
		}
		s := w.node(key, "Project")
		w.lit(s, "project_uuid", pi.uuid)
		w.lit(s, "project_name", fmt.Sprintf("Project %d", i))
		w.lit(s, "project_status", "active")
		created := g.when(i, t.Projects)
		w.at(s, "project_created_at", created)
		w.at(s, "project_updated_at", created)
		w.deletedAt(s, "project_deleted_at", pi.deleted, created.Add(span/2))
		w.edge(s, "project_created_by", g.person(pi.members[0]))
		team := w.ref(g.teams[pi.team].key)
		w.edge(s, "project_team", team)
		w.edge(team, "team_projects", s)
		for j, m := range pi.members {
			w.edge(s, "project_members", g.person(m))
			w.edge(g.person(m), "user_projects", s)
			if j < 2 {
				w.edge(s, "project_admins", g.person(m))
			}
		}
		g.projects = append(g.projects, pi)
		g.count("projects")
		w.boundary()
	}
	g.f.BoardProject, g.f.SpaceProject = g.projects[0].uuid, g.projects[1].uuid
	for k := 0; k < 3; k++ {
		g.f.CustomStatus = append(g.f.CustomStatus, id(g.f.Seed, "status"+strconv.Itoa(k)))
	}
}

// channels_: #general (ch0) has everyone, bots and external accounts too; the
// viewer is in ViewerChannels channels, a deleted one among them.
func (g *gen) channels_() {
	t, w := g.t, g.w
	all := make([]int, len(g.f.People))
	for i := range all {
		all[i] = i
	}
	for i := 0; i < t.Channels; i++ {
		key := "ch" + strconv.Itoa(i)
		ci := chanInfo{key: key, uuid: id(g.f.Seed, key), deleted: i == 2}
		switch {
		case i == 0:
			ci.members = all
		default:
			n := t.ChannelMembersMin + g.r.IntN(t.ChannelMembersMax-t.ChannelMembersMin+1)
			ci.members = g.pick(g.live[1:], min(n, len(g.live)-1))
			if i < t.ViewerChannels {
				ci.members = append([]int{0}, ci.members...)
			}
			if i == 1 || i == 3 {
				ci.members = append(ci.members, 1) // the super poster posts here too
			}
		}
		s := w.node(key, "Channel")
		w.lit(s, "ch_uuid", ci.uuid)
		w.lit(s, "ch_name", []string{"general", "engineering", "old-launch", "design"}[min(i, 3)]+strconv.Itoa(i))
		w.lit(s, "ch_about", "A channel of the fixture")
		w.boolean(s, "ch_private", i > 3 && i%5 == 0)
		created := g.when(i, t.Channels*2)
		w.at(s, "ch_created_at", created)
		w.at(s, "ch_updated_at", created)
		w.deletedAt(s, "ch_deleted_at", ci.deleted, created.Add(span/2))
		w.edge(s, "ch_created_by", g.person(ci.members[0]))
		w.edge(s, "ch_moderators", g.person(ci.members[0]))
		for _, m := range ci.members {
			w.edge(s, "ch_members", g.person(m))
			w.edge(g.person(m), "user_channels", s)
		}
		g.channels = append(g.channels, ci)
		g.count("channels")
		w.boundary()
	}
	g.f.General, g.f.ViewerChannel = g.channels[0].uuid, g.channels[1].uuid
}

// posts: #general takes 40% of the ordinary posts and half the super
// poster's; 2% are deleted. Some have comments, reactions and mentions, and
// the viewer is mentioned in a share of the mentions in their channels.
func (g *gen) posts() {
	t, w := g.t, g.w
	var mine []int // the viewer's channels, live, but #general
	for i, c := range g.channels {
		if i > 0 && !c.deleted && len(c.members) > 0 && c.members[0] == 0 {
			mine = append(mine, i)
		}
	}
	for i := 0; i < t.Posts; i++ {
		// Exactly SuperPosterPosts of them, spread through the history.
		super := g.r.IntN(t.Posts-i) < t.SuperPosterPosts-g.f.Counts["superposts"]
		var ch int
		switch {
		case super && g.pct(50):
			ch = 0
		case super:
			ch = []int{1, 3}[g.r.IntN(2)]
		case g.pct(40):
			ch = 0
		case g.pct(35) && len(mine) > 0:
			ch = mine[g.r.IntN(len(mine))]
		default:
			ch = 1 + g.r.IntN(len(g.channels)-1)
		}
		c := g.channels[ch]
		author := 1
		if !super {
			author = g.one(c.members)
			if g.f.People[author].Deleted || (g.f.People[author].External && !g.f.People[author].Bot) {
				author = g.one(g.live)
			}
			if ch != 0 && c.members[0] == 0 && g.pct(6) {
				author = 0 // the viewer writes some, for the inbox's comments and reactions
			}
		}
		key := "post" + strconv.Itoa(i)
		s := w.node(key, "Post")
		puuid := id(g.f.Seed, key)
		created := g.when(i, t.Posts)
		deleted := g.pct(2)
		w.lit(s, "post_uuid", puuid)
		w.lit(s, "post_text", fmt.Sprintf("Post %d about the fixture's work, with a few words to read.", i))
		w.at(s, "post_created_at", created)
		w.at(s, "post_updated_at", created)
		w.deletedAt(s, "post_deleted_at", deleted, created.Add(time.Hour))
		chRef, by := w.ref(c.key), g.person(author)
		w.edge(s, "post_by", by)
		w.edge(s, "post_channel", chRef)
		w.edge(chRef, "ch_posts", s)
		w.edge(by, "user_posts", s)
		g.count("posts")
		if super {
			g.count("superposts")
		}
		if ch == 0 {
			g.f.GeneralPosts++
		}
		if g.pct(8) {
			for k, n := 0, 1+g.r.IntN(3); k < n; k++ {
				ck := key + "c" + strconv.Itoa(k)
				cs := w.node(ck, "Comment")
				by := g.one(g.live)
				w.lit(cs, "comment_uuid", id(g.f.Seed, ck))
				w.lit(cs, "comment_text", "A comment on the post")
				at := created.Add(time.Duration(k+1) * time.Minute)
				w.at(cs, "comment_created_at", at)
				w.at(cs, "comment_updated_at", at)
				w.deletedAt(cs, "comment_deleted_at", g.pct(3), at.Add(time.Hour))
				w.edge(cs, "comment_by", g.person(by))
				w.edge(cs, "comment_post", s)
				w.edge(cs, "comment_on_content_added_by", g.person(author))
				w.edge(s, "post_comments", cs)
				g.count("comments")
			}
		}
		if g.pct(12) {
			for k, n := 0, 1+g.r.IntN(3); k < n; k++ {
				rk := key + "r" + strconv.Itoa(k)
				rs := w.node(rk, "Reaction")
				w.lit(rs, "reaction_emoji_id", []string{"+1", "tada", "eyes", "heart"}[k%4])
				w.at(rs, "reaction_added_at", created.Add(time.Duration(k+2)*time.Minute))
				w.edge(rs, "reaction_added_by", g.person(g.one(g.live)))
				w.edge(rs, "reaction_on_content_added_by", g.person(author))
				w.edge(s, "post_reactions", rs)
				g.count("reactions")
			}
		}
		if g.pct(6) {
			mk := key + "m"
			ms := w.node(mk, "Mention")
			w.lit(ms, "mention_post_uuid", puuid)
			w.at(ms, "mention_created_at", created)
			w.at(ms, "mention_updated_at", created)
			w.edge(ms, "mention_post", s)
			mentioned := g.one(c.members)
			if c.members[0] == 0 && g.pct(30) || ch == 0 && g.pct(5) {
				mentioned = 0
			}
			w.edge(ms, "mention_users", g.person(mentioned))
			w.edge(s, "post_mentions", ms)
			g.count("mentions")
		}
		w.boundary()
	}
}

// conversations: the viewer's DMs with the first people, then DMs between
// others; groups of 3 to 8, ViewerGroups of them the viewer's.
func (g *gen) conversations() {
	t, w := g.t, g.w
	seen := map[[2]int]bool{}
	others := g.live[1:]
	for i := 0; i < t.DMs; i++ {
		var a, b int
		if i < t.ViewerDMs {
			a, b = 0, others[i%len(others)]
			if i >= len(others) {
				b = g.one(others)
			}
		} else {
			for {
				a, b = g.one(g.live), g.one(others)
				if g.pct(3) {
					b = g.firstBot() // a DM with a bot now and then
				}
				if a > b {
					a, b = b, a
				}
				if a != b && a != 0 && !seen[[2]int{a, b}] {
					break
				}
			}
		}
		if seen[[2]int{a, b}] {
			continue
		}
		seen[[2]int{a, b}] = true
		key := "dm" + strconv.Itoa(len(g.dms))
		grouping := helpers.GetGroupingId(g.f.People[a].UUID, g.f.People[b].UUID)
		g.dm(key, grouping, "", []int{a, b})
		if a == 0 {
			g.viewerDMs = append(g.viewerDMs, key)
		}
	}
	for i := 0; i < t.Groups; i++ {
		members := g.pick(others, 2+g.r.IntN(7))
		if i < t.ViewerGroups {
			members = append([]int{0}, members...)
		}
		ids := make([]string, len(members))
		for j, m := range members {
			ids[j] = g.f.People[m].UUID
		}
		grouping, err := helpers.GenerateGroupID(ids)
		if err != nil {
			panic(err)
		}
		key := "dm" + strconv.Itoa(len(g.dms))
		g.dm(key, grouping, fmt.Sprintf("Group %d", i), members)
		if i == 0 {
			g.f.ViewerGroup = grouping
		}
	}
	g.f.BusiestDM = g.dms[0].grouping
	_ = w
}

func (g *gen) dm(key, grouping, name string, people []int) {
	w := g.w
	s := w.node(key, "Dm")
	w.lit(s, "dm_grouping_id", grouping)
	if name != "" {
		w.lit(s, "dm_name", name)
	}
	for _, p := range people {
		w.edge(s, "dm_participants", g.person(p))
		w.edge(g.person(p), "user_dms", s)
	}
	g.dms = append(g.dms, dmInfo{key: key, grouping: grouping, people: people})
	if name == "" {
		g.count("dms")
	} else {
		g.count("groups")
	}
	w.boundary()
}

// chats: the busiest DM (the viewer's first) takes BusiestDMChats; the rest
// go 60% to the viewer's conversations. A tenth of the DMs get none.
func (g *gen) chats() {
	t, w := g.t, g.w
	var viewerConvs []int
	for i, d := range g.dms {
		if d.people[0] == 0 && i%10 != 9 {
			viewerConvs = append(viewerConvs, i)
		}
	}
	for i := 0; i < t.Chats; i++ {
		var di int
		switch {
		case i < t.BusiestDMChats:
			di = 0
		case g.pct(60):
			di = viewerConvs[g.r.IntN(len(viewerConvs))]
		default:
			for di = g.r.IntN(len(g.dms)); di%10 == 9; di = g.r.IntN(len(g.dms)) {
			}
		}
		d := g.dms[di]
		from := d.people[g.r.IntN(len(d.people))]
		key := "chat" + strconv.Itoa(i)
		s := w.node(key, "Chat")
		created := g.when(i, t.Chats)
		w.lit(s, "chat_uuid", id(g.f.Seed, key))
		w.lit(s, "chat_body_text", fmt.Sprintf("Message %d, a short line between people.", i))
		w.at(s, "chat_created_at", created)
		w.at(s, "chat_updated_at", created)
		w.deletedAt(s, "chat_deleted_at", g.pct(2), created.Add(time.Hour))
		w.edge(s, "chat_from", g.person(from))
		if len(d.people) == 2 {
			w.edge(s, "chat_to", g.person(d.people[0]+d.people[1]-from))
		}
		dmRef := w.ref(d.key)
		w.edge(s, "chat_dm", dmRef)
		w.edge(dmRef, "dm_chats", s)
		g.count("chats")
		if g.pct(5) {
			rk := key + "r"
			rs := w.node(rk, "Reaction")
			w.lit(rs, "reaction_emoji_id", "+1")
			w.at(rs, "reaction_added_at", created.Add(time.Minute))
			w.edge(rs, "reaction_added_by", g.person(d.people[0]))
			w.edge(rs, "reaction_on_content_added_by", g.person(from))
			w.edge(s, "chat_reactions", rs)
			g.count("reactions")
		}
		w.boundary()
	}
}

// docs_: doc0 is the backlinked doc and doc1 the wide page, both workspace
// pages; doc2.. doc(1+DocDepth) are a chain DocDepth deep in the space
// project. The rest go to the workspace (40%), project and team spaces (35%)
// and private trees (25%, the viewer's among them), each under an earlier
// doc of its space two times in three. Private docs are shared now and then,
// with the viewer often; 3% of docs are deleted, a doc shared with the viewer
// among them. Docs mention earlier docs, and Backlinks docs mention doc0.
func (g *gen) docs_() {
	t, w := g.t, g.w
	bySpace := map[string][]int{}
	viewerTeam := 0
	for i, tm := range g.teams {
		if len(tm.members) > 0 && tm.members[0] == 0 && !tm.deleted {
			viewerTeam = i
			break
		}
	}
	for i := 0; i < t.Docs; i++ {
		key := "doc" + strconv.Itoa(i)
		d := docInfo{key: key, uuid: id(g.f.Seed, key), owner: g.one(g.live)}
		parent := -1
		switch {
		case i <= 1:
			d.owner = 0
		case i < 2+t.DocDepth:
			d.space = "project:1"
			if i > 2 {
				parent = i - 1
			}
		case i < 2+t.DocDepth+t.DocParentChildren:
			parent = 1
		default:
			switch r := g.r.IntN(100); {
			case r < 40:
			case r < 55:
				d.space = "project:" + strconv.Itoa(g.r.IntN(len(g.projects)-1))
			case r < 75:
				d.space = "team:" + strconv.Itoa(g.r.IntN(len(g.teams)))
			case r < 85:
				d.space, d.private, d.owner = "private:0", true, 0
			default:
				d.private = true
				d.space = "private:" + strconv.Itoa(d.owner)
			}
			if cands := bySpace[d.space]; len(cands) > 0 && g.r.IntN(3) < 2 {
				p := cands[g.r.IntN(len(cands))]
				if g.docs[p].depth < t.DocDepth-1 {
					parent = p
				}
			}
		}
		if parent >= 0 {
			d.depth = g.docs[parent].depth + 1
			d.private = g.docs[parent].private
		}
		backlink := i >= 2+t.DocDepth+t.DocParentChildren && i < 2+t.DocDepth+t.DocParentChildren+t.Backlinks
		d.deleted = i > 2+t.DocDepth && !backlink && g.pct(3)
		if i == t.Docs-1 {
			// The share that outlived its doc: someone's private doc, deleted,
			// that was shared with the viewer (written below).
			parent, d.space, d.private, d.deleted, d.owner = -1, "private:"+strconv.Itoa(g.live[1]), true, true, g.live[1]
		}
		s := w.node(key, "Doc")
		created := g.when(i, t.Docs)
		w.lit(s, "doc_uuid", d.uuid)
		w.lit(s, "doc_title", fmt.Sprintf("Doc %d", i))
		w.lit(s, "doc_snippet", "The first lines of the doc.")
		w.boolean(s, "doc_private", d.private)
		w.at(s, "doc_created_at", created)
		w.at(s, "doc_updated_at", created.Add(time.Duration(g.r.IntN(48))*time.Hour))
		w.deletedAt(s, "doc_deleted_at", d.deleted, created.Add(span/3))
		w.lit(s, "doc_position", fmt.Sprintf("a%07d", i))
		owner := g.person(d.owner)
		w.edge(s, "doc_created_by", owner)
		w.edge(s, "doc_owner", owner)
		if parent >= 0 {
			w.edge(s, "doc_parent", w.ref(g.docs[parent].key))
		}
		switch kind, n := splitSpace(d.space); kind {
		case "project":
			w.lit(s, "doc_space_kind", "project")
			w.lit(s, "doc_space_uuid", g.projects[n].uuid)
		case "team":
			w.lit(s, "doc_space_kind", "team")
			w.lit(s, "doc_space_uuid", g.teams[n].uuid)
		}
		if d.private && i > 1 {
			if g.pct(30) {
				for _, p := range g.pick(g.live[1:], 1+g.r.IntN(5)) {
					w.edge(s, []string{"doc_reading_users", "doc_commenting_users", "doc_editing_users"}[p%3], g.person(p))
				}
			}
			if d.owner != 0 && g.pct(25) {
				w.edge(s, "doc_reading_users", g.person(0))
				g.count("docsSharedWithViewer")
			}
			if g.pct(10) {
				w.edge(s, "doc_team_viewers", w.ref(g.teams[viewerTeam].key))
			}
		}
		if backlink {
			w.edge(s, "doc_mentions", w.ref("doc0"))
		} else if i > 2 && g.pct(10) {
			// Never doc0: its backlinks are exactly Backlinks.
			for k := 0; k < 1+g.r.IntN(3); k++ {
				w.edge(s, "doc_mentions", w.ref(g.docs[2+g.r.IntN(i-2)].key))
			}
		}
		g.docs = append(g.docs, d)
		if i > 1 { // doc1's children are exactly DocParentChildren
			bySpace[d.space] = append(bySpace[d.space], i)
		}
		g.count("docs")
		w.boundary()
	}
	// The viewer's share of a deleted doc: a share whose doc is gone.
	w.edge(w.ref(g.docs[len(g.docs)-1].key), "doc_reading_users", g.person(0))
	g.count("deletedShares")
	g.f.BacklinkedDoc, g.f.DocParent, g.f.DeepDoc = g.docs[0].uuid, g.docs[1].uuid, g.docs[1+t.DocDepth].uuid
	// Docs to decide access on: a fixed spread of every kind (private and
	// not, shared and not, deleted), in key order.
	for i := 0; i < len(g.docs); i += max(1, len(g.docs)/1000) {
		g.f.ViewerDocs = append(g.f.ViewerDocs, g.docs[i].uuid)
	}
}

func splitSpace(space string) (string, int) {
	for i := 0; i < len(space); i++ {
		if space[i] == ':' {
			n, _ := strconv.Atoi(space[i+1:])
			return space[:i], n
		}
	}
	return space, 0
}

// boards: whiteboards, private now and then, shared with a few people.
func (g *gen) boards() {
	t, w := g.t, g.w
	for i := 0; i < t.Boards; i++ {
		key := "board" + strconv.Itoa(i)
		s := w.node(key, "Board")
		created := g.when(i, t.Boards)
		owner := g.one(g.live)
		if i%4 == 0 {
			owner = 0
		}
		w.lit(s, "board_uuid", id(g.f.Seed, key))
		w.lit(s, "board_title", fmt.Sprintf("Board %d", i))
		w.boolean(s, "board_private", i%3 == 0)
		w.at(s, "board_created_at", created)
		w.at(s, "board_updated_at", created)
		w.deletedAt(s, "board_deleted_at", i%17 == 16, created.Add(span/3))
		w.edge(s, "board_created_by", g.person(owner))
		for _, p := range g.pick(g.live, 3) {
			w.edge(s, []string{"board_editing_users", "board_reading_users"}[p%2], g.person(p))
		}
		g.count("boards")
		w.boundary()
	}
}

var statuses = []string{"todo", "inProgress", "backlog", "inReview", "done", "canceled"}

var depKinds = []string{"fs", "ss", "ff", "sf"}

// tasks: the board project takes BoardTasks; project 2 holds the long chain,
// each of its tasks waiting on the one before and on two more besides; the
// rest spread over the projects, the archived one too. 15% are subtasks, 2%
// deleted, a tenth of the board's in a custom status. The viewer is assigned
// a share, and some tasks link docs.
func (g *gen) tasks() {
	t, w := g.t, g.w
	chainProj := 3             // the viewer is in it (teamsAndProjects)
	var tops = map[int][]int{} // live top-level tasks by project
	n := 0
	write := func(proj int, parent int, blockers []int, facets bool) int {
		i := n
		n++
		key := "task" + strconv.Itoa(i)
		p := g.projects[proj]
		s := w.node(key, "Task")
		created := g.when(i, t.Tasks)
		status := statuses[g.r.IntN(len(statuses))]
		deleted := parent < 0 && len(blockers) == 0 && g.pct(2)
		w.lit(s, "task_uuid", id(g.f.Seed, key))
		w.lit(s, "task_name", fmt.Sprintf("Task %d", i))
		w.lit(s, "task_status", status)
		w.lit(s, "task_priority", []string{"low", "medium", "high", "urgent"}[i%4])
		w.float(s, "task_rank", float64(g.r.IntN(1000000))/7.0)
		w.at(s, "task_created_at", created)
		w.at(s, "task_updated_at", created)
		w.at(s, "task_status_since", created)
		w.at(s, "task_start_date", created.Add(24*time.Hour))
		w.at(s, "task_due_date", created.Add(time.Duration(2+g.r.IntN(20))*24*time.Hour))
		w.deletedAt(s, "task_deleted_at", deleted, created.Add(span/4))
		if proj == 0 && g.pct(10) {
			w.lit(s, "task_custom_status", g.f.CustomStatus[g.r.IntN(len(g.f.CustomStatus))])
			w.lit(s, "task_custom_status_name", "Custom")
		}
		pref := w.ref(p.key)
		w.edge(s, "task_project", pref)
		w.edge(pref, "project_tasks", s)
		w.edge(s, "task_created_by", g.person(p.members[0]))
		assignee := g.one(p.members)
		if p.members[0] == 0 && g.pct(8) {
			assignee = 0
		}
		w.edge(s, "task_assignee", g.person(assignee))
		w.edge(g.person(assignee), "user_tasks", s)
		if parent >= 0 {
			pk := w.ref("task" + strconv.Itoa(parent))
			w.edge(s, "task_parent_task", pk)
			w.edge(pk, "task_sub_tasks", s)
		}
		for _, b := range blockers {
			o := w.ref("task" + strconv.Itoa(b))
			if facets {
				lag := g.r.IntN(4)
				f := fmt.Sprintf("kind=%q", depKinds[g.r.IntN(len(depKinds))])
				if lag > 0 {
					f += fmt.Sprintf(", lag=%d", lag)
				}
				w.edgeFacets(s, "task_blocked_by", o, f)
			} else {
				w.edge(s, "task_blocked_by", o)
			}
			g.count("dependencies")
		}
		if g.pct(5) && len(g.docs) > 0 {
			w.edge(s, "linked_docs", w.ref(g.docs[g.r.IntN(len(g.docs))].key))
		}
		if parent < 0 && !deleted {
			tops[proj] = append(tops[proj], i)
		}
		g.count("tasks")
		w.boundary()
		return i
	}
	// The chain: c0 <- c1 <- ... each also blocking two side tasks.
	prev := -1
	for k := 0; k < t.ChainLength; k++ {
		var blockers []int
		if prev >= 0 {
			blockers = []int{prev}
		}
		c := write(chainProj, -1, blockers, true)
		if k == 0 {
			g.f.ChainHead = id(g.f.Seed, "task"+strconv.Itoa(c))
		}
		for j := 0; j < 2; j++ {
			write(chainProj, -1, []int{c}, true)
		}
		prev = c
	}
	for n < t.Tasks {
		proj := 0
		if n >= t.ChainLength*3+t.BoardTasks {
			proj = 1 + g.r.IntN(len(g.projects)-1)
		}
		parent := -1
		if cands := tops[proj]; len(cands) > 0 && g.pct(15) {
			parent = cands[g.r.IntN(len(cands))]
		}
		var blockers []int
		if cands := tops[proj]; parent < 0 && len(cands) > 0 && g.pct(5) {
			blockers = []int{cands[g.r.IntN(len(cands))]}
		}
		write(proj, parent, blockers, true)
	}
}

// memory: workspace memory items, owned by people, in channels and projects.
func (g *gen) memory() {
	t, w := g.t, g.w
	for i := 0; i < t.MemoryItems; i++ {
		key := "mem" + strconv.Itoa(i)
		s := w.node(key, "MemoryItem")
		created := g.when(i, t.MemoryItems)
		w.lit(s, "mem_uuid", id(g.f.Seed, key))
		w.lit(s, "mem_kind", []string{"decision", "commitment", "fact"}[i%3])
		w.lit(s, "mem_content", fmt.Sprintf("Memory %d: what the team decided.", i))
		w.lit(s, "mem_status", []string{"open", "done"}[i%2])
		w.num(s, "mem_confidence", 50+i%50)
		w.at(s, "mem_created_at", created)
		w.at(s, "mem_updated_at", created)
		w.edge(s, "mem_owner", g.person(g.one(g.live)))
		if i%2 == 0 {
			w.edge(s, "mem_channel", w.ref(g.channels[g.r.IntN(len(g.channels))].key))
		} else {
			w.edge(s, "mem_project", w.ref(g.projects[g.r.IntN(len(g.projects))].key))
		}
		g.count("memory")
		w.boundary()
	}
}

// sharedDocPosts is how many of the doc posts share the backlinked doc.
const sharedDocPosts = 40

// docPosts: posts that share a doc (post_doc), as a message with a doc
// pasted in does, for the work graph (DG5): the backlinked doc shared
// sharedDocPosts times, a third of them by the super poster and a third in
// #general, the rest of the docs at random. They draw from their own random
// stream, so adding them changed nothing else the fixture writes.
func (g *gen) docPosts() {
	t, w := g.t, g.w
	r := rand.New(rand.NewPCG(g.f.Seed^0x5eedd0c5, g.f.Seed))
	for i := 0; i < t.DocPosts; i++ {
		key := "share" + strconv.Itoa(i)
		d := g.docs[0]
		if i >= sharedDocPosts {
			d = g.docs[r.IntN(len(g.docs))]
		}
		ch := g.channels[r.IntN(len(g.channels))]
		author := g.live[r.IntN(len(g.live))]
		switch i % 3 {
		case 0:
			author = 1
		case 1:
			ch = g.channels[0]
		}
		s := w.node(key, "Post")
		created := base.Add(time.Duration(r.Int64N(int64(span))))
		w.lit(s, "post_uuid", id(g.f.Seed, key))
		w.lit(s, "post_text", fmt.Sprintf("Share %d: have a look at this doc.", i))
		w.at(s, "post_created_at", created)
		w.at(s, "post_updated_at", created)
		w.deletedAt(s, "post_deleted_at", r.IntN(100) < 2, created.Add(time.Hour))
		chRef, by := w.ref(ch.key), g.person(author)
		w.edge(s, "post_by", by)
		w.edge(s, "post_channel", chRef)
		w.edge(chRef, "ch_posts", s)
		w.edge(by, "user_posts", s)
		w.edge(s, "post_doc", w.ref(d.key))
		g.count("docPosts")
		w.boundary()
	}
}

// referringPosts is how many of the ref posts link the backlinked doc.
const referringPosts = 200

// refPosts: posts whose body links a doc and a task, with the refers_to edges
// the work graph keeps for them (DG6): the backlinked doc linked
// referringPosts times (a super-node of references), a third by the super
// poster and a third in #general, the rest to docs at random; each also links
// a task at random. Their own random stream, so nothing else changes.
func (g *gen) refPosts() {
	t, w := g.t, g.w
	if t.RefPosts == 0 || t.Tasks == 0 {
		return
	}
	r := rand.New(rand.NewPCG(g.f.Seed^0x4ef5ee75, g.f.Seed))
	for i := 0; i < t.RefPosts; i++ {
		key := "ref" + strconv.Itoa(i)
		d := g.docs[0]
		if i >= referringPosts {
			d = g.docs[r.IntN(len(g.docs))]
		}
		task := "task" + strconv.Itoa(r.IntN(t.Tasks))
		ch := g.channels[r.IntN(len(g.channels))]
		author := g.live[r.IntN(len(g.live))]
		switch i % 3 {
		case 0:
			author = 1
		case 1:
			ch = g.channels[0]
		}
		s := w.node(key, "Post")
		created := base.Add(time.Duration(r.Int64N(int64(span))))
		w.lit(s, "post_uuid", id(g.f.Seed, key))
		w.lit(s, "post_text", fmt.Sprintf(`<p>Ref %d: <span data-type="entityMention" data-id="doc:%s"></span> and <a href="/app/task/%s">the task</a></p>`, i, d.uuid, id(g.f.Seed, task)))
		w.at(s, "post_created_at", created)
		w.at(s, "post_updated_at", created)
		w.deletedAt(s, "post_deleted_at", false, created)
		chRef, by := w.ref(ch.key), g.person(author)
		w.edge(s, "post_by", by)
		w.edge(s, "post_channel", chRef)
		w.edge(chRef, "ch_posts", s)
		w.edge(by, "user_posts", s)
		w.edge(s, "refers_to", w.ref(d.key))
		w.edge(s, "refers_to", w.ref(task))
		g.count("refPosts")
		w.boundary()
	}
	g.f.RefPost = id(g.f.Seed, "ref0")
}

// whoKnows names Who knows' seeds (DG9): the first 80 docs, tasks and 40
// posts by key, so the count reads authors, assignees and commenters across
// every kind. It writes nothing and draws nothing from the random stream.
func (g *gen) whoKnows() {
	for i := 0; i < len(g.docs) && i < 80; i++ {
		g.f.WhoKnows.Docs = append(g.f.WhoKnows.Docs, g.docs[i].uuid)
	}
	for i := 0; i < g.t.Tasks && i < 80; i++ {
		g.f.WhoKnows.Tasks = append(g.f.WhoKnows.Tasks, id(g.f.Seed, "task"+strconv.Itoa(i)))
	}
	for i := 0; i < g.t.Posts && i < 40; i++ {
		g.f.WhoKnows.Posts = append(g.f.WhoKnows.Posts, id(g.f.Seed, "post"+strconv.Itoa(i)))
	}
}
