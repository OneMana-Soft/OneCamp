package domain

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	"github.com/google/uuid"
)

// DocsOwnedBy is up to first of the docs a person owns, live or in the
// trash, and how many there are, by the owner rule the lists use (ListDocs'
// OwnerUUID): a doc's owner, or its maker while it has none. For handing a
// departed member's docs on (docs C6-2): the trash too, since its owner is
// who restores a doc.
func DocsOwnedBy(ctx context.Context, userUUID string, first int) ([]string, int, error) {
	if first < 1 {
		first = 1
	}
	query := fmt.Sprintf(`query owned($owner: string) {
		var(func: eq(user_uuid, $owner)) {
			theirs as ~doc_owner
			theyMade as ~doc_created_by
		}
		cands as var(func: uid(theirs, theyMade)) @filter(has(doc_uuid) AND (uid(theirs) OR NOT has(doc_owner)))
		doc_count(func: uid(cands)) {
			count: count(uid)
		}
		docs(func: uid(cands), orderasc: doc_uuid, first: %d) {
			doc_uuid
		}
	}`, first)
	out, err := dgexec.Read[struct {
		Count []struct {
			Count int `json:"count"`
		} `json:"doc_count"`
		Docs []struct {
			UUID string `json:"doc_uuid"`
		} `json:"docs"`
	}](ctx, query, map[string]string{"$owner": userUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DocsOwnedBy err: %+v", err)
		return nil, 0, err
	}
	ids := make([]string, 0, len(out.Docs))
	for _, d := range out.Docs {
		if d.UUID != "" {
			ids = append(ids, d.UUID)
		}
	}
	total := 0
	if len(out.Count) > 0 {
		total = out.Count[0].Count
	}
	return ids, total, nil
}

// ownedCount is one person's row of DocsOwnedCounts: the docs they own, and
// the docs they made that have no owner.
type ownedCount struct {
	UUID  string `json:"user_uuid"`
	Owned []struct {
		N int `json:"n"`
	} `json:"owned"`
	Made []struct {
		N int `json:"n"`
	} `json:"made"`
}

// DocsOwnedCounts is how many docs each person owns, live or in the trash, by
// DocsOwnedBy's rule, in one read for every person: a person who owns none is
// left out. Keys are lower-case uuids.
func DocsOwnedCounts(ctx context.Context, people []uuid.UUID) (map[string]int, error) {
	counts := map[string]int{}
	list := dgraphquery.UUIDListOf(people)
	if list == "" {
		return counts, nil
	}
	// The two edges are apart: a doc whose owner is the person, and one they
	// made that has no owner, so nothing is counted twice.
	query := `{
		people(func: eq(user_uuid, [` + list + `])) {
			user_uuid
			owned: ~doc_owner @filter(has(doc_uuid)) { n: count(uid) }
			made: ~doc_created_by @filter(has(doc_uuid) AND NOT has(doc_owner)) { n: count(uid) }
		}
	}`
	out, err := dgexec.Read[struct {
		People []ownedCount `json:"people"`
	}](ctx, query, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DocsOwnedCounts err: %+v", err)
		return nil, err
	}
	for _, p := range out.People {
		n := 0
		for _, c := range p.Owned {
			n += c.N
		}
		for _, c := range p.Made {
			n += c.N
		}
		if n > 0 {
			counts[strings.ToLower(p.UUID)] += n
		}
	}
	return counts, nil
}
