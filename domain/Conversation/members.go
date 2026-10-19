package domain

// The conversations index (Postgres: conversations, conversation_members), kept
// beside the graph's membership edges by the functions that write those edges:
// business/Channel's member, bot and moderator edges and a channel's making, a
// DM's first message, a group's, and business/Conversation's group opens.
//
// THE ORDER. The index must never grant more than the graph: an addition is
// written to it after the graph took it, a removal before the graph's (and put
// back when the graph's fails). A write to it that fails is logged and marks it
// dirty (source.go), which sends every decision back to the graph until the
// reconcile has repaired it. So a failed write here never fails the change
// that caused it, except a removal, which the index must take first.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	"github.com/google/uuid"
)

// The kinds of conversation (business/Conversation's KindChannel, KindDM and
// KindGroup).
const (
	KindChannel = models.KindChannel
	KindDM      = models.KindDM
	KindGroup   = models.KindGroup
)

// The roles a member has.
const (
	RoleMember  = models.RoleMember
	RoleManager = models.RoleManager
)

// Seams for tests: the rows' writes and the graph's uid lookup.
var (
	ensureRow  = models.Ensure
	joinRows   = models.Join
	leaveRows  = models.Leave
	setRole    = models.SetRole
	memberOf   = models.MemberOf
	uuidsByUID = uuidsOfUIDs
)

// rowID is the id a new conversations row gets: a channel's own uuid, else a
// new one.
func rowID(kind, ref string) uuid.UUID {
	if kind == KindChannel {
		if id, err := uuid.Parse(ref); err == nil {
			return id
		}
	}
	return uuid.New()
}

// conversation is the row for kind and ref, made when there is none.
func conversation(ctx context.Context, kind, ref, creator string) (uuid.UUID, error) {
	if ref == "" || (kind != KindChannel && kind != KindDM && kind != KindGroup) {
		return uuid.Nil, fmt.Errorf("no conversation %q of kind %q", ref, kind)
	}
	c := models.Conversation{ID: rowID(kind, ref), Kind: kind, Ref: ref}
	if id, err := uuid.Parse(creator); err == nil {
		c.CreatedBy = &id
	}
	return ensureRow(ctx, c)
}

// people are the ids that are uuids; anything else is left out, and said.
func people(ctx context.Context, ids []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, s := range ids {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/Conversation left out a member who isn't a person's id: %q", s)
			continue
		}
		out = append(out, id)
	}
	return out
}

// Opened records a conversation as it is made: its people, and its managers
// among them (a channel's maker).
func Opened(ctx context.Context, kind, ref, creator string, members, managers []string) {
	id, err := conversation(ctx, kind, ref, creator)
	if err == nil {
		now := time.Now()
		err = joinRows(ctx, id, people(ctx, members), RoleMember, now)
		if err == nil {
			err = joinRows(ctx, id, people(ctx, managers), RoleManager, now)
		}
	}
	if err != nil {
		failed(ctx, "opening "+kind+" "+ref, err)
	}
}

// Added records people joining a conversation, as role.
func Added(ctx context.Context, kind, ref, role string, members ...string) {
	if len(members) == 0 {
		return
	}
	id, err := conversation(ctx, kind, ref, "")
	if err == nil {
		err = joinRows(ctx, id, people(ctx, members), role, time.Now())
	}
	if err != nil {
		failed(ctx, "adding to "+kind+" "+ref, err)
	}
}

// AddedByUID is Added for people named by their graph uids (a bot's, a new
// channel manager's).
func AddedByUID(ctx context.Context, kind, ref, role string, uids ...string) {
	ids, err := uuidsByUID(ctx, uids)
	if err != nil {
		failed(ctx, "adding to "+kind+" "+ref, err)
		return
	}
	Added(ctx, kind, ref, role, ids...)
}

// Remove takes people out of a conversation: out of the index first, then out
// of the graph by write. When write fails they are put back in the index as
// they were, and its error is answered. A removal the index can't take is not
// made at all: its error is answered and write isn't called.
func Remove(ctx context.Context, kind, ref string, members []string, write func() error) error {
	ids := people(ctx, members)
	id, err := conversation(ctx, kind, ref, "")
	if err != nil {
		return fmt.Errorf("taking people out of %s %s: %w", kind, ref, err)
	}
	// As they are now, to put back.
	was := map[uuid.UUID]*models.Member{}
	for _, p := range ids {
		m, err := memberOf(ctx, id, p)
		if err != nil {
			return fmt.Errorf("taking people out of %s %s: %w", kind, ref, err)
		}
		was[p] = m
	}
	if err := leaveRows(ctx, id, ids, time.Now()); err != nil {
		return fmt.Errorf("taking people out of %s %s: %w", kind, ref, err)
	}
	werr := write()
	if werr == nil {
		return nil
	}
	// The graph kept them: so does the index.
	for p, m := range was {
		if m == nil || !m.Live() {
			continue
		}
		if err := joinRows(ctx, id, []uuid.UUID{p}, m.Role, m.JoinedAt); err != nil {
			failed(ctx, "putting someone back in "+kind+" "+ref, err)
		}
	}
	return werr
}

// RemoveByUID is Remove for people named by their graph uids.
func RemoveByUID(ctx context.Context, kind, ref string, uids []string, write func() error) error {
	ids, err := uuidsByUID(ctx, uids)
	if err != nil {
		return fmt.Errorf("taking people out of %s %s: %w", kind, ref, err)
	}
	return Remove(ctx, kind, ref, ids, write)
}

// Promoted makes a member, named by their graph uid, a manager. Someone who
// isn't a live member stays as they are: in the graph a manager's edge alone
// gives nothing, membership is asked first.
func Promoted(ctx context.Context, kind, ref, uid string) {
	ids, err := uuidsByUID(ctx, []string{uid})
	if err == nil && len(ids) == 1 {
		person, _ := uuid.Parse(ids[0])
		var id uuid.UUID
		if id, err = conversation(ctx, kind, ref, ""); err == nil {
			err = setRole(ctx, id, person, RoleManager, time.Now())
		}
	}
	if err != nil {
		failed(ctx, "making a manager in "+kind+" "+ref, err)
	}
}

// Demote makes a manager a member: in the index first, then in the graph by
// write, and back when write fails.
func Demote(ctx context.Context, kind, ref, uid string, write func() error) error {
	ids, err := uuidsByUID(ctx, []string{uid})
	if err != nil {
		return fmt.Errorf("demoting in %s %s: %w", kind, ref, err)
	}
	if len(ids) == 0 {
		return write()
	}
	person, _ := uuid.Parse(ids[0])
	id, err := conversation(ctx, kind, ref, "")
	if err != nil {
		return fmt.Errorf("demoting in %s %s: %w", kind, ref, err)
	}
	if err := setRole(ctx, id, person, RoleMember, time.Now()); err != nil {
		return fmt.Errorf("demoting in %s %s: %w", kind, ref, err)
	}
	if werr := write(); werr != nil {
		if err := setRole(ctx, id, person, RoleManager, time.Now()); err != nil {
			failed(ctx, "putting a manager back in "+kind+" "+ref, err)
		}
		return werr
	}
	return nil
}

// failed logs a write to the index that didn't happen and marks it dirty.
func failed(ctx context.Context, what string, err error) {
	helpers.LogErrorWithContext(ctx, "domain/Conversation could not record %s in the conversations index: %+v", what, err)
	MarkDirty(ctx, what+": "+err.Error())
}

// uuidsOfUIDs is the people's uuids for their graph uids, in one read. A uid
// that names nobody is left out.
func uuidsOfUIDs(ctx context.Context, uids []string) ([]string, error) {
	var list []string
	for _, u := range uids {
		if helpers.IsDgraphUID(u) {
			list = append(list, u)
		}
	}
	if len(list) == 0 {
		return nil, nil
	}
	if dgraphInit.DgraphClient == nil {
		return nil, errors.New("no graph connection")
	}
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx,
		`{ people(func: uid(`+strings.Join(list, ", ")+`)) @filter(has(user_uuid)) { user_uuid } }`)
	if err != nil {
		return nil, err
	}
	return parsePeopleUUIDs(resp.Json)
}
