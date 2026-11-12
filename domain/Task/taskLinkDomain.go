package Domain

// Typed links between tasks (jira-01 J-H): relates, duplicates and clones.
// Blocking is not a link: it is the date dependency (task_blocked_by, finish
// to start and the rest, taskDependencyDomain.go), and a link never says it
// twice. Each link is one TaskLink node (tasklink_*): relates is symmetric,
// one node per pair whichever side added it; duplicates and clones are
// directed, from the task that duplicates (or clones) to the one it
// duplicates. Removing a link stamps tasklink_deleted_at and _by; adding it
// again revives the same node. Nothing is ever deleted.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

// The kinds of link.
const (
	LinkRelates    = "relates"
	LinkDuplicates = "duplicates"
	LinkClones     = "clones"
	// MaxLinks is how many live links a task holds, both ways together: the
	// next one is refused, and a read stops there.
	MaxLinks = 200
)

// linkLive keeps the live TaskLink nodes: a live link's
// tasklink_deleted_at is the zero time.
var linkLive = "type(TaskLink) AND " + dgraphquery.Live("tasklink_deleted_at")

// ErrLink is a link that can't be made that way; the refusal's words say why.
var ErrLink = errors.New("tasks: that link isn't open")

// LinkRefusal is a link refusal (ErrLink) in words.
func LinkRefusal(words string) error { return refusal{words, ErrLink} }

// LinkKind is nil for a kind of link; blocking is refused, in words pointing
// to dependencies.
func LinkKind(kind string) error {
	switch kind {
	case LinkRelates, LinkDuplicates, LinkClones:
		return nil
	case "blocks", "blocked_by", "is blocked by":
		return LinkRefusal("Blocking is a dependency, not a link: use Dependencies on the task (finish to start and the rest), which also moves the dates of the tasks that wait.")
	}
	return LinkRefusal("A link relates, duplicates or clones.")
}

// LinkEnds is the link of kind between task and other, as the task's side
// names it (direction "in": other is its from end; anything else: task is),
// as its from and to ends and its key: relates keys the pair in order, so
// either side names one node. Pure.
func LinkEnds(kind, task, other, direction string) (from, to, key string, err error) {
	if err := LinkKind(kind); err != nil {
		return "", "", "", err
	}
	task, other = strings.ToLower(task), strings.ToLower(other)
	if task == other {
		return "", "", "", LinkRefusal("A task can't link to itself.")
	}
	from, to = task, other
	if kind == LinkRelates {
		a, b := task, other
		if b < a {
			a, b = b, a
		}
		return from, to, kind + ":" + a + ":" + b, nil
	}
	if direction == "in" {
		from, to = other, task
	}
	return from, to, kind + ":" + from + ":" + to, nil
}

// LinkLabel is how a task reads its link of kind, from its side (direction
// "out": it is the from end). Pure.
func LinkLabel(kind, direction string) string {
	switch {
	case kind == LinkRelates:
		return "relates to"
	case direction == "in" && kind == LinkDuplicates:
		return "is duplicated by"
	case direction == "in" && kind == LinkClones:
		return "is cloned by"
	}
	return kind
}

// LinkChange is what changing a link has to check, read in the change's own
// transaction: both tasks as the person may see them (nil: not there for
// them) and their places (nil: not a live task of a project; each read by
// uuid as a Task node only), the link's node by its key when there is one,
// and how many live links each task holds (by uuid).
type LinkChange struct {
	From, To           *dgraphStruct.DgraphTask
	FromPlace, ToPlace *Place
	Node               *LinkNode
	Held               map[string]int
}

// LinkNode is a link's node as read: Ours when its two ends are the change's
// two tasks (either way round for relates), which a node is only ever taken
// to be (MayChange: its type, its ends).
type LinkNode struct {
	Uid        string
	Live, Ours bool
}

// linkQuery reads the node of key $k and the live links tasks $f and $t hold.
var linkQuery = `query q($k: string, $f: string, $t: string) {
	l(func: eq(tasklink_key, $k), first: 1) @filter(type(TaskLink)) {
		uid
		tasklink_deleted_at
		tasklink_from { task_uuid }
		tasklink_to { task_uuid }
	}
	f(func: eq(task_uuid, $f)) @filter(type(Task)) { task_uuid outs: count(~tasklink_from @filter(` + linkLive + `)) ins: count(~tasklink_to @filter(` + linkLive + `)) }
	t(func: eq(task_uuid, $t)) @filter(type(Task)) { task_uuid outs: count(~tasklink_from @filter(` + linkLive + `)) ins: count(~tasklink_to @filter(` + linkLive + `)) }
}`

type linkRead struct {
	L []struct {
		Uid       string     `json:"uid"`
		DeletedAt *time.Time `json:"tasklink_deleted_at"`
		From      *struct {
			Uuid string `json:"task_uuid"`
		} `json:"tasklink_from"`
		To *struct {
			Uuid string `json:"task_uuid"`
		} `json:"tasklink_to"`
	} `json:"l"`
	F []linkHeld `json:"f"`
	T []linkHeld `json:"t"`
}

type linkHeld struct {
	Uuid string `json:"task_uuid"`
	Out  int    `json:"outs"`
	In   int    `json:"ins"`
}

// execReader is a dgexec transaction's read, as readDependencyPair takes it.
func execReader(tx *dgexec.Txn) pairReader {
	return func(q string, vars map[string]string, out any) error {
		raw, err := dgexec.ReadIn[json.RawMessage](tx, q, vars)
		if err != nil || len(raw) == 0 {
			return err
		}
		return json.Unmarshal(raw, out)
	}
}

// ChangeLink adds the link of kind from from to to (key, from LinkEnds), or
// with remove takes it off, once check has passed the change as it stands,
// in one transaction (dgexec.InTxn) and one request; userDgraphUID is the
// person's graph uid. Adding revives the link's node when it was removed;
// removing stamps it (tasklink_deleted_at, _by) and keeps it. Both tasks are
// stamped (task_updated_at), so two changes on one task conflict and the
// cap holds, and both take a history line (linkHistory) in the same
// mutation. It reports whether anything changed.
func ChangeLink(ctx context.Context, from, to, key, kind, userDgraphUID string, remove bool, check func(*LinkChange) error) (bool, error) {
	changed := false
	err := dgexec.InTxn(ctx, func(tx *dgexec.Txn) error {
		changed = false
		pair, err := readDependencyPair(ctx, execReader(tx), from, to, userDgraphUID, false)
		if err != nil {
			return err
		}
		places, err := Places(ctx, func(q string, vars map[string]string) (map[string][]placeRow, error) {
			return dgexec.ReadIn[map[string][]placeRow](tx, q, vars)
		}, from, to)
		if err != nil {
			return err
		}
		got, err := dgexec.ReadIn[linkRead](tx, linkQuery, map[string]string{"$k": key, "$f": from, "$t": to})
		if err != nil {
			return err
		}
		c := &LinkChange{From: pair.Task, To: pair.Blocker, Held: map[string]int{}}
		if p, ok := places[from]; ok {
			c.FromPlace = &p
		}
		if p, ok := places[to]; ok {
			c.ToPlace = &p
		}
		for _, h := range append(got.F, got.T...) {
			c.Held[strings.ToLower(h.Uuid)] = h.Out + h.In
		}
		if len(got.L) > 0 {
			n := got.L[0]
			ends := func(a, b string) bool {
				return n.From != nil && n.To != nil && strings.EqualFold(n.From.Uuid, a) && strings.EqualFold(n.To.Uuid, b)
			}
			c.Node = &LinkNode{Uid: n.Uid, Live: n.DeletedAt == nil || n.DeletedAt.Year() <= 1970,
				Ours: ends(from, to) || (kind == LinkRelates && ends(to, from))}
		}
		if err := check(c); err != nil {
			return err
		}
		// check passes only live tasks as the person sees them; the uids
		// written are those of the Task and TaskLink nodes read here.
		if c.From == nil || c.FromPlace == nil || c.To == nil || c.ToPlace == nil || c.From.Uid != c.FromPlace.Uid || c.To.Uid != c.ToPlace.Uid {
			return fmt.Errorf("%w: %s, %s", ErrHierarchyMissing, from, to)
		}
		if c.Node != nil && !c.Node.Ours {
			return LinkRefusal("That link can't be changed here.")
		}
		if (remove && (c.Node == nil || !c.Node.Live)) || (!remove && c.Node != nil && c.Node.Live) {
			return nil
		}
		now := time.Now().UTC()
		me := map[string]any{"uid": userDgraphUID}
		set := []map[string]any{{"uid": c.FromPlace.Uid, "task_updated_at": now}, {"uid": c.ToPlace.Uid, "task_updated_at": now}}
		muts := []dgexec.Mutation{}
		switch {
		case remove:
			set = append(set, map[string]any{"uid": c.Node.Uid, "tasklink_deleted_at": now, "tasklink_deleted_by": me})
		case c.Node != nil:
			// The same node, live again: its removal's stamp goes.
			set = append(set, map[string]any{"uid": c.Node.Uid, "tasklink_deleted_at": time.Time{}, "tasklink_created_by": me, "tasklink_created_at": now})
			muts = append(muts, dgexec.Mutation{DelNQuads: "<" + c.Node.Uid + "> <tasklink_deleted_by> * ."})
		default:
			set = append(set, map[string]any{"uid": "_:link", "dgraph.type": "TaskLink", "tasklink_key": key, "tasklink_kind": kind,
				"tasklink_from": map[string]any{"uid": c.FromPlace.Uid}, "tasklink_to": map[string]any{"uid": c.ToPlace.Uid},
				"tasklink_created_by": me, "tasklink_created_at": now, "tasklink_deleted_at": time.Time{}})
		}
		set = append(set, linkHistory(userDgraphUID, kind, remove, *c.FromPlace, *c.ToPlace, now)...)
		muts = append(muts, dgexec.Mutation{Set: set})
		if _, err := tx.Mutate("", nil, muts...); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// LinkRow is one live link of a task as read: its kind, the task's side
// ("out": the task is its from end) and the task on its other end, with its
// project as the reader stands to it.
type LinkRow struct {
	Kind, Direction string
	Task            *dgraphStruct.DgraphTask
}

// linkEnd is what a link's read takes of the task on its other end.
const linkEnd = `uid task_uuid task_name task_status task_custom_status_name task_deleted_at
			task_assignee { uid } task_created_by { uid }
			task_project { uid project_uuid project_name project_deleted_at
				project_is_member: count(project_members @filter(uid($u)))
				project_is_admin: count(project_admins @filter(uid($u))) }`

// linksQuery reads task $t's live links both ways, MaxLinks each way at most,
// with their live other ends.
var linksQuery = fmt.Sprintf(`query q($t: string, $u: string) {
	q(func: eq(task_uuid, $t)) @filter(type(Task)) {
		outs: ~tasklink_from @filter(%[1]s) (first: %[2]d) { tasklink_kind end: tasklink_to @filter(type(Task) AND %[3]s) { %[4]s } }
		ins: ~tasklink_to @filter(%[1]s) (first: %[2]d) { tasklink_kind end: tasklink_from @filter(type(Task) AND %[3]s) { %[4]s } }
	}
}`, linkLive, MaxLinks, dgraphStruct.TASK_LIVE_FILTER, linkEnd)

type linksRead struct {
	Q []struct {
		Out []linkEdge `json:"outs"`
		In  []linkEdge `json:"ins"`
	} `json:"q"`
}

type linkEdge struct {
	Kind string                   `json:"tasklink_kind"`
	End  *dgraphStruct.DgraphTask `json:"end"`
}

// ReadLinks is the live links of the task taskUUID, both ways, at most
// MaxLinks in all, each with its live other end read as the reader of ctx
// (models/taskread) and with their standing (userDgraphUID) in its project:
// an end the reader may not see is left out whole. The caller judges the
// rest (Work Access).
func ReadLinks(ctx context.Context, taskUUID, userDgraphUID string) ([]LinkRow, error) {
	got, err := dgexec.Read[linksRead](ctx, linksQuery, map[string]string{"$t": taskUUID, "$u": userDgraphUID})
	if err != nil || len(got.Q) == 0 {
		return nil, err
	}
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	var out []LinkRow
	for _, side := range []struct {
		dir   string
		edges []linkEdge
	}{{"out", got.Q[0].Out}, {"in", got.Q[0].In}} {
		for _, e := range side.edges {
			if len(out) == MaxLinks {
				return out, nil
			}
			if e.End == nil || e.End.Uuid == "" || e.End.Project == nil || tr.Hides(e.End.Uuid) {
				continue
			}
			tr.Keep(e.End)
			out = append(out, LinkRow{Kind: e.Kind, Direction: side.dir, Task: e.End})
		}
	}
	return out, nil
}
