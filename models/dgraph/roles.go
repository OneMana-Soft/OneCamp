package models

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// ErrRoleIDs refuses a role change that names its node, or one of its people,
// by anything but a uuid.
var ErrRoleIDs = errors.New("a role change names its node and its people by their uuids")

// RoleChange is one role on a node: the people (user uuids) a change gives it
// to and takes it from. Predicate is the node's edge to those who hold it.
type RoleChange struct {
	Predicate   string
	Add, Remove []string
}

// ChangeRoles writes role changes on the node whose nodePred is nodeUUID in
// one upsert, as a board's and a doc's sharing do. The ids reach Dgraph only
// as query variables, never as query text. A person's edges are written only
// when the node and that person exist: an upsert variable that matched
// nothing is made a new, empty node, so an id that named no one used to give
// the role to a node that was nobody. ErrRoleIDs when an id isn't a uuid; ""
// names no one and is skipped.
func ChangeRoles(ctx context.Context, nodePred, nodeUUID string, changes []RoleChange) error {
	q, vars, muts, err := roleUpsert(nodePred, nodeUUID, changes)
	if err != nil || len(muts) == 0 {
		return err
	}
	_, err = dgexec.Mutate(ctx, q, vars, muts...)
	return err
}

// roleUpsert is ChangeRoles' request: a query naming the node (node) and each
// person (p0, p1, ...) by variables, and one mutation per person, which sets
// and deletes that person's edges.
func roleUpsert(nodePred, nodeUUID string, changes []RoleChange) (string, map[string]string, []dgexec.Mutation, error) {
	node, ok := storedUUID(nodeUUID)
	if !ok {
		return "", nil, nil, ErrRoleIDs
	}
	vars := map[string]string{"$node": node}
	params := []string{"$node: string"}
	blocks := []string{"node as var(func: eq(" + nodePred + ", $node))"}
	type person struct {
		id, name string
		set, del map[string]any
	}
	var people []*person
	byID := map[string]*person{}
	for _, c := range changes {
		for i, ids := range [][]string{c.Add, c.Remove} {
			for _, raw := range ids {
				if raw == "" {
					continue
				}
				id, ok := storedUUID(raw)
				if !ok {
					return "", nil, nil, ErrRoleIDs
				}
				p := byID[id]
				if p == nil {
					p = &person{id: id, name: "p" + strconv.Itoa(len(people))}
					byID[id] = p
					people = append(people, p)
				}
				side, target := &p.set, p.name
				if i == 1 {
					side, target = &p.del, p.name+"all"
				}
				if *side == nil {
					*side = map[string]any{"uid": "uid(node)"}
				}
				(*side)[c.Predicate] = []map[string]string{{"uid": "uid(" + target + ")"}}
			}
		}
	}
	muts := make([]dgexec.Mutation, 0, len(people))
	for _, p := range people {
		vars["$"+p.name] = p.id
		params = append(params, "$"+p.name+": string")
		blocks = append(blocks, p.name+" as var(func: "+dgraphquery.PersonByUUID("$"+p.name)+")")
		if p.del != nil {
			// A removal reaches every node holding the uuid: imports made more of
			// them, and adds went through each of those before they named the first.
			blocks = append(blocks, p.name+"all as var(func: eq(user_uuid, $"+p.name+"))")
		}
		m := dgexec.Mutation{Cond: "@if(eq(len(node), 1) AND gt(len(" + p.name + "), 0))"}
		if p.set != nil {
			m.Set = p.set
		}
		if p.del != nil {
			m.Delete = p.del
		}
		muts = append(muts, m)
	}
	q := "query roles(" + strings.Join(params, ", ") + ") {\n\t" + strings.Join(blocks, "\n\t") + "\n}"
	return q, vars, muts, nil
}

// storedUUID is id as uuids are stored (lower case), when it is a uuid.
func storedUUID(id string) (string, bool) {
	if !helpers.IsCanonicalUUID(id) {
		return "", false
	}
	return strings.ToLower(id), true
}
