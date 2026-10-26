package domain

// Writing who may do what with docs: grants to people and teams, general
// access, the owner and the lock (the Access Resolver's writes; business/Doc
// docAccessApply.go decides what to write). One upsert per change, so a change
// to a page and every page under it lands in one transaction or not at all.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// The lists a person can be on, strongest first. Someone is on one list at
// most: a write that puts them on one takes them off the others.
var personLists = map[string]string{
	"full":    "doc_full_access_users",
	"edit":    "doc_editing_users",
	"comment": "doc_commenting_users",
	"view":    "doc_reading_users",
}

// The lists a team can be on.
var teamLists = map[string]string{
	"edit":    "doc_team_editors",
	"comment": "doc_team_commenters",
	"view":    "doc_team_viewers",
}

// AccessWrite is one change to docs' sharing.
type AccessWrite struct {
	// People and Teams put each person or team (by uuid) on the list of a
	// level ("full", "edit", "comment" or "view"; teams have no "full"), or
	// with "" on none.
	People map[string]string
	Teams  map[string]string
	// ClearPeople and ClearTeams take every person or team off the docs first,
	// so the docs end with exactly the grants named (a page that adopts its
	// new parent's sharing).
	ClearPeople bool
	ClearTeams  bool
	// Private, PublicComment and PublicEdit set the docs' general access.
	Private       *bool
	PublicComment *bool
	PublicEdit    *bool
	// OwnerUUID hands the docs to that person.
	OwnerUUID string
	// Lock sets the lock ("" lifts it); LockedByUUID is who set it.
	Lock         *string
	LockedByUUID string
	At           time.Time
}

// ErrNoSuchPrincipal is a person or a team a write names that isn't there.
var ErrNoSuchPrincipal = fmt.Errorf("domain/doc: no such person or team")

// WriteDocAccess writes one change to the docs (by uuid) in one transaction.
// Docs that aren't there are left out; a person or team that isn't there is
// ErrNoSuchPrincipal, and nothing is written.
func WriteDocAccess(ctx context.Context, docUUIDs []string, w AccessWrite) error {
	docs := quoteUUIDs(docUUIDs)
	if len(docs) == 0 {
		return ErrNotFound
	}
	var q strings.Builder
	var set, del strings.Builder
	var checks []string
	q.WriteString("query {\n\t\tdocs as var(func: eq(doc_uuid, [" + strings.Join(docs, ", ") + "]))\n")

	name := func(prefix string, i int) string { return fmt.Sprintf("%s%d", prefix, i) }
	people := sortedKeys(w.People)
	for i, id := range people {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return ErrNoSuchPrincipal
		}
		v := name("p", i)
		fmt.Fprintf(&q, "\t\t%s as var(func: %s)\n", v, dgraphquery.PersonByUUID(fmt.Sprintf("%q", parsed.String())))
		checks = append(checks, "eq(len("+v+"), 1)")
		for _, list := range sortedValues(personLists) {
			fmt.Fprintf(&del, "uid(docs) <%s> uid(%s) .\n", list, v)
		}
		if lvl := w.People[id]; lvl != "" {
			list, ok := personLists[lvl]
			if !ok {
				return fmt.Errorf("domain/WriteDocAccess: no level %q for a person", lvl)
			}
			fmt.Fprintf(&set, "uid(docs) <%s> uid(%s) .\n", list, v)
		}
	}
	teams := sortedKeys(w.Teams)
	for i, id := range teams {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return ErrNoSuchPrincipal
		}
		v := name("t", i)
		fmt.Fprintf(&q, "\t\t%s as var(func: eq(team_uuid, %q)) @filter(%s)\n", v, parsed.String(), liveTeamFilter)
		checks = append(checks, "eq(len("+v+"), 1)")
		for _, list := range sortedValues(teamLists) {
			fmt.Fprintf(&del, "uid(docs) <%s> uid(%s) .\n", list, v)
		}
		if lvl := w.Teams[id]; lvl != "" {
			list, ok := teamLists[lvl]
			if !ok {
				return fmt.Errorf("domain/WriteDocAccess: no level %q for a team", lvl)
			}
			fmt.Fprintf(&set, "uid(docs) <%s> uid(%s) .\n", list, v)
		}
	}
	if w.ClearPeople {
		for _, list := range sortedValues(personLists) {
			fmt.Fprintf(&del, "uid(docs) <%s> * .\n", list)
		}
	}
	if w.ClearTeams {
		for _, list := range sortedValues(teamLists) {
			fmt.Fprintf(&del, "uid(docs) <%s> * .\n", list)
		}
	}
	setBool := func(pred string, v *bool) {
		if v != nil {
			fmt.Fprintf(&set, "uid(docs) <%s> \"%t\" .\n", pred, *v)
		}
	}
	setBool("doc_private", w.Private)
	setBool("doc_public_comment", w.PublicComment)
	setBool("doc_public_edit", w.PublicEdit)
	if w.OwnerUUID != "" {
		parsed, err := uuid.Parse(w.OwnerUUID)
		if err != nil {
			return ErrNoSuchPrincipal
		}
		fmt.Fprintf(&q, "\t\towner as var(func: %s)\n", dgraphquery.PersonByUUID(fmt.Sprintf("%q", parsed.String())))
		checks = append(checks, "eq(len(owner), 1)")
		set.WriteString("uid(docs) <doc_owner> uid(owner) .\n")
	}
	if w.Lock != nil {
		if *w.Lock == "" {
			del.WriteString("uid(docs) <doc_lock_level> * .\nuid(docs) <doc_locked_by> * .\nuid(docs) <doc_locked_at> * .\n")
		} else {
			lockJSON, _ := json.Marshal(*w.Lock)
			at, _ := json.Marshal(w.At.UTC().Format(time.RFC3339Nano))
			fmt.Fprintf(&set, "uid(docs) <doc_lock_level> %s .\nuid(docs) <doc_locked_at> %s .\n", lockJSON, at)
			del.WriteString("uid(docs) <doc_locked_by> * .\n")
			if parsed, err := uuid.Parse(w.LockedByUUID); err == nil {
				fmt.Fprintf(&q, "\t\tlocker as var(func: %s)\n", dgraphquery.PersonByUUID(fmt.Sprintf("%q", parsed.String())))
				set.WriteString("uid(docs) <doc_locked_by> uid(locker) .\n")
			}
		}
	}
	if set.Len() == 0 && del.Len() == 0 {
		return nil
	}
	// Every person and team named must be there, and so must a doc.
	q.WriteString("\t\tfound(func: uid(docs)) { doc_uuid }\n")
	for i := range people {
		fmt.Fprintf(&q, "\t\tfound_%s(func: uid(%s)) { uid }\n", name("p", i), name("p", i))
	}
	for i := range teams {
		fmt.Fprintf(&q, "\t\tfound_%s(func: uid(%s)) { uid }\n", name("t", i), name("t", i))
	}
	if w.OwnerUUID != "" {
		q.WriteString("\t\tfound_owner(func: uid(owner)) { uid }\n")
	}
	if w.Lock != nil && *w.Lock != "" && strings.Contains(q.String(), "locker as") {
		q.WriteString("\t\tfound_locker(func: uid(locker)) { uid }\n")
	}
	q.WriteString("\t}")
	cond := "@if(gt(len(docs), 0)"
	for _, c := range checks {
		cond += " AND " + c
	}
	cond += ")"
	mu := &api.Mutation{Cond: cond}
	if set.Len() > 0 {
		mu.SetNquads = []byte(set.String())
	}
	if del.Len() > 0 {
		mu.DelNquads = []byte(del.String())
	}
	resp, err := dgraphInit.DoCommitNow(ctx, &api.Request{Query: q.String(), Mutations: []*api.Mutation{mu}})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WriteDocAccess err: %+v", err)
		return err
	}
	var found map[string][]json.RawMessage
	if err := json.Unmarshal(resp.GetJson(), &found); err != nil {
		return err
	}
	if len(found["found"]) == 0 {
		return ErrNotFound
	}
	for k, v := range found {
		if k != "found" && k != "found_locker" && len(v) != 1 {
			return ErrNoSuchPrincipal
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
