// Package workGraphDomain is the work graph's DQL (DG5): what is one or two
// hops from a thing, and the shortest ways two things are connected, over a
// fixed allow-list of edges.
//
// WHAT IT IS NOT. It decides nothing about who may see what: every node it
// returns goes through business/EntityLink ReadEntities, as the reader, in
// business/WorkGraph, and a path through anything the reader can't see is
// dropped whole there. It takes uuids and kinds, never a graph uid from a
// client: a root is found by its kind's uuid predicate, as a query variable.
//
// THE EDGES. Edges says which relationships count as a connection, from each
// kind, in both directions where the graph can walk both: an @reverse edge
// (~linked_docs), or a forward edge written on both sides (task_project and
// project_tasks). Membership edges (ch_members, project_members,
// team_members, user_channels and the rest) are gates, not connections, and
// are never walked: being in #general with 200 people doesn't connect you to
// them. A hub (a channel's posts, a person's posts) is reached only through a
// scan of the posts that share a doc (Edge.Scan), never by walking its list.
//
// EVERY HOP keeps live nodes only (dgraphquery.Live on the target kind's
// deletion time, or has(mem_uuid) for memory, which is removed rather than
// deleted), is capped (first: N per edge), and names its target by a kind's
// uuid predicate.
package workGraphDomain

import (
	"sort"
	"strings"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
)

// Kind is a kind of thing in the work graph, as business/EntityLink names it.
type Kind string

const (
	KindDoc     Kind = "doc"
	KindTask    Kind = "task"
	KindProject Kind = "project"
	KindPerson  Kind = "person"
	KindBoard   Kind = "board"
	KindChannel Kind = "channel"
	KindMessage Kind = "message" // a channel post
	KindChat    Kind = "chat"    // a direct or group message
	KindTeam    Kind = "team"
	KindComment Kind = "comment"
	KindMemory  Kind = "memory"
)

// kindSpec is how the graph names a kind: its uuid predicate and its
// deletion predicate ("" for none: memory is removed, not deleted).
type kindSpec struct {
	UUID    string
	Deleted string
}

var specs = map[Kind]kindSpec{
	KindDoc:     {"doc_uuid", "doc_deleted_at"},
	KindTask:    {"task_uuid", "task_deleted_at"},
	KindProject: {"project_uuid", "project_deleted_at"},
	KindPerson:  {"user_uuid", "user_deleted_at"},
	KindBoard:   {"board_uuid", "board_deleted_at"},
	KindChannel: {"ch_uuid", "ch_deleted_at"},
	KindMessage: {"post_uuid", "post_deleted_at"},
	KindChat:    {"chat_uuid", "chat_deleted_at"},
	KindTeam:    {"team_uuid", "team_deleted_at"},
	KindComment: {"comment_uuid", "comment_deleted_at"},
	KindMemory:  {"mem_uuid", ""},
}

// Kinds is every kind, in a fixed order.
func Kinds() []Kind {
	out := make([]Kind, 0, len(specs))
	for k := range specs {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Known is whether k is a kind of the work graph.
func Known(k Kind) bool { _, ok := specs[k]; return ok }

// DefaultKinds are the kinds a Connected panel shows when it names none.
// Comments only lead back to what they're on, and memory is the assistant's
// (DG7 asks for it by name), so neither is shown unless asked for.
// A comment that links a thing is shown with it all the same (business/
// WorkGraph): it is a reference, not a comment that is merely on the thing.
var DefaultKinds = []Kind{KindDoc, KindTask, KindProject, KindPerson, KindBoard, KindChannel, KindMessage, KindChat, KindTeam}

// Edge is one relationship that counts as a connection: from a kind, along a
// predicate as Dgraph walks it ("~pred" for a reverse edge), to one or more
// kinds. Rel is what it means, from From's side, as the web app words it.
type Edge struct {
	From Kind
	Rel  string
	Pred string
	To   []Kind
	// Scan is a hub's edge with no reverse to walk (a channel's posts, a
	// person's posts): its targets are found from the posts that share a doc
	// (has(post_doc)) whose forward edge (Pred without "~") points at the
	// node. Only ever from a query's root, and never in a shortest path.
	Scan bool
	// Opt is an edge followed only when its target kind is asked for by name
	// (comments, memory).
	Opt bool
}

// Edges is the allow-list. Its order is the order a panel lists a node's
// reasons in.
var Edges = []Edge{
	// A doc.
	{From: KindDoc, Rel: "linked_from", Pred: "~linked_docs", To: []Kind{KindTask, KindProject}},
	{From: KindDoc, Rel: "links_to", Pred: "doc_mentions", To: []Kind{KindDoc}},
	{From: KindDoc, Rel: "mentioned_by", Pred: "~doc_mentions", To: []Kind{KindDoc}},
	{From: KindDoc, Rel: "parent", Pred: "doc_parent", To: []Kind{KindDoc}},
	{From: KindDoc, Rel: "subpage", Pred: "~doc_parent", To: []Kind{KindDoc}},
	{From: KindDoc, Rel: "shared_in", Pred: "~post_doc", To: []Kind{KindMessage}},
	{From: KindDoc, Rel: "created_by", Pred: "doc_created_by", To: []Kind{KindPerson}},
	{From: KindDoc, Rel: "owned_by", Pred: "doc_owner", To: []Kind{KindPerson}},
	{From: KindDoc, Rel: "links_to", Pred: RefersTo, To: RefTargets},
	{From: KindDoc, Rel: "linked_from", Pred: "~" + RefersTo, To: RefSources},
	{From: KindDoc, Rel: "comment", Pred: "doc_comments", To: []Kind{KindComment}, Opt: true},
	// A task.
	{From: KindTask, Rel: "links_to", Pred: "linked_docs", To: []Kind{KindDoc}},
	{From: KindTask, Rel: "links_board", Pred: "linked_boards", To: []Kind{KindBoard}},
	{From: KindTask, Rel: "in_project", Pred: "task_project", To: []Kind{KindProject}},
	{From: KindTask, Rel: "assigned_to", Pred: "task_assignee", To: []Kind{KindPerson}},
	{From: KindTask, Rel: "blocked_by", Pred: "task_blocked_by", To: []Kind{KindTask}},
	{From: KindTask, Rel: "blocks", Pred: "~task_blocked_by", To: []Kind{KindTask}},
	{From: KindTask, Rel: "subtask_of", Pred: "task_parent_task", To: []Kind{KindTask}},
	{From: KindTask, Rel: "subtask", Pred: "task_sub_tasks", To: []Kind{KindTask}},
	{From: KindTask, Rel: "linked_from", Pred: "~" + RefersTo, To: RefSources},
	{From: KindTask, Rel: "comment", Pred: "task_comments", To: []Kind{KindComment}, Opt: true},
	// A project.
	{From: KindProject, Rel: "links_to", Pred: "linked_docs", To: []Kind{KindDoc}},
	{From: KindProject, Rel: "links_board", Pred: "linked_boards", To: []Kind{KindBoard}},
	{From: KindProject, Rel: "task", Pred: "project_tasks", To: []Kind{KindTask}},
	{From: KindProject, Rel: "in_team", Pred: "project_team", To: []Kind{KindTeam}},
	{From: KindProject, Rel: "linked_from", Pred: "~" + RefersTo, To: RefSources},
	{From: KindProject, Rel: "memory", Pred: "~mem_project", To: []Kind{KindMemory}, Opt: true},
	// A whiteboard.
	{From: KindBoard, Rel: "linked_from", Pred: "~linked_boards", To: []Kind{KindTask, KindProject}},
	// A channel post.
	{From: KindMessage, Rel: "shares", Pred: "post_doc", To: []Kind{KindDoc}},
	{From: KindMessage, Rel: "posted_by", Pred: "post_by", To: []Kind{KindPerson}},
	{From: KindMessage, Rel: "in_channel", Pred: "post_channel", To: []Kind{KindChannel}},
	{From: KindMessage, Rel: "links_to", Pred: RefersTo, To: RefTargets},
	{From: KindMessage, Rel: "comment", Pred: "post_comments", To: []Kind{KindComment}, Opt: true},
	// A direct or group message: what it links to (DG6). Who wrote it and the
	// conversation it's in are gates for the reader, never walked.
	{From: KindChat, Rel: "links_to", Pred: RefersTo, To: RefTargets},
	// A channel: the posts in it that share a doc.
	{From: KindChannel, Rel: "message", Pred: "~post_channel", To: []Kind{KindMessage}, Scan: true},
	// A person: what they made, own and are given, and their posts that share a doc.
	{From: KindPerson, Rel: "created", Pred: "~doc_created_by", To: []Kind{KindDoc}},
	{From: KindPerson, Rel: "owns", Pred: "~doc_owner", To: []Kind{KindDoc}},
	{From: KindPerson, Rel: "assigned", Pred: "~task_assignee", To: []Kind{KindTask}},
	{From: KindPerson, Rel: "posted", Pred: "~post_by", To: []Kind{KindMessage}, Scan: true},
	// A team.
	{From: KindTeam, Rel: "project", Pred: "team_projects", To: []Kind{KindProject}},
	// A comment, back to what it's on.
	{From: KindComment, Rel: "on", Pred: "comment_task", To: []Kind{KindTask}},
	{From: KindComment, Rel: "on", Pred: "comment_doc", To: []Kind{KindDoc}},
	{From: KindComment, Rel: "on", Pred: "comment_post", To: []Kind{KindMessage}},
	{From: KindComment, Rel: "links_to", Pred: RefersTo, To: RefTargets},
	// A memory item, back to its project.
	{From: KindMemory, Rel: "about", Pred: "mem_project", To: []Kind{KindProject}},
}

// Gates are the membership edges: who may see a thing, never a connection.
// The guard test holds Edges to none of them.
var Gates = []string{
	"ch_members", "ch_moderators", "user_channels", "user_fav_channels",
	"project_members", "project_admins", "user_projects",
	"team_members", "team_admins", "user_teams",
	"doc_editing_users", "doc_reading_users", "doc_commenting_users",
	"doc_team_editors", "doc_team_commenters", "doc_team_viewers",
	"board_editing_users", "board_reading_users", "board_commenting_users",
	"dm_participants", "user_dms",
}

// complements are forward edges written on both sides: walking one from its
// target is walking the other.
var complements = map[string]string{
	"task_project":     "project_tasks",
	"task_parent_task": "task_sub_tasks",
	"doc_comments":     "comment_doc",
	"task_comments":    "comment_task",
	"post_comments":    "comment_post",
	"project_team":     "team_projects",
}

// Inverse is pred walked the other way: its complement, or its reverse.
func Inverse(pred string) string {
	for a, b := range complements {
		if pred == a {
			return b
		}
		if pred == b {
			return a
		}
	}
	if strings.HasPrefix(pred, "~") {
		return pred[1:]
	}
	return "~" + pred
}

// EdgeFrom is the allow-list's edge from kind along pred, if there is one.
func EdgeFrom(from Kind, pred string) (Edge, bool) {
	for _, e := range Edges {
		if e.From == from && e.Pred == pred {
			return e, true
		}
	}
	return Edge{}, false
}

// edgesFrom is the edges followed from kinds (a node that may be any of
// them), each predicate once, keeping only those whose targets include a
// wanted kind or a kind expanded beyond (mids). Scan edges only from a root.
func edgesFrom(kinds []Kind, want func(Edge) bool, root bool) []int {
	var out []int
	seen := map[string]bool{}
	for i, e := range Edges {
		if !hasKind(kinds, e.From) || seen[e.Pred] || (e.Scan && !root) || !want(e) {
			continue
		}
		seen[e.Pred] = true
		out = append(out, i)
	}
	return out
}

func hasKind(ks []Kind, k Kind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}

// liveOf is the filter that keeps live nodes of any of kinds: each is one of
// them (has its uuid predicate) and none is deleted. A node lacks the other
// kinds' deletion predicates, so not gt() on them holds for it.
func liveOf(kinds []Kind) string {
	var is, live []string
	for _, k := range kinds {
		s := specs[k]
		is = append(is, "has("+s.UUID+")")
		if s.Deleted != "" {
			live = append(live, dgraphquery.Live(s.Deleted))
		}
	}
	f := "(" + strings.Join(is, " OR ") + ")"
	if len(live) > 0 {
		f += " AND " + strings.Join(live, " AND ")
	}
	return f
}

// uuidPreds is the uuid predicates of kinds, a node's names.
func uuidPreds(kinds []Kind) string {
	var out []string
	for _, k := range kinds {
		out = append(out, specs[k].UUID)
	}
	return strings.Join(out, " ")
}

// allUUIDPreds is every kind's uuid predicate, for a node of any kind.
func allUUIDPreds() string { return uuidPreds(Kinds()) }

// kindOf is the kind of a node by the uuid predicate it has, of kinds.
func kindOf(node map[string]any, kinds []Kind) (Kind, string) {
	for _, k := range kinds {
		if v, ok := node[specs[k].UUID].(string); ok && v != "" {
			return k, strings.ToLower(v)
		}
	}
	return "", ""
}
