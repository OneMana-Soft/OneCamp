package domain

// A person's stray nodes. Before 26 Oct an import's file stages named a
// file's maker with a new node holding the importer's user_uuid, one for each
// file they kept, and every write through eq(user_uuid) reached those nodes
// too: connects typed them and role changes gave them edges. A person's own
// node is the first one holding their uuid (dgraphquery.PersonByUUID); every
// other node holding it is a stray. Merging a stray moves each edge into it
// onto the person's own node, gives the person the stray's own edges they
// lack, and deletes the stray (business/User strayMerge.go runs it).

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// GraphPredicate is a predicate of the graph's live schema.
type GraphPredicate struct {
	Name    string `json:"predicate"`
	Type    string `json:"type"`
	List    bool   `json:"list"`
	Reverse bool   `json:"reverse"`
}

// predicateName is a predicate name that can be written into a query as it is.
var predicateName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// LivePredicates reads the graph's schema as Dgraph holds it, so predicates it
// inferred from a write are there too (attachment_created_by is in no schema
// file), without Dgraph's own.
func LivePredicates(ctx context.Context) ([]GraphPredicate, error) {
	s, err := dgexec.Read[struct {
		Schema []GraphPredicate `json:"schema"`
	}](ctx, `schema {}`, nil)
	if err != nil {
		return nil, err
	}
	var out []GraphPredicate
	for _, p := range s.Schema {
		if !strings.HasPrefix(p.Name, "dgraph.") && predicateName.MatchString(p.Name) {
			out = append(out, p)
		}
	}
	return out, nil
}

// StrayGroup is a uuid more than one node holds: the person's own node, the
// first, and the strays after it.
type StrayGroup struct {
	UserUUID string
	Person   string
	Strays   []string
	// Disagree is that a stray holds another email than the person's own node
	// (trimmed, in any case): someone else's, so the group is left as it is.
	Disagree bool
	// Renamed is that a stray holds another name than the person's own node:
	// the person renamed themselves after writes stopped reaching it. They are
	// the same person, and the merge keeps their own node's name (main's
	// ruling, 26 Oct).
	Renamed bool
}

// strayIdentity is what a group's nodes are compared on.
const strayIdentity = "user_email_id user_name user_full_name"

// PeopleWithStrays reads the nodes holding a user_uuid, size at a time from
// the one after the uid after ("" for the first page), and answers each uuid
// among them that more than one node holds, with every node holding it. next
// is the uid the following page starts after, "" after the last page.
func PeopleWithStrays(ctx context.Context, after string, size int) (groups []StrayGroup, next string, err error) {
	from := ""
	if after != "" {
		if !helpers.IsDgraphUID(after) {
			return nil, "", fmt.Errorf("stray nodes: page after %q, not a uid", after)
		}
		from = ", after: " + after
	}
	type held struct {
		Uid  string `json:"uid"`
		UUID string `json:"user_uuid"`
	}
	page, err := dgexec.Read[struct {
		Page []held `json:"page"`
	}](ctx, `{ page(func: has(user_uuid), first: `+strconv.Itoa(size)+from+`) { uid user_uuid } }`, nil)
	if err != nil {
		return nil, "", err
	}
	if len(page.Page) == size {
		next = page.Page[size-1].Uid
	}
	uuids := make([]string, len(page.Page))
	for i, n := range page.Page {
		uuids[i] = n.UUID
	}
	list := dgraphquery.UUIDList(uuids)
	if list == "" {
		return nil, next, nil
	}
	nodes, err := dgexec.Read[struct {
		Nodes []map[string]any `json:"nodes"`
	}](ctx, `{ nodes(func: eq(user_uuid, [`+list+`])) { uid user_uuid `+strayIdentity+` } }`, nil)
	if err != nil {
		return nil, "", err
	}
	byUUID := map[string][]map[string]any{}
	var order []string
	for _, n := range nodes.Nodes {
		id, _ := n["user_uuid"].(string)
		if byUUID[id] == nil {
			order = append(order, id)
		}
		byUUID[id] = append(byUUID[id], n)
	}
	for _, id := range order {
		held := byUUID[id]
		if len(held) < 2 {
			continue
		}
		slices.SortFunc(held, func(a, b map[string]any) int { return compareUIDs(a["uid"].(string), b["uid"].(string)) })
		g := StrayGroup{UserUUID: id, Person: held[0]["uid"].(string)}
		for _, s := range held[1:] {
			g.Strays = append(g.Strays, s["uid"].(string))
			g.Disagree = g.Disagree || disagree(held[0], s)
			g.Renamed = g.Renamed || renamed(held[0], s)
		}
		groups = append(groups, g)
	}
	return groups, next, nil
}

// compareUIDs orders two uids as numbers, as Dgraph gives them out.
func compareUIDs(a, b string) int {
	x, _ := strconv.ParseUint(strings.TrimPrefix(a, "0x"), 16, 64)
	y, _ := strconv.ParseUint(strings.TrimPrefix(b, "0x"), 16, 64)
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// disagree is that the person's own node and a stray each hold an email, and
// not the same one; an email only one of them holds is no disagreement.
func disagree(person, stray map[string]any) bool {
	p, s := email(person), email(stray)
	return p != "" && s != "" && p != s
}

// email is a node's email as it compares: trimmed, in lower case.
func email(n map[string]any) string {
	e, _ := n["user_email_id"].(string)
	return strings.ToLower(strings.TrimSpace(e))
}

// renamed is that the person's own node and a stray each hold a name, and not
// the same one.
func renamed(person, stray map[string]any) bool {
	for _, f := range []string{"user_name", "user_full_name"} {
		if p, s := person[f], stray[f]; p != nil && s != nil && p != s {
			return true
		}
	}
	return false
}

// StrayNode is a stray as the graph held it: its predicates by name, edges
// with their facets, as JSON.
type StrayNode struct {
	UserUUID, Person, Stray string
	Fields                  map[string]json.RawMessage
}

// StrayEdge is an edge into a stray, from Other, or out of one, to Other,
// with the edge's facets by "<predicate>|<facet>".
type StrayEdge struct {
	UserUUID, Person, Stray string
	Predicate, Other        string
	List                    bool
	// Added is that the person's own node had no such edge, so the merge gives
	// it one, which a restore takes away again.
	Added  bool
	Facets map[string]json.RawMessage
}

// StrayPlan is what merging some groups' strays changes, as read in one
// transaction.
type StrayPlan struct {
	Nodes   []StrayNode
	In, Out []StrayEdge
}

// PlanStrayMerge reads, in tx, each stray of the groups as it is, every edge
// into one (by the reverse edge where the schema keeps one, by a scan of the
// predicate where it doesn't) and the person's own edges the stray's would
// repeat.
func PlanStrayMerge(tx *dgexec.Txn, preds []GraphPredicate, groups []StrayGroup) (*StrayPlan, error) {
	of := map[string]StrayGroup{}
	var strays, ends []string
	for _, g := range groups {
		ends = append(ends, g.Person)
		for _, s := range g.Strays {
			of[s] = g
			strays = append(strays, s)
		}
	}
	ends = append(ends, strays...)
	plan := &StrayPlan{}
	if err := plan.readNodes(tx, preds, strays, of); err != nil {
		return nil, err
	}
	if err := plan.readOut(tx, preds, of); err != nil {
		return nil, err
	}
	if err := plan.readIn(tx, preds, strays, ends, of); err != nil {
		return nil, err
	}
	return plan, nil
}

// readNodes reads each stray with every predicate it holds.
func (p *StrayPlan) readNodes(tx *dgexec.Txn, preds []GraphPredicate, strays []string, of map[string]StrayGroup) error {
	fields := []string{"uid", "dgraph.type"}
	for _, pr := range preds {
		if pr.Type == "uid" {
			fields = append(fields, pr.Name+" @facets { uid }")
		} else {
			fields = append(fields, pr.Name)
		}
	}
	got, err := dgexec.ReadIn[struct {
		Strays []map[string]json.RawMessage `json:"strays"`
	}](tx, `{ strays(func: uid(`+strings.Join(strays, ", ")+`)) { `+strings.Join(fields, " ")+` } }`, nil)
	if err != nil {
		return err
	}
	for _, f := range got.Strays {
		var uid string
		_ = json.Unmarshal(f["uid"], &uid)
		g := of[uid]
		p.Nodes = append(p.Nodes, StrayNode{UserUUID: g.UserUUID, Person: g.Person, Stray: uid, Fields: f})
	}
	return nil
}

// targets are the nodes an edge field points at, as Dgraph answers a list
// (an array) or a single edge (an object), each with its facets.
func targets(raw json.RawMessage) []map[string]json.RawMessage {
	if len(raw) > 0 && raw[0] == '{' {
		raw = append(append(json.RawMessage{'['}, raw...), ']')
	}
	var out []map[string]json.RawMessage
	_ = json.Unmarshal(raw, &out)
	return out
}

// edgeEnd splits a target into its uid and its facets.
func edgeEnd(t map[string]json.RawMessage) (uid string, facets map[string]json.RawMessage) {
	_ = json.Unmarshal(t["uid"], &uid)
	for k, v := range t {
		if k != "uid" {
			if facets == nil {
				facets = map[string]json.RawMessage{}
			}
			facets[k] = v
		}
	}
	return uid, facets
}

// readOut lists each stray's own edges, Added where the person's own node
// has no such edge (for a single edge, no edge of that predicate at all).
func (p *StrayPlan) readOut(tx *dgexec.Txn, preds []GraphPredicate, of map[string]StrayGroup) error {
	type end struct{ stray, other string }
	byPred := map[string][]end{}
	var names []string
	facets := map[end]map[string]json.RawMessage{}
	list := map[string]bool{}
	for _, pr := range preds {
		if pr.Type != "uid" {
			continue
		}
		list[pr.Name] = pr.List
		for _, n := range p.Nodes {
			for _, t := range targets(n.Fields[pr.Name]) {
				uid, f := edgeEnd(t)
				e := end{n.Stray, uid}
				if byPred[pr.Name] == nil {
					names = append(names, pr.Name)
				}
				byPred[pr.Name] = append(byPred[pr.Name], e)
				facets[e] = f
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	// The person's own edges of each predicate a stray holds: for a list, only
	// those to the stray's objects.
	blocks := make([]string, len(names))
	for i, name := range names {
		var people, objects []string
		for _, e := range byPred[name] {
			people = append(people, of[e.stray].Person)
			objects = append(objects, e.other)
		}
		filter := ""
		if list[name] {
			filter = " @filter(uid(" + strings.Join(objects, ", ") + "))"
		}
		blocks[i] = fmt.Sprintf("o%d(func: uid(%s)) { uid %s%s { uid } }", i, strings.Join(people, ", "), name, filter)
	}
	got, err := dgexec.ReadIn[map[string][]map[string]json.RawMessage](tx, "{ "+strings.Join(blocks, "\n")+" }", nil)
	if err != nil {
		return err
	}
	type key struct{ person, pred, other string }
	has := map[key]bool{}
	for i, name := range names {
		for _, person := range got["o"+strconv.Itoa(i)] {
			var uid string
			_ = json.Unmarshal(person["uid"], &uid)
			for _, t := range targets(person[name]) {
				other, _ := edgeEnd(t)
				has[key{uid, name, other}] = true
				has[key{uid, name, ""}] = true
			}
		}
	}
	for _, name := range names {
		for _, e := range byPred[name] {
			g := of[e.stray]
			k := key{g.Person, name, e.other}
			if !list[name] {
				k.other = ""
			}
			added := !has[k]
			has[k] = true
			p.Out = append(p.Out, StrayEdge{UserUUID: g.UserUUID, Person: g.Person, Stray: e.stray,
				Predicate: name, Other: e.other, List: list[name], Added: added, Facets: facets[e]})
		}
	}
	return nil
}

// readIn lists every edge into a stray, Added where the subject's list
// doesn't hold the person's own node already (a single edge always moves).
// ends are the strays and their people.
func (p *StrayPlan) readIn(tx *dgexec.Txn, preds []GraphPredicate, strays, ends []string, of map[string]StrayGroup) error {
	inStrays := strings.Join(strays, ", ")
	onEnds := " @facets @filter(uid(" + strings.Join(ends, ", ") + ")) { uid }"
	var reverse []GraphPredicate
	for _, pr := range preds {
		if pr.Type != "uid" {
			continue
		}
		if pr.Reverse {
			reverse = append(reverse, pr)
			continue
		}
		// No reverse edge to follow: a scan of the predicate's subjects.
		got, err := dgexec.ReadIn[struct {
			In []map[string]json.RawMessage `json:"in"`
		}](tx, `{ in(func: has(`+pr.Name+`)) @filter(uid_in(`+pr.Name+`, [`+inStrays+`])) { uid `+pr.Name+onEnds+` } }`, nil)
		if err != nil {
			return err
		}
		p.addIn(pr, got.In, of)
	}
	if len(reverse) == 0 {
		return nil
	}
	vars := make([]string, len(reverse))
	blocks := make([]string, len(reverse))
	for i, pr := range reverse {
		vars[i] = fmt.Sprintf("s%d as ~%s", i, pr.Name)
		blocks[i] = fmt.Sprintf("i%d(func: uid(s%d)) { uid %s%s }", i, i, pr.Name, onEnds)
	}
	got, err := dgexec.ReadIn[map[string][]map[string]json.RawMessage](tx,
		"{ var(func: uid("+inStrays+")) { "+strings.Join(vars, " ")+" }\n"+strings.Join(blocks, "\n")+" }", nil)
	if err != nil {
		return err
	}
	for i, pr := range reverse {
		p.addIn(pr, got["i"+strconv.Itoa(i)], of)
	}
	return nil
}

// addIn adds the edges of pr from each subject to a stray.
func (p *StrayPlan) addIn(pr GraphPredicate, subjects []map[string]json.RawMessage, of map[string]StrayGroup) {
	for _, s := range subjects {
		var subject string
		_ = json.Unmarshal(s["uid"], &subject)
		ends := targets(s[pr.Name])
		holds := map[string]bool{}
		for _, t := range ends {
			uid, _ := edgeEnd(t)
			holds[uid] = true
		}
		for _, t := range ends {
			uid, facets := edgeEnd(t)
			g, stray := of[uid]
			if !stray {
				continue
			}
			added := !pr.List || !holds[g.Person]
			holds[g.Person] = true
			p.In = append(p.In, StrayEdge{UserUUID: g.UserUUID, Person: g.Person, Stray: uid,
				Predicate: pr.Name, Other: subject, List: pr.List, Added: added, Facets: facets})
		}
	}
}

// edgeSet is a set of one edge with its facets, as a JSON mutation takes it.
func edgeSet(subject, pred, object string, list bool, facets map[string]json.RawMessage) map[string]any {
	end := map[string]any{"uid": object}
	for k, v := range facets {
		end[k] = v
	}
	if list {
		return map[string]any{"uid": subject, pred: []any{end}}
	}
	return map[string]any{"uid": subject, pred: end}
}

// mutation is a set of nodes and edges and the N-Quads deletes, or nothing to
// set when there is none (a nil slice would be sent as null).
func mutation(set []map[string]any, del string) dgexec.Mutation {
	m := dgexec.Mutation{DelNQuads: del}
	if len(set) > 0 {
		m.Set = set
	}
	return m
}

// Mutation is the merge: each edge into a stray moved onto the person's own
// node (a single edge is replaced; a list's edge to the stray deleted, and
// one to the person added unless the list holds it), the stray's edges the
// person lacks added to them, and every predicate of each stray deleted. By
// name, as an untyped stray's `* *` deletes nothing.
func (p *StrayPlan) Mutation() dgexec.Mutation {
	var set []map[string]any
	var del strings.Builder
	for _, e := range p.In {
		if e.Added {
			set = append(set, edgeSet(e.Other, e.Predicate, e.Person, e.List, e.Facets))
		}
		if e.List {
			fmt.Fprintf(&del, "<%s> <%s> <%s> .\n", e.Other, e.Predicate, e.Stray)
		}
	}
	for _, e := range p.Out {
		if e.Added {
			set = append(set, edgeSet(e.Person, e.Predicate, e.Other, e.List, e.Facets))
		}
	}
	for _, n := range p.Nodes {
		for name := range n.Fields {
			if name != "uid" {
				fmt.Fprintf(&del, "<%s> <%s> * .\n", n.Stray, name)
			}
		}
	}
	return mutation(set, del.String())
}

// StrayRestoreMutation puts merged strays back: each node with every
// predicate it held, each edge into one pointed at it again, and each edge
// the merge added to the person's own node taken away.
func StrayRestoreMutation(nodes []StrayNode, in, out []StrayEdge) (dgexec.Mutation, error) {
	var set []map[string]any
	var del strings.Builder
	for _, n := range nodes {
		if !helpers.IsDgraphUID(n.Stray) {
			return dgexec.Mutation{}, fmt.Errorf("stray nodes: restoring %q, not a uid", n.Stray)
		}
		node := map[string]any{}
		for k, v := range n.Fields {
			node[k] = v
		}
		node["uid"] = n.Stray
		set = append(set, node)
	}
	for _, e := range append(slices.Clone(in), out...) {
		if !helpers.IsDgraphUID(e.Person) || !helpers.IsDgraphUID(e.Stray) || !helpers.IsDgraphUID(e.Other) || !predicateName.MatchString(e.Predicate) {
			return dgexec.Mutation{}, fmt.Errorf("stray nodes: restoring %s <%s> %s, not uids and a predicate", e.Other, e.Predicate, e.Stray)
		}
	}
	for _, e := range in {
		set = append(set, edgeSet(e.Other, e.Predicate, e.Stray, e.List, e.Facets))
		if e.Added && e.List {
			fmt.Fprintf(&del, "<%s> <%s> <%s> .\n", e.Other, e.Predicate, e.Person)
		}
	}
	for _, e := range out {
		if e.Added {
			fmt.Fprintf(&del, "<%s> <%s> <%s> .\n", e.Person, e.Predicate, e.Other)
		}
	}
	return mutation(set, del.String()), nil
}

// StraysLeft is which of the uids still hold a user_uuid.
func StraysLeft(ctx context.Context, uids []string) (map[string]bool, error) {
	if !dgraphquery.AllUIDs(uids) {
		return nil, fmt.Errorf("stray nodes: %v, not uids", uids)
	}
	got, err := dgexec.Read[struct {
		Left []struct {
			Uid string `json:"uid"`
		} `json:"left"`
	}](ctx, `{ left(func: uid(`+strings.Join(uids, ", ")+`)) @filter(has(user_uuid)) { uid } }`, nil)
	if err != nil {
		return nil, err
	}
	left := map[string]bool{}
	for _, n := range got.Left {
		left[n.Uid] = true
	}
	return left, nil
}
