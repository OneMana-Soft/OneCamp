package business

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// Access taken away: a doc's or a board's people losing their level or role
// there. The change leaves no node or edge behind that says who held what, so
// its audit row, written before it (RecordedAs: no record, no action), is the
// lasting record of whom it was taken from, by whom and when, as a team's
// (V-243) and a channel's (T1) people taken out are recorded.

// accessRank orders the levels a doc or a board gives, lowest first; "" is
// none. A board's editors, commenters and viewers are its edit, comment and
// view levels.
var accessRank = map[string]int{"": 0, "view": 1, "comment": 2, "edit": 3, "full": 4}

// AccessShift is one person's access as a change moves it: the level they had
// (From) and the level they have after it (To), "" for none.
type AccessShift struct {
	UserUUID string `json:"user_uuid"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// Taken is the shifts whose level falls, in their order: the people a change
// takes access from, or lowers. Pure.
func Taken(shifts []AccessShift) []AccessShift {
	var out []AccessShift
	for _, s := range shifts {
		if accessRank[s.To] < accessRank[s.From] {
			out = append(out, s)
		}
	}
	return out
}

// Given is the shifts whose level rises, in their order: the people a change
// gives access to, or raises. A grant is told after the change by the
// resource's own events; a taking is recorded before it (RecordedTaking). Pure.
func Given(shifts []AccessShift) []AccessShift {
	var out []AccessShift
	for _, s := range shifts {
		if accessRank[s.To] > accessRank[s.From] {
			out = append(out, s)
		}
	}
	return out
}

// RoleShifts is how a change to who holds which role on something moves each
// person it names. before is each holder's level now; add and remove are the
// change's user uuids by level ("edit", "comment", "view"). A person ends with
// the highest level they hold after the change. Ids are matched as the role
// writer stores them, in lower case. People the change doesn't name, and names
// that move nothing, are left out; the order is the change's first mention of
// each person. Pure.
func RoleShifts(before map[string]string, add, remove map[string][]string) []AccessShift {
	held := map[string]map[string]bool{}
	was := map[string]string{}
	for id, l := range before {
		id = strings.ToLower(id)
		held[id] = map[string]bool{l: true}
		was[id] = l
	}
	var order []string
	seen := map[string]bool{}
	note := func(id string) {
		if !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
		if held[id] == nil {
			held[id] = map[string]bool{}
		}
	}
	for _, l := range []string{"edit", "comment", "view"} {
		for _, id := range remove[l] {
			if id = strings.ToLower(id); id != "" {
				note(id)
				delete(held[id], l)
			}
		}
	}
	for _, l := range []string{"edit", "comment", "view"} {
		for _, id := range add[l] {
			if id = strings.ToLower(id); id != "" {
				note(id)
				held[id][l] = true
			}
		}
	}
	var out []AccessShift
	for _, id := range order {
		to := ""
		for l := range held[id] {
			if accessRank[l] > accessRank[to] {
				to = l
			}
		}
		if from := was[id]; from != to {
			out = append(out, AccessShift{UserUUID: id, From: from, To: to})
		}
	}
	return out
}

// AccessMeta is the audit metadata of a change that takes access away: the
// resource's uuid under kind ("doc_uuid", "board_uuid") and each person it
// takes access from or lowers ("people": user_uuid, from, to). Every such row
// names them alike, so an audit reads a doc's and a board's the same way.
// Never a title or a body: an admin reading the log may not open the doc.
func AccessMeta(kind, resourceUUID string, taken []AccessShift) map[string]interface{} {
	people := make([]map[string]string, 0, len(taken))
	for _, s := range taken {
		people = append(people, map[string]string{"user_uuid": s.UserUUID, "from": s.From, "to": s.To})
	}
	return map[string]interface{}{kind + "_uuid": resourceUUID, "people": people}
}

// Principal is who an audit row names when it's written with no request to
// read: the accountable person's Postgres id and email, and whether a person,
// an agent or the workspace itself acted (ActorHuman, ActorAgent, ActorSystem).
type Principal struct {
	ID    *uuid.UUID
	Email string
	Kind  string
}

// ContextPrincipal is who acts in ctx: the person whose request it carries, or
// an agent acting with their authority when agentUUID is set, or the workspace
// itself (a sweep, the demo reset) when ctx names no one.
func ContextPrincipal(ctx context.Context, agentUUID string) Principal {
	id, email, ok := ActorFromContext(ctx)
	switch {
	case !ok:
		return Principal{Kind: ActorSystem}
	case agentUUID != "":
		return Principal{ID: &id, Email: email, Kind: ActorAgent}
	default:
		return Principal{ID: &id, Email: email, Kind: ActorHuman}
	}
}

// RecordedTaking makes change, which may take access away, as RecordedAs
// does when it takes any (its row's action, summary and AccessMeta, with
// extra keys added), and at once when it takes none: a grant is told by the
// doc's own events.
func RecordedTaking(ctx context.Context, p Principal, action, summary, kind, resourceUUID string, shifts []AccessShift, extra map[string]interface{}, change func() error) error {
	taken := Taken(shifts)
	if len(taken) == 0 {
		return change()
	}
	meta := AccessMeta(kind, resourceUUID, taken)
	for k, v := range extra {
		meta[k] = v
	}
	return RecordedAs(ctx, p, action, takingCategory(kind), summary, meta, change)
}

// takingCategory is the log's area for a taking on a resource of kind: a
// board's rows sit with its other rows under "boards"; docs, and the kinds
// that share their access rules, under "docs".
func takingCategory(kind string) string {
	if kind == "board" {
		return CategoryBoards
	}
	return CategoryDocs
}
