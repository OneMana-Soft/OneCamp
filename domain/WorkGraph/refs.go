package workGraphDomain

// References (DG6): refers_to, the docs, tasks and projects a body links to.
//
// A source is a channel post, a direct or group message, a comment (on
// anything) or a doc; its body is read here and parsed by business/Refs
// (helpers/bodyrefs). A target is a doc, a task or a project, always found by
// its uuid in the same request that writes the edge, so nothing a client sent
// names a node: a source's uid is the one this package read by its uuid, and a
// removed edge's target uid is the one read off that source's own refers_to.
//
// The edge is a list with @reverse and nothing else: no @upsert and no token
// index, so a save adds no conflict key beyond the source node's own edge.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// RefersTo is the predicate.
const RefersTo = "refers_to"

// RefTargets are the kinds a body links to; RefSources the kinds whose
// bodies are read for links.
var (
	RefTargets = []Kind{KindDoc, KindTask, KindProject}
	RefSources = []Kind{KindMessage, KindChat, KindComment, KindDoc}
)

// bodyOf is each source kind's body predicate.
var bodyOf = map[Kind]string{
	KindMessage: "post_text",
	KindChat:    "chat_body_text",
	KindComment: "comment_text",
	KindDoc:     "doc_body",
}

// IsRefSource is whether k's bodies are read for links.
func IsRefSource(k Kind) bool { _, ok := bodyOf[k]; return ok }

// Target is one edge a source has now: the target's uid (as the source's
// own edge names it) and what it is.
type Target struct {
	UID string
	Ref Ref
}

// Source is a body and the edges it has now.
type Source struct {
	Kind Kind
	UID  string
	UUID string
	Body string
	Have []Target
}

// sourceNode is a source as a query reads it: its names by alias.
type sourceNode struct {
	UID      string           `json:"uid"`
	UUID     string           `json:"uuid"`
	Body     string           `json:"body"`
	RefersTo []map[string]any `json:"refers_to"`
}

func (n sourceNode) source(k Kind) Source {
	s := Source{Kind: k, UID: n.UID, UUID: strings.ToLower(n.UUID), Body: n.Body}
	for _, t := range n.RefersTo {
		uid, _ := t["uid"].(string)
		tk, id := kindOf(t, RefTargets)
		if tk == "" || !helpers.IsDgraphUID(uid) {
			// An edge to something that is no target any more (or never was):
			// kept as a target of no kind, so the write takes it off.
			if helpers.IsDgraphUID(uid) {
				s.Have = append(s.Have, Target{UID: uid})
			}
			continue
		}
		s.Have = append(s.Have, Target{UID: uid, Ref: Ref{Kind: tk, UUID: id}})
	}
	return s
}

// sourceFields is what a source read takes of each node.
func sourceFields(k Kind) string {
	return fmt.Sprintf("uid uuid: %s body: %s %s { uid %s }", specs[k].UUID, bodyOf[k], RefersTo, uuidPreds(RefTargets))
}

// ReadSource reads one source by its uuid: nil when there is none. One query.
func ReadSource(ctx context.Context, k Kind, id string) (*Source, error) {
	if !IsRefSource(k) {
		return nil, fmt.Errorf("work graph: %q has no body to read links from", k)
	}
	if !helpers.IsCanonicalUUID(id) {
		return nil, nil
	}
	q := fmt.Sprintf("query s($id: string) {\n\ts(func: eq(%s, $id), first: 1) { %s }\n}", specs[k].UUID, sourceFields(k))
	nodes, err := readSources(ctx, q, map[string]string{"$id": strings.ToLower(id)})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WorkGraph ReadSource %s %s: %+v", k, id, err)
		return nil, err
	}
	if len(nodes) == 0 || !helpers.IsDgraphUID(nodes[0].UID) {
		return nil, nil
	}
	s := nodes[0].source(k)
	return &s, nil
}

// ReadSourcesQuery is a page of a kind's sources, by uid after $after ("" for
// the first page): every node with the kind's uuid, live or deleted (a
// deleted one's edges are kept as its body says, and every hop of every read
// hides it).
func ReadSourcesQuery(k Kind, after string, n int) string {
	page := "first: " + strconv.Itoa(n)
	if after != "" {
		page += ", after: " + after
	}
	return fmt.Sprintf("{\n\ts(func: has(%s), %s) { %s }\n}", specs[k].UUID, page, sourceFields(k))
}

// ReadSources reads a page of k's sources: at most n, after the uid after.
// One query.
func ReadSources(ctx context.Context, k Kind, after string, n int) ([]Source, error) {
	if !IsRefSource(k) {
		return nil, fmt.Errorf("work graph: %q has no body to read links from", k)
	}
	if after != "" && !helpers.IsDgraphUID(after) {
		return nil, fmt.Errorf("work graph: page after %q, which is no uid", after)
	}
	nodes, err := readSources(ctx, ReadSourcesQuery(k, after, n), nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WorkGraph ReadSources %s after %s: %+v", k, after, err)
		return nil, err
	}
	out := make([]Source, 0, len(nodes))
	for _, n := range nodes {
		if helpers.IsDgraphUID(n.UID) {
			out = append(out, n.source(k))
		}
	}
	return out, nil
}

// ReadSourcesByUUID reads k's sources by uuid (at most a page's worth: the
// caller bounds ids). One query.
func ReadSourcesByUUID(ctx context.Context, k Kind, ids []string) ([]Source, error) {
	if !IsRefSource(k) {
		return nil, fmt.Errorf("work graph: %q has no body to read links from", k)
	}
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return nil, nil
	}
	nodes, err := readSources(ctx, fmt.Sprintf("{\n\ts(func: eq(%s, [%s])) { %s }\n}", specs[k].UUID, list, sourceFields(k)), nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WorkGraph ReadSourcesByUUID %s: %+v", k, err)
		return nil, err
	}
	out := make([]Source, 0, len(nodes))
	for _, n := range nodes {
		if helpers.IsDgraphUID(n.UID) {
			out = append(out, n.source(k))
		}
	}
	return out, nil
}

// readSources runs a source read (seam for tests).
var readSources = func(ctx context.Context, q string, vars map[string]string) ([]sourceNode, error) {
	return dgexec.List[sourceNode](ctx, q, vars, "s")
}

// Fix is one source's change: the edges to take off (by the target uids its
// own edges named) and the targets to add (by uuid).
type Fix struct {
	Source string
	Del    []string
	Add    []Ref
}

// Empty is a fix that changes nothing.
func (f Fix) Empty() bool { return len(f.Del) == 0 && len(f.Add) == 0 }

// Diff is the fix that makes s's edges exactly want: each target wanted and
// missing added, each edge not wanted (or to no target) taken off. Pure.
func Diff(s Source, want []Ref) Fix {
	wanted := map[Ref]bool{}
	for _, r := range want {
		wanted[Ref{Kind: r.Kind, UUID: strings.ToLower(r.UUID)}] = true
	}
	f := Fix{Source: s.UID}
	have := map[Ref]bool{}
	for _, t := range s.Have {
		if t.Ref.Kind == "" || !wanted[t.Ref] || have[t.Ref] {
			f.Del = append(f.Del, t.UID)
			continue
		}
		have[t.Ref] = true
	}
	for r := range wanted {
		if !have[r] {
			f.Add = append(f.Add, r)
		}
	}
	sort.Slice(f.Add, func(i, j int) bool {
		if f.Add[i].Kind != f.Add[j].Kind {
			return f.Add[i].Kind < f.Add[j].Kind
		}
		return f.Add[i].UUID < f.Add[j].UUID
	})
	sort.Strings(f.Del)
	return f
}

// FixRequest is the one upsert that applies fixes: each added target found by
// its kind's uuid in the query (a uuid that names nothing adds nothing), and
// each removed edge named by the uids the sources' own edges had. Empty when
// there is nothing to do.
func FixRequest(fixes []Fix) (q, set, del string, err error) {
	var qb, sb, db strings.Builder
	for i, f := range fixes {
		if f.Empty() {
			continue
		}
		if !helpers.IsDgraphUID(f.Source) {
			return "", "", "", fmt.Errorf("work graph: a fix for %q, which is no uid", f.Source)
		}
		for _, uid := range f.Del {
			if !helpers.IsDgraphUID(uid) {
				return "", "", "", fmt.Errorf("work graph: a removed edge to %q, which is no uid", uid)
			}
			fmt.Fprintf(&db, "<%s> <%s> <%s> .\n", f.Source, RefersTo, uid)
		}
		byKind := map[Kind][]string{}
		for _, r := range f.Add {
			byKind[r.Kind] = append(byKind[r.Kind], r.UUID)
		}
		for _, k := range RefTargets {
			list := dgraphquery.UUIDList(byKind[k])
			if list == "" {
				continue
			}
			v := fmt.Sprintf("a%d_%s", i, k)
			fmt.Fprintf(&qb, "\t%s as var(func: eq(%s, [%s]))\n", v, specs[k].UUID, list)
			fmt.Fprintf(&sb, "<%s> <%s> uid(%s) .\n", f.Source, RefersTo, v)
		}
	}
	if qb.Len() > 0 {
		q = "query {\n" + qb.String() + "}"
	}
	return q, sb.String(), db.String(), nil
}

// WriteFixes applies fixes in one request. Nothing is sent when none changes
// anything.
func WriteFixes(ctx context.Context, fixes []Fix) error {
	q, set, del, err := FixRequest(fixes)
	if err != nil || (set == "" && del == "") {
		return err
	}
	if err := mutate(ctx, q, dgexec.Mutation{SetNQuads: set, DelNQuads: del}); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WorkGraph WriteFixes: %+v", err)
		return err
	}
	return nil
}

// mutate commits a request (seam for tests).
var mutate = func(ctx context.Context, q string, m dgexec.Mutation) error {
	_, err := dgexec.Mutate(ctx, q, nil, m)
	return err
}

// Existing is which of refs name a node of their kind: one query. For the
// reconciler, which counts a link to nothing as no drift.
func Existing(ctx context.Context, refs []Ref) (map[Ref]bool, error) {
	byKind := map[Kind][]string{}
	for _, r := range refs {
		byKind[r.Kind] = append(byKind[r.Kind], r.UUID)
	}
	var b strings.Builder
	b.WriteString("{\n")
	asked := false
	for _, k := range RefTargets {
		if list := dgraphquery.UUIDList(byKind[k]); list != "" {
			asked = true
			fmt.Fprintf(&b, "\t%s(func: eq(%s, [%s])) { %s }\n", k, specs[k].UUID, list, specs[k].UUID)
		}
	}
	b.WriteString("}")
	out := map[Ref]bool{}
	if !asked {
		return out, nil
	}
	raw, err := read(ctx, b.String(), nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WorkGraph Existing: %+v", err)
		return nil, err
	}
	for _, k := range RefTargets {
		var nodes []map[string]any
		if err := json.Unmarshal(orEmpty(raw[string(k)]), &nodes); err != nil {
			return nil, err
		}
		for _, n := range nodes {
			if id, ok := n[specs[k].UUID].(string); ok {
				out[Ref{Kind: k, UUID: strings.ToLower(id)}] = true
			}
		}
	}
	return out, nil
}
