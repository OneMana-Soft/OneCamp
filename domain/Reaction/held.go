package domain

// The reactions on one message or comment, as one person's emoji (chat B25,
// PROGRAM.md D-M7): read them, add one at most once, take some away. Every
// reaction in the product is written here, whatever it is on: a channel's
// message (post_reactions), a DM's or group's (chat_reactions), and a reply,
// a doc's comment or a task's (comment_reactions). Before, three writers each
// made a new node for every add, so two quick clicks, a retried request or an
// import run twice gave someone the same emoji twice.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// ErrNoParent is a message or comment that isn't there to react to.
var ErrNoParent = errors.New("no such message or comment")

// MaxHeld is the most reactions one read takes in. The rules cap a message at
// 50 emoji and a person at 23 of them (business/Reaction), so even a busy
// channel's message stays far below it.
const MaxHeld = 5000

// Held is one reaction as the rules read it: its node, its emoji and whose.
type Held struct {
	UID   string
	Emoji string
	By    string // the graph uid of who added it
}

// parent is how a reactions edge's owner is found: by its uuid, of its type.
type parent struct{ uuidPred, typ string }

var parents = map[string]parent{
	"post_reactions":    {"post_uuid", "Post"},
	"chat_reactions":    {"chat_uuid", "Chat"},
	"comment_reactions": {"comment_uuid", "Comment"},
}

func parentOf(edge string) (parent, error) {
	p, ok := parents[edge]
	if !ok {
		return parent{}, fmt.Errorf("no reactions edge %q", edge)
	}
	return p, nil
}

// heldQuery reads a parent's node and its reactions through the edge.
func heldQuery(edge string, p parent) string {
	return fmt.Sprintf(`query Held($id: string) {
		on(func: eq(%s, $id), first: 1) @filter(type(%s)) {
			uid
			reactions: %s (first: %d) {
				uid
				reaction_emoji_id
				reaction_added_by { uid }
			}
		}
	}`, p.uuidPred, p.typ, edge, MaxHeld)
}

// ReactionsOn is the parent's graph uid and every reaction it holds through
// edge, found by the parent's uuid. ErrNoParent when there is no such parent.
func ReactionsOn(ctx context.Context, edge, parentUUID string) (string, []Held, error) {
	p, err := parentOf(edge)
	if err != nil {
		return "", nil, err
	}
	var out struct {
		On []struct {
			UID       string `json:"uid"`
			Reactions []struct {
				UID     string `json:"uid"`
				Emoji   string `json:"reaction_emoji_id"`
				AddedBy *struct {
					UID string `json:"uid"`
				} `json:"reaction_added_by"`
			} `json:"reactions"`
		} `json:"on"`
	}
	if err := dgraphInit.ReadInto(ctx, heldQuery(edge, p), map[string]string{"$id": parentUUID}, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ReactionsOn %s %s err: %+v", edge, parentUUID, err)
		return "", nil, err
	}
	if len(out.On) == 0 || out.On[0].UID == "" {
		return "", nil, ErrNoParent
	}
	held := make([]Held, 0, len(out.On[0].Reactions))
	for _, r := range out.On[0].Reactions {
		h := Held{UID: r.UID, Emoji: r.Emoji}
		if r.AddedBy != nil {
			h.By = r.AddedBy.UID
		}
		held = append(held, h)
	}
	return out.On[0].UID, held, nil
}

// NewReaction is a reaction to add: whose, what, and whose the content is.
type NewReaction struct {
	By        string // graph uid of who reacts
	ContentBy string // graph uid of who wrote what they react to ("" when unknown)
	Emoji     string
	At        time.Time
}

// Key is a reaction's identity on its parent: one person's one emoji there
// (reaction_key). It is indexed with @upsert, so of two adds of it at once
// Dgraph commits one and aborts the other, which then finds it.
func Key(parentUID, by, emoji string) string {
	return parentUID + "|" + by + "|" + emoji
}

// AddOnce adds r through edge to the parent parentUUID names, unless that
// person already holds that emoji there by its key: then nothing is written
// and the reaction they hold answers. created says which. ErrNoParent when
// there is no such parent.
func AddOnce(ctx context.Context, edge, parentUUID, parentUID string, r NewReaction) (uid string, created bool, err error) {
	p, err := parentOf(edge)
	if err != nil {
		return "", false, err
	}
	ids := []string{parentUID, r.By}
	if r.ContentBy != "" {
		ids = append(ids, r.ContentBy)
	}
	if !dgraphquery.AllUIDs(ids) {
		return "", false, fmt.Errorf("domain/AddOnce: not graph uids: %v", ids)
	}
	if r.At.IsZero() {
		r.At = time.Now()
	}
	node := map[string]any{
		"uid":               "_:reaction",
		"dgraph.type":       "Reaction",
		"reaction_key":      Key(parentUID, r.By, r.Emoji),
		"reaction_emoji_id": r.Emoji,
		"reaction_added_at": r.At.UTC().Format(time.RFC3339Nano),
		"reaction_added_by": map[string]string{"uid": r.By},
	}
	if r.ContentBy != "" {
		node["reaction_on_content_added_by"] = map[string]string{"uid": r.ContentBy}
	}
	set, err := json.Marshal(map[string]any{"uid": "uid(p)", edge: []any{node}})
	if err != nil {
		return "", false, err
	}
	// The parent is found by its uuid and must be the node read before (a
	// uuid that now names another node writes nothing).
	query := fmt.Sprintf(`query Add($id: string, $key: string) {
		p as var(func: eq(%s, $id), first: 1) @filter(type(%s) AND uid(%s))
		k as var(func: eq(reaction_key, $key))
		held(func: uid(k)) { uid }
	}`, p.uuidPred, p.typ, parentUID)
	res, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query: query,
		Vars:  map[string]string{"$id": parentUUID, "$key": Key(parentUID, r.By, r.Emoji)},
		Mutations: []*api.Mutation{{
			Cond:    "@if(eq(len(p), 1) AND eq(len(k), 0))",
			SetJson: set,
		}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/AddOnce %s %s err: %+v", edge, parentUUID, err)
		return "", false, err
	}
	if uid := res.GetUids()["reaction"]; uid != "" {
		return uid, true, nil
	}
	var found struct {
		Held []struct {
			UID string `json:"uid"`
		} `json:"held"`
	}
	if err := json.Unmarshal(res.GetJson(), &found); err != nil {
		return "", false, err
	}
	if len(found.Held) > 0 && found.Held[0].UID != "" {
		return found.Held[0].UID, false, nil
	}
	return "", false, ErrNoParent
}

// Retag changes the reaction uid on the parent parentUID (through edge) to be
// by's emoji, keeping its node, unless by already holds that emoji there by
// its key. Answers whether it changed. uid must be by's reaction on the
// parent (business/Reaction checks): this writes what it is given.
func Retag(ctx context.Context, edge, parentUID, uid, by, emoji string, at time.Time) (bool, error) {
	if _, err := parentOf(edge); err != nil {
		return false, err
	}
	if !dgraphquery.AllUIDs([]string{parentUID, uid, by}) {
		return false, fmt.Errorf("domain/Retag: not graph uids")
	}
	key := Key(parentUID, by, emoji)
	set, err := json.Marshal(map[string]any{
		"uid":               "uid(r)",
		"reaction_emoji_id": emoji,
		"reaction_key":      key,
		"reaction_added_at": at.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return false, err
	}
	res, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query: fmt.Sprintf(`query Retag($key: string) {
			var(func: uid(%s)) { r as %s @filter(uid(%s) AND type(Reaction)) }
			k as var(func: eq(reaction_key, $key))
			held(func: uid(k)) { uid }
			named(func: uid(r)) { uid }
		}`, parentUID, edge, uid),
		Vars:      map[string]string{"$key": key},
		Mutations: []*api.Mutation{{Cond: "@if(eq(len(r), 1) AND eq(len(k), 0))", SetJson: set}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/Retag %s %s err: %+v", edge, uid, err)
		return false, err
	}
	var found struct {
		Held  []struct{ UID string } `json:"held"`
		Named []struct{ UID string } `json:"named"`
	}
	if err := json.Unmarshal(res.GetJson(), &found); err != nil {
		return false, err
	}
	return len(found.Named) == 1 && len(found.Held) == 0, nil
}

// RemoveFrom takes the reactions uids off the parent parentUID through edge
// and empties their nodes. Each uid must be one the parent holds (read
// through ReactionsOn or checked by business/Reaction's MayChange): this
// writes what it is given.
func RemoveFrom(ctx context.Context, edge, parentUID string, uids []string) error {
	if _, err := parentOf(edge); err != nil {
		return err
	}
	if len(uids) == 0 {
		return nil
	}
	if !dgraphquery.AllUIDs(append([]string{parentUID}, uids...)) {
		return fmt.Errorf("domain/RemoveFrom: not graph uids")
	}
	off := make([]map[string]string, 0, len(uids))
	for _, uid := range uids {
		off = append(off, map[string]string{"uid": uid})
	}
	del := []any{map[string]any{"uid": parentUID, edge: off}}
	for _, uid := range uids {
		del = append(del, map[string]any{
			"uid":                          uid,
			"reaction_key":                 nil,
			"reaction_emoji_id":            nil,
			"reaction_added_at":            nil,
			"reaction_added_by":            nil,
			"reaction_on_content_added_by": nil,
		}, map[string]string{"uid": uid})
	}
	body, err := json.Marshal(del)
	if err != nil {
		return err
	}
	if _, err := dgraphInit.DoCommitNow(ctx, &api.Request{Mutations: []*api.Mutation{{DeleteJson: body}}}); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RemoveFrom %s %s err: %+v", edge, parentUID, err)
		return err
	}
	return nil
}
